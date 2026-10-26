package duties

// This file is the record of a duty (docs/ARCHITECTURE.md section 22.1):
// who does it, when, what, and its limits, with the defaults that a write
// fills and the problems that a write refuses, each in words.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-workbench/duties/schedule"
	"github.com/bdubs00/featherweight-workbench/sources"
)

// Duty is the record of a duty, as a person writes it and the store keeps
// it. When holds exactly one start, Do exactly one action, and Limits a
// budget: a duty without a budget does not save, because a paid default
// would be a silent cost.
type Duty struct {
	Name       string      `json:"name"`
	Agent      string      `json:"agent"`
	Pool       string      `json:"pool,omitempty"`
	Owner      string      `json:"owner,omitempty"`
	When       When        `json:"when"`
	CheckFirst *CheckFirst `json:"checkFirst,omitempty"`
	Do         Do          `json:"do"`
	Limits     Limits      `json:"limits"`
	WhileAway  *WhileAway  `json:"whileAway,omitempty"`
	Notify     *Notify     `json:"notify,omitempty"`
	RunAs      string      `json:"runAs,omitempty"`
	Paused     bool        `json:"paused"`

	// from is the hidden part of an imported duty (imported.go): its
	// templates and the address and the secrets that the configuration of
	// the server gave it. No read and no write carries it.
	from *imported
}

// When is the start of a duty: a schedule, the words of a schedule, an
// event of the fleet, or a call of another app. The front turns Say into
// Schedule and stores only Schedule.
type When struct {
	Schedule *schedule.Schedule `json:"schedule,omitempty"`
	Say      string             `json:"say,omitempty"`
	Event    *Event             `json:"event,omitempty"`
	Webhook  *Webhook           `json:"webhook,omitempty"`
}

// Event is an event of the fleet that starts a duty. Kind is one of the
// Event constants. A later package serves the events (RegisterEvents).
type Event struct {
	Kind  string `json:"kind"`
	Agent string `json:"agent,omitempty"`
	After string `json:"after,omitempty"`
	Path  string `json:"path,omitempty"`
}

// Kinds of an event.
const (
	EventBridgeClosed    = "bridge.closed"
	EventRunFailed       = "run.failed"
	EventQuestionWaiting = "question.waiting"
	EventPageChanged     = "page.changed"
	EventRoutineReady    = "routine.ready"
	EventCapReached      = "cap.reached"
)

// Webhook is the start of a duty by another app, on POST
// /v1/duties/{name}/call. Auth is the credential of the caller, as for a
// webhook source. Key is a dotted path in the body to the key of the
// event: a second call with the same key within a week is a duplicate.
// Without Key, a second call with the same body within ten minutes is.
type Webhook struct {
	Auth WebhookAuth `json:"auth"`
	Key  string      `json:"key,omitempty"`
}

// WebhookAuth is how the caller of a webhook proves itself: the mode
// bearer, hmac, or none, the variable of the front that holds the secret,
// and the header of an HMAC signature.
type WebhookAuth struct {
	Mode   string `json:"mode"`
	Secret string `json:"secret,omitempty"`
	Header string `json:"header,omitempty"`
}

// CheckFirst looks for work before the duty wakes the model. Look reads a
// JSON document, Items is the dotted path of the list of items in it, Key
// the dotted path of the key of an item, Where the filters of an item,
// and Wake when the items wake the agent.
type CheckFirst struct {
	Look  Look    `json:"look"`
	Items string  `json:"items,omitempty"`
	Key   string  `json:"key,omitempty"`
	Where []Where `json:"where,omitempty"`
	Wake  Wake    `json:"wake"`
}

// Look is where a check looks: an HTTP address, or a tool of the agent
// with its input.
type Look struct {
	HTTP  *LookHTTP      `json:"http,omitempty"`
	Tool  string         `json:"tool,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

// LookHTTP is a GET of an address. Secret names a variable of the front
// that workbench.duties.secrets allows for the host of the address. The
// secret goes in Header as it is, or as "Authorization: Bearer <secret>"
// without Header.
type LookHTTP struct {
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
	Header string `json:"header,omitempty"`
}

// Where is one filter of an item: Field is a dotted path in the item, and
// exactly one test follows.
type Where struct {
	Field    string            `json:"field"`
	Is       json.RawMessage   `json:"is,omitempty"`
	IsNot    json.RawMessage   `json:"isNot,omitempty"`
	OneOf    []json.RawMessage `json:"oneOf,omitempty"`
	Contains json.RawMessage   `json:"contains,omitempty"`
	AtLeast  json.RawMessage   `json:"atLeast,omitempty"`
	AtMost   json.RawMessage   `json:"atMost,omitempty"`
}

// Wake says when the items of a check wake the agent: WakeNew for items
// whose key the duty did not see before, WakeAny for any item, and
// WakeAtLeast for Count items or more.
type Wake struct {
	When  string `json:"when,omitempty"`
	Count int    `json:"count,omitempty"`
}

// UnmarshalJSON takes the object, or the words new and any alone.
func (w *Wake) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*w = Wake{When: s}
		return nil
	}
	type plain Wake
	var p plain
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	*w = Wake(p)
	return nil
}

// Wakes of a check.
const (
	WakeNew     = "new"
	WakeAny     = "any"
	WakeAtLeast = "atLeast"
)

// Do is what a duty does: exactly one action. Phase 1 has Ask; a later
// package registers the others (RegisterAction).
type Do struct {
	Ask     *Ask        `json:"ask,omitempty"`
	Post    *Post       `json:"post,omitempty"`
	Open    *OpenBridge `json:"open,omitempty"`
	Manager *AskManager `json:"manager,omitempty"`
}

// Ask asks the agent of the duty: a run with the message, on the queue of
// the pool that runs the agent.
type Ask struct {
	Message string `json:"message"`
}

// Post posts the message in a bridge that stays open.
type Post struct {
	Bridge  string   `json:"bridge"`
	To      []string `json:"to,omitempty"`
	Message string   `json:"message"`
}

// OpenBridge opens a bridge from a playbook.
type OpenBridge struct {
	Playbook string `json:"playbook"`
	Goal     string `json:"goal,omitempty"`
}

// AskManager sends the message to the manager.
type AskManager struct {
	Message string `json:"message"`
}

// Actions of do.
const (
	ActionAsk     = "ask"
	ActionPost    = "post"
	ActionOpen    = "open"
	ActionManager = "manager"
)

// Name returns the action of do, or "" without one.
func (d Do) Name() string {
	switch n := d.count(); {
	case n != 1:
		return ""
	case d.Ask != nil:
		return ActionAsk
	case d.Post != nil:
		return ActionPost
	case d.Open != nil:
		return ActionOpen
	}
	return ActionManager
}

func (d Do) count() int {
	n := 0
	for _, set := range []bool{d.Ask != nil, d.Post != nil, d.Open != nil, d.Manager != nil} {
		if set {
			n++
		}
	}
	return n
}

// Message returns the message of the action, with its placeholders.
func (d Do) Message() string {
	switch {
	case d.Ask != nil:
		return d.Ask.Message
	case d.Post != nil:
		return d.Post.Message
	case d.Open != nil:
		return d.Open.Goal
	case d.Manager != nil:
		return d.Manager.Message
	}
	return ""
}

// Limits bound a duty. Budget bounds each run and is required. PerDayUSD
// bounds the cost of the runs of a day, in the time zone of the duty,
// with 0 for no bound. Overlap says what a time does while the last run
// works. CatchUp is how old a missed time can be and still run once.
// Retries is how many times a failed run goes again.
type Limits struct {
	Budget    *Budget `json:"budget,omitempty"`
	PerDayUSD float64 `json:"perDayUSD,omitempty"`
	Overlap   string  `json:"overlap,omitempty"`
	CatchUp   string  `json:"catchUp,omitempty"`
	Retries   *int    `json:"retries,omitempty"`
}

// Budget is the budget of each run of a duty. Every limit counts the
// whole run, and at least one is above zero.
type Budget struct {
	MaxCostUSD   float64 `json:"maxCostUSD,omitempty"`
	MaxTurns     int     `json:"maxTurns,omitempty"`
	MaxTokens    int     `json:"maxTokens,omitempty"`
	MaxToolCalls int     `json:"maxToolCalls,omitempty"`
}

// Overlaps of a time with a run that works.
const (
	OverlapSkip  = "skip"  // the time does not run, and the history says so
	OverlapQueue = "queue" // the time waits for the run to end, one time at most
	OverlapAllow = "allow" // the time runs beside the run
)

// WhileAway says who decides the questions of a run of the duty while
// nobody watches, after how long, and what happens then. The front
// applies it (a later package); the record keeps it.
type WhileAway struct {
	Decider string `json:"decider"`
	Timeout string `json:"timeout"`
	Then    string `json:"then"`
}

// Notify names who hears of a failed run and of a question of a run:
// "owner", the subject of a person, or channel:<name> for a shared
// channel of the operator (workbench.notifications.shared), which the
// notification reaches without a person.
type Notify struct {
	Failure  []string `json:"failure,omitempty"`
	NeedsYou []string `json:"needsYou,omitempty"`
}

// NotifyChannelPrefix starts an entry of a notify list that names a shared
// channel of the operator, such as channel:ops-room.
const NotifyChannelPrefix = "channel:"

// ChannelOf returns the name of the shared channel that an entry of a
// notify list names, and reports whether it names one.
func ChannelOf(who string) (string, bool) {
	name, ok := strings.CutPrefix(who, NotifyChannelPrefix)
	return name, ok
}

// Principals of the runs of a duty.
const (
	RunAsDuty  = "duty"  // the principal duty:<name>, of the kind workload
	RunAsOwner = "owner" // the owner of the duty
)

// Defaults and bounds of a duty.
const (
	DefaultOverlap  = OverlapSkip
	DefaultCatchUp  = time.Hour
	MaxCatchUp      = 24 * time.Hour
	DefaultRetries  = 1
	MaxRetries      = 3
	DefaultDecider  = "manager"
	DefaultAwayFor  = 2 * time.Hour
	DefaultThen     = "deny"
	MinAwayFor      = time.Minute
	MaxAwayFor      = 7 * 24 * time.Hour
	MaxMessageBytes = 32 << 10
	maxNotify       = 10
)

// namePattern is the form of the name of a duty: it names a route, a
// batch, and the keys of the buckets.
var namePattern = regexp.MustCompile(`^[a-z0-9_-]{1,63}$`)

// pathPattern is a dotted path of keys.
var pathPattern = regexp.MustCompile(`^[^.\s]+(\.[^.\s]+)*$`)

// envPattern is the form of the name of a variable.
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// headerPattern is the form of the name of a header.
var headerPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// Names that no duty takes. The route GET /v1/duties/options takes
// options. DraftName stands for a duty that has no name yet: the page
// checks such a duty with a dry run of PUT /v1/duties/new and asks
// POST /v1/duties/new/write-check for its check.
const (
	reservedName = "options"
	DraftName    = "new"
)

// CheckName returns an error in words for a name that is not the name of
// a duty.
func CheckName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("the name %q must be 1 to 63 of a-z, 0-9, '-', and '_'", name)
	}
	switch name {
	case reservedName:
		return fmt.Errorf("the name %s is taken by the route GET /v1/duties/options: pick another", name)
	case DraftName:
		return fmt.Errorf("the name %s stands for a duty that has no name yet: pick another", name)
	}
	return nil
}

// durText writes a span as hours and minutes, such as 0, 30m, 1h, or
// 1h30m, and in the form of Go for a span with seconds.
func durText(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	if d%time.Minute != 0 {
		return d.String()
	}
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// readDur reads a span, with "0" as zero.
func readDur(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// normalize fills the defaults of a duty that a write stores: the trimmed
// names, the schedule in its usual form with zone as its default time
// zone, the overlap, the catch-up, the retries, whileAway, notify, and
// runAs. It leaves a value that does not read as it is, so problems names
// it. The words of when.say stay: the write turns them into a schedule.
func (d *Duty) normalize(zone string) {
	d.Name, d.Agent, d.Pool = strings.TrimSpace(d.Name), strings.TrimSpace(d.Agent), strings.TrimSpace(d.Pool)
	if s := d.When.Schedule; s != nil {
		n := s.Normalize(zone)
		d.When.Schedule = &n
	}
	if w := d.When.Webhook; w != nil {
		w.Auth.Mode = strings.ToLower(strings.TrimSpace(w.Auth.Mode))
		w.Auth.Secret, w.Auth.Header, w.Key = strings.TrimSpace(w.Auth.Secret), strings.TrimSpace(w.Auth.Header), strings.TrimSpace(w.Key)
		if w.Auth.Mode == sources.AuthHMAC && w.Auth.Header == "" {
			w.Auth.Header = sources.DefaultSignatureHeader
		}
	}
	if c := d.CheckFirst; c != nil {
		c.Items, c.Key = strings.TrimSpace(c.Items), strings.TrimSpace(c.Key)
		if h := c.Look.HTTP; h != nil {
			h.URL, h.Secret, h.Header = strings.TrimSpace(h.URL), strings.TrimSpace(h.Secret), strings.TrimSpace(h.Header)
		}
		c.Look.Tool = strings.TrimSpace(c.Look.Tool)
		if c.Wake.When == "" {
			c.Wake.When = WakeNew
		}
	}
	l := &d.Limits
	switch {
	case d.Do.Post != nil:
		// A post spends nothing of its own, so it keeps no budget and no
		// daily cap.
		l.Budget, l.PerDayUSD = nil, 0
	case d.Do.Open != nil:
		// The budget of an open is the turn cap of the bridge. The daily
		// cap counts the runs of a duty, and an open starts none.
		l.PerDayUSD = 0
	}
	l.Overlap = strings.ToLower(strings.TrimSpace(l.Overlap))
	if l.Overlap == "" {
		l.Overlap = DefaultOverlap
	}
	if strings.TrimSpace(l.CatchUp) == "" {
		l.CatchUp = durText(DefaultCatchUp)
	} else if v, err := readDur(l.CatchUp); err == nil {
		l.CatchUp = durText(v)
	}
	if l.Retries == nil {
		n := DefaultRetries
		l.Retries = &n
	}
	if d.WhileAway == nil {
		d.WhileAway = &WhileAway{}
	}
	w := d.WhileAway
	w.Decider, w.Then = strings.ToLower(strings.TrimSpace(w.Decider)), strings.ToLower(strings.TrimSpace(w.Then))
	if w.Decider == "" {
		w.Decider = DefaultDecider
	}
	if strings.TrimSpace(w.Timeout) == "" {
		w.Timeout = durText(DefaultAwayFor)
	} else if v, err := readDur(w.Timeout); err == nil {
		w.Timeout = durText(v)
	}
	if w.Then == "" {
		w.Then = DefaultThen
	}
	if d.Notify == nil {
		d.Notify = &Notify{Failure: []string{RunAsOwner}, NeedsYou: []string{RunAsOwner}}
	}
	for _, list := range [][]string{d.Notify.Failure, d.Notify.NeedsYou} {
		for i, who := range list {
			if name, ok := ChannelOf(strings.TrimSpace(who)); ok {
				list[i] = NotifyChannelPrefix + strings.TrimSpace(name)
			}
		}
	}
	d.RunAs = strings.ToLower(strings.TrimSpace(d.RunAs))
	if d.RunAs == "" {
		d.RunAs = RunAsDuty
	}
}

// catchUp returns the catch-up of a normalized duty.
func (d *Duty) catchUp() time.Duration {
	v, err := readDur(d.Limits.CatchUp)
	if err != nil {
		return DefaultCatchUp
	}
	return v
}

// retries returns the retries of a normalized duty.
func (d *Duty) retries() int {
	if d.Limits.Retries == nil {
		return DefaultRetries
	}
	return *d.Limits.Retries
}

// zone returns the time zone of the days of a duty: the zone of its
// schedule, else UTC.
func (d *Duty) zone() *time.Location {
	if s := d.When.Schedule; s != nil {
		if loc, err := time.LoadLocation(s.TimeZone); err == nil && s.TimeZone != "" {
			return loc
		}
	}
	return time.UTC
}

// Location returns the time zone of the days of a duty: the zone of its
// schedule, else UTC. A start of the duty has its date in this zone, as
// the title of a bridge that the duty opens names it.
func (d *Duty) Location() *time.Location { return d.zone() }

// starts counts the starts of when.
func (w When) starts() int {
	n := 0
	for _, set := range []bool{w.Schedule != nil, strings.TrimSpace(w.Say) != "", w.Event != nil, w.Webhook != nil} {
		if set {
			n++
		}
	}
	return n
}

// problems lists what does not fit in a normalized duty, in words. Each
// problem of a field starts with the dotted path of the field and a colon,
// such as "limits.budget: ...", so the page shows the text beside the
// field. It reads the record alone; the service adds what needs the fleet
// and the configuration (Service.contextProblems).
func (d *Duty) problems() []string {
	var p []string
	add := func(path, format string, args ...any) { p = append(p, problem(path, format, args...)) }
	if err := CheckName(d.Name); err != nil {
		add("name", "%s.", capital(err.Error()))
	}
	if d.Agent == "" {
		add("agent", "Name the agent that does the duty. The access rules of the duty are those of the agent.")
	}
	switch d.When.starts() {
	case 0:
		add("when", "Pick a start: schedule, say, event, or webhook.")
	case 1:
	default:
		add("when", "Keep one start of schedule, say, event, and webhook.")
	}
	if s := d.When.Schedule; s != nil {
		for _, q := range s.Check() {
			p = append(p, scheduleProblem(q))
		}
	}
	if e := d.When.Event; e != nil {
		switch e.Kind {
		case EventBridgeClosed, EventRunFailed, EventQuestionWaiting, EventPageChanged, EventRoutineReady, EventCapReached:
		default:
			add("when.event.kind", "%q is not a kind of event. Pick bridge.closed, run.failed, question.waiting, page.changed, routine.ready, or cap.reached.", e.Kind)
		}
		if e.After != "" {
			if v, err := readDur(e.After); err != nil || v < 0 {
				add("when.event.after", "%q is not a span. Write a span such as 1h.", e.After)
			}
		}
	}
	if w := d.When.Webhook; w != nil {
		switch w.Auth.Mode {
		case sources.AuthBearer, sources.AuthHMAC:
			if !envPattern.MatchString(w.Auth.Secret) {
				add("when.webhook.auth.secret", "Name the variable of the server that holds the %s secret.", w.Auth.Mode)
			}
			if w.Auth.Mode == sources.AuthHMAC && !headerPattern.MatchString(w.Auth.Header) {
				add("when.webhook.auth.header", "%q is not a header name.", w.Auth.Header)
			}
		case sources.AuthNone:
			if w.Auth.Secret != "" {
				add("when.webhook.auth.secret", "The mode is none, so leave out the secret.")
			}
		case "":
			add("when.webhook.auth.mode", "Pick bearer, hmac, or none. A webhook without a credential says none.")
		default:
			add("when.webhook.auth.mode", "%q is not a mode. Pick bearer, hmac, or none.", w.Auth.Mode)
		}
		if w.Key != "" && !pathPattern.MatchString(w.Key) {
			add("when.webhook.key", "%q is not a dotted path, such as event.id.", w.Key)
		}
	}
	if c := d.CheckFirst; c != nil {
		p = append(p, c.problems()...)
	}
	switch d.Do.count() {
	case 0:
		add("do", "Pick an action, such as ask with the message for the agent.")
	case 1:
		msg := d.Do.Message()
		switch {
		case d.Do.Ask != nil && strings.TrimSpace(msg) == "":
			add("do.ask.message", "The message is empty. Say what the agent does.")
		case len(msg) > MaxMessageBytes:
			add(d.Do.messagePath(), "The text is longer than %d KB.", MaxMessageBytes>>10)
		}
	default:
		add("do", "Keep one action of ask, post, open, and manager.")
	}
	l := d.Limits
	switch b := l.Budget; {
	case d.Do.Post != nil:
		// A post spends nothing of its own: the seats of the bridge spend
		// from their own budgets. normalize drops the budget.
	case d.Do.Open != nil && b != nil && (b.MaxCostUSD != 0 || b.MaxTokens != 0 || b.MaxToolCalls != 0):
		add("limits.budget", "A bridge counts turns. Give maxTurns, such as 40, and leave out the other limits.")
	case d.Do.Open != nil && (b == nil || b.MaxTurns <= 0):
		add("limits.budget.maxTurns", "A duty that opens a bridge needs the most turns of the whole bridge, such as 40.")
	case b == nil || (b.MaxCostUSD <= 0 && b.MaxTurns <= 0 && b.MaxTokens <= 0 && b.MaxToolCalls <= 0):
		add("limits.budget", "A budget is required, with at least one limit above zero, such as {\"maxCostUSD\": 0.5}. A duty without a budget does not save, because a paid default would be a silent cost.")
	case b.MaxCostUSD < 0 || b.MaxTurns < 0 || b.MaxTokens < 0 || b.MaxToolCalls < 0:
		add("limits.budget", "A limit is below zero.")
	}
	if l.PerDayUSD < 0 {
		add("limits.perDayUSD", "The cap is below zero. Write 0 for no daily cap.")
	}
	switch l.Overlap {
	case OverlapSkip, OverlapQueue, OverlapAllow:
	default:
		add("limits.overlap", "%q is not an overlap. Pick skip, queue, or allow.", l.Overlap)
	}
	if v, err := readDur(l.CatchUp); err != nil || v < 0 || v > MaxCatchUp {
		add("limits.catchUp", "%q is not a span from 0 to 24h. Write one such as 1h.", l.CatchUp)
	}
	if r := l.Retries; r != nil && (*r < 0 || *r > MaxRetries) {
		add("limits.retries", "%d is out of range. Pick 0 to %d.", *r, MaxRetries)
	}
	if w := d.WhileAway; w != nil {
		if w.Decider != "manager" && w.Decider != "nobody" {
			add("whileAway.decider", "%q is not a decider. Pick manager or nobody.", w.Decider)
		}
		if v, err := readDur(w.Timeout); err != nil || v < MinAwayFor || v > MaxAwayFor {
			add("whileAway.timeout", "%q is not a span from 1m to 168h. Write one such as 2h.", w.Timeout)
		}
		if w.Then != "deny" && w.Then != "cancel" {
			add("whileAway.then", "%q is not a choice. Pick deny or cancel.", w.Then)
		}
	}
	if n := d.Notify; n != nil {
		for _, f := range []struct {
			path string
			list []string
		}{{"notify.failure", n.Failure}, {"notify.needsYou", n.NeedsYou}} {
			if len(f.list) > maxNotify {
				add(f.path, "The list names %d people, and the most is %d.", len(f.list), maxNotify)
			}
			for _, who := range f.list {
				if name, ok := ChannelOf(who); ok {
					if !namePattern.MatchString(name) {
						add(f.path, "%q is not a shared channel. Write channel: and the name of a shared channel, such as channel:ops-room.", who)
					}
					continue
				}
				if strings.TrimSpace(who) == "" || len(who) > 256 || strings.ContainsAny(who, "\r\n\t") {
					add(f.path, "%q is not owner, the subject of a person, or channel: and the name of a shared channel.", who)
				}
			}
		}
	}
	if d.RunAs != RunAsDuty && d.RunAs != RunAsOwner {
		add("runAs", "%q is not a choice. Pick duty or owner.", d.RunAs)
	}
	return p
}

// messagePath returns the dotted path of the text of the action of do:
// the goal of open, else the message.
func (d Do) messagePath() string {
	if d.Open != nil {
		return "do.open.goal"
	}
	if name := d.Name(); name != "" {
		return "do." + name + ".message"
	}
	return "do"
}

// problem writes a problem of the field at the dotted path: the path, a
// colon, and the text. A problem of the whole duty has no path.
func problem(path, format string, args ...any) string {
	text := fmt.Sprintf(format, args...)
	if path == "" {
		return text
	}
	return path + ": " + text
}

// problemText returns the text of a problem without the path of its
// field.
func problemText(p string) string {
	if path, text, ok := strings.Cut(p, ": "); ok && pathPattern.MatchString(path) {
		return text
	}
	return p
}

// scheduleFields are the fields of a schedule, the first word of most of
// the problems of schedule.Check.
var scheduleFields = map[string]bool{"repeat": true, "days": true, "everyWeeks": true, "dayOfMonth": true, "times": true, "every": true, "between": true, "timeZone": true, "start": true, "end": true}

// scheduleProblem gives a problem of schedule.Check the path of its
// field, such as when.schedule.times, else when.schedule.
func scheduleProblem(q string) string {
	word, _, _ := strings.Cut(q, " ")
	if scheduleFields[word] {
		return "when.schedule." + word + ": " + q
	}
	return "when.schedule: " + q
}

// problems lists what does not fit in a check, each with the path of its
// field.
func (c *CheckFirst) problems() []string {
	var p []string
	add := func(path, format string, args ...any) { p = append(p, problem(path, format, args...)) }
	switch {
	case c.Look.HTTP != nil && c.Look.Tool != "":
		add("checkFirst.look", "Keep one of http and tool.")
	case c.Look.HTTP == nil && c.Look.Tool == "":
		add("checkFirst.look", "Pick http, with the address, or tool, with its input.")
	case c.Look.HTTP != nil:
		h := c.Look.HTTP
		u, err := url.Parse(h.URL)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			add("checkFirst.look.http.url", "Write an absolute http or https address.")
		case u.User != nil:
			add("checkFirst.look.http.url", "The address holds a user or a password. Name a variable in secret instead.")
		}
		if h.Secret != "" && !envPattern.MatchString(h.Secret) {
			add("checkFirst.look.http.secret", "%q is not the name of a variable of the server.", h.Secret)
		}
		if h.Header != "" && (!headerPattern.MatchString(h.Header) || h.Secret == "") {
			add("checkFirst.look.http.header", "%q must be a header name, and it needs secret.", h.Header)
		}
		if c.Look.Input != nil {
			add("checkFirst.look.input", "The input goes with tool, not with http.")
		}
	}
	if c.Items != "" && !pathPattern.MatchString(c.Items) {
		add("checkFirst.items", "%q is not a dotted path, such as result.records.", c.Items)
	}
	switch {
	case c.Key != "" && !pathPattern.MatchString(c.Key):
		add("checkFirst.key", "%q is not a dotted path, such as number.", c.Key)
	case c.Key == "" && c.Wake.When == WakeNew:
		add("checkFirst.key", "A key is required with wake new. It names what makes an item new, such as number.")
	}
	for i, w := range c.Where {
		at := fmt.Sprintf("checkFirst.where[%d]", i)
		if !pathPattern.MatchString(strings.TrimSpace(w.Field)) {
			add(at+".field", "%q is not a dotted path, such as priority.", w.Field)
		}
		n := 0
		for _, set := range []bool{len(w.Is) > 0, len(w.IsNot) > 0, w.OneOf != nil, len(w.Contains) > 0, len(w.AtLeast) > 0, len(w.AtMost) > 0} {
			if set {
				n++
			}
		}
		if n != 1 {
			add(at, "A filter needs exactly one of is, isNot, oneOf, contains, atLeast, and atMost.")
		}
	}
	switch c.Wake.When {
	case WakeNew, WakeAny:
		if c.Wake.Count != 0 {
			add("checkFirst.wake.count", "The count goes with atLeast.")
		}
	case WakeAtLeast:
		if c.Wake.Count < 1 {
			add("checkFirst.wake.count", "Write 1 or more with atLeast.")
		}
	default:
		add("checkFirst.wake.when", "%q is not a choice. Pick new, any, or atLeast.", c.Wake.When)
	}
	return p
}

// capital writes the first letter of s as a capital.
func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
