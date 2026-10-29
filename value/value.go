package value

// This file serves the value of the work: GET /v1/value, PUT
// /v1/value/estimates, and the MCP tools value and value_estimate, which
// call the same functions with the same access rules. It also keeps the
// estimates of people in a key-value bucket of the bus.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/bossmode"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	fw.RegisterService(Start)
	native.Extend(mount)
	mcp.RegisterTools(tools)
}

// MaxEstimateMinutes bounds an estimate: 40 hours for one piece.
const MaxEstimateMinutes = 2400

// bucketConfig is the bucket of the estimates.
var bucketConfig = bucket.Config{
	Name:        "VALUE",
	Description: "The estimates of the people of a featherweight fleet: the minutes that a person takes for one piece of a line of work.",
	History:     8,
	MaxValue:    4 << 10,
}

// ErrOff means that the process keeps no estimates. A route answers 503.
var ErrOff = errors.New("value: off")

// Store keeps the estimates of one agent.
type Store struct {
	b   bucket.Bucket
	now func() time.Time
}

var stores sync.Map // *fw.Agent to *Store

// Of returns the store of the estimates of an agent, or nil.
func Of(a *fw.Agent) *Store {
	if v, ok := stores.Load(a); ok {
		return v.(*Store)
	}
	return nil
}

// Attach makes s the store of a. A nil s detaches.
func Attach(a *fw.Agent, s *Store) {
	if s == nil {
		stores.Delete(a)
		return
	}
	stores.Store(a, s)
}

// NewStore returns a store on a bucket.
func NewStore(b bucket.Bucket) *Store { return &Store{b: b, now: time.Now} }

// Start is the service of the estimates (fw.Service). It opens the bucket
// on the bus of the configuration. On a bus that keeps no store, the value
// still answers, without estimates.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "value")
	b, closeFn, err := bucket.Open(ctx, a.Config(), bucketConfig, log)
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), bucketConfig)
		log.Warn("value: the value of the work keeps no estimates; they live "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("value: %w", err)
	}
	s := NewStore(b)
	Attach(a, s)
	return closer(func() {
		if Of(a) == s {
			Attach(a, nil)
		}
		if closeFn != nil {
			closeFn()
		}
	}), nil
}

type closer func()

func (c closer) Close() error {
	c()
	return nil
}

func estimateKey(line string) string {
	return "line." + base64.RawURLEncoding.EncodeToString([]byte(line))
}

// Estimates returns every estimate by the key of its line.
func (s *Store) Estimates(ctx context.Context) (map[string]Estimate, error) {
	out := map[string]Estimate{}
	if s == nil {
		return out, nil
	}
	keys, err := bucket.KeysUnder(ctx, s.b, "line")
	if err != nil {
		return nil, fmt.Errorf("value: read the estimates: %w", err)
	}
	for _, k := range keys {
		e, err := s.b.Get(ctx, k)
		if err != nil {
			continue
		}
		var v struct {
			Line string `json:"line"`
			Estimate
		}
		if json.Unmarshal(e.Value, &v) == nil && v.Line != "" {
			out[v.Line] = v.Estimate
		}
	}
	return out, nil
}

// set writes or removes the estimate of a line.
func (s *Store) set(ctx context.Context, line string, minutes int, by string) (*Estimate, error) {
	key := estimateKey(line)
	if minutes == 0 {
		e, err := s.b.Get(ctx, key)
		if errors.Is(err, bucket.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return nil, s.b.Delete(ctx, key, e.Revision)
	}
	est := Estimate{Minutes: minutes, By: by, At: s.now().UTC().Truncate(time.Second)}
	raw, _ := json.Marshal(struct {
		Line string `json:"line"`
		Estimate
	}{line, est})
	_, err := bucket.Modify(ctx, s.b, key, 8, func([]byte, bool) ([]byte, error) { return raw, nil })
	if err != nil {
		return nil, err
	}
	return &est, nil
}

// For returns the value of the work for the caller in ctx over the last
// days, at the time now.
func For(ctx context.Context, a *fw.Agent, now time.Time, days int) (Answer, error) {
	if days == 0 {
		days = DefaultDays
	}
	if !slices.Contains(Windows, days) {
		return Answer{}, fmt.Errorf("%w: days is 7, 30, or 90", fw.ErrInvalidRequest)
	}
	since := now.Add(-time.Duration(days) * 24 * time.Hour)
	runs, err := fleet.WorkSince(ctx, a, since)
	if err != nil {
		return Answer{}, err
	}
	est, err := Of(a).Estimates(ctx)
	if err != nil {
		a.Logger().Warn("value: the estimates do not read", "error", err)
		est = nil
	}
	// A bridge is one task: the fold reads its state from its record.
	var teams map[string]bridges.Brief
	if st := bridges.Of(a); st != nil {
		if teams, err = st.Briefs(ctx); err != nil {
			return Answer{}, err
		}
	}
	return Fold(runs, Options{Now: now, Days: days, Coordination: coordination(a), Estimates: est, Teams: teams}), nil
}

// coordination names the agents whose runs coordinate the work: the
// manager and the quick chat of bossmode.
func coordination(a *fw.Agent) map[string]bool {
	out := map[string]bool{}
	c, err := bossmode.Load(a.Config())
	if err != nil {
		return out
	}
	for _, n := range []string{c.Manager, c.Chat.Agent} {
		if n = strings.TrimSpace(n); n != "" {
			out[n] = true
		}
	}
	return out
}

// EstimateRequest is the body of PUT /v1/value/estimates and the arguments
// of the tool value_estimate: the key of a line, such as duty:nightly-report,
// and the minutes that a person takes for one piece of it. Zero minutes
// removes the estimate.
type EstimateRequest struct {
	Line    string `json:"line"`
	Minutes int    `json:"minutes"`
}

// SetEstimate sets the estimate of a line for the caller in ctx. The
// caller must see the line in the last 90 days, else the error wraps
// fw.ErrRunNotFound, and must own its agent (agent.edit), else
// fw.ErrForbidden. It returns the line with its new estimate.
func SetEstimate(ctx context.Context, a *fw.Agent, now time.Time, req EstimateRequest) (Line, error) {
	s := Of(a)
	if s == nil {
		return Line{}, fmt.Errorf("%w: this server keeps no estimates, because its bus keeps no store", ErrOff)
	}
	req.Line = strings.TrimSpace(req.Line)
	if req.Minutes < 0 || req.Minutes > MaxEstimateMinutes {
		return Line{}, fmt.Errorf("%w: minutes is from 0 to %d", fw.ErrInvalidRequest, MaxEstimateMinutes)
	}
	ans, err := For(ctx, a, now, 90)
	if err != nil {
		return Line{}, err
	}
	i := slices.IndexFunc(ans.Lines, func(l Line) bool { return l.Key == req.Line })
	if i < 0 {
		return Line{}, fmt.Errorf("%w: no line of work %q that you can view in the last 90 days", fw.ErrRunNotFound, req.Line)
	}
	line := ans.Lines[i]
	if err := faces.Check(ctx, a, line.Agent, fw.ActionAgentEdit); err != nil {
		if errors.Is(err, fw.ErrProfileNotFound) {
			return Line{}, fmt.Errorf("%w: the agent %s of the line is gone", fw.ErrRunNotFound, line.Agent)
		}
		return Line{}, err
	}
	by := ""
	if p := fw.PrincipalFrom(ctx); p != nil {
		by = p.Subject
	}
	if act := fw.ActingFor(ctx); act != "" {
		by = act
	}
	est, err := s.set(ctx, req.Line, req.Minutes, by)
	if err != nil {
		return Line{}, fmt.Errorf("value: keep the estimate: %w", err)
	}
	line.Estimate, line.EstimatedMinutes = est, 0
	if est != nil {
		line.EstimatedMinutes = int64(est.Minutes) * int64(line.Counts.Done-line.Checks.SentBack)
	}
	return line, nil
}

// mount serves GET /v1/value and PUT /v1/value/estimates.
func mount(k *native.Kit) error {
	a := k.Agent()
	k.Handle("GET /v1/value", func(w http.ResponseWriter, r *http.Request) {
		days := 0
		if v := r.URL.Query().Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				native.WriteProblem(w, r, http.StatusBadRequest, "days is 7, 30, or 90")
				return
			}
			days = n
		}
		ctx, _, _ := manager.OnBehalf(r.Context())
		ans, err := For(ctx, a, time.Now(), days)
		if err != nil {
			fail(w, r, k, err)
			return
		}
		native.WriteJSON(w, http.StatusOK, ans)
	})
	k.Handle("PUT /v1/value/estimates", func(w http.ResponseWriter, r *http.Request) {
		var req EstimateRequest
		if !k.Decode(w, r, &req) {
			return
		}
		ctx, _, _ := manager.OnBehalf(r.Context())
		line, err := SetEstimate(ctx, a, time.Now(), req)
		if err != nil {
			fail(w, r, k, err)
			return
		}
		native.WriteJSON(w, http.StatusOK, line)
	})
	return nil
}

// fail maps an error of the package to a problem document.
func fail(w http.ResponseWriter, r *http.Request, k *native.Kit, err error) {
	msg := err.Error()
	for _, p := range []string{ErrOff.Error() + ": ", fw.ErrInvalidRequest.Error() + ": ", fw.ErrRunNotFound.Error() + ": ", fw.ErrForbidden.Error() + ": "} {
		msg = strings.ReplaceAll(msg, p, "")
	}
	switch {
	case errors.Is(err, ErrOff):
		native.WriteProblem(w, r, http.StatusServiceUnavailable, msg)
	case errors.Is(err, fw.ErrInvalidRequest):
		native.WriteProblem(w, r, http.StatusBadRequest, msg)
	case errors.Is(err, fw.ErrRunNotFound):
		native.WriteProblem(w, r, http.StatusNotFound, msg)
	default:
		k.Fail(w, r, err)
	}
}

// Names of the tools.
const (
	ToolValue    = "value"
	ToolEstimate = "value_estimate"
)

func ptr[T any](v T) *T { return &v }

// tools builds the tools value and value_estimate for one MCP server.
func tools(srv *mcp.Server) []mcp.Tool {
	a := srv.Agent()
	type valueIn struct {
		Days int `json:"days,omitempty"`
	}
	return []mcp.Tool{
		mcp.NewTool(&sdk.Tool{
			Name:        ToolValue,
			Title:       "What the work gave",
			Description: "Say what the work of the agents that you can view gave and took over the last 7, 30, or 90 days (days, 30 by default): the pieces of work that finished, failed, or stopped; the verdicts of people on the results (accepted, sent back, unchecked); the actions of people that the work needed and the time that it waited for them; the median time from the ask to the first end; and the spend, also per finished piece. A bridge (a team) counts as one piece, done at its close, with the cost of its seats. Lines split the numbers by duty, source, and agent. An estimate of a person, the minutes that a person takes for one piece, turns the finished work of a line into hours of a person; value_estimate sets it. The numbers claim only what the records hold: an unchecked result is not a good one, and runs whose model has no price make the spend a lower bound (a model whose price is zero has a price).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"days":{"type":"integer","enum":[7,30,90],"description":"The window in days, 30 by default."}}}`),
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in valueIn) (*sdk.CallToolResult, any, error) {
			ctx, _, _ = manager.OnBehalf(ctx)
			ans, err := For(ctx, a, time.Now(), in.Days)
			if err != nil {
				return mcp.ErrorResult(mcp.Explain(err)), nil, nil
			}
			return mcp.Result(text(ans), ans), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolEstimate,
			Title:       "Set the minutes of a piece of work",
			Description: "Set the estimate of a person for one line of work of the tool value: line, the key of the line such as duty:nightly-report or agent:triage, and minutes, the minutes that a person takes for one piece of it, from 1 to 2400. Zero minutes removes the estimate. The estimate names you and the time. You must own the agent of the line.",
			InputSchema: json.RawMessage(`{"type":"object","required":["line","minutes"],"properties":{"line":{"type":"string"},"minutes":{"type":"integer","minimum":0,"maximum":2400}}}`),
			Annotations: &sdk.ToolAnnotations{IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in EstimateRequest) (*sdk.CallToolResult, any, error) {
			ctx, _, _ = manager.OnBehalf(ctx)
			line, err := SetEstimate(ctx, a, time.Now(), in)
			if err != nil {
				return mcp.ErrorResult(mcp.Explain(err)), nil, nil
			}
			msg := fmt.Sprintf("The line %s has no estimate now.", line.Key)
			if line.Estimate != nil {
				msg = fmt.Sprintf("The line %s takes a person %d min a piece, by your estimate. Its %d finished pieces in the last 90 days would take a person %s.", line.Key, line.Estimate.Minutes, line.Counts.Done-line.Checks.SentBack, Duration(line.EstimatedMinutes*60))
			}
			return mcp.Result(msg, line), nil, nil
		}),
	}
}

// text words the answer for the model: the sentence, then one line per
// line of work.
func text(a Answer) string {
	var b strings.Builder
	b.WriteString(a.Sentence)
	for _, l := range a.Lines {
		fmt.Fprintf(&b, "\n- %s: %d done, %d failed or stopped, %d open; %d accepted, %d checked, %d sent back, %d unchecked; %d without a person; %s", l.Key, l.Counts.Done, l.Counts.Failed+l.Counts.Stopped, l.Counts.Open, l.Checks.Accepted, l.Checks.Checked, l.Checks.SentBack, l.Checks.Unchecked, l.NoPerson, Dollars(l.USD))
		if l.Estimate != nil {
			fmt.Fprintf(&b, "; %d min a piece by %s", l.Estimate.Minutes, l.Estimate.By)
		}
	}
	return b.String()
}
