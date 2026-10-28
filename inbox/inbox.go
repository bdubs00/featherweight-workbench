// Package inbox answers what needs the caller now (docs/ARCHITECTURE.md
// section 23.3): the runs that wait for an approval or an answer, the
// questions of the seats of the bridges, the failed runs of the duties of
// the caller in the last day, the duties of the caller that paused
// themselves, the results that the caller asked to check, and the results
// of the duties that found something and tell the caller of them. It mounts
// GET /v1/inbox on the native surface and adds the MCP tool inbox, which
// calls the same Go function, so a client of a person reads the items of
// Activity and answers them with approve, send, and work_review.
//
// The package keeps no truth of its own. The runs that wait come from the
// home of the dashboard (dashboard.HomeOf, its list needsYou), the
// questions of the seats from the bridges (bridges.Store.Waiting, the
// count needsYou of a bridge), and the duties from their store.
package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	"github.com/bdubs00/featherweight-workbench/teams"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	native.Extend(mount)
	mcp.RegisterTools(tools)
}

// Kinds of an item.
const (
	KindApproval = "approval" // a run waits for a decision to call a tool
	KindQuestion = "question" // a run or a seat waits for an answer
	KindFailure  = "failure"  // a run of a duty of the caller failed in the last day
	KindPaused   = "paused"   // a duty of the caller paused itself
	KindResult   = "result"   // a result that the caller asked to check waits for the check
)

// ResultsFor is how far back the results to check reach.
const ResultsFor = 7 * 24 * time.Hour

// FailuresFor is how far back the failed runs of the duties reach.
const FailuresFor = 24 * time.Hour

// Item is one thing that needs the caller: its kind, the run, the duty,
// the agent, and the bridge that it concerns, each "" when it does not
// apply, a text in words, and since when it waits.
type Item struct {
	Kind   string `json:"kind"`
	Run    string `json:"run"`
	Duty   string `json:"duty"`
	Agent  string `json:"agent"`
	Bridge string `json:"bridge"`
	// Approval is the id of the approval that the run waits for, so a
	// surface decides it without a read of the run. A question of a seat
	// of a bridge has none.
	Approval string    `json:"approval"`
	Text     string    `json:"text"`
	Since    time.Time `json:"since"`
}

// Answer is the inbox of the caller: approvals and questions first, the
// oldest first, then the failures and the paused duties.
type Answer struct {
	Items []Item `json:"items"`
}

// rank orders the kinds of an item.
var rank = map[string]int{KindApproval: 0, KindQuestion: 0, KindFailure: 1, KindPaused: 2, KindResult: 3}

// For returns the inbox of the caller in ctx at the time now.
func For(ctx context.Context, a *fw.Agent, now time.Time) (Answer, error) {
	items := []Item{}
	runs, err := dashboard.Runs(ctx, a, dashboard.ScopeFleet, "")
	if err != nil {
		return Answer{}, err
	}
	home, err := dashboard.HomeOf(ctx, a, runs, now)
	if err != nil {
		return Answer{}, err
	}
	seen := map[string]bool{}
	for _, it := range home.NeedsYou {
		items = append(items, runItem(it.Run))
		seen[it.ID] = true
	}
	var mine []Item
	asks, hears := map[string]bool{}, map[string]bool{}
	if svc := duties.Of(a); svc != nil {
		if p := fw.PrincipalFrom(ctx); p != nil && p.Subject != "" {
			if mine, asks, hears, err = dutyItems(ctx, svc, p.Subject, now); err != nil {
				return Answer{}, err
			}
		}
	}
	// A run of a duty acts as the duty, so the home of the owner does not
	// hold it. Its questions go to the people of notify.needsYou.
	for _, r := range runs {
		if !seen[r.ID] && r.Parent == "" && asks[duties.NameOf(r)] && dashboard.StatusOf(r) == dashboard.StatusNeedsYou {
			items = append(items, runItem(r))
			seen[r.ID] = true
		}
	}
	if st := bridges.Of(a); st != nil {
		waiting, err := st.Waiting(ctx)
		if err != nil {
			return Answer{}, err
		}
		for _, w := range waiting {
			if w.Question == 0 && (w.Run == "" || seen[w.Run]) {
				continue
			}
			items = append(items, seatItem(w))
			if w.Run != "" && w.Question == 0 {
				seen[w.Run] = true
			}
		}
	}
	items = append(items, mine...)
	// The results that wait for a check: a run of the caller or of the
	// teams of the caller, or a run of a duty whose questions or results go
	// to the caller, that completed in the last ResultsFor with the metadata
	// fleet.MetaReview and that nobody reacted to yet. A run of a duty that
	// tells only of what it finds and that found something (fleet.Found)
	// goes to the people of notify.result alone.
	team, err := teams.By(ctx, a.Config(), runs, teams.ByTeam)
	if err != nil {
		return Answer{}, err
	}
	near := map[string]bool{}
	for _, r := range team {
		near[r.ID] = true
	}
	// The facts of the runs tell a new answer after Needs work, which waits
	// for a check again.
	facts := map[string]fleet.Work{}
	recent, err := fleet.WorkSince(ctx, a, now.Add(-ResultsFor))
	if err != nil {
		return Answer{}, err
	}
	for _, w := range recent {
		if w.Run != nil {
			facts[w.Run.ID] = w.Work
		}
	}
	for _, r := range runs {
		if r.Parent != "" || !fleet.ToCheck(r, facts[r.ID]) || now.Sub(r.UpdatedAt) > ResultsFor {
			continue
		}
		_, found := fleet.Found(r)
		found = found && r.Metadata[fleet.MetaReview] != "1"
		if found && !hears[duties.NameOf(r)] || !found && !(near[r.ID] || asks[duties.NameOf(r)] || hears[duties.NameOf(r)]) {
			continue
		}
		it := Item{Kind: KindResult, Run: r.ID, Duty: duties.NameOf(r), Agent: r.Agent, Since: r.UpdatedAt, Text: dashboard.TitleOf(r) + ": a result to check."}
		if found {
			it.Text = dashboard.TitleOf(r) + ": it found something."
		}
		if fleet.Again(facts[r.ID]) {
			it.Text = dashboard.TitleOf(r) + ": a new answer to check, after your note."
		}
		if b, ok := strings.CutPrefix(r.Metadata[fw.MetaBatch], bridges.BatchPrefix); ok {
			it.Bridge = b
		}
		items = append(items, it)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if rank[items[i].Kind] != rank[items[j].Kind] {
			return rank[items[i].Kind] < rank[items[j].Kind]
		}
		return items[i].Since.Before(items[j].Since)
	})
	return Answer{Items: items}, nil
}

// runItem is a run that waits for a person.
func runItem(r *fw.Run) Item {
	it := Item{Kind: KindApproval, Run: r.ID, Duty: duties.NameOf(r), Agent: r.Agent, Since: r.UpdatedAt}
	if b, ok := strings.CutPrefix(r.Metadata[fw.MetaBatch], bridges.BatchPrefix); ok {
		it.Bridge = b
	}
	title := dashboard.TitleOf(r)
	p := r.Pending
	if p == nil {
		it.Text = title + ": the run waits for you."
		return it
	}
	it.Approval = p.ID
	if !p.RequestedAt.IsZero() {
		it.Since = p.RequestedAt
	}
	switch {
	case p.Connect != nil:
		it.Text = fmt.Sprintf("%s: %s", title, oneLine(p.Reason, 300))
	case len(p.Schema) > 0:
		it.Kind = KindQuestion
		it.Text = fmt.Sprintf("%s: %s", title, oneLine(p.Reason, 300))
	default:
		it.Text = fmt.Sprintf("%s: the agent %s asks to run %s.", title, r.Agent, p.Tool)
		if reason := oneLine(p.Reason, 200); reason != "" {
			it.Text += " " + strings.TrimSuffix(capital(reason), ".") + "."
		}
	}
	return it
}

// seatItem is a seat of a bridge that needs a person.
func seatItem(w bridges.Waiting) Item {
	it := Item{Kind: KindApproval, Run: w.Run, Agent: w.Agent, Bridge: w.Bridge, Since: w.At}
	if w.Question > 0 {
		it.Kind = KindQuestion
		it.Text = fmt.Sprintf("%s asks in %s: %s", w.Handle, w.Title, oneLine(w.Text, 300))
		return it
	}
	it.Approval = w.Approval
	it.Text = fmt.Sprintf("%s in %s waits for a decision.", w.Handle, w.Title)
	return it
}

// dutyItems lists the failed runs of the last day and the duties that
// paused themselves, of the duties that the person owns or whose
// notify.failure names the person. It also returns the duties whose
// questions go to the person: the person owns them, or their
// notify.needsYou names the person. Last, it returns the duties whose
// results go to the person by notify.result.
func dutyItems(ctx context.Context, svc *duties.Service, subject string, now time.Time) ([]Item, map[string]bool, map[string]bool, error) {
	recs, err := svc.Records(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	var out []Item
	asks, hears := map[string]bool{}, map[string]bool{}
	for _, r := range recs {
		d := r.Duty
		if d.Owner == subject || (d.Notify != nil && slices.Contains(d.Notify.NeedsYou, subject)) {
			asks[d.Name] = true
		}
		if d.Notify != nil && (slices.Contains(d.Notify.Result, subject) || d.Owner == subject && slices.Contains(d.Notify.Result, duties.RunAsOwner)) {
			hears[d.Name] = true
		}
		if !concerns(d, subject) {
			continue
		}
		if d.Paused && r.PausedReason != "" && d.Owner == subject {
			out = append(out, Item{Kind: KindPaused, Duty: d.Name, Agent: d.Agent, Text: fmt.Sprintf("The duty %s paused itself: %s", d.Name, r.PausedReason), Since: r.Changed})
		}
		starts, err := svc.Starts(ctx, d.Name, now.Add(-FailuresFor))
		if err != nil {
			return nil, nil, nil, err
		}
		for _, e := range starts {
			if e.State != duties.StateFailed {
				continue
			}
			text := fmt.Sprintf("A run of the duty %s failed.", d.Name)
			if e.Reason != "" {
				text = fmt.Sprintf("The duty %s: %s", d.Name, oneLine(e.Reason, 300))
			}
			out = append(out, Item{Kind: KindFailure, Run: e.Run, Duty: d.Name, Agent: d.Agent, Text: text, Since: e.At})
		}
	}
	return out, asks, hears, nil
}

// concerns reports whether the failures of a duty concern a person: the
// person owns it, or its notify.failure names the person.
func concerns(d duties.Duty, subject string) bool {
	if d.Owner == subject {
		return true
	}
	return d.Notify != nil && slices.Contains(d.Notify.Failure, subject)
}

// oneLine cuts a text to one line of at most n bytes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n] + "..."
}

// capital writes the first letter of s as a capital.
func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// mount serves GET /v1/inbox.
func mount(k *native.Kit) error {
	a := k.Agent()
	k.Handle("GET /v1/inbox", func(w http.ResponseWriter, r *http.Request) {
		ctx, _, _ := manager.OnBehalf(r.Context())
		ans, err := For(ctx, a, time.Now())
		if err != nil {
			k.Fail(w, r, err)
			return
		}
		native.WriteJSON(w, http.StatusOK, ans)
	})
	return nil
}

// ToolInbox is the name of the tool.
const ToolInbox = "inbox"

func ptr[T any](v T) *T { return &v }

// tools builds the tool inbox for one MCP server.
func tools(srv *mcp.Server) []mcp.Tool {
	a := srv.Agent()
	return []mcp.Tool{mcp.NewTool(&sdk.Tool{
		Name:        ToolInbox,
		Title:       "What needs me",
		Description: "List what needs the person now, the items of Activity: each run that waits for an approval or an answer, each question of a seat of a bridge, each failed run of a duty of the person in the last day, each duty of the person that paused itself, and each result that the person asked to check or that a duty found and tells the person of, in the last week. Answer an approval with approve and a question with approve or send, and check a result with work_review. Each item has kind (approval, question, failure, paused, result), run, duty, agent, bridge, approval (the id of the approval that the run waits for), text, and since.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
		ctx, _, _ = manager.OnBehalf(ctx)
		ans, err := For(ctx, a, time.Now())
		if err != nil {
			return mcp.ErrorResult(mcp.Explain(err)), nil, nil
		}
		return mcp.Result(text(ans), ans), nil, nil
	})}
}

// text words the inbox for the model.
func text(ans Answer) string {
	if len(ans.Items) == 0 {
		return "Nothing needs you now."
	}
	var b strings.Builder
	if len(ans.Items) == 1 {
		b.WriteString("1 item needs you:")
	} else {
		fmt.Fprintf(&b, "%d items need you:", len(ans.Items))
	}
	for _, it := range ans.Items {
		fmt.Fprintf(&b, "\n- %s: %s", it.Kind, it.Text)
		if it.Run != "" {
			b.WriteString(" (run " + it.Run + ")")
		}
	}
	return b.String()
}
