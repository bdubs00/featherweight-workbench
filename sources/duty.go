package sources

// This file lends the delivery of the sources to the duties
// (docs/ARCHITECTURE.md section 22.8). At its start, the server copies
// each entry of workbench.sources into the store of the duties, and the
// duty then runs the entry. The sources run only the entries that the
// duties did not take over (Adopt). A duty that keeps the templates of its
// entry delivers its events here, with the same claims, the same run
// requests, the same redrive, and the same counters as the entry did, so
// its runs stay as they were. The old address POST /v1/sources/{name}
// answers as the call of the duty (RegisterTakeover).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/heldback"
)

// ErrNotStarted means that the sources of the process have not started,
// so a duty cannot deliver the events of its entry yet.
var ErrNotStarted = errors.New("sources: the store of the sources of this server has not started")

// adopted holds the names of the entries that the duties took over, by
// agent. The sources run none of them.
var adopted sync.Map // *fw.Agent -> *nameSet

type nameSet struct {
	mu    sync.Mutex
	names map[string]bool
}

// Adopt marks the entries that the duties of the agent run. A poll
// source of such a name polls no more, and the route of a webhook source
// of such a name answers as the call of the duty.
func Adopt(a *fw.Agent, names []string) {
	v, _ := adopted.LoadOrStore(a, &nameSet{names: map[string]bool{}})
	set := v.(*nameSet)
	set.mu.Lock()
	defer set.mu.Unlock()
	for _, n := range names {
		set.names[n] = true
	}
}

// Adopted reports whether the duties of the agent run the entry name.
func Adopted(a *fw.Agent, name string) bool {
	v, ok := adopted.Load(a)
	if !ok {
		return false
	}
	set := v.(*nameSet)
	set.mu.Lock()
	defer set.mu.Unlock()
	return set.names[name]
}

// takeover answers POST /v1/sources/{name} for an entry that the duties
// took over, and reports whether it answered.
var takeover struct {
	mu sync.Mutex
	fn func(a *fw.Agent, w http.ResponseWriter, r *http.Request) bool
}

// RegisterTakeover sets the function that answers the old address of an
// entry that the duties took over. The package duties calls it in init.
func RegisterTakeover(fn func(a *fw.Agent, w http.ResponseWriter, r *http.Request) bool) {
	takeover.mu.Lock()
	defer takeover.mu.Unlock()
	takeover.fn = fn
}

func takeoverFn() func(a *fw.Agent, w http.ResponseWriter, r *http.Request) bool {
	takeover.mu.Lock()
	defer takeover.mu.Unlock()
	return takeover.fn
}

// Delivered is what a delivery of the events of a duty did: Events counts
// the events of the document, Runs the run requests that went to the
// queue, Duplicates the events whose key made a run within the window,
// Busy the events that another replica holds, Refused the items that do
// not fit the templates, and Failed the events that made no run because
// the store or the queue failed, with Err.
type Delivered struct {
	Events     int
	Runs       []DeliveredRun
	Duplicates int
	Busy       int
	Refused    int
	Failed     int
	Err        error
}

// DeliveredRun is one run request of a delivery: the position of its
// event, from 1, its id, its key, and its title.
type DeliveredRun struct {
	Event int
	ID    string
	Key   string
	Title string
}

// runner returns the source of a duty: the entry src as the duty keeps
// it, with the counters of the entry of the same name of the
// configuration.
func (s *Sources) runner(ctx context.Context, src Source) (*source, error) {
	src = withDefaults(src)
	tpl, err := compileTemplates(src)
	if err != nil {
		return nil, fmt.Errorf("sources: the templates of %s: %w", src.Name, err)
	}
	queue, err := fleet.QueueFor(ctx, s.a, src.Agent, src.Pool)
	if err != nil {
		return nil, fmt.Errorf("sources: %s: %w", src.Name, err)
	}
	out := &source{cfg: src, tpl: tpl, queue: queue, count: &counter{st: Stat{Source: src.Name, Kind: src.Kind(), Agent: src.Agent}}}
	if known := s.byName[src.Name]; known != nil {
		// The counters of the entry go on, so the metrics count the runs
		// of the duty as those of the entry.
		out.count = known.count
	}
	return out, nil
}

// renderAll renders each event with the templates of src. With strict,
// an event that does not fit is an error, as for a webhook. Without
// strict, it counts as refused and the others go on, as for a poll. Each
// request gets its id, so the duty names its run at once.
func (s *Sources) renderAll(src *source, events []map[string]any, strict bool) ([]rendered, int, error) {
	items := make([]rendered, 0, len(events))
	refused := 0
	for i, e := range events {
		it, err := src.render(i+1, e)
		if err != nil {
			if strict {
				return nil, 0, fmt.Errorf("event %d: %w", i+1, err)
			}
			refused++
			src.count.refused(1, fmt.Sprintf("item %d: %s", i+1, err), s.now())
			s.log.Warn("sources: an item does not fit the templates", "source", src.cfg.Name, "reason", fmt.Sprintf("item %d: %s", i+1, err))
			continue
		}
		it.req.ID = fw.NewID()
		heldback.Mark(&it.req, src.cfg.ComparisonRuns)
		items = append(items, it)
	}
	return items, refused, nil
}

// delivered turns a delivery into its answer for a duty.
func delivered(events, refused int, d delivery) Delivered {
	out := Delivered{Events: events, Duplicates: len(d.duplicates), Busy: len(d.busy), Refused: refused, Failed: d.failed, Err: d.err}
	for _, it := range d.accepted {
		out.Runs = append(out.Runs, DeliveredRun{Event: it.index, ID: it.req.ID, Key: it.key, Title: it.req.Metadata[fw.MetaTitle]})
	}
	return out
}

// DeliverDocument delivers the items of the document of a poll for the
// duty src: the items at the dotted path src.Items, each rendered with the
// templates of src, through the claims and the queue of a poll source. An
// item that does not fit the templates counts as refused, and the others
// go on.
func DeliverDocument(ctx context.Context, a *fw.Agent, src Source, body []byte) (Delivered, error) {
	s := of(a)
	if s == nil {
		return Delivered{}, ErrNotStarted
	}
	run, err := s.runner(ctx, src)
	if err != nil {
		return Delivered{}, err
	}
	events, err := decodeEvents(body, run.cfg.Items)
	if err != nil {
		run.count.failed(err.Error(), s.now())
		return Delivered{}, err
	}
	run.count.polled(s.now())
	itemsOf(ctx, events)
	items, refused, _ := s.renderAll(run, events, false)
	if len(items) == 0 {
		return Delivered{Events: len(events), Refused: refused}, nil
	}
	return delivered(len(events), refused, s.deliver(ctx, run, items)), nil
}

// ServeEvents answers a call of the webhook of the duty src, after the
// duty checked the credential of the caller: it reads the events of the
// body, renders them with the templates of src, delivers them, and writes
// the answer of POST /v1/sources/{name}. It reports what it delivered.
func ServeEvents(w http.ResponseWriter, r *http.Request, a *fw.Agent, src Source, body []byte) Delivered {
	s := of(a)
	if s == nil {
		w.Header().Set("Retry-After", retryAfter)
		native.WriteProblem(w, r, http.StatusServiceUnavailable, "The store of the sources of this server has not started yet.")
		return Delivered{Err: ErrNotStarted}
	}
	run, err := s.runner(r.Context(), src)
	if err != nil {
		w.Header().Set("Retry-After", retryAfter)
		native.WriteProblem(w, r, http.StatusServiceUnavailable, "The duty cannot reach its queue now: "+err.Error()+".")
		return Delivered{Err: err}
	}
	return s.answerEvents(w, r, run, body)
}
