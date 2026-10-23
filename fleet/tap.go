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

// RunEvent is one event of the egress stream that the view applied, with
// what the view knows of its run: the member that holds the run, the
// profile of the run, and the first line of its input, at most 200 bytes.
// Agent and Input are empty while the view has not seen the start of the
// run.
type RunEvent struct {
	Member string
	Agent  string
	Input  string
	Event  fw.Event
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
// applies from the egress stream, with the agent of the fleet. A repeated
// event and an event that arrives after a later event of its run do not
// reach it, because the view applies neither. The function runs while the
// view holds its lock, so it must not block and must not call the view;
// it hands the event to a goroutine of its own. Every replica of the front
// reads the stream, so every replica calls it for every event, and the
// consumer must count each event once across replicas. The types must be
// in EgressTypes, or the workers do not publish them.
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
			ev = &RunEvent{Member: r.member, Agent: r.run.Agent, Input: firstLine(r.run.Input, tapInputLimit), Event: e}
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

// mark notes what an applied event tells of the run for the summary of the
// routines: fw.routine.applied with ok means that a routine completed the
// run, and a quality receipt with the verification rejected means that the
// world sent the run back.
func (r *platformRun) mark(e fw.Event) {
	switch e.Type {
	case fw.EvRoutineApplied:
		var d fw.RoutineAppliedData
		if e.Decode(&d) == nil && d.OK {
			r.byRoutine = true
		}
	case fw.EvRunAnnotated:
		var d fw.QualityAnnotatedData
		if e.Decode(&d) == nil && d.Quality != nil && d.Quality.Verification == "rejected" {
			r.sentBack = true
		}
	}
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
