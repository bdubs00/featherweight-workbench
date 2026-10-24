package notify

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/internal/hostguard"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
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

// outbox holds what the sends of a test sent, and a failure to give.
type outbox struct {
	mu   sync.Mutex
	sent []sent
	fail error
}

// sent is one send of a test: the person and the channel, the kind of the
// channel and of the notification, the words, and the place.
type sent struct {
	To, Channel, Kind, Event, Title, Text, URL, Secret, EmailTo string
	Ntfy                                                        Ntfy
	Run, Duty                                                   string
}

func (o *outbox) send(_ context.Context, t target, m *message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail != nil {
		return o.fail
	}
	o.sent = append(o.sent, sent{To: m.To, Channel: t.id, Kind: t.kind, Event: m.Kind, Title: m.Title, Text: m.Text, URL: t.url, Secret: t.secret, EmailTo: t.to, Ntfy: t.ntfy, Run: m.Run, Duty: m.Duty})
	return nil
}

func (o *outbox) list() []sent {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]sent(nil), o.sent...)
}

func (o *outbox) setFail(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fail = err
}

// queue is the queue of the runs of the kind agent in a test.
type queue struct {
	mu     sync.Mutex
	reqs   []fw.RunRequest
	agents []string
}

func (q *queue) enqueue(_ context.Context, agent, _ string, reqs []fw.RunRequest) (string, int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.reqs = append(q.reqs, reqs...)
	for range reqs {
		q.agents = append(q.agents, agent)
	}
	return "fw.jobs." + agent, len(reqs), nil
}

func (q *queue) list() []fw.RunRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]fw.RunRequest(nil), q.reqs...)
}

// people takes the bearer token as the subject of a person.
type people struct{}

func (people) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return nil, fw.ErrUnauthorized
	}
	return &fw.Principal{Subject: tok, Kind: fw.KindUser}, nil
}

func (people) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
}

// front is a process with the duties and the notifications in memory,
// served on the native surface. alice owns the agent triage, and bob views
// it. The loop of the notifications does not run: a test calls pass. With
// real, the notifications go to their channels for real. Else the sends
// land in out. The runs of the kind agent land in queue.
type front struct {
	a     *fw.Agent
	s     *Service
	url   string
	clock *clock
	out   *outbox
	queue *queue
	b     bucket.Bucket
	logs  *logBuffer
}

// t0 is Friday, October 23, 2026, at 12:00 in New York.
var t0 = time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC)

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

func newFront(t *testing.T, block string, real bool) *front {
	t.Helper()
	return newFrontOn(t, block, real, bucket.NewMem(notifyBucket))
}

// newFrontOn is newFront on a bucket that a test shares between replicas.
func newFrontOn(t *testing.T, block string, real bool, b bucket.Bucket) *front {
	t.Helper()
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Access.Admins = []string{"root"}
	cfg.Agents = map[string]fw.AgentProfile{
		"triage": {Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"bob"}}},
	}
	cfg.Extensions = workbench(t, block)
	f := &front{clock: &clock{t: t0}, out: &outbox{}, queue: &queue{}, b: b, logs: &logBuffer{}}
	start := func(_ context.Context, a *fw.Agent) (io.Closer, error) {
		c, err := LoadConfig(a.Config())
		if err != nil {
			return nil, err
		}
		s := newService(a, b, c, slog.New(slog.NewTextHandler(f.logs, nil)))
		s.now, s.replica = f.clock.Now, "front-"+fw.NewID()[20:]
		s.enqueue = f.queue.enqueue
		if !real {
			s.sendFn = f.out.send
		}
		attach(a, s)
		return closer(func() { s.Close() }), nil
	}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()),
		fw.WithServices(duties.Start, start), fw.WithLogger(quiet()))
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
	if f.s == nil || duties.Of(a) == nil {
		t.Fatal("the notifications or the duties are off")
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
	return fw.WithPrincipal(context.Background(), &fw.Principal{Subject: who, Kind: fw.KindUser})
}

// set writes the settings of who and fails the test on an error.
func (f *front) set(t *testing.T, who string, body Body) Answer {
	t.Helper()
	ans, err := f.s.Put(as(who), body, "")
	if err != nil {
		t.Fatalf("Put(%s) = %v", who, err)
	}
	return ans
}

// duty writes a duty of triage owned by alice that starts on a call, so
// the engine never starts it by itself.
func (f *front) duty(t *testing.T, name string, notify *duties.Notify) {
	t.Helper()
	d := duties.Duty{
		Name: name, Agent: "triage",
		When:   duties.When{Webhook: &duties.Webhook{Auth: duties.WebhookAuth{Mode: "none"}}},
		Do:     duties.Do{Ask: &duties.Ask{Message: "Write the report."}},
		Limits: duties.Limits{Budget: &duties.Budget{MaxCostUSD: 0.5}},
		Notify: notify,
	}
	if _, err := duties.Of(f.a).Put(as("alice"), name, duties.DutyBody{Duty: d}, duties.PutOptions{}); err != nil {
		t.Fatalf("put the duty %s: %v", name, err)
	}
}

// tell sends a notice of a duty and waits until the notifications took it.
func (f *front) tell(t *testing.T, kind, duty, reason string) {
	t.Helper()
	if err := duties.Of(f.a).Tell(context.Background(), kind, duty, "", reason); err != nil {
		t.Fatal(err)
	}
	f.s.work.Wait()
}

// pass runs one pass of the loop at the time of the clock.
func (f *front) pass(t *testing.T) {
	t.Helper()
	f.s.pass(context.Background())
}

// send makes a request with the bearer token of who.
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

// connectMCP serves the MCP surface of the agent and connects a client
// with the token.
func connectMCP(t *testing.T, a *fw.Agent, token string) *sdk.ClientSession {
	t.Helper()
	h, err := mcp.Handler(a)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: bearer{token: token}}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "v1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + mcp.DefaultPath, HTTPClient: hc, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// call calls a tool.
func call(t *testing.T, cs *sdk.ClientSession, name string, args any) *sdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func textOf(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func structured(t *testing.T, res *sdk.CallToolResult, v any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("structuredContent %s: %v", b, err)
	}
}

// logBuffer holds the log lines of a front.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// ntfyServer is an ntfy server of a test: it records each JSON publish
// with its token, and answers status.
type ntfyServer struct {
	*httptest.Server
	mu     sync.Mutex
	got    []publish
	tokens []string
	status int
}

func newNtfy(t *testing.T) *ntfyServer {
	t.Helper()
	n := &ntfyServer{status: http.StatusOK}
	n.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var p publish
		_ = json.NewDecoder(r.Body).Decode(&p)
		if n.status != http.StatusOK {
			w.WriteHeader(n.status)
			_, _ = w.Write([]byte(`{"code":40101,"http":401,"error":"unauthorized"}`))
			return
		}
		n.got = append(n.got, p)
		n.tokens = append(n.tokens, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	t.Cleanup(n.Close)
	return n
}

func (n *ntfyServer) published() ([]publish, []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]publish(nil), n.got...), append([]string(nil), n.tokens...)
}

func (n *ntfyServer) setStatus(code int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.status = code
}

// hookServer is a TLS server of a test that stands for Slack, Discord, a
// Teams workflow, and a webhook: it records each request with its host,
// its path, its headers, and its body, and answers status with answer.
type hookServer struct {
	*httptest.Server
	mu     sync.Mutex
	hits   []hit
	status int
	answer string
}

type hit struct {
	Host, Path, Query string
	Header            http.Header
	Body              []byte
}

func newHook(t *testing.T) *hookServer {
	t.Helper()
	h := &hookServer{status: http.StatusOK}
	h.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.hits = append(h.hits, hit{Host: r.Host, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
		w.WriteHeader(h.status)
		_, _ = w.Write([]byte(h.answer))
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *hookServer) list() []hit {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hit(nil), h.hits...)
}

func (h *hookServer) setAnswer(status int, answer string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status, h.answer = status, answer
}

// through sends every request of the clients of s to the TLS server h,
// whatever the host of the address, and trusts its certificate for the
// name example.com. The host rules of the front still apply before the
// send.
func through(s *Service, h *hookServer) {
	pool := x509.NewCertPool()
	pool.AddCert(h.Certificate())
	mk := func() *http.Client {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, h.Listener.Addr().String())
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: "example.com"}
		return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return hostguard.ErrRedirect }}
	}
	s.listed, s.public = mk(), mk()
}

// trust makes the clients of s trust the certificate of the TLS server h,
// and keeps their dialers.
func trust(s *Service, h *hookServer) {
	for _, c := range []*http.Client{s.listed, s.public} {
		c.Transport.(*http.Transport).TLSClientConfig = h.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	}
}

// smtpServer is an SMTP server of a test. With tlsConfig, it offers
// STARTTLS, and it offers AUTH PLAIN once the connection is private or
// when it offers no STARTTLS at all.
type smtpServer struct {
	addr      string
	tlsConfig *tls.Config
	mu        sync.Mutex
	mails     []smtpMail
}

// smtpMail is one mail that the server took: the sender, the recipients,
// the data, the login, and whether the connection was private.
type smtpMail struct {
	From, Data, Auth string
	To               []string
	TLS              bool
}

func newSMTP(t *testing.T, tlsConfig *tls.Config) *smtpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &smtpServer{addr: ln.Addr().String(), tlsConfig: tlsConfig}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *smtpServer) serve(c net.Conn) {
	defer c.Close()
	tp := textproto.NewConn(c)
	_ = tp.PrintfLine("220 test ESMTP")
	private := false
	var m smtpMail
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, rest, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			lines := []string{"test"}
			if s.tlsConfig != nil && !private {
				lines = append(lines, "STARTTLS")
			}
			if private || s.tlsConfig == nil {
				lines = append(lines, "AUTH PLAIN")
			}
			lines = append(lines, "8BITMIME")
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				_ = tp.PrintfLine("250%s%s", sep, l)
			}
		case "STARTTLS":
			_ = tp.PrintfLine("220 go ahead")
			tc := tls.Server(c, s.tlsConfig)
			if tc.Handshake() != nil {
				return
			}
			c, tp, private = tc, textproto.NewConn(tc), true
		case "AUTH":
			m.Auth = rest
			_ = tp.PrintfLine("235 ok")
		case "MAIL":
			m.From = strings.TrimSuffix(strings.TrimPrefix(rest, "FROM:<"), ">")
			if i := strings.Index(m.From, ">"); i >= 0 {
				m.From = m.From[:i]
			}
			_ = tp.PrintfLine("250 ok")
		case "RCPT":
			m.To = append(m.To, strings.TrimSuffix(strings.TrimPrefix(rest, "TO:<"), ">"))
			_ = tp.PrintfLine("250 ok")
		case "DATA":
			_ = tp.PrintfLine("354 go ahead")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			m.Data, m.TLS = string(data), private
			s.mu.Lock()
			s.mails = append(s.mails, m)
			s.mu.Unlock()
			m = smtpMail{}
			_ = tp.PrintfLine("250 ok")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			return
		default:
			_ = tp.PrintfLine("250 ok")
		}
	}
}

func (s *smtpServer) list() []smtpMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]smtpMail(nil), s.mails...)
}

func ptrBool(v bool) *bool { return &v }

// on is send with every switch on.
var on = Send{Questions: true, Failures: true, Digest: true}

// ntfyChannel is a channel of the kind ntfy on the server of the front.
func ntfyChannel(id, topic string, send Send) Channel {
	return Channel{ID: id, Kind: KindNtfy, Send: send, Ntfy: &Ntfy{Topic: topic}}
}
