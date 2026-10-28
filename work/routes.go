package work

// This file gathers the records of the board for a caller, each with the
// access rule of its own route, and serves the board: GET /v1/work on the
// native surface and the MCP tool work, which call the same Go function.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/sources"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	"github.com/bdubs00/featherweight-workbench/teams"
	"github.com/bdubs00/featherweight-workbench/titles"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	native.Extend(mount)
	mcp.RegisterTools(tools)
}

// MaxDays bounds the window of the board.
const MaxDays = 90

// maxJobs bounds the jobs whose queues one board reads.
const maxJobs = 20

// Query is what a caller asks of the board: the window in days, 1 to
// MaxDays, DefaultDays when 0, and All for every card that the caller can
// view rather than the cards of the caller and of the teams of the caller.
type Query struct {
	Days int  `json:"days,omitempty"`
	All  bool `json:"all,omitempty"`
}

// CacheFor is how long For keeps the board of a caller for the same query.
// A page reads the board every 30 seconds and after each event of its
// stream, so many reads come close together.
var CacheFor = 10 * time.Second

// For returns the board of the caller in ctx at the time now. Each record
// comes with the rule of its own route: the runs as the fleet lists them
// (dashboard.Runs, and fleet.WorkWhere for the archive with the facts of
// the work), the bridges as GET /v1/bridges lists them, and the duties
// whose agent the caller can view, as GET /v1/duties lists them. The
// board of one caller and one query stays for CacheFor.
func For(ctx context.Context, a *fw.Agent, now time.Time, q Query) (Answer, error) {
	if q.Days == 0 {
		q.Days = DefaultDays
	}
	if q.Days < 1 || q.Days > MaxDays {
		return Answer{}, fmt.Errorf("%w: days is %d, not 1 to %d", fw.ErrInvalidRequest, q.Days, MaxDays)
	}
	c := cacheOf(a)
	key := cacheKey(ctx, q)
	if ans, ok := c.get(key, now); ok {
		return ans, nil
	}
	ans, err := build(ctx, a, now, q)
	if err != nil {
		return Answer{}, err
	}
	c.put(key, ans, now)
	return ans, nil
}

// build gathers the records of the board and assembles it.
func build(ctx context.Context, a *fw.Agent, now time.Time, q Query) (Answer, error) {
	in := Input{Now: now, Window: time.Duration(q.Days) * 24 * time.Hour, All: q.All, Mine: map[string]bool{}, Queued: map[string]int{}}
	if p := fw.PrincipalFrom(ctx); p != nil {
		in.Me = p.Subject
	}
	live, err := dashboard.Runs(ctx, a, dashboard.ScopeFleet, "")
	if err != nil {
		return Answer{}, err
	}
	titles.Want(a, live...)
	// One read of the archive gives the runs that no member holds and the
	// facts of the work of every run: the runs that did not end, however
	// old, and the runs that changed in the window.
	cut := now.Add(-in.Window)
	facts, err := fleet.WorkWhere(ctx, a, func(r *fw.Run) bool { return !r.State.Terminal() || !r.UpdatedAt.Before(cut) })
	if err != nil {
		return Answer{}, err
	}
	byID := map[string]int{}
	for _, w := range facts {
		if w.Run == nil || !fleet.Listed(w.Run) {
			continue
		}
		byID[w.Run.ID] = len(in.Runs)
		in.Runs = append(in.Runs, Run{Run: w.Run, Work: w.Work, Lost: w.Lost})
	}
	for _, r := range live {
		// The view holds the run as it is now, with its activity; the
		// archive adds the facts.
		if i, ok := byID[r.ID]; ok {
			in.Runs[i].Run, in.Runs[i].Lost = r, false
			continue
		}
		byID[r.ID] = len(in.Runs)
		in.Runs = append(in.Runs, Run{Run: r})
	}
	all := make([]*fw.Run, 0, len(in.Runs))
	for _, r := range in.Runs {
		all = append(all, r.Run)
	}
	near, err := teams.By(ctx, a.Config(), all, teams.ByTeam)
	if err != nil {
		return Answer{}, err
	}
	for _, r := range near {
		in.Mine[r.ID] = true
	}
	if st := bridges.Of(a); st != nil {
		if in.Bridges, err = st.List(ctx, true); err != nil {
			return Answer{}, err
		}
		if in.Waiting, err = st.Waiting(ctx); err != nil {
			return Answer{}, err
		}
	}
	if svc := duties.Of(a); svc != nil {
		list, err := svc.List(ctx)
		if err != nil {
			return Answer{}, err
		}
		for _, v := range list.Duties {
			in.Duties = append(in.Duties, v.Duty)
			starts, err := svc.Starts(ctx, v.Name, now.Add(-in.Window))
			if err != nil {
				return Answer{}, err
			}
			for _, e := range starts {
				in.Starts = append(in.Starts, Start{Duty: v.Duty, Entry: e})
			}
		}
	}
	if bv, ok := fleet.Of(a).(fleet.BatchView); ok {
		for _, batch := range jobBatches(in) {
			b, err := bv.Batch(ctx, batch)
			if err != nil {
				return Answer{}, err
			}
			if b.Queued > 0 {
				in.Queued[batch] = b.Queued
			}
		}
	}
	return Assemble(in), nil
}

// jobBatches lists the batches of the jobs of the caller whose runs did
// not all end, at most maxJobs of them: only their queues can still hold
// items that no worker took.
func jobBatches(in Input) []string {
	open := map[string]bool{}
	var out []string
	for _, r := range in.Runs {
		md := r.Metadata
		batch := md[fw.MetaBatch]
		if batch == "" || r.Parent != "" || r.Lost || !in.Mine[r.ID] || strings.HasPrefix(batch, bridges.BatchPrefix) || md[duties.MetaDuty] != "" || md[sources.MetaSource] != "" {
			continue
		}
		if !r.State.Terminal() && !open[batch] && len(out) < maxJobs {
			open[batch] = true
			out = append(out, batch)
		}
	}
	return out
}

// mount serves GET /v1/work.
func mount(k *native.Kit) error {
	a := k.Agent()
	k.Handle("GET /v1/work", func(w http.ResponseWriter, r *http.Request) {
		q := Query{}
		if s := r.URL.Query().Get("days"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil {
				native.WriteProblem(w, r, http.StatusBadRequest, "days is not a number of days")
				return
			}
			q.Days = n
		}
		switch r.URL.Query().Get("all") {
		case "", "false":
		case "true":
			q.All = true
		default:
			native.WriteProblem(w, r, http.StatusBadRequest, "all is true or false")
			return
		}
		ctx, _, _ := manager.OnBehalf(r.Context())
		ans, err := For(ctx, a, time.Now(), q)
		if err != nil {
			k.Fail(w, r, err)
			return
		}
		native.WriteJSON(w, http.StatusOK, ans)
	})
	return nil
}

// ToolWork is the name of the tool.
const ToolWork = "work"

func ptr[T any](v T) *T { return &v }

// tools builds the tool work for one MCP server.
func tools(srv *mcp.Server) []mcp.Tool {
	a := srv.Agent()
	return []mcp.Tool{mcp.NewTool(&sdk.Tool{
		Name:  ToolWork,
		Title: "Where the work stands",
		Description: "Show the board of the work of the person: one card per piece of work, from the request to its end. A piece of work is a bridge, a start of a duty, a job of many items, an event of a source, or a conversation with an agent. " +
			"Each card has a stage (waiting for a worker, working, needs-you, stopped, review, done), the reason in words, since when, who moves it next (worker, agent, you, person, manager), who asked for it, and its links: the run to open, the approval to decide with approve, the bridge, the duty, and the batch. " +
			"The lanes count the cards of each stage and name the oldest wait. A card that did not end stays on the board however old it is, so work that stopped never drops out of sight. " +
			"days is the window for the cards that ended, 1 to 90, 7 by default. all true shows every card that the person can view, not only the cards of the person and of the teams of the person.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"days":{"type":"integer","minimum":1,"maximum":90},"all":{"type":"boolean"}}}`),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, q Query) (*sdk.CallToolResult, any, error) {
		ctx, _, _ = manager.OnBehalf(ctx)
		now := time.Now()
		ans, err := For(ctx, a, now, q)
		if err != nil {
			return mcp.ErrorResult(mcp.Explain(err)), nil, nil
		}
		return mcp.Result(Text(ans, now), ans), nil, nil
	})}
}

// stageWords names a stage for a person.
var stageWords = map[string]string{
	StageWaiting:  "Coming up",
	StageWorking:  "Working",
	StageNeedsYou: "Needs you",
	StageStopped:  "Stopped",
	StageReview:   "To check",
	StageDone:     "Done",
}

// kindWords names a kind of card with its article.
var kindWords = map[string]string{
	KindConversation: "a conversation",
	KindBridge:       "a bridge",
	KindDuty:         "a duty",
	KindJob:          "a job",
	KindEvent:        "an event",
}

// Text words the board for a host that shows only text: the sentence,
// then the cards of each stage that holds one, one line per card.
func Text(ans Answer, now time.Time) string {
	var b strings.Builder
	b.WriteString(ans.Sentence)
	for _, l := range ans.Lanes {
		if l.Count == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n\n%s (%d):", stageWords[l.Stage], l.Count)
		for _, c := range ans.Cards {
			if c.Stage != l.Stage {
				continue
			}
			fmt.Fprintf(&b, "\n- %s, %s", c.Title, kindWords[c.Kind])
			if c.From.Name != "" {
				fmt.Fprintf(&b, " from %s", c.From.Name)
			}
			if c.Reason != "" {
				b.WriteString(": " + c.Reason)
			}
			fmt.Fprintf(&b, " For %s.", Span(now.Sub(c.Since)))
			if c.Next != "" {
				b.WriteString(" Next: " + c.Next + ".")
			}
			if c.Links.Run != "" {
				b.WriteString(" Run " + c.Links.Run + ".")
			}
			if c.Links.Approval != "" {
				b.WriteString(" Approval " + c.Links.Approval + ".")
			}
			if c.Links.Bridge != "" {
				b.WriteString(" Bridge " + c.Links.Bridge + ".")
			}
		}
	}
	return b.String()
}

// cache keeps the boards of the callers of one process for CacheFor.
type cache struct {
	mu   sync.Mutex
	list map[string]cached
}

type cached struct {
	ans Answer
	at  time.Time
}

// maxCached bounds the boards that a process keeps.
const maxCached = 256

var caches sync.Map // *fw.Agent to *cache

func cacheOf(a *fw.Agent) *cache {
	c, _ := caches.LoadOrStore(a, &cache{list: map[string]cached{}})
	return c.(*cache)
}

// cacheKey names the board of the caller in ctx for a query.
func cacheKey(ctx context.Context, q Query) string {
	who := ""
	if p := fw.PrincipalFrom(ctx); p != nil {
		who = p.Kind + "\x00" + p.Subject
	}
	return fmt.Sprintf("%s\x00%d\x00%t", who, q.Days, q.All)
}

func (c *cache) get(key string, now time.Time) (Answer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	x, ok := c.list[key]
	if !ok || CacheFor <= 0 || now.Sub(x.at) >= CacheFor || now.Before(x.at) {
		return Answer{}, false
	}
	return x.ans, true
}

func (c *cache) put(key string, ans Answer, now time.Time) {
	if CacheFor <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.list) >= maxCached {
		for k, x := range c.list {
			if now.Sub(x.at) >= CacheFor {
				delete(c.list, k)
			}
		}
		if len(c.list) >= maxCached {
			clear(c.list)
		}
	}
	c.list[key] = cached{ans: ans, at: now}
}
