package duties

import (
	"encoding/json"
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
	if *d.WhileAway != (WhileAway{Decider: "manager", Timeout: "2h", Then: "deny"}) {
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

// Each problem of a record names its field in words.
func TestProblems(t *testing.T) {
	for _, c := range []struct {
		mut  func(*Duty)
		want string
	}{
		{func(d *Duty) { d.Name = "Daily Report" }, `The name "Daily Report" must be 1 to 63 of a-z, 0-9, '-', and '_'.`},
		{func(d *Duty) { d.Agent = "" }, "agent is required"},
		{func(d *Duty) { d.When = When{} }, "when needs a start"},
		{func(d *Duty) { d.When.Webhook = &Webhook{Auth: WebhookAuth{Mode: "none"}} }, "when holds more than one start"},
		{func(d *Duty) { d.When.Schedule.Times = []string{"25:00"} }, `when.schedule: times holds "25:00"`},
		{func(d *Duty) { d.When = When{Event: &Event{Kind: "lunch"}} }, `when.event.kind is "lunch"`},
		{func(d *Duty) { d.When = When{Webhook: &Webhook{}} }, "when.webhook.auth.mode is required"},
		{func(d *Duty) { d.When = When{Webhook: &Webhook{Auth: WebhookAuth{Mode: "bearer"}}} }, "when.webhook.auth.secret must name the variable"},
		{func(d *Duty) { d.When = When{Webhook: &Webhook{Auth: WebhookAuth{Mode: "none", Secret: "X"}}} }, "the mode is none"},
		{func(d *Duty) {
			d.When = When{Webhook: &Webhook{Auth: WebhookAuth{Mode: "none"}, Key: "a..b"}}
		}, "when.webhook.key"},
		{func(d *Duty) { d.Do = Do{} }, "do needs an action"},
		{func(d *Duty) { d.Do.Manager = &AskManager{Message: "x"} }, "do holds more than one action"},
		{func(d *Duty) { d.Do.Ask.Message = " " }, "do.ask.message is empty"},
		{func(d *Duty) { d.Do.Ask.Message = strings.Repeat("x", MaxMessageBytes+1) }, "longer than 32 KB"},
		{func(d *Duty) { d.Limits.Budget = nil }, "limits.budget is required"},
		{func(d *Duty) { d.Limits.Budget = &Budget{} }, "a paid default would be a silent cost"},
		{func(d *Duty) { d.Limits.Budget.MaxTurns = -1 }, "a limit below zero"},
		{func(d *Duty) { d.Limits.PerDayUSD = -1 }, "limits.perDayUSD is below zero"},
		{func(d *Duty) { d.Limits.Overlap = "never" }, `limits.overlap is "never"`},
		{func(d *Duty) { d.Limits.CatchUp = "25h" }, `limits.catchUp is "25h"`},
		{func(d *Duty) { d.Limits.Retries = intp(4) }, "limits.retries is 4"},
		{func(d *Duty) { d.WhileAway = &WhileAway{Decider: "boss", Timeout: "2h", Then: "deny"} }, `whileAway.decider is "boss"`},
		{func(d *Duty) { d.WhileAway = &WhileAway{Decider: "nobody", Timeout: "10s", Then: "deny"} }, `whileAway.timeout is "10s"`},
		{func(d *Duty) { d.WhileAway = &WhileAway{Decider: "nobody", Timeout: "1h", Then: "allow"} }, `whileAway.then is "allow"`},
		{func(d *Duty) { d.Notify = &Notify{Failure: []string{""}} }, `notify.failure holds ""`},
		{func(d *Duty) { d.RunAs = "root" }, `runAs is "root"`},
		{func(d *Duty) { d.CheckFirst = &CheckFirst{Key: "id"} }, "checkFirst.look needs http"},
		{func(d *Duty) {
			d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://x"}, Tool: "t"}, Key: "id"}
		}, "checkFirst.look holds http and tool"},
		{func(d *Duty) { d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "ftp://x"}}, Key: "id"} }, "absolute http or https"},
		{func(d *Duty) {
			d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://u:p@x"}}, Key: "id"}
		}, "holds a user or a password"},
		{func(d *Duty) { d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://x"}}} }, "checkFirst.key is required with wake new"},
		{func(d *Duty) {
			d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://x"}}, Key: "id", Where: []Where{{Field: "p"}}}
		}, "needs exactly one of is"},
		{func(d *Duty) {
			d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://x"}}, Wake: Wake{When: WakeAtLeast}}
		}, "checkFirst.wake.count must be 1 or more"},
		{func(d *Duty) {
			d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://x"}}, Wake: Wake{When: "often"}}
		}, `checkFirst.wake.when is "often"`},
	} {
		d := daily("report")
		c.mut(&d)
		d.normalize("")
		got := strings.Join(d.problems(), " | ")
		if !strings.Contains(got, c.want) {
			t.Errorf("problems = %q, want %q", got, c.want)
		}
	}
}

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
	if got := readback(&d); got != "Every weekday at 9:00 AM, New York time, from October 26, 2026 until December 31, 2026. First it looks at example/api/queue for the items at result.records, where priority is 1 or 2. Only items that it did not see before wake docs-scribe, by their number. The run gets the list." {
		t.Fatalf("readback = %q", got)
	}
	var s schedule.Schedule = *d.When.Schedule
	if s.Repeat != schedule.RepeatWeekdays {
		t.Fatalf("schedule = %+v", s)
	}
}
