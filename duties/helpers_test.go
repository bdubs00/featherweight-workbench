package duties

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/duties/schedule"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/sources"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// clock is the time of a test, which the test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// queue holds the run requests that the engine put on the queues, and a
// failure to give.
type queue struct {
	mu   sync.Mutex
	reqs []fw.RunRequest
	fail error
}

func (q *queue) enqueue(_ context.Context, agent, pool string, reqs []fw.RunRequest) (string, int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fail != nil {
		return "fw.jobs." + agent, 0, q.fail
	}
	q.reqs = append(q.reqs, reqs...)
	return "fw.jobs." + agent, len(reqs), nil
}

func (q *queue) list() []fw.RunRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]fw.RunRequest(nil), q.reqs...)
}

func (q *queue) last(t *testing.T) fw.RunRequest {
	t.Helper()
	l := q.list()
	if len(l) == 0 {
		t.Fatal("no run request went out")
	}
	return l[len(l)-1]
}

func (q *queue) setFail(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.fail = err
}

// view holds the runs that the fleet shows, by id.
type view struct {
	mu   sync.Mutex
	runs map[string]*fw.Run
}

func (v *view) set(id string, state fw.RunState, cost float64, mut ...func(*fw.Run)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.runs == nil {
		v.runs = map[string]*fw.Run{}
	}
	r := &fw.Run{ID: id, State: state, Usage: fw.RunUsage{CostUSD: cost}, Metadata: map[string]string{}}
	for _, m := range mut {
		m(r)
	}
	v.runs[id] = r
}

func (v *view) get(context.Context, string) (map[string]*fw.Run, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[string]*fw.Run{}
	for id, r := range v.runs {
		c := *r
		out[id] = &c
	}
	return out, nil
}

// secrets holds the variables of a test.
type secrets map[string]string

func (s secrets) get(_ context.Context, name string) (string, error) {
	if v, ok := s[name]; ok && v != "" {
		return v, nil
	}
	return "", fw.ErrSecretNotFound
}

// notices collects the notices of the tests, by agent.
type notices struct {
	mu   sync.Mutex
	list map[*fw.Agent][]Notice
}

func (n *notices) add(_ context.Context, a *fw.Agent, x Notice) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.list == nil {
		n.list = map[*fw.Agent][]Notice{}
	}
	n.list[a] = append(n.list[a], x)
}

// of returns the notices of a duty of an agent.
func (n *notices) of(a *fw.Agent, duty string) []Notice {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []Notice
	for _, x := range n.list[a] {
		if x.Duty == duty {
			out = append(out, x)
		}
	}
	return out
}

// kinds returns the kinds of the notices of a duty of an agent.
func (n *notices) kinds(a *fw.Agent, duty string) []string {
	var out []string
	for _, x := range n.of(a, duty) {
		out = append(out, x.Kind)
	}
	return out
}

var allNotices = &notices{}

func init() { OnNotice(allNotices.add) }

// people takes the bearer token as the subject of a person, fleet as the
// static token of the fleet, and robot as a workload.
type people struct{}

func (people) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return nil, fw.ErrUnauthorized
	}
	switch tok {
	case "fleet":
		return &fw.Principal{Subject: fw.KindToken, Kind: fw.KindToken}, nil
	case "robot":
		return &fw.Principal{Subject: "spiffe://lab/robot", Kind: fw.KindWorkload}, nil
	}
	return &fw.Principal{Subject: tok, Kind: fw.KindUser}, nil
}

func (people) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="fw"`)
	w.WriteHeader(http.StatusUnauthorized)
}

// front is a process with the duties in memory, a clock, a queue, and a
// view of the fleet of the test, served on the native surface. alice owns
// the agent triage, bob operates it, and carol views it. dave owns
// private. root is an admin. The engine does not run by itself: a test
// calls pass.
type front struct {
	a     *fw.Agent
	s     *Service
	url   string
	clock *clock
	queue *queue
	view  *view
	sec   secrets
	store *Store // the store of the duties; a replica of a test shares it
}

// t0 is Friday, October 23, 2026, at 12:00 in New York.
var t0 = time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC)

// workbench returns the extension block of the YAML.
func workbench(t *testing.T, block string) map[string]yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Content) == 0 {
		return nil
	}
	return map[string]yaml.Node{"workbench": *doc.Content[0]}
}

func newFront(t *testing.T, block string, mut func(*fw.Config)) *front {
	t.Helper()
	return newFrontOn(t, fw.NewMemBus(), block, mut)
}

// newFrontOn is newFront on a bus that a test shares with its workers.
func newFrontOn(t *testing.T, bus fw.Bus, block string, mut func(*fw.Config)) *front {
	t.Helper()
	return newReplica(t, bus, block, mut, nil)
}

// newReplica is newFrontOn with the store of the duties st, which another
// replica of the test shares, or a new store for nil.
func newReplica(t *testing.T, bus fw.Bus, block string, mut func(*fw.Config), st *Store) *front {
	t.Helper()
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Access.Admins = []string{"root"}
	cfg.Time.Zone = "UTC"
	cfg.Agents = map[string]fw.AgentProfile{
		"triage":  {Access: &fw.Access{Owners: []string{"alice", "spiffe://lab/robot"}, Operators: []string{"bob"}, Viewers: []string{"carol"}}},
		"private": {Access: &fw.Access{Owners: []string{"dave"}}},
	}
	cfg.Extensions = workbench(t, block)
	if mut != nil {
		mut(cfg)
	}
	f := &front{clock: &clock{t: t0}, queue: &queue{}, view: &view{}, sec: secrets{}, store: st}
	start := func(_ context.Context, a *fw.Agent) (io.Closer, error) {
		c, err := LoadConfig(a.Config())
		if err != nil {
			return nil, err
		}
		// The sources start before the duties, as the services of the
		// process do, and the duties import their entries.
		srcs, err := sources.Start(context.Background(), a)
		if err != nil {
			return nil, err
		}
		st := f.store
		if st == nil {
			st = NewStore(bucket.NewMem(dutiesBucket), bucket.NewMem(runsBucket), bucket.NewMem(seenBucket), quiet())
		}
		st.now = f.clock.Now
		s := newService(a, st, c, quiet())
		s.now, s.enqueue, s.runs, s.secret, s.replica = f.clock.Now, f.queue.enqueue, f.view.get, f.sec.get, "front-0"
		s.importSources(context.Background())
		attach(a, s)
		return closer(func() {
			attach(a, nil)
			if srcs != nil {
				_ = srcs.Close()
			}
		}), nil
	}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus), fw.WithSurfaces(native.New()),
		fw.WithServices(start), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	h, err := a.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	f.a, f.s, f.url = a, Of(a), srv.URL
	if f.s != nil {
		f.store = f.s.st
	}
	if f.s == nil {
		t.Fatal("the duties are off")
	}
	return f
}

type closer func()

func (c closer) Close() error {
	c()
	return nil
}

// as returns a context with the person who.
func as(who string) context.Context {
	switch who {
	case "":
		return context.Background()
	case "robot":
		return fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "spiffe://lab/robot", Kind: fw.KindWorkload})
	}
	return fw.WithPrincipal(context.Background(), &fw.Principal{Subject: who, Kind: fw.KindUser})
}

// put writes a duty as who and fails the test on an error.
func (f *front) put(t *testing.T, who string, d Duty) *View {
	t.Helper()
	ans, err := f.s.Put(as(who), d.Name, DutyBody{Duty: d}, PutOptions{})
	if err != nil {
		t.Fatalf("Put(%s) = %v", d.Name, err)
	}
	return ans.View
}

// pass runs one pass of the engine at the time of the clock.
func (f *front) pass(t *testing.T) {
	t.Helper()
	f.s.pass(context.Background())
}

// history returns the history of a duty, newest first.
func (f *front) history(t *testing.T, name string) []RunEntry {
	t.Helper()
	h, err := f.s.st.history(context.Background(), name, 100)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// states returns the states of the history, newest first.
func (f *front) states(t *testing.T, name string) []string {
	t.Helper()
	var out []string
	for _, e := range f.history(t, name) {
		out = append(out, e.State)
	}
	return out
}

// daily is a duty of triage every day at 9:00 in New York.
func daily(name string) Duty {
	return Duty{
		Name:   name,
		Agent:  "triage",
		When:   When{Schedule: scheduleOf("daily", "09:00")},
		Do:     Do{Ask: &Ask{Message: "Write the report of {date} at {time} for {duty}."}},
		Limits: Limits{Budget: &Budget{MaxCostUSD: 0.5, MaxTurns: 40}},
	}
}

func intp(n int) *int { return &n }

// scheduleOf is a schedule in New York.
func scheduleOf(repeat string, times ...string) *schedule.Schedule {
	return &schedule.Schedule{Repeat: repeat, Times: times, TimeZone: "America/New_York"}
}

// send makes a request with the bearer token of who, none for "".
func send(t *testing.T, method, url, who, body string, header ...string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+who)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func decodeJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var errDown = errors.New("the bus is down")
