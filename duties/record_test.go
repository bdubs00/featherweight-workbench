package duties

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-workbench/duties/schedule"
)

// A write fills the visible defaults of a duty: the overlap, the catch-up,
// the retries, whileAway, notify, runAs, and the wake of a check.
func TestNormalizeDefaults(t *testing.T) {
	d := daily("report")
	d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: " https://example.com/q "}}, Key: "id"}
	d.normalize("America/Chicago")
	if d.Limits.Overlap != OverlapSkip || d.Limits.CatchUp != "1h" || *d.Limits.Retries != 1 {
		t.Fatalf("the limits = %+v", d.Limits)
	}
	// Nobody decides while the owner is away, unless the owner picks the
	// manager: one default for a duty of the door and of New duty.
	if *d.WhileAway != (WhileAway{Decider: "nobody", Timeout: "2h", Then: "deny"}) {
		t.Fatalf("whileAway = %+v", d.WhileAway)
	}
	if d.Notify.Failure[0] != "owner" || d.Notify.NeedsYou[0] != "owner" || d.RunAs != RunAsDuty {
		t.Fatalf("notify = %+v, runAs = %q", d.Notify, d.RunAs)
	}
	if d.CheckFirst.Wake.When != WakeNew || d.CheckFirst.Look.HTTP.URL != "https://example.com/q" {
		t.Fatalf("checkFirst = %+v", d.CheckFirst)
	}
	// The schedule names its zone, so the default of the caller does not
	// change it.
	if d.When.Schedule.TimeZone != "America/New_York" {
		t.Fatalf("the zone = %q", d.When.Schedule.TimeZone)
	}
	if p := d.problems(); len(p) != 0 {
		t.Fatalf("problems of a good duty: %v", p)
	}
	z := Duty{Limits: Limits{CatchUp: "90m", Retries: intp(0)}}
	z.normalize("")
	if z.Limits.CatchUp != "1h30m" || *z.Limits.Retries != 0 {
		t.Fatalf("the limits = %+v", z.Limits)
	}
}

// Each problem of a record starts with the dotted path of its field and
// a colon, and says the problem in words.
func TestProblems(t *testing.T) {
	for _, c := range []struct {
		mut  func(*Duty)
		want string
	}{
		{func(d *Duty) { d.Name = "Daily Report" }, `name: The name "Daily Report" must be 1 to 63 of a-z, 0-9, '-', and '_'.`},
		{func(d *Duty) { d.Agent = "" }, "agent: Name the agent"},
		{func(d *Duty) { d.When = When{} }, "when: Pick a start"},
		{func(d *Duty) { d.When.Webhook = &Webhook{Auth: WebhookAuth{Mode: "none"}} }, "when: Keep one start"},
		{func(d *Duty) { d.When.Schedule.Times = []string{"25:00"} }, `when.schedule.times: times holds "25:00"`},
		{func(d *Duty) { d.When = When{Event: &Event{Kind: "lunch"}} }, `when.event.kind: "lunch" is not a kind of event`},
		{func(d *Duty) { d.When = When{Webhook: &Webhook{}} }, "when.webhook.auth.mode: Pick bearer, hmac, or none"},
		{func(d *Duty) { d.When = When{Webhook: &Webhook{Auth: WebhookAuth{Mode: "bearer"}}} }, "when.webhook.auth.secret: Name the variable"},
		{func(d *Duty) { d.When = When{Webhook: &Webhook{Auth: WebhookAuth{Mode: "none", Secret: "X"}}} }, "when.webhook.auth.secret: The mode is none"},
		{func(d *Duty) {
			d.When = When{Webhook: &Webhook{Auth: WebhookAuth{Mode: "none"}, Key: "a..b"}}
		}, `when.webhook.key: "a..b" is not a dotted path`},
		{func(d *Duty) { d.Do = Do{} }, "do: Pick an action"},
		{func(d *Duty) { d.Do.Manager = &AskManager{Message: "x"} }, "do: Keep one action"},
		{func(d *Duty) { d.Do.Ask.Message = " " }, "do.ask.message: The message is empty"},
		{func(d *Duty) { d.Do.Ask.Message = strings.Repeat("x", MaxMessageBytes+1) }, "do.ask.message: The text is longer than 32 KB"},
		{func(d *Duty) { d.Limits.Budget = nil }, "limits.budget: A budget is required"},
		{func(d *Duty) { d.Limits.Budget = &Budget{} }, "a paid default would be a silent cost"},
		{func(d *Duty) { d.Limits.Budget.MaxTurns = -1 }, "limits.budget: A limit is below zero"},
		{func(d *Duty) { d.Limits.PerDayUSD = -1 }, "limits.perDayUSD: The cap is below zero"},
		{func(d *Duty) { d.Limits.Overlap = "never" }, `limits.overlap: "never" is not an overlap`},
		{func(d *Duty) { d.Limits.CatchUp = "25h" }, `limits.catchUp: "25h" is not a span`},
		{func(d *Duty) { d.Limits.Retries = intp(4) }, "limits.retries: 4 is out of range"},
		{func(d *Duty) { d.WhileAway = &WhileAway{Decider: "boss", Timeout: "2h", Then: "deny"} }, `whileAway.decider: "boss" is not a decider`},
		{func(d *Duty) { d.WhileAway = &WhileAway{Decider: "nobody", Timeout: "10s", Then: "deny"} }, `whileAway.timeout: "10s" is not a span`},
		{func(d *Duty) { d.WhileAway = &WhileAway{Decider: "nobody", Timeout: "1h", Then: "allow"} }, `whileAway.then: "allow" is not a choice`},
		{func(d *Duty) { d.Notify = &Notify{Failure: []string{""}} }, `notify.failure: "" is not owner`},
		{func(d *Duty) { d.Notify = &Notify{NeedsYou: []string{"channel:Ops Room"}} }, `notify.needsYou: "channel:Ops Room" is not a shared channel`},
		{func(d *Duty) { d.RunAs = "root" }, `runAs: "root" is not a choice`},
		{func(d *Duty) { d.CheckFirst = &CheckFirst{Key: "id"} }, "checkFirst.look: Pick http"},
		{func(d *Duty) {
			d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://x"}, Tool: "t"}, Key: "id"}
		}, "checkFirst.look: Keep one of http and tool"},
		{func(d *Duty) { d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "ftp://x"}}, Key: "id"} }, "checkFirst.look.http.url: Write an absolute http or https address"},
		{func(d *Duty) {
			d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://u:p@x"}}, Key: "id"}
		}, "checkFirst.look.http.url: The address holds a user or a password"},
		{func(d *Duty) { d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://x"}}} }, "checkFirst.key: A key is required with wake new"},
		{func(d *Duty) {
			d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://x"}}, Key: "id", Where: []Where{{Field: "p"}}}
		}, "checkFirst.where[0]: A filter needs exactly one of is"},
		{func(d *Duty) {
			d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://x"}}, Wake: Wake{When: WakeAtLeast}}
		}, "checkFirst.wake.count: Write 1 or more"},
		{func(d *Duty) {
			d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://x"}}, Wake: Wake{When: "often"}}
		}, `checkFirst.wake.when: "often" is not a choice`},
	} {
		d := daily("report")
		c.mut(&d)
		d.normalize("")
		got := strings.Join(d.problems(), " | ")
		if !strings.Contains(got, c.want) {
			t.Errorf("problems = %q, want %q", got, c.want)
		}
		for _, q := range d.problems() {
			if !problemPattern.MatchString(q) {
				t.Errorf("the problem %q does not start with the path of its field", q)
			}
		}
	}
}

// problemPattern is a problem of a field: a dotted path, a colon, and the
// text.
var problemPattern = regexp.MustCompile(`^[a-z][A-Za-z]*(\[[0-9]+\])?(\.[a-z][A-Za-z]*(\[[0-9]+\])?)*: \S`)

// The wake of a check reads as an object or as a word alone, and an
// object with an unknown key does not read.
func TestWakeJSON(t *testing.T) {
	var c CheckFirst
	if err := json.Unmarshal([]byte(`{"wake": "any"}`), &c); err != nil || c.Wake.When != WakeAny {
		t.Fatalf("wake any = %+v, %v", c.Wake, err)
	}
	if err := json.Unmarshal([]byte(`{"wake": {"when": "atLeast", "count": 5}}`), &c); err != nil || c.Wake != (Wake{When: WakeAtLeast, Count: 5}) {
		t.Fatalf("wake atLeast = %+v, %v", c.Wake, err)
	}
	if err := json.Unmarshal([]byte(`{"wake": {"often": true}}`), &c); err == nil {
		t.Fatal("an unknown key of wake read")
	}
}

// The record of the contract reads as a duty, and only its parts that
// later work brings answer "not available yet" in a write.
func TestContractRecord(t *testing.T) {
	const record = `{
	  "name": "daily-homelab-report", "agent": "docs-scribe", "pool": "", "owner": "alice@example.com",
	  "when": {"schedule": {"repeat": "weekdays", "times": ["09:00"], "timeZone": "America/New_York", "start": "2026-10-26", "end": "2026-12-31"}},
	  "checkFirst": {"look": {"http": {"url": "https://example/api/queue", "secret": "ENV_NAME", "header": ""}},
	    "items": "result.records", "key": "number", "where": [{"field": "priority", "oneOf": [1, 2]}], "wake": {"when": "new", "count": 0}},
	  "do": {"ask": {"message": "Write the report of {date}. Found: {found}"}},
	  "limits": {"budget": {"maxCostUSD": 0.5, "maxTurns": 40, "maxTokens": 0, "maxToolCalls": 0}, "perDayUSD": 2.0, "overlap": "skip", "catchUp": "1h", "retries": 1},
	  "whileAway": {"decider": "manager", "timeout": "2h", "then": "deny"},
	  "notify": {"failure": ["owner"], "needsYou": ["owner"]},
	  "runAs": "duty", "paused": false
	}`
	var d Duty
	dec := json.NewDecoder(strings.NewReader(record))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		t.Fatal(err)
	}
	d.normalize("")
	if p := d.problems(); len(p) != 0 {
		t.Fatalf("problems of the record of the contract: %v", p)
	}
	if got := readback(&d, ""); got != "Every weekday at 9:00 AM, New York time, from October 26, 2026 until December 31, 2026. First it looks at example/api/queue, where priority is 1 or 2. Only items that it did not see before wake docs-scribe. The run gets the list." {
		t.Fatalf("readback = %q", got)
	}
	if got := details(&d); got != "The check reads the items at result.records of the answer. An item is new by its number." {
		t.Fatalf("details = %q", got)
	}
	var s schedule.Schedule = *d.When.Schedule
	if s.Repeat != schedule.RepeatWeekdays {
		t.Fatalf("schedule = %+v", s)
	}
}

func TestOfferedNamesWhatIsRegistered(t *testing.T) {
	events, actions, looks := Offered()
	if !slices.Contains(actions, ActionAsk) || !slices.Contains(looks, "http") {
		t.Fatalf("Offered() = %v, %v, %v; want ask and http always", events, actions, looks)
	}
	if !slices.IsSorted(events) || !slices.IsSorted(actions) || !slices.IsSorted(looks) {
		t.Fatalf("Offered() = %v, %v, %v; want sorted lists", events, actions, looks)
	}
}

// The readback says who decides while the owner is away, and the defaults
// read as nobody, after 2 hours, then no.
func TestAwayWords(t *testing.T) {
	for _, c := range []struct {
		w    *WhileAway
		want string
	}{
		{nil, "Nobody decides a question for you. After 2 hours without an answer, the answer is no."},
		{&WhileAway{}, "Nobody decides a question for you. After 2 hours without an answer, the answer is no."},
		{&WhileAway{Decider: "manager", Timeout: "30m", Then: "cancel"}, "The manager decides a question for you. After 30 minutes without an answer, the run stops."},
		{&WhileAway{Decider: "nobody", Timeout: "24h", Then: "deny"}, "Nobody decides a question for you. After 24 hours without an answer, the answer is no."},
	} {
		if got := AwayWords(c.w); got != c.want {
			t.Errorf("AwayWords(%+v) = %q, want %q", c.w, got, c.want)
		}
	}
}
