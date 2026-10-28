// Package work answers where each piece of work stands: one card per
// piece of work, from the request to its end, as a kanban card does. A
// piece of work is a bridge, a start of a duty, a job of many items, an
// event of a source, or a conversation with an agent. The card says its
// stage, what it waits for, who moves it next, since when, and its links.
//
// The package keeps no truth of its own. It assembles the cards from the
// records that exist: the runs of the view of the fleet and of the
// archive, the bridges with their seats, the starts of the duties, and
// the jobs that wait in a queue. Assemble is a pure function of those
// records, so a test feeds it any records. For gathers them for a caller
// with the access rules of their own routes (sources.go).
package work

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/sources"
)

// Stages of a card, in the order of the flow. A card leaves the board
// only through done, so work that stopped never drops out of sight.
const (
	StageWaiting  = "waiting"   // asked, and no worker took it yet
	StageWorking  = "working"   // an agent works on it
	StageNeedsYou = "needs-you" // it waits for a decision or an answer of a person
	StageStopped  = "stopped"   // it stopped short of its end: failed, at a limit, or lost with its worker
	StageReview   = "review"    // it ended with a result that nobody accepted yet
	StageDone     = "done"      // accepted, ended with nothing to read, canceled, or closed
)

// Stages lists the stages in the order of the flow.
var Stages = []string{StageWaiting, StageWorking, StageNeedsYou, StageStopped, StageReview, StageDone}

// weight orders the stages of the parts of one card: the stage of the card
// is the stage of its part with the highest weight. A part that needs a
// person shows first. A part that moves comes next, so a job with one
// failed item and ten that work still reads as working, with the failed
// item in its counts.
var weight = map[string]int{StageNeedsYou: 6, StageWorking: 5, StageWaiting: 4, StageStopped: 3, StageReview: 2, StageDone: 1}

// Verdicts of a done card: who or what checked its result.
const (
	VerdictAccepted  = "accepted"  // a person gave a thumb up, or a receipt accepted it
	VerdictChecked   = "checked"   // the checks of the definition of done of the agent passed, and the pass was not weak (fleet.Checked)
	VerdictSentBack  = "sent-back" // a person gave a thumb down
	VerdictUnchecked = "unchecked" // nothing and nobody checked it
)

// Kinds of a card.
const (
	KindConversation = "conversation" // a run of a person, its child runs, and the runs that continue it
	KindBridge       = "bridge"       // a team of seats with one channel
	KindDuty         = "duty"         // one start of a duty
	KindJob          = "job"          // the items of one batch, one run each
	KindEvent        = "event"        // one event of a source
)

// Who moves a card next.
const (
	NextWorker  = "worker"  // a worker takes it from its queue
	NextAgent   = "agent"   // the agent works
	NextYou     = "you"     // the caller
	NextPerson  = "person"  // another person
	NextManager = "manager" // the manager decides while the owner is away
)

// From says who asked for the work: Kind is person, duty, app, manager,
// or agent; Name names it; Via says how the request came: page, mcp,
// queue, bridge, schedule, event, call, or now. Display is a name of a
// person that people read, beside the subject, such as the email for a
// subject that the issuer made as an id. It is empty when the front knows
// no such name.
type From struct {
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Display string `json:"display,omitempty"`
	Via     string `json:"via,omitempty"`
}

// Parts counts the parts of a card by stage: the seats of a bridge, or the
// runs of a job or of a conversation. Queued counts the items of a job
// that no worker took yet, which have no run.
type Parts struct {
	Waiting  int `json:"waiting,omitempty"`
	Working  int `json:"working,omitempty"`
	NeedsYou int `json:"needsYou,omitempty"`
	Stopped  int `json:"stopped,omitempty"`
	Review   int `json:"review,omitempty"`
	Done     int `json:"done,omitempty"`
	Total    int `json:"total"`
}

func (p *Parts) add(stage string) {
	p.Total++
	switch stage {
	case StageWaiting:
		p.Waiting++
	case StageWorking:
		p.Working++
	case StageNeedsYou:
		p.NeedsYou++
	case StageStopped:
		p.Stopped++
	case StageReview:
		p.Review++
	case StageDone:
		p.Done++
	}
}

// Links are the records behind a card. Run is the run to open first: the
// run that waits for a person, else the newest one. Approval is the
// approval that Run waits for, so approve decides it without a read.
type Links struct {
	Run      string `json:"run,omitempty"`
	Approval string `json:"approval,omitempty"`
	Bridge   string `json:"bridge,omitempty"`
	Duty     string `json:"duty,omitempty"`
	Batch    string `json:"batch,omitempty"`
	Source   string `json:"source,omitempty"`
	// Runs holds the ids of the runs of the card, newest first, at most
	// MaxRuns of them.
	Runs []string `json:"runs,omitempty"`
}

// MaxRuns bounds Links.Runs.
const MaxRuns = 20

// Card is one piece of work. Opened is when it was asked, Since when it
// entered its stage, and Ended when it reached done. Reason says in words
// what the card waits for or why it stopped. Next names who moves it.
type Card struct {
	ID      string     `json:"id"`
	Kind    string     `json:"kind"`
	Title   string     `json:"title"`
	From    From       `json:"from"`
	Stage   string     `json:"stage"`
	Next    string     `json:"next,omitempty"`
	Reason  string     `json:"reason,omitempty"`
	Opened  time.Time  `json:"opened"`
	Since   time.Time  `json:"since"`
	Ended   *time.Time `json:"ended,omitempty"`
	Agents  []string   `json:"agents"`
	Parts   Parts      `json:"parts"`
	Links   Links      `json:"links"`
	CostUSD float64    `json:"costUSD"`
	// Verdict says who or what checked the result of a done card:
	// accepted, checked, sent-back, or unchecked, or let-go for a stopped
	// card that a person let go. A canceled card has none.
	Verdict string `json:"verdict,omitempty"`
	// Lost is true when no worker holds a part of the card that had not
	// ended: the pool replaced its worker, so the part cannot go on.
	Lost bool `json:"lost,omitempty"`
}

// Lane is one stage of the board: its cards, the cards whose next move is
// the caller's, and the time since which the card that waits longest is in
// the stage.
type Lane struct {
	Stage  string    `json:"stage"`
	Count  int       `json:"count"`
	Yours  int       `json:"yours"`
	Oldest time.Time `json:"oldest,omitzero"`
}

// Answer is the board of the caller: the lanes in the order of the flow,
// the cards in that order with the longest wait first in each stage, and
// a sentence for a host that shows only text.
type Answer struct {
	Sentence string `json:"sentence"`
	Lanes    []Lane `json:"lanes"`
	Cards    []Card `json:"cards"`
	// Days is the window of the board: the cards that ended or changed
	// in the last Days days. A card that did not end stays, however old.
	Days int `json:"days"`
}

// Run is a run that the caller can view, with the facts of its work
// (fleet.Work). Lost is true for a run that never ended and that no member
// holds: the pool replaced its worker, so it cannot go on.
type Run struct {
	*fw.Run
	Work fleet.Work
	Lost bool
}

// Start is one start of a duty that the caller can view.
type Start struct {
	Duty  duties.Duty
	Entry duties.RunEntry
}

// Input is what the board reads for one caller. Me is the subject of the
// caller. Mine holds the ids of the runs of the caller and of the teams of
// the caller; with All false, the board keeps the cards of the caller
// alone (mine). Waiting holds the seats that need a person, with the text
// of their questions. Queued counts the jobs that no worker took yet, by
// batch name.
type Input struct {
	Me      string
	All     bool
	Mine    map[string]bool
	Runs    []Run
	Bridges []bridges.Item
	Waiting []bridges.Waiting
	Duties  []duties.Duty
	Starts  []Start
	// Next holds the start that comes next of each duty that runs on a
	// schedule and is not paused, by name.
	Next   map[string]time.Time
	Queued map[string]int
	// LetGo holds the let-go of the cards that people let go, by the id of
	// the card (letgo.go).
	LetGo map[string]LetGo
	// Names holds a name of a person that people read, by subject, for
	// the subjects that the records name alone (From.Display).
	Names  map[string]string
	Now    time.Time
	Window time.Duration
}

// DefaultDays is the window of the board in days, as the recent results of
// the home reach back seven days.
const DefaultDays = 7

// Assemble builds the board from its records.
func Assemble(in Input) Answer {
	if in.Window <= 0 {
		in.Window = DefaultDays * 24 * time.Hour
	}
	b := &board{in: in, byID: map[string]Run{}, cards: map[string]*draft{}, key: map[string]string{}}
	for _, r := range in.Runs {
		if r.Run != nil {
			b.byID[r.ID] = r
		}
	}
	b.dutyOf = map[string]duties.Duty{}
	for _, d := range in.Duties {
		b.dutyOf[d.Name] = d
	}
	for _, s := range in.Starts {
		b.dutyOf[s.Duty.Name] = s.Duty
	}
	for _, it := range in.Bridges {
		b.bridge(it)
	}
	for _, s := range in.Starts {
		b.start(s)
	}
	for _, d := range in.Duties {
		if at, ok := in.Next[d.Name]; ok {
			b.comingUp(d, at)
		}
	}
	runs := slices.Clone(in.Runs)
	slices.SortStableFunc(runs, func(x, y Run) int { return x.CreatedAt.Compare(y.CreatedAt) })
	for _, r := range runs {
		if r.Run != nil {
			b.run(r)
		}
	}
	return b.answer()
}

// board is the work of one Assemble.
type board struct {
	in     Input
	byID   map[string]Run
	dutyOf map[string]duties.Duty
	cards  map[string]*draft
	order  []string
	// key holds the card of each run that a card took.
	key map[string]string
}

// draft is a card while the board builds it, with the stages of its parts.
type draft struct {
	Card
	mine    bool
	parts   []part
	agents  map[string]bool
	settled bool // the record of the card (a bridge or a start) gave the stage
}

// part is one seat or one run of a card: its stage, the reason in words,
// since when, the run, and its approval.
type part struct {
	stage, reason string
	since         time.Time
	run, approval string
	verdict       string
	lost          bool
	// quiet is true for a part that counts but does not speak for its
	// card: a run of a conversation that a newer run continued, and a
	// child run that ended.
	quiet bool
}

func (b *board) card(id string) (*draft, bool) {
	if d, ok := b.cards[id]; ok {
		return d, false
	}
	d := &draft{Card: Card{ID: id}, agents: map[string]bool{}}
	b.cards[id] = d
	b.order = append(b.order, id)
	return d, true
}

// bridge makes the card of a bridge from its seats.
func (b *board) bridge(it bridges.Item) {
	d, _ := b.card("bridge:" + it.ID)
	d.Kind, d.Title, d.Opened, d.settled = KindBridge, it.Title, it.CreatedAt, true
	d.Links.Bridge, d.Links.Batch = it.ID, bridges.BatchOf(it.ID)
	d.From = b.fromSubject(it.Operator, it.OperatorName, "bridge")
	if it.Manager != nil && it.Manager.Run != "" && d.From.Kind == "person" {
		if r, ok := b.byID[it.Manager.Run]; ok && r.Agent != "" && it.Placement == bridges.PlacementQueue {
			d.From.Via = "manager"
		}
	}
	d.mine = it.Operator == b.in.Me || b.ownsDuty(it.Operator)
	last := it.CreatedAt
	if it.LastAt != nil {
		last = *it.LastAt
	}
	if it.State == bridges.StateClosed {
		at := last
		if it.ClosedAt != nil {
			at = *it.ClosedAt
		}
		p := part{stage: StageDone, reason: "Closed.", since: at, verdict: VerdictUnchecked}
		if it.Verdict == bridges.VerdictAccepted {
			// A person said Looks right, which closed the bridge.
			p.reason, p.verdict = "Accepted.", VerdictAccepted
		}
		d.parts = append(d.parts, p)
		for _, m := range it.Members {
			d.agents[m.Agent] = true
		}
		return
	}
	asks := map[string]bridges.Waiting{}
	for _, w := range b.in.Waiting {
		if w.Bridge == it.ID {
			asks[w.Handle] = w
		}
	}
	for _, m := range it.Members {
		d.agents[m.Agent] = true
		p := part{since: last}
		if len(m.Runs) > 0 {
			p.run = m.Runs[len(m.Runs)-1]
			if r, ok := b.byID[p.run]; ok {
				p.since = r.UpdatedAt
			}
		}
		lost := m.Gone
		if r, ok := b.byID[p.run]; ok && r.Lost {
			lost = true
		}
		switch {
		case (m.Presence == bridges.PresenceNeedsYou || m.Question > 0) && lost:
			// The seat asked, and then its worker went: nobody can move
			// the seat by an answer, so it stopped (walk finding L20).
			p.stage, p.lost = StageStopped, true
			p.reason = m.Handle + " lost its worker while its question waited, so it cannot go on."
			d.parts = append(d.parts, p)
			continue
		}
		switch m.Presence {
		case bridges.PresenceNeedsYou:
			p.stage = StageNeedsYou
			if w, ok := asks[m.Handle]; ok {
				p.since, p.approval = w.At, w.Approval
				if w.Question > 0 {
					p.reason = m.Handle + " asks: " + oneLine(plainText(w.Text), 200)
				} else {
					p.reason = m.Handle + " waits for a decision."
				}
			} else {
				p.reason = m.Handle + " waits for you."
			}
		case bridges.PresenceWorking:
			p.stage, p.reason = StageWorking, m.Handle+" works on its part."
		case bridges.PresenceWaiting:
			p.stage, p.reason = StageWaiting, m.Handle+" waits for a worker."
		case bridges.PresenceError:
			p.stage, p.reason = StageStopped, m.Handle+" stopped with an error."
			if m.Gone {
				p.reason, p.lost = m.Handle+" lost its worker before it ended.", true
			}
		case bridges.PresenceLimit:
			p.stage, p.reason = StageStopped, m.Handle+" stopped at a limit."
		case bridges.PresenceNotStarted:
			p.stage, p.reason = StageStopped, m.Handle+" never started."
		case bridges.PresenceFired:
			continue
		default:
			// idle and asleep: the seat ended its turn.
			p.stage, p.reason = StageReview, teamDone
			p.since = last
		}
		d.parts = append(d.parts, p)
	}
	if len(d.parts) == 0 {
		d.parts = append(d.parts, part{stage: StageStopped, reason: "The team has no agent.", since: last})
	}
}

// start makes the card of a start of a duty. A start that found no work
// or did not start is no card, and a start of the actions post and open
// starts no run, so the card of its bridge stands for it.
func (b *board) start(s Start) {
	e := s.Entry
	switch e.State {
	case duties.StateQuiet, duties.StateSkipped:
		return
	case duties.StateCompleted:
		if e.Run == "" {
			return
		}
	}
	d, _ := b.card(dutyKey(s.Duty.Name, e.Slot))
	d.Kind, d.settled = KindDuty, true
	// A start reads as its duty: the title of the duty, else the title of
	// its run, else the name of the duty.
	d.Title = cmp.Or(strings.TrimSpace(s.Duty.Title), e.Title, s.Duty.Name)
	d.Opened = e.Slot
	if e.At.Before(d.Opened) || d.Opened.IsZero() {
		d.Opened = e.At
	}
	d.From = From{Kind: "duty", Name: s.Duty.Name, Via: e.Start}
	d.Links.Duty, d.Links.Run = s.Duty.Name, e.Run
	d.agents[s.Duty.Agent] = true
	d.mine = b.ownsDuty("duty:"+s.Duty.Name) || b.notifies(s.Duty)
	d.CostUSD = e.CostUSD
	p := part{since: e.At, run: e.Run, reason: oneLine(e.Reason, 200)}
	switch e.State {
	case duties.StateStarting, duties.StateQueued:
		p.stage = StageWaiting
		if p.reason == "" {
			p.reason = "Waits for a worker."
		}
	case duties.StateWaiting:
		p.stage = StageWaiting
		if p.reason == "" {
			p.reason = "Waits for the run before it."
		}
	case duties.StateRetrying:
		p.stage = StageWaiting
		if p.reason == "" {
			p.reason = "Goes again soon."
		}
	case duties.StateCompleted:
		p.stage, p.reason, p.verdict = StageDone, "Ended.", VerdictUnchecked
	case string(fw.StateRunning), string(fw.StateSuspended), string(fw.StateInputRequired):
		// A start that works carries the state of its run, and its run
		// below says more. Without the run in the view it still works.
		p.stage = StageWorking
		if e.State == string(fw.StateInputRequired) {
			p.stage = StageNeedsYou
		}
		if p.reason == "" {
			p.reason = agentWorks
		}
	case duties.StateCanceled:
		p.stage, p.reason = StageDone, "Canceled."
	case duties.StateLost:
		p.stage, p.lost = StageStopped, true
		if p.reason == "" {
			p.reason = "No worker reported its run."
		}
	default:
		// failed, missed, check-failed, and a state of a newer front.
		p.stage = StageStopped
		if p.reason == "" {
			p.reason = "Stopped."
		}
	}
	// The run of a start that is on the queue or that completed knows
	// more than the start: it can work, wait for a person, end with a
	// result to read, or have lost its worker.
	switch e.State {
	case duties.StateStarting, duties.StateQueued, duties.StateCompleted,
		string(fw.StateRunning), string(fw.StateSuspended), string(fw.StateInputRequired):
		if r, ok := b.byID[e.Run]; ok {
			p = b.runPart(r)
		}
	}
	d.parts = append(d.parts, p)
}

// comingUp makes the card of the start that comes next of a duty: it
// waits for its time, with since at that time.
func (b *board) comingUp(dt duties.Duty, at time.Time) {
	d, fresh := b.card(dutyKey(dt.Name, at))
	if !fresh {
		return
	}
	d.Kind, d.settled, d.Title = KindDuty, true, cmp.Or(dt.Title, dt.Name)
	d.From = From{Kind: "duty", Name: dt.Name, Via: duties.StartSchedule}
	d.Opened = at
	d.Links.Duty = dt.Name
	d.agents[dt.Agent] = true
	d.mine = b.ownsDuty("duty:"+dt.Name) || b.notifies(dt)
	d.parts = append(d.parts, part{stage: StageWaiting, reason: "Starts on its schedule.", since: at})
}

// run puts a run on its card: the card of its bridge, of its start, of
// its root, of its job, or a card of its own.
func (b *board) run(r Run) {
	id := b.keyOf(r, 0)
	if id == "" {
		return
	}
	d, fresh := b.card(id)
	if fresh {
		b.newCard(d, r)
	}
	if d.Links.Run == "" || r.Parent == "" {
		// The newest top-level run is the run to open.
		d.Links.Run = r.ID
	}
	d.agents[r.Agent] = true
	if d.Kind != KindDuty || !d.settled {
		// A start counts the cost of all its attempts already.
		d.CostUSD += r.Usage.CostUSD
	}
	d.Links.Runs = append(d.Links.Runs, r.ID)
	if b.in.Mine[r.ID] {
		d.mine = true
	}
	if r.CreatedAt.Before(d.Opened) || d.Opened.IsZero() {
		d.Opened = r.CreatedAt
	}
	if d.settled {
		// The bridge or the start gave the stage of the card; the run
		// only adds its links and its cost.
		return
	}
	p := b.runPart(r)
	if d.Kind == KindConversation {
		if r.Parent == "" {
			// A run that continues a conversation takes over from the
			// run before it: the conversation stands where its newest
			// run stands.
			for i := range d.parts {
				d.parts[i].quiet = true
			}
		} else if p.stage == StageStopped || p.stage == StageReview || p.stage == StageDone {
			// A child run that ended reported to its parent, which
			// speaks for the conversation.
			p.quiet = true
		}
	}
	d.parts = append(d.parts, p)
}

// keyOf returns the id of the card of a run. depth bounds a chain.
func (b *board) keyOf(r Run, depth int) string {
	if k, ok := b.key[r.ID]; ok {
		return k
	}
	k := b.findKey(r, depth)
	b.key[r.ID] = k
	return k
}

func (b *board) findKey(r Run, depth int) string {
	md := r.Metadata
	if id, ok := strings.CutPrefix(md[fw.MetaBatch], bridges.BatchPrefix); ok && id != "" {
		if d, ok := b.cards["bridge:"+id]; ok && d.settled {
			return "bridge:" + id
		}
		// A bridge that the caller cannot list shows no card.
		return ""
	}
	if name := md[duties.MetaDuty]; name != "" && md[duties.MetaSlot] != "" {
		if t, err := time.Parse(time.RFC3339Nano, md[duties.MetaSlot]); err == nil {
			return dutyKey(name, t)
		}
	}
	if depth < 32 {
		for _, link := range []string{r.Parent, md[fleet.MetaContinues], md[fw.MetaForkOf]} {
			if link == "" || link == r.ID {
				continue
			}
			if up, ok := b.byID[link]; ok {
				if k := b.keyOf(up, depth+1); k != "" {
					return k
				}
			}
		}
	}
	if src := md[sources.MetaSource]; src != "" {
		return "run:" + r.ID
	}
	if batch := md[fw.MetaBatch]; batch != "" && r.Parent == "" {
		return "job:" + r.Principal + "/" + batch
	}
	return "run:" + r.ID
}

// dutyKey is the id of the card of a start of a duty.
func dutyKey(name string, slot time.Time) string {
	return "duty:" + name + "@" + slot.UTC().Format(time.RFC3339Nano)
}

// newCard fills a card that a run makes.
func (b *board) newCard(d *draft, r Run) {
	md := r.Metadata
	d.Opened = r.CreatedAt
	switch {
	case strings.HasPrefix(d.ID, "duty:"):
		name := md[duties.MetaDuty]
		d.Kind, d.Title = KindDuty, cmp.Or(strings.TrimSpace(b.dutyOf[name].Title), titleOf(r.Run))
		d.From = From{Kind: "duty", Name: name}
		d.Links.Duty = name
		d.mine = b.ownsDuty("duty:"+name) || b.notifies(b.dutyOf[name])
	case strings.HasPrefix(d.ID, "job:"):
		d.Kind, d.Title = KindJob, md[fw.MetaBatch]
		d.From = b.fromRun(r)
		d.From.Via = "queue"
		d.Links.Batch = md[fw.MetaBatch]
	case md[sources.MetaSource] != "":
		d.Kind, d.Title = KindEvent, titleOf(r.Run)
		d.From = From{Kind: "app", Name: md[sources.MetaSource], Via: "call"}
		d.Links.Source, d.Links.Batch = md[sources.MetaSource], md[fw.MetaBatch]
	default:
		d.Kind, d.Title = KindConversation, titleOf(r.Run)
		d.From = b.fromRun(r)
	}
}

// fromRun says who asked for a run.
func (b *board) fromRun(r Run) From {
	md := r.Metadata
	if via := md[manager.MetaVia]; via != "" {
		agent, _, _ := strings.Cut(via, ":")
		f := b.fromSubject(r.Principal, "", "")
		if agent == "manager" {
			f.Via = "manager"
		} else {
			f.Via = "agent " + agent
		}
		return f
	}
	f := b.fromSubject(r.Principal, "", "")
	switch {
	case r.Origin == fw.OriginMCP:
		f.Via = "mcp"
	case r.Origin == fw.OriginBus:
		f.Via = "queue"
	case r.Origin == fw.OriginHTTP:
		f.Via = "page"
	default:
		f.Via = r.Origin
	}
	return f
}

// fromSubject words a principal: a duty, an app of a source, or a person.
func (b *board) fromSubject(subject, name, via string) From {
	switch {
	case strings.HasPrefix(subject, "duty:"):
		return From{Kind: "duty", Name: strings.TrimPrefix(subject, "duty:"), Via: via}
	case strings.HasPrefix(subject, "source:"):
		return From{Kind: "app", Name: strings.TrimPrefix(subject, "source:"), Via: via}
	}
	return From{Kind: "person", Name: cmp.Or(name, subject), Display: b.display(subject, name), Via: via}
}

// display is a name of a person that people read: the name that the
// record keeps, else the name that the front knows for the subject, else
// the subject when it is no id that the issuer made, else "".
func (b *board) display(subject, name string) string {
	if n := personName(name); n != "" {
		return n
	}
	if n := personName(b.in.Names[subject]); n != "" {
		return n
	}
	if subject != "" && !opaqueID(subject) {
		return subject
	}
	return ""
}

// ownsDuty reports whether the caller owns the duty of a principal
// duty:<name>.
func (b *board) ownsDuty(subject string) bool {
	name, ok := strings.CutPrefix(subject, "duty:")
	if !ok {
		return false
	}
	d, ok := b.dutyOf[name]
	return ok && b.in.Me != "" && d.Owner == b.in.Me
}

// notifies reports whether a duty tells the caller of its failures, its
// questions, or its results.
func (b *board) notifies(d duties.Duty) bool {
	if b.in.Me == "" {
		return false
	}
	if d.Owner == b.in.Me {
		return true
	}
	if d.Notify == nil {
		return false
	}
	return slices.Contains(d.Notify.Failure, b.in.Me) || slices.Contains(d.Notify.NeedsYou, b.in.Me) || slices.Contains(d.Notify.Result, b.in.Me)
}

// runPart is the stage of one run.
func (b *board) runPart(r Run) part {
	p := part{run: r.ID, since: r.UpdatedAt}
	if r.Lost {
		p.stage, p.lost = StageStopped, true
		p.reason = "Its worker was replaced before it ended, so it cannot go on."
		return p
	}
	if r.Blocked != "" {
		p.stage, p.reason = StageStopped, "Its worker is blocked: "+oneLine(r.Blocked, 160)
		return p
	}
	switch r.State {
	case fw.StateInputRequired:
		p.stage = StageNeedsYou
		p.reason = pendingWords(r.Run)
		if a := r.Pending; a != nil {
			p.approval = a.ID
			if !a.RequestedAt.IsZero() {
				p.since = a.RequestedAt
			}
		}
	case fw.StateQueued:
		p.stage, p.reason, p.since = StageWaiting, "Waits for a worker.", r.CreatedAt
	case fw.StateRunning, fw.StateSuspended:
		p.stage, p.since = StageWorking, r.CreatedAt
		p.reason = agentWorks
		if a := r.Activity; a != nil && a.Tool != "" {
			p.reason = capital(toolWords(a.Tool)) + "."
		}
	case fw.StateCompleted:
		p.stage = StageDone
		switch v := judged(r); {
		case fleet.ToCheck(r.Run, r.Work) && fleet.Again(r.Work):
			p.stage, p.reason = StageReview, "A new answer to check."
			if n := strings.TrimSpace(r.Work.Note); n != "" {
				p.reason = "A new answer to check. Your note: " + oneLine(n, 120)
			}
		case fleet.ToCheck(r.Run, r.Work):
			// The owner asked to check each result, or a duty that tells
			// only of what it finds found something, and nobody checked it
			// yet.
			p.stage, p.reason = StageReview, "A result to check."
			if what, ok := fleet.Found(r.Run); ok {
				p.reason = "Found: " + oneLine(plainText(what), 200)
			}
		case v == VerdictAccepted:
			p.verdict, p.reason = v, "Accepted."
		case v == VerdictSentBack:
			p.verdict, p.reason = v, "Sent back."
		case fleet.Checked(r.Run, r.Work):
			p.verdict, p.reason = VerdictChecked, "Its checks passed."
		case strings.TrimSpace(r.Output) == "":
			p.verdict, p.reason = VerdictUnchecked, "Ended with nothing to read."
		default:
			p.verdict, p.reason = VerdictUnchecked, "Ended. Nobody checked the result."
		}
	case fw.StateFailed:
		p.stage = StageStopped
		switch {
		case r.Done != nil && r.Done.State == fw.DoneFailed:
			p.reason = "Not done: " + notDone(r.Done)
		case strings.HasPrefix(r.Error, "budget exceeded"):
			p.reason = "Stopped at " + limitWords(r.Error) + "."
		case strings.TrimSpace(r.Error) != "":
			p.reason = "Failed: " + oneLine(r.Error, 200)
		default:
			p.reason = "Failed."
		}
	case fw.StateCanceled:
		p.stage, p.reason = StageDone, "Canceled."
	default:
		p.stage, p.reason = StageWorking, agentWorks
	}
	return p
}

// Reasons of a card in plain words: no card says "Works." alone, and none
// names the seats or the turns of a team (walk finding L8).
const (
	agentWorks = "The agent works on it."
	teamDone   = "The agents finished, and the result waits for a check."
)

// judged is the verdict of a person, or of a receipt, on the result of a
// run: the newest verdict of its facts, else its reactions. A thumb down
// sends the result back, and a thumb up accepts it. A heart keeps the
// result on the wiki, and it is no verdict.
func judged(r Run) string {
	switch cmp.Or(r.Work.LastVerdict, r.Work.Verdict) {
	case fleet.VerdictAccepted:
		return VerdictAccepted
	case fleet.VerdictRejected:
		return VerdictSentBack
	}
	switch {
	case r.Reactions[fw.ReactionDown] > 0:
		return VerdictSentBack
	case r.Reactions[fw.ReactionUp] > 0:
		return VerdictAccepted
	}
	return ""
}

// notDone words a definition of done that failed: the checks that failed,
// when the run object holds them, else that its checks did not pass.
func notDone(d *fw.RunDone) string {
	var failed []string
	for _, c := range d.Checks {
		if !c.Passed {
			failed = append(failed, cmp.Or(c.Name, c.Tool))
		}
	}
	if len(failed) == 0 {
		return "its checks did not pass."
	}
	return "the check " + strings.Join(failed, ", ") + " failed."
}

// pendingWords says what a run that waits for a person asks.
func pendingWords(r *fw.Run) string {
	a := r.Pending
	switch {
	case a == nil:
		return "Waits for you."
	case a.Connect != nil:
		return "Asks you to connect a service."
	case len(a.Schema) > 0:
		if q := oneLine(a.Reason, 200); q != "" {
			return "Asks: " + q
		}
		return "Asks you a question."
	case a.Tool != "":
		return "Asks before it " + toolWords(a.Tool) + "."
	}
	return "Waits for you."
}

// answer settles every card and orders the board.
func (b *board) answer() Answer {
	ans := Answer{Lanes: []Lane{}, Cards: []Card{}, Days: int(b.in.Window / (24 * time.Hour))}
	lanes := map[string]*Lane{}
	for _, s := range Stages {
		lanes[s] = &Lane{Stage: s}
	}
	for _, id := range b.order {
		d := b.cards[id]
		if len(d.parts) == 0 {
			continue
		}
		if !b.in.All && !d.mine {
			continue
		}
		b.settle(d)
		b.letGo(d)
		if d.Stage == StageDone && d.Since.Before(b.in.Now.Add(-b.in.Window)) {
			continue
		}
		if d.Stage == StageReview && d.Since.Before(b.in.Now.Add(-b.in.Window)) && d.Kind != KindBridge {
			// A result that nobody read in the window falls silent, as
			// on the home. An open bridge stays until a person closes it.
			continue
		}
		ans.Cards = append(ans.Cards, d.Card)
		l := lanes[d.Stage]
		l.Count++
		if d.Next == NextYou {
			l.Yours++
		}
		// A start that comes next waits for no one yet.
		if !d.Since.After(b.in.Now) && (l.Oldest.IsZero() || d.Since.Before(l.Oldest)) {
			l.Oldest = d.Since
		}
	}
	rank := map[string]int{}
	for i, s := range Stages {
		rank[s] = i
	}
	slices.SortStableFunc(ans.Cards, func(x, y Card) int {
		if c := cmp.Compare(rank[x.Stage], rank[y.Stage]); c != 0 {
			return c
		}
		if x.Stage == StageDone {
			return y.Since.Compare(x.Since)
		}
		return x.Since.Compare(y.Since)
	})
	for _, s := range Stages {
		ans.Lanes = append(ans.Lanes, *lanes[s])
	}
	ans.Sentence = sentence(ans, b.in.Now)
	return ans
}

// settle gives a draft its stage from its parts, and its next move.
func (b *board) settle(d *draft) {
	var top part
	d.Parts = Parts{}
	for _, p := range d.parts {
		d.Parts.add(p.stage)
		if p.quiet {
			continue
		}
		if p.lost {
			d.Lost = true
		}
		switch {
		case top.stage == "", weight[p.stage] > weight[top.stage]:
			top = p
		case p.stage == top.stage && p.since.Before(top.since) && p.stage != StageDone && p.stage != StageReview:
			// The part that waits longest speaks for its stage.
			top = p
		case p.stage == top.stage && p.since.After(top.since) && (p.stage == StageDone || p.stage == StageReview):
			top = p
		}
	}
	if d.Kind == KindJob {
		if n := b.in.Queued[d.Links.Batch]; n > 0 {
			d.Parts.Waiting += n
			d.Parts.Total += n
			if weight[StageWaiting] > weight[top.stage] {
				top = part{stage: StageWaiting, reason: plural(n, "item waits", "items wait") + " for a worker.", since: d.Opened}
			}
		}
	}
	d.Stage, d.Reason, d.Since = top.stage, top.reason, top.since
	switch {
	case d.Kind == KindBridge && d.Stage == StageReview:
		d.Reason = teamDone
	case d.Kind == KindJob && d.Stage != StageNeedsYou,
		d.Kind == KindBridge && d.Parts.Total > 1 && (d.Stage == StageWorking || d.Stage == StageWaiting):
		if s := partWords(d.Parts); s != "" {
			d.Reason = s
		}
	}
	if top.run != "" && (top.stage == StageNeedsYou || top.stage == StageStopped) {
		d.Links.Run = top.run
	}
	d.Links.Approval = ""
	if top.stage == StageNeedsYou {
		d.Links.Approval = top.approval
	}
	if d.Stage == StageDone {
		at := d.Since
		d.Ended = &at
		d.Verdict = top.verdict
	}
	d.Next = b.next(d)
	d.Agents = make([]string, 0, len(d.agents))
	for a := range d.agents {
		if a != "" {
			d.Agents = append(d.Agents, a)
		}
	}
	slices.Sort(d.Agents)
	slices.Reverse(d.Links.Runs)
	if len(d.Links.Runs) > MaxRuns {
		d.Links.Runs = d.Links.Runs[:MaxRuns]
	}
}

// next says who moves a card next.
func (b *board) next(d *draft) string {
	switch d.Stage {
	case StageWaiting:
		return NextWorker
	case StageWorking:
		return NextAgent
	case StageDone:
		return ""
	case StageNeedsYou:
		if d.Kind == KindDuty {
			if dt, ok := b.dutyOf[d.Links.Duty]; ok && dt.WhileAway != nil && dt.WhileAway.Decider == "manager" && d.Links.Approval != "" {
				if r, ok := b.byID[d.Links.Run]; ok && r.Pending != nil && r.Pending.Connect == nil && len(r.Pending.Schema) == 0 {
					return NextManager
				}
			}
		}
	}
	if d.mine {
		return NextYou
	}
	return NextPerson
}

// partWords counts the parts of a job in words, such as "3 working, 1
// stopped, 6 done".
func partWords(p Parts) string {
	var out []string
	for _, x := range []struct {
		n    int
		word string
	}{{p.NeedsYou, "need you"}, {p.Waiting, "coming up"}, {p.Working, "working"}, {p.Stopped, "stopped"}, {p.Review, "to check"}, {p.Done, "done"}} {
		if x.n > 0 {
			out = append(out, fmt.Sprintf("%d %s", x.n, x.word))
		}
	}
	return strings.Join(out, ", ") + map[bool]string{true: ".", false: ""}[len(out) > 0]
}

// laneWords names a stage in the sentence.
var laneWords = map[string][2]string{
	StageWaiting:  {"is coming up", "are coming up"},
	StageWorking:  {"is in work", "are in work"},
	StageNeedsYou: {"needs you", "need you"},
	StageStopped:  {"stopped", "stopped"},
	StageReview:   {"is to check", "are to check"},
	StageDone:     {"is done", "are done"},
}

// sentence words the board in one paragraph: the cards in flight, then
// each stage that holds a card, with the wait of the oldest one.
func sentence(ans Answer, now time.Time) string {
	open := 0
	for _, l := range ans.Lanes {
		if l.Stage != StageDone {
			open += l.Count
		}
	}
	if len(ans.Cards) == 0 {
		return "No work in the last " + plural(ans.Days, "day", "days") + "."
	}
	parts := []string{plural(open, "task is open", "tasks are open") + "."}
	for _, l := range ans.Lanes {
		if l.Count == 0 || l.Stage == StageDone {
			continue
		}
		w := laneWords[l.Stage]
		verb := w[1]
		if l.Count == 1 {
			verb = w[0]
		}
		s := fmt.Sprintf("%d %s", l.Count, verb)
		if l.Stage != StageWorking && !l.Oldest.IsZero() {
			s += ", the oldest for " + Span(now.Sub(l.Oldest))
		}
		parts = append(parts, s+".")
	}
	for _, l := range ans.Lanes {
		if l.Stage == StageDone && l.Count > 0 {
			parts = append(parts, fmt.Sprintf("%d done in the last %s.", l.Count, plural(ans.Days, "day", "days")))
		}
	}
	return strings.Join(parts, " ")
}

// Span words a duration the way the page writes a time: 40 min, 3 h, 5 d.
func Span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute", "minutes")
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour", "hours")
	}
	return plural(int(d/(24*time.Hour)), "day", "days")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// titleOf is the title of a run.
func titleOf(r *fw.Run) string {
	if t := strings.TrimSpace(r.Title); t != "" {
		return t
	}
	if t := fw.TitleFrom(r.Input); t != "" {
		return t
	}
	return r.Agent
}

// oneLine cuts a text to one line of at most n bytes.
// limitWords names the limit of a run that stopped at its budget, from
// its error "budget exceeded: <limit>", in words for a person, such as
// "its limit of steps" for maxTurns. An unknown limit is "a limit of its
// budget".
func limitWords(err string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(err, "budget exceeded"), ":")), " ")
	switch strings.TrimRight(name, ".,;") {
	case "maxTurns":
		return "its limit of steps"
	case "maxToolCalls":
		return "its limit of tool calls"
	case "maxCallsPerTurn":
		return "its limit of tool calls in one step"
	case "maxTokens":
		return "its limit of tokens"
	case "maxCostUSD":
		return "its limit of cost"
	case "deadline":
		return "its time limit"
	}
	return "a limit of its budget"
}

// markdownMarks are the marks of Markdown that a line of a card drops:
// strong and code spans. A single underscore stays, as in node_exporter.
var markdownMarks = strings.NewReplacer("**", "", "__", "", "`", "")

// plainText returns the text of a message without the marks of Markdown,
// so a card shows the words of a question, not its markup: no strong or
// code marks, no heading, quote, or list mark at the start of a line, and
// the text of a link without its address.
func plainText(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		l = strings.TrimSpace(l)
		l = strings.TrimLeft(l, "#> ")
		for _, mark := range []string{"- ", "* ", "+ "} {
			l = strings.TrimPrefix(l, mark)
		}
		lines[i] = reMarkdownLink.ReplaceAllString(l, "$1")
	}
	return markdownMarks.Replace(strings.Join(lines, "\n"))
}

// reMarkdownLink matches a link of Markdown, [text](address).
var reMarkdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

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
