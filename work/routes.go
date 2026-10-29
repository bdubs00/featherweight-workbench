package work

// This file gathers the records of the board for a caller, each with the
// access rule of its own route, and serves the board: GET /v1/work on the
// native surface and the MCP tool work, which call the same Go function.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/bossmode"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/knowhow"
	"github.com/bdubs00/featherweight-workbench/manager"
	persons "github.com/bdubs00/featherweight-workbench/people"
	"github.com/bdubs00/featherweight-workbench/sources"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	"github.com/bdubs00/featherweight-workbench/teams"
	"github.com/bdubs00/featherweight-workbench/titles"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	fw.RegisterService(Service)
	native.Extend(mount)
	mcp.RegisterTools(tools)
	// A thumb on a run through the route of the kernel is a verdict too,
	// and the end of a run moves its card, such as the new answer after
	// Needs work, so the board of every caller reads it at once.
	fleet.OnChange(func(a *fw.Agent) { cacheOf(a).clear() })
}

// maxJobs bounds the jobs whose queues one board reads.
const maxJobs = 20

// Query is what a caller asks of the board: the window in days, 7, 30, or
// 90, DefaultDays when 0, and All for every card that the caller can
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
	if q.Days != 7 && q.Days != 30 && q.Days != 90 {
		return Answer{}, fmt.Errorf("%w: days is %d, not 7, 30, or 90", fw.ErrInvalidRequest, q.Days)
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

// Fresh returns the board of the caller in ctx for the default query, read
// now and not from the cache, and keeps it in the cache. The inbox reads
// the one count of the work that needs the caller from it (Waiting) after
// each change that the page sees, so the badge never lags the board.
func Fresh(ctx context.Context, a *fw.Agent, now time.Time) (Answer, error) {
	q := Query{Days: DefaultDays}
	ans, err := build(ctx, a, now, q)
	if err != nil {
		return Answer{}, err
	}
	cacheOf(a).put(cacheKey(ctx, q), ans, now)
	return ans, nil
}

// mergeRuns puts the runs of the view beside the runs of the archive, one
// per id. The view holds a run as it is now, with its activity, and the
// archive adds the facts of its work. A run that ended in both keeps the
// run of the archive when the archive saw its end: after a start of the
// front, the view knows the run of a Task that sleeps from the index of the
// runs alone, without its title, its output, its metadata, or its done,
// and the card then lost its title and its verdict. A run whose archive
// saw later events than the view keeps the run of the archive too: the
// stream carries an event at once, and the view reads its members every
// few seconds, so after Needs work the view still holds the end before.
func mergeRuns(facts []fleet.WorkRun, live []*fw.Run) []Run {
	var out []Run
	byID := map[string]int{}
	for _, w := range facts {
		if w.Run == nil || !fleet.Listed(w.Run) {
			continue
		}
		byID[w.Run.ID] = len(out)
		out = append(out, Run{Run: w.Run, Work: w.Work, Lost: w.Lost})
	}
	for _, r := range live {
		i, ok := byID[r.ID]
		switch {
		case !ok:
			byID[r.ID] = len(out)
			out = append(out, Run{Run: r})
		case r.State.Terminal() && out[i].Run.State.Terminal() && out[i].Run.Seq >= r.Seq, out[i].Run.Seq > r.Seq:
			out[i].Lost = false
		default:
			out[i].Run, out[i].Lost = r, false
		}
	}
	return out
}

// build gathers the records of the board and assembles it.
func build(ctx context.Context, a *fw.Agent, now time.Time, q Query) (Answer, error) {
	in := Input{Now: now, Window: time.Duration(q.Days) * 24 * time.Hour, All: q.All, Mine: map[string]bool{}, Queued: map[string]int{}, Names: map[string]string{}}
	st := bridges.Of(a)
	if p := fw.PrincipalFrom(ctx); p != nil {
		in.Me = p.Subject
		if st != nil {
			st.Learn(p)
		} else {
			persons.Learn(ctx, a, p)
		}
		if p.Subject != "" {
			in.Names[p.Subject] = persons.Display(ctx, a, p.Subject, p.Name)
		}
	}
	letGo, err := Of(a).All(ctx)
	if err != nil {
		return Answer{}, err
	}
	in.LetGo = letGo
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
	in.Runs = mergeRuns(facts, live)
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
	if st != nil {
		if in.Bridges, err = st.List(ctx, true); err != nil {
			return Answer{}, err
		}
		if in.Waiting, err = st.Waiting(ctx); err != nil {
			return Answer{}, err
		}
		// The display names of the people of the bridges, from the names
		// that the front keeps (persons.Display).
		for _, it := range in.Bridges {
			if _, ok := in.Names[it.Operator]; !ok && it.Operator != "" {
				in.Names[it.Operator] = st.KnownName(it.Operator, it.OperatorName)
			}
		}
	}
	// The display names of the people of the runs.
	for _, r := range in.Runs {
		if _, ok := in.Names[r.Principal]; !ok && r.Principal != "" {
			in.Names[r.Principal] = persons.Display(ctx, a, r.Principal, "")
		}
	}
	if svc := duties.Of(a); svc != nil {
		list, err := svc.List(ctx)
		if err != nil {
			return Answer{}, err
		}
		in.Next = map[string]time.Time{}
		for _, v := range list.Duties {
			in.Duties = append(in.Duties, v.Duty)
			if !list.Paused && !v.Paused && len(v.Next) > 0 {
				in.Next[v.Name] = v.Next[0]
			}
			starts, err := svc.Starts(ctx, v.Name, now.Add(-in.Window))
			if err != nil {
				return Answer{}, err
			}
			for _, e := range starts {
				in.Starts = append(in.Starts, Start{Duty: v.Duty, Entry: e})
			}
		}
	}
	// The notes of Know-how that wait for the caller count in the one count
	// of the work that needs the caller. A store that does not read counts
	// none, and the board still answers.
	if notes, err := knowhow.Of(a).WaitingFor(ctx); err != nil {
		a.Logger().Warn("work: the notes that wait for a person do not read", "error", err)
	} else {
		in.Notes = len(notes)
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

// mount serves GET /v1/work, POST /v1/work/plan, POST /v1/work/review, and
// POST /v1/work/letgo.
func mount(k *native.Kit) error {
	a := k.Agent()
	bossmode.OfferWork()
	k.Handle("GET /v1/work", func(w http.ResponseWriter, r *http.Request) {
		q := Query{}
		if s := r.URL.Query().Get("days"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil {
				native.WriteProblem(w, r, http.StatusBadRequest, "days is 7, 30, or 90")
				return
			}
			q.Days = n
		}
		switch r.URL.Query().Get("all") {
		case "", "0", "false":
		case "1", "true":
			q.All = true
		default:
			native.WriteProblem(w, r, http.StatusBadRequest, "all is 0 or 1")
			return
		}
		ctx, _, _ := manager.OnBehalf(r.Context())
		ans, err := For(ctx, a, time.Now(), q)
		if err != nil {
			fail(w, r, k, err)
			return
		}
		native.WriteJSON(w, http.StatusOK, ans)
	})
	k.Handle("POST /v1/work/plan", func(w http.ResponseWriter, r *http.Request) {
		var req PlanRequest
		if !k.Decode(w, r, &req) {
			return
		}
		ctx, _, _ := manager.OnBehalf(r.Context())
		p, err := PlanFor(ctx, a, time.Now(), r.Header.Get(duties.HeaderTimeZone), req)
		if err != nil {
			fail(w, r, k, err)
			return
		}
		native.WriteJSON(w, http.StatusOK, p)
	})
	k.Handle("POST /v1/work/review", func(w http.ResponseWriter, r *http.Request) {
		var req ReviewRequest
		if !k.Decode(w, r, &req) {
			return
		}
		ctx, _, acting := manager.OnBehalf(r.Context())
		if acting {
			fail(w, r, k, ErrNotPerson)
			return
		}
		ans, err := Review(ctx, a, req)
		if err != nil {
			fail(w, r, k, err)
			return
		}
		cacheOf(a).clear()
		native.WriteJSON(w, http.StatusOK, ans)
	})
	k.Handle("POST /v1/work/letgo", func(w http.ResponseWriter, r *http.Request) {
		var req LetGoRequest
		if !k.Decode(w, r, &req) {
			return
		}
		ctx, _, acting := manager.OnBehalf(r.Context())
		if acting {
			native.WriteProblem(w, r, http.StatusForbidden, "only a person lets a card go")
			return
		}
		c, err := LetGoCard(ctx, a, time.Now(), req)
		if err != nil {
			fail(w, r, k, err)
			return
		}
		native.WriteJSON(w, http.StatusOK, c)
	})
	return nil
}

// fail answers an error of the package with its problem document: a
// request that does not read is 400, a check by a run of an agent is 403,
// a bridge or a card that is not there is 404, a closed bridge is 409, a
// member that refused is its status, and the rest as the kernel answers
// it.
func fail(w http.ResponseWriter, r *http.Request, k *native.Kit, err error) {
	var me *fleet.MemberError
	switch {
	case errors.Is(err, fw.ErrInvalidRequest):
		native.WriteProblem(w, r, http.StatusBadRequest, strings.ReplaceAll(bridges.Detail(err), fw.ErrInvalidRequest.Error()+": ", ""))
	case errors.Is(err, ErrNotPerson):
		native.WriteProblem(w, r, http.StatusForbidden, ErrNotPerson.Error())
	case errors.Is(err, bridges.ErrNotFound):
		native.WriteProblem(w, r, http.StatusNotFound, bridges.Detail(err))
	case errors.Is(err, bridges.ErrConflict):
		native.WriteProblem(w, r, http.StatusConflict, bridges.Detail(err))
	case errors.Is(err, ErrNoCard):
		native.WriteProblem(w, r, http.StatusNotFound, strings.TrimPrefix(err.Error(), ErrNoCard.Error()+": "))
	case errors.Is(err, ErrNotStopped):
		native.WriteProblem(w, r, http.StatusConflict, strings.TrimPrefix(err.Error(), ErrNotStopped.Error()+": "))
	case errors.Is(err, ErrOff):
		native.WriteProblem(w, r, http.StatusServiceUnavailable, strings.TrimPrefix(err.Error(), ErrOff.Error()+": "))
	case errors.Is(err, ErrNoBridges):
		native.WriteProblem(w, r, http.StatusServiceUnavailable, "the bridges are off in this process")
	case errors.As(err, &me) && me.Status >= 400 && me.Status < 500:
		native.WriteProblem(w, r, me.Status, me.Detail)
	default:
		k.Fail(w, r, err)
	}
}

// Names of the tools.
const (
	ToolWork   = "work"
	ToolPlan   = "work_plan"
	ToolReview = "work_review"
	ToolLetGo  = "work_letgo"
)

func ptr[T any](v T) *T { return &v }

// tools builds the tools work, work_plan, work_review, and work_letgo for
// one MCP server.
func tools(srv *mcp.Server) []mcp.Tool {
	a := srv.Agent()
	type planIn struct {
		PlanRequest
		TimeZone string `json:"timeZone,omitempty"`
	}
	return []mcp.Tool{mcp.NewTool(&sdk.Tool{
		Name:  ToolWork,
		Title: "Where the work stands",
		Description: "Show the board of the work of the person: one card per task, from the request to its end. A task is a bridge, a start of a duty, a job of many items, an event of a source, or a conversation with an agent. " +
			"Each card has a stage (waiting: coming up, working, needs-you, stopped, review: a result to check, done), the reason in words, since when, who moves it next (worker, agent, you, person, manager), who asked for it, and its links: the run to open, the approval to decide with approve, the bridge, the duty, and the batch. " +
			"A done card has a verdict: accepted, checked, sent-back, or unchecked, or let-go for a stopped card that a person let go with work_letgo. A card that waits or works stays on the board however old it is. A stopped card stays for 7 days, then lets itself go to done with the verdict let-go. To check and done show the newest card first. lost means that the agent that held the task is gone. from.display is a name of the person that people read. " +
			"days is the window for the cards that ended: 7, 30, or 90, 7 by default. all true shows every card that the person can view, not only the cards of the person and of the teams of the person.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"days":{"type":"integer","enum":[7,30,90]},"all":{"type":"boolean"}}}`),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, q Query) (*sdk.CallToolResult, any, error) {
		ctx, _, _ = manager.OnBehalf(ctx)
		now := time.Now()
		ans, err := For(ctx, a, now, q)
		if err != nil {
			return mcp.ErrorResult(mcp.Explain(err)), nil, nil
		}
		return mcp.Result(Text(ans, now), ans), nil, nil
	}), mcp.NewTool(&sdk.Tool{
		Name:  ToolPlan,
		Title: "Read a task back before it starts",
		Description: "Read back what the person needs done, in their words, before anything starts. It starts and saves nothing, and it calls no model. " +
			"The answer says the kind (once, repeat, or team), who does it, when (the schedule in words and the next three times), what runs of the agent cost (never a limit), when it asks the person, when it stays quiet, who decides a question of a task that repeats while the person is away (away), where the result lands, what no agent here can do (gaps), and problems that stop a start. " +
			"start holds the record to send: duty to PUT /v1/duties/{name} (duty_put), bridge to POST /v1/bridges (bridge_open), or run to POST /v1/runs?member= (run). The person sets the most it can cost before the start: limits.budget.maxCostUSD, caps.maxCostUSD, or budget.maxCostUSD. " +
			"agent picks the agent, and tell picks when the person hears of a task that repeats: each, found, or failures. timeZone is an IANA name, UTC by default.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"words":{"type":"string","maxLength":4000},"agent":{"type":"string"},"tell":{"type":"string","enum":["each","found","failures"]},"timeZone":{"type":"string"}},"required":["words"]}`),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in planIn) (*sdk.CallToolResult, any, error) {
		ctx, _, _ = manager.OnBehalf(ctx)
		p, err := PlanFor(ctx, a, time.Now(), in.TimeZone, in.PlanRequest)
		if err != nil {
			return mcp.ErrorResult(mcp.Explain(err)), nil, nil
		}
		return mcp.Result(PlanText(p), p), nil, nil
	}), mcp.NewTool(&sdk.Tool{
		Name:  ToolReview,
		Title: "Check a result",
		Description: "Check the result of a run or of a bridge (a team) for the person: name run or bridge, not both. verdict up (or right) is Looks right: a thumb up on the run, or the bridge closes with the verdict accepted. " +
			"verdict down (or work) is Needs work and needs a note: a thumb down on the run, and the note goes to the agent as a follow-up message, so the run works again; for a bridge, the note goes in its channel to every seat, and the bridge stays open. " +
			"Only a person checks a result: a run of an agent cannot. The person needs the operator role on the run, or the right to use the bridge.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"run":{"type":"string"},"bridge":{"type":"string"},"verdict":{"type":"string","enum":["up","down","right","work"]},"note":{"type":"string","maxLength":4000}},"required":["verdict"]}`),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: false, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, req *sdk.CallToolRequest, in ReviewRequest) (*sdk.CallToolResult, any, error) {
		ctx, _, acting := manager.OnBehalf(ctx)
		if acting {
			return mcp.ErrorResult("Forbidden: " + ErrNotPerson.Error() + "; a run of an agent cannot."), nil, nil
		}
		ans, err := Review(withCredential(ctx, req), a, in)
		if err != nil {
			return mcp.ErrorResult(mcp.Explain(err)), nil, nil
		}
		cacheOf(a).clear()
		text := "Looks right: the run " + ans.Run + " has a thumb up."
		switch {
		case ans.Bridge != "" && ans.Reaction == fw.ReactionDown:
			text = "Needs work: the note went in the channel of the bridge " + ans.Bridge + " to every seat, and the bridge stays open."
		case ans.Bridge != "":
			text = "Looks right: the bridge " + ans.Bridge + " is closed with the verdict accepted."
		case ans.Reaction == fw.ReactionDown:
			text = "Needs work: the run " + ans.Run + " has a thumb down, and the note went to the agent."
		}
		return mcp.Result(text, ans), nil, nil
	}), mcp.NewTool(&sdk.Tool{
		Name:  ToolLetGo,
		Title: "Let a stopped task go",
		Description: "Let a stopped card of the board go, for the person: card is the id of the card, as the tool work answers it. The card moves to done with the verdict let-go, and the front keeps who let it go and when. Its runs keep their state, so the value of the work counts them as before. " +
			"Only a person whose move the card is (next is you), or an owner of one of its agents, can let it go. A card that a person let go already stays as it is. A card that is not stopped cannot go.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"card":{"type":"string"}},"required":["card"]}`),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in LetGoRequest) (*sdk.CallToolResult, any, error) {
		ctx, _, acting := manager.OnBehalf(ctx)
		if acting {
			return mcp.ErrorResult("Forbidden: only a person lets a card go; a run of an agent cannot."), nil, nil
		}
		c, err := LetGoCard(ctx, a, time.Now(), in)
		if err != nil {
			return mcp.ErrorResult(mcp.Explain(err)), nil, nil
		}
		return mcp.Result("Let go: "+c.Title+". "+c.Reason+" It is in Done now.", c), nil, nil
	})}
}

// withCredential adds the bearer credential of an MCP request to ctx, so a
// caller that is no person, such as the static token, reaches the member
// that holds a run with its own credential.
func withCredential(ctx context.Context, req *sdk.CallToolRequest) context.Context {
	if fw.CredentialFrom(ctx) != "" || req == nil {
		return ctx
	}
	ex := req.GetExtra()
	if ex == nil || ex.Header == nil {
		return ctx
	}
	h := strings.TrimSpace(ex.Header.Get("Authorization"))
	if scheme, tok, ok := strings.Cut(h, " "); !ok || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(tok) == "" {
		return ctx
	}
	return fw.WithCredential(ctx, h)
}

// PlanText words a plan for a host that shows only text.
func PlanText(p Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s).", p.Title, p.Kind)
	for _, w := range p.Who {
		fmt.Fprintf(&b, "\nWho: %s %s.", w.Name, w.Does)
	}
	fmt.Fprintf(&b, "\nWhen: %s", p.When.Readback)
	if p.Cost.Basis != "" {
		b.WriteString("\nWhat it can cost: " + p.Cost.Basis + " Set the most it can spend before the start.")
	}
	if len(p.Asks) > 0 {
		b.WriteString("\nIt asks you: " + strings.Join(p.Asks, "; ") + ".")
	}
	if len(p.Quiet) > 0 {
		b.WriteString("\nIt stays quiet: " + strings.Join(p.Quiet, "; ") + ".")
	}
	if p.Away != "" {
		b.WriteString("\nWhile you are away: " + p.Away)
	}
	b.WriteString("\nThe result: " + p.Result + ".")
	for _, g := range p.Gaps {
		b.WriteString("\nIt cannot: " + g)
	}
	for _, x := range p.Problems {
		b.WriteString("\nProblem: " + x)
	}
	if p.Same != nil {
		b.WriteString("\nThe same work: your duty " + p.Same.Title + " (" + p.Same.Duty + ") does it already.")
	}
	return b.String()
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
	KindBridge:       "a team",
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
			if who := cmp.Or(c.From.Display, c.From.Name); who != "" {
				fmt.Fprintf(&b, " from %s", who)
			}
			if c.Reason != "" {
				b.WriteString(": " + c.Reason)
			}
			if c.Since.After(now) {
				fmt.Fprintf(&b, " Starts in %s.", Span(c.Since.Sub(now)))
			} else {
				fmt.Fprintf(&b, " For %s.", Span(now.Sub(c.Since)))
			}
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

// clear forgets every board, after a change that a person made here.
func (c *cache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.list)
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
