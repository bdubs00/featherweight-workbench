package bridges

// This file holds the shapes of a bridge: the record, its members, and the
// entries of its channel, as the routes answer them (the contract of the
// page, sections 5 and 6), and the bookkeeping that the record keeps
// beside them in the store.

import (
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/skills"
)

// States of a bridge.
const (
	StateOpen   = "open"
	StateClosed = "closed"
)

// Placements of a bridge: how it seats its members (section 18.3).
const (
	PlacementQueue = "queue"
	PlacementSpawn = "spawn"
)

// MetaMember is the metadata key of a run that names the seat of the run
// in its bridge: the handle of the member.
const MetaMember = "fw.bridge.member"

// BatchPrefix starts the batch of every run of a bridge: bridge:<id>.
const BatchPrefix = "bridge:"

// BatchOf returns the batch of the runs of a bridge.
func BatchOf(id string) string { return BatchPrefix + id }

// Reserved handles. A seat never takes one: operator addresses the people
// of the bridge, and manager names the manager run in the channel.
const (
	HandleOperator = "operator"
	HandleManager  = "manager"
)

// Kinds of a sender (From.Kind).
const (
	KindPerson = "person"
	KindAgent  = "agent"
	KindSystem = "system"
)

// Events of a system line (Message.Event).
const (
	EventOpened   = "opened"
	EventClosed   = "closed"
	EventReopened = "reopened"
	EventStarted  = "started"
	EventJoined   = "joined"
	EventFinished = "finished"
	EventNeedsYou = "needs-you"
	EventAsked    = "asked"
	EventDecided  = "decided"
	EventFailed   = "failed"
	EventAsleep   = "asleep"
	EventFired    = "fired"
	EventCanceled = "canceled"
	EventMuted    = "muted"
	EventTask     = "task"
	EventChanged  = "changed"
	EventCap      = "cap"
)

// Presences of a member (Presence, section 18.7).
const (
	PresenceWorking  = "working"
	PresenceNeedsYou = "needs-you"
	PresenceIdle     = "idle"
	PresenceAsleep   = "asleep"
	PresenceError    = "error"
	PresenceFired    = "fired"
)

// TaskBlocked is the state of a Task that a storage failure blocked, for
// Presence: the member of the view has a BlockedReason.
const TaskBlocked = "blocked"

// Reactions on an entry, as the kernel names them.
const (
	ReactionUp    = "up"
	ReactionDown  = "down"
	ReactionHeart = "heart"
)

// Bridge is the record of a bridge: one job split into seats, with one
// channel and one batch, BatchOf(ID). Members holds the seats, with the
// presence and the activity of each when a route answers it.
type Bridge struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Goal       string     `json:"goal"`
	Operator   string     `json:"operator"`
	State      string     `json:"state"`
	Placement  string     `json:"placement"`
	CreatedAt  time.Time  `json:"createdAt"`
	ClosedAt   *time.Time `json:"closedAt"`
	Manager    *Manager   `json:"manager"`
	Delegation Delegation `json:"delegation"`
	Caps       Caps       `json:"caps"`
	Members    []Member   `json:"members,omitempty"`
	// LastSeq is the seq of the newest entry of the channel. A change of
	// the record moves it, so two replicas never give two entries one seq.
	LastSeq uint64 `json:"lastSeq"`
}

// Manager names the manager of a bridge: the fleet member that runs it and
// the run that holds the bridge. In a spawn bridge the run is the root of
// the tree of the seats. A bridge that the manager opened names the run
// that opened it.
type Manager struct {
	Member string `json:"member"`
	Run    string `json:"run"`
}

// Delegation says what the operator lets the manager do in one bridge.
type Delegation struct {
	Approve bool `json:"approve"`
	Budget  bool `json:"budget"`
	Members bool `json:"members"`
	Report  bool `json:"report"`
}

// Caps bound the fan-out of a bridge (section 18.5).
type Caps struct {
	MaxMessages        int `json:"maxMessages"`
	MaxFanoutPerMinute int `json:"maxFanoutPerMinute"`
	MaxPairMessages    int `json:"maxPairMessages"`
}

// Member is one seat of a bridge. Runs lists the runs of the seat in the
// bridge, oldest first; the last one is the newest. Presence and Activity
// come from the view of the fleet when a route or a hook gives the member,
// and the store keeps neither.
type Member struct {
	Handle   string    `json:"handle"`
	Agent    string    `json:"agent"`
	Pool     string    `json:"pool,omitempty"`
	Task     string    `json:"task"`
	Runs     []string  `json:"runs"`
	Fired    bool      `json:"fired"`
	Muted    bool      `json:"muted"`
	Presence string    `json:"presence"`
	Activity *Activity `json:"activity"`
}

// newest returns the newest run of the seat, or "".
func (m Member) newest() string {
	if len(m.Runs) == 0 {
		return ""
	}
	return m.Runs[len(m.Runs)-1]
}

// Activity is what the newest run of a member does now (the K4 activity of
// the kernel): its turn, the tool call that it waits for, a phrase for a
// person, and the time that it began. Phrase is empty when the front
// cannot word the call, because the view holds no tool input.
type Activity struct {
	Turn   int       `json:"turn"`
	Tool   string    `json:"tool,omitempty"`
	CallID string    `json:"callId,omitempty"`
	Phrase string    `json:"phrase"`
	Since  time.Time `json:"since"`
}

// Item is a bridge as GET /v1/bridges lists it for one caller, and as the
// other routes answer it with Members: the record and the counts of the
// caller. Unread is at most MaxUnread, and UnreadMore is true when the
// count reached past it. Usage is set by GET /v1/bridges/{id} alone.
type Item struct {
	Bridge
	Read        uint64          `json:"read"`
	Unread      int             `json:"unread"`
	UnreadMore  bool            `json:"unreadMore"`
	MemberCount int             `json:"memberCount"`
	NeedsYou    int             `json:"needsYou"`
	Usage       *fleet.UsageSum `json:"usage,omitempty"`
}

// Message is one entry of the channel of a bridge. Page names the page of
// the wiki that a system line is about, such as bridges/<id>/report.md, so
// the page of bossmode links it without a link in the text. Pinned is true
// for a message that a person pinned and for a slash command: each of its
// deliveries is a pinned follow-up, which survives every compaction of the
// run that gets it.
type Message struct {
	Seq       uint64              `json:"seq"`
	At        time.Time           `json:"at"`
	From      From                `json:"from"`
	To        []string            `json:"to"`
	ReplyTo   *uint64             `json:"replyTo"`
	Text      string              `json:"text"`
	Event     string              `json:"event,omitempty"`
	Page      string              `json:"page,omitempty"`
	Cut       bool                `json:"cut"`
	Pinned    bool                `json:"pinned"`
	Mirrors   *Mirror             `json:"mirrors"`
	Reactions map[string][]string `json:"reactions"`
	Decision  *Decision           `json:"decision"`
	// Skill names the skill and the arguments of a slash command of a
	// person. The text of the entry is then the line Skill and a summary,
	// and the runs of the members hold the whole message.
	Skill *skills.Command `json:"skill,omitempty"`
}

// From is the sender of an entry. A person has Name, the subject, and
// Display, a name to show. An agent has Handle and Run. The system has
// neither.
type From struct {
	Kind    string `json:"kind"`
	Handle  string `json:"handle,omitempty"`
	Run     string `json:"run,omitempty"`
	Name    string `json:"name,omitempty"`
	Display string `json:"display,omitempty"`
}

// Mirror names the run event that an entry mirrors, so a reaction on the
// entry reaches the run.
type Mirror struct {
	Run  string `json:"run"`
	Seq  uint64 `json:"seq"`
	Type string `json:"type"`
}

// Decision is the approval or the question of a member run on the entry
// that mirrors it: the fields of dashboard.Decision, the run, and the state
// of the decision. Always is true when the card can offer Always allow, by
// the rule of a pool (docs/ARCHITECTURE.md section 14.9).
type Decision struct {
	Run string `json:"run"`
	dashboard.Decision
	State     string     `json:"state"`
	DecidedBy string     `json:"decidedBy,omitempty"`
	DecidedAt *time.Time `json:"decidedAt,omitempty"`
	Note      string     `json:"note,omitempty"`
	Answer    string     `json:"answer,omitempty"`
	Always    bool       `json:"always"`
}

// States of a decision. DecisionUnknown is the state of a card whose run
// the front failed to read after decisionTries tries: the run no longer
// waits for the card, and nobody can decide it on the page. A later read
// that works writes the card again with its real state.
const (
	DecisionPending = "pending"
	DecisionAllow   = "allow"
	DecisionDeny    = "deny"
	DecisionUnknown = "unknown"
)

// record is a bridge as the store keeps it: the record of the routes and
// the bookkeeping of the front beside it. Each change of a record is a
// compare-and-swap, so two replicas never lose a change.
type record struct {
	Bridge
	// OperatorKind is the kind of the principal of the operator, for the
	// runs that the bridge starts as the operator.
	OperatorKind string `json:"operatorKind,omitempty"`
	// Teams holds the teams of the operator at the open, so the teammates
	// of the operator find the bridge in their list.
	Teams []string `json:"teams,omitempty"`
	// Seats holds the bookkeeping of each seat, by handle.
	Seats map[string]*seatMark `json:"seats,omitempty"`
	// Marks holds what the channel mirrored of each run of the bridge, by
	// run id, so a fact gets one entry, never two.
	Marks map[string]*runMark `json:"marks,omitempty"`
	// Cards lists the entries whose decision waits, which count as unread.
	Cards []uint64 `json:"cards,omitempty"`
	// Fan holds the counters of the caps.
	Fan fanMark `json:"fan"`
	// Report is the report that the close asked the manager for.
	Report *reportMark `json:"report,omitempty"`
}

// seatMark is the bookkeeping of one seat.
type seatMark struct {
	// Seatings counts the runs that the bridge started for the seat; the
	// idempotency key of the next one is bridge:<id>:<handle>:<n>.
	Seatings int `json:"seatings,omitempty"`
	// Budget is the budget of the seat, for a seat again and for a spawn.
	Budget *SeatBudget `json:"budget,omitempty"`
	// Asleep is true when the channel says that the Task of the seat
	// sleeps and no line after it says that it woke.
	Asleep bool `json:"asleep,omitempty"`
	// QuietAt is the time at which a replica first saw the Task QuietOn of
	// the newest run of the seat quiet. A replica that sees that Task in
	// another state clears both, so the channel says that the Task sleeps
	// only once no replica saw it awake for sleepHold (sleepLines).
	QuietAt *time.Time `json:"quietAt,omitempty"`
	QuietOn string     `json:"quietOn,omitempty"`
	// Again is the newest run of the seat when the bridge last seated it
	// again, and AgainAt the time, so a second patch with the same task
	// before a new run shows seats nothing more (giveTask).
	Again   string     `json:"again,omitempty"`
	AgainAt *time.Time `json:"againAt,omitempty"`
}

// SeatBudget is the budget of one seat: the turns and the working time of
// its runs. Deadline is a duration such as 30m.
type SeatBudget struct {
	MaxTurns int    `json:"maxTurns,omitempty"`
	Deadline string `json:"deadline,omitempty"`
}

// runMark is what the channel mirrored of one run.
type runMark struct {
	// Seat is the handle of the seat of the run, or HandleManager.
	Seat string `json:"seat"`
	// Started is true once the channel holds the start of the run.
	Started bool `json:"started,omitempty"`
	// Done is the seq of the last end of the run that the channel holds.
	Done uint64 `json:"done,omitempty"`
	// Pending is the approval that the card Card shows, while it waits.
	Pending string `json:"pending,omitempty"`
	Card    uint64 `json:"card,omitempty"`
	// AskSeq is the seq of the run when the card was posted, where the
	// read of the decision starts. Question is true when the card asks a
	// question, whose decision carries an answer.
	AskSeq   uint64 `json:"askSeq,omitempty"`
	Question bool   `json:"question,omitempty"`
	// Limit is true once the channel says that the run failed and takes no
	// message: at a limit of its budget, or for another reason.
	Limit bool `json:"limit,omitempty"`
	// Lost holds the cards of the run that show the state unknown, because
	// the front failed to read the run after decisionTries tries. The
	// watch reads the run again for them, so a later read writes each one
	// again with its real decision.
	Lost []lostCard `json:"lost,omitempty"`
}

// lostCard is a card whose decision the front failed to read: the
// approval, the seq of the card, the seq of the run where the read
// starts, and whether the card asks a question.
type lostCard struct {
	Pending  string `json:"pending"`
	Card     uint64 `json:"card"`
	AskSeq   uint64 `json:"askSeq,omitempty"`
	Question bool   `json:"question,omitempty"`
}

// maxLost bounds the lost cards that the mark of one run keeps. An older
// one stays unknown.
const maxLost = 16

// fanMark holds the counters of the caps of a bridge.
type fanMark struct {
	// Messages counts the messages of people and agents in the channel.
	Messages int `json:"messages,omitempty"`
	// Minute is the start of the minute that Count counts deliveries in,
	// and MinuteCapped is the minute that the channel holds a cap line of.
	Minute       time.Time `json:"minute,omitempty"`
	Count        int       `json:"count,omitempty"`
	MinuteCapped time.Time `json:"minuteCapped,omitempty"`
	// Capped is true once the channel says that the bridge reached
	// maxMessages; a change of the caps clears it.
	Capped bool `json:"capped,omitempty"`
	// Pairs counts the messages between two agents since the last message
	// of a person, by pairKey, and PairCapped names the pairs that the
	// channel holds a cap line of.
	Pairs      map[string]int  `json:"pairs,omitempty"`
	PairCapped map[string]bool `json:"pairCapped,omitempty"`
}

// reportMark is the report that the close of a bridge asked the manager
// for, in the page bridges/<id>/report.md.
type reportMark struct {
	AskedAt time.Time `json:"askedAt"`
	Posted  bool      `json:"posted,omitempty"`
}

// pairKey names a pair of handles in either order.
func pairKey(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return a + "|" + b
}

// alloc gives the next seq of the channel.
func (r *record) alloc() uint64 {
	r.LastSeq++
	return r.LastSeq
}

// seat returns the bookkeeping of a seat, made on first use.
func (r *record) seat(handle string) *seatMark {
	if r.Seats == nil {
		r.Seats = map[string]*seatMark{}
	}
	s := r.Seats[handle]
	if s == nil {
		s = &seatMark{}
		r.Seats[handle] = s
	}
	return s
}

// mark returns the mark of a run, made on first use.
func (r *record) mark(run, seat string) *runMark {
	if r.Marks == nil {
		r.Marks = map[string]*runMark{}
	}
	m := r.Marks[run]
	if m == nil {
		m = &runMark{Seat: seat}
		r.Marks[run] = m
	}
	return m
}

// member returns the seat with the handle, or nil.
func (r *record) member(handle string) *Member {
	for i := range r.Members {
		if r.Members[i].Handle == handle {
			return &r.Members[i]
		}
	}
	return nil
}

// seatOfRun returns the handle of the seat whose runs hold the run, or "".
func (r *record) seatOfRun(run string) string {
	for _, m := range r.Members {
		for _, id := range m.Runs {
			if id == run {
				return m.Handle
			}
		}
	}
	return ""
}

// dropCard removes a card from the cards that wait.
func (r *record) dropCard(seq uint64) {
	out := r.Cards[:0]
	for _, c := range r.Cards {
		if c != seq {
			out = append(out, c)
		}
	}
	r.Cards = out
}

// waits reports whether the card with the seq waits for a decision.
func (r *record) waits(seq uint64) bool {
	for _, c := range r.Cards {
		if c == seq {
			return true
		}
	}
	return false
}

// public returns the record as the routes answer it, without the
// bookkeeping, with a copy of the members.
func (r *record) public() Bridge {
	b := r.Bridge
	b.Members = append([]Member(nil), r.Members...)
	for i := range b.Members {
		b.Members[i].Runs = append([]string{}, b.Members[i].Runs...)
	}
	return b
}

// seqKey is the key of an entry of the channel: <bridge>.<seq>, with the
// seq in twelve digits, so the keys of one bridge sort by seq.
func seqKey(id string, seq uint64) string {
	return id + "." + leftPad(strconv.FormatUint(seq, 10), 12)
}

func leftPad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return strings.Repeat("0", n-len(s)) + s
}

// newMessage returns an entry with the empty fields that the page reads.
func newMessage(from From, text string) Message {
	return Message{From: from, To: []string{}, Text: text, Reactions: emptyReactions()}
}

// emptyReactions is the reactions of an entry that has none.
func emptyReactions() map[string][]string {
	return map[string][]string{ReactionUp: {}, ReactionDown: {}, ReactionHeart: {}}
}

// system returns a system line.
func system(event, text string) Message {
	m := newMessage(From{Kind: KindSystem}, text)
	m.Event = event
	return m
}
