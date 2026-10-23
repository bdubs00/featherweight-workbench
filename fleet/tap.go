package fleet

// This file lets another package read the events of the egress stream that
// the view applies (docs/ARCHITECTURE.md section 4.17), so a projection
// such as the evidence of the routines hangs on the stream that the front
// reads already, without a second subscription. It also keeps two marks of
// each run for the summary of the routines: whether a routine completed
// the run, and whether a quality receipt sent the run back.

import (
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// tapInputLimit bounds the first line of the input of a run that a
// RunEvent carries, in bytes.
const tapInputLimit = 200

// RunEvent is one event of the egress stream, with what the view knows of
// its run: the profile of the run, and its line, the title that the start
// of the run names (fw.MetaTitle), else the first line of its input, at
// most 200 bytes. Agent and Line are empty
// while the view has not seen the start of the run.
type RunEvent struct {
	Agent string
	Line  string
	Event fw.Event
}

// tap is one function that Tap registered, with its types.
type tap struct {
	types map[string]bool
	fn    func(*fw.Agent, RunEvent)
}

var taps struct {
	mu   sync.Mutex
	list []tap
}

// Tap registers a function that gets each event of the types that the view
// reads from the egress stream, with the agent of the fleet: also a
// repeated event and an event that arrives after a later event of its run,
// which the view itself does not apply, because a bus can deliver the
// events of a run out of order. The function runs while the view holds its
// lock, so it must not block and must not call the view; it hands the
// event to a goroutine of its own. Every replica of the front reads the
// stream, so every replica calls it for every event, and the consumer must
// count each event once, across replicas and repeats. A worker publishes a
// type only when its pool lists it in bus.egressTypes.
func Tap(fn func(a *fw.Agent, ev RunEvent), types ...string) {
	set := make(map[string]bool, len(types))
	for _, t := range types {
		set[t] = true
	}
	taps.mu.Lock()
	defer taps.mu.Unlock()
	taps.list = append(taps.list, tap{types: set, fn: fn})
}

// tapped calls the functions that Tap registered for the event. The caller
// holds p.mu.
func (p *platform) tapped(r *platformRun, e fw.Event) {
	taps.mu.Lock()
	list := taps.list
	taps.mu.Unlock()
	var ev *RunEvent
	for _, t := range list {
		if !t.types[e.Type] {
			continue
		}
		if ev == nil {
			line := r.run.Metadata[fw.MetaTitle]
			if strings.TrimSpace(line) == "" {
				line = r.run.Input
			}
			ev = &RunEvent{Agent: r.run.Agent, Line: firstLine(line, tapInputLimit), Event: e}
		}
		t.fn(p.f.a, *ev)
	}
}

// firstLine returns the first line of s without the spaces at its ends, cut
// to n bytes at a rune.
func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// AppliedOK reports whether an event is fw.routine.applied of a routine
// that completed its run.
func AppliedOK(e fw.Event) bool {
	if e.Type != fw.EvRoutineApplied {
		return false
	}
	var d fw.RoutineAppliedData
	return e.Decode(&d) == nil && d.OK
}

// Verification returns the verification of the quality observation that
// an event fw.run.annotated carries, such as accepted or rejected, or "".
func Verification(e fw.Event) string {
	if e.Type != fw.EvRunAnnotated {
		return ""
	}
	var d fw.QualityAnnotatedData
	if e.Decode(&d) != nil || d.Quality == nil {
		return ""
	}
	return d.Quality.Verification
}

// mark notes what an applied event tells of the run for the summary of the
// routines: a routine completed the run, or a quality observation sent the
// run back.
func (r *platformRun) mark(e fw.Event) {
	r.byRoutine = r.byRoutine || AppliedOK(e)
	r.sentBack = r.sentBack || Verification(e) == "rejected"
}

// Outcome returns what the view learned of the run id of another member
// from the egress stream: whether a routine completed it (fw.routine.applied
// with ok), whether a quality receipt sent it back (the verification
// rejected), and whether the view saw the start of the run on the stream,
// so it saw every such event after it. It returns false three times for a
// run that the view does not hold.
func (f *Fleet) Outcome(id string) (byRoutine, sentBack, whole bool) {
	p := f.platform
	if p == nil {
		return false, false, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.runs[id]
	if r == nil {
		return false, false, false
	}
	return r.byRoutine, r.sentBack, r.streamed
}
