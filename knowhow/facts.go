package knowhow

// This file reads what a finished run of the archive tells beside its
// usage: whether a system verified it (docs/KNOWHOW.md section 1), whether
// a routine did the work, whether its text came from the open world, and
// what started it. Know-how learns only from results that a system records:
// a receipt of accepted or satisfied, an outcome grader that passed, a
// definition of done that passed in the kernel, and a thumbs up of a
// person. The service keeps the facts of each run in memory at the revision
// of its summary in the archive, so a pass reads the events of a run again
// only after the run changed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// maxFacts bounds the facts that the service keeps in memory.
const maxFacts = 50000

// runFacts is what one run tells. Verification is the newest receipt. Grade
// is the verdict of its outcome grader. Checked is true when the newest end
// of the run says that its definition of done passed and the pass was not
// weak (fw.CheckedDone): a check that only reads a file that was there
// before the run shows nothing about the run, so it verifies nothing.
// Untrusted is true when its text came from a block found, event, or
// source, or from a message of another principal. Trigger is a hash of what
// started the run: its input and those blocks.
type runFacts struct {
	updated      time.Time
	Verification string
	Grade        string
	Up, Down     bool
	Checked      bool
	ByRoutine    bool
	Untrusted    bool
	Trigger      string
}

// verified reports whether a system recorded that the run did its work.
func (f runFacts) verified() bool {
	return f.Verification == "accepted" || f.Verification == "satisfied" || f.Grade == fw.GradeSatisfied || f.Checked || f.Up
}

// rejected reports whether a receipt sent the run back.
func (f runFacts) rejected() bool { return f.Verification == "rejected" }

// failedGrade reports whether the grader of the run did not pass it.
func (f runFacts) failedGrade() bool {
	return f.Grade == fw.GradeNeedsRevision || f.Grade == fw.GradeMaxIterations
}

// doneWell reports whether a run completed and nothing that a system or a
// person recorded says otherwise.
func doneWell(r *fw.Run, f runFacts) bool {
	return r.State == fw.StateCompleted && !f.rejected() && !f.Down && !f.failedGrade()
}

// heldBack reports whether the run was held back from Know-how.
func heldBack(r *fw.Run) bool { return r.Metadata[fw.MetaKnowhow] == fw.KnowhowOff }

// dutyOf returns the Duty of a run, or "".
func dutyOf(r *fw.Run) string { return r.Metadata[fw.MetaDuty] }

// contextBlock matches one block that the kernel put at the start of a
// run (fw.ContextBlock).
var contextBlock = regexp.MustCompile(`(?s)<context source="([a-z][a-z0-9-]{0,31})">\n(.*?)\n</context>`)

// untrustedSources are the blocks whose text came from the open world.
var untrustedSources = map[string]bool{"found": true, "event": true, "source": true}

// factsFrom reads the facts of a run from its events.
func factsFrom(e fleet.ArchiveEntry, events []fw.Event) runFacts {
	r := e.Run
	f := runFacts{updated: e.Updated}
	if r.Outcome != nil {
		f.Grade = r.Outcome.Verdict
	}
	f.Up, f.Down = r.Reactions[fw.ReactionUp] > 0, r.Reactions[fw.ReactionDown] > 0
	f.Checked = r.Done != nil && fw.CheckedDone(r.Done.State, r.Done.Weak)
	trigger := sha256.New()
	trigger.Write([]byte(r.Input))
	for _, ev := range events {
		switch ev.Type {
		case fw.EvRunAnnotated:
			if v := fleet.Verification(ev); v != "" {
				f.Verification = v
			}
		case fw.EvRunFinished:
			// A follow-up ends the run again, so the newest end tells.
			var d fw.RunFinishedData
			if ev.Decode(&d) == nil {
				f.Checked = fw.CheckedDone(d.Done, d.DoneWeak)
			}
		case fw.EvRoutineApplied:
			f.ByRoutine = f.ByRoutine || fleet.AppliedOK(ev)
		case fw.EvRunStarted:
			var d fw.RunStartedData
			if ev.Decode(&d) != nil {
				continue
			}
			for _, m := range contextBlock.FindAllStringSubmatch(d.Context, -1) {
				if untrustedSources[m[1]] {
					f.Untrusted = true
					trigger.Write([]byte{0})
					trigger.Write([]byte(m[2]))
				}
			}
			for src := range untrustedSources {
				if strings.Contains(d.Context, `<context source="`+src+`">`) {
					f.Untrusted = true
				}
			}
		case fw.EvMessageReceived:
			var d fw.MessageReceivedData
			if ev.Decode(&d) != nil || d.Role != fw.RoleUser || d.Note != "" {
				continue
			}
			if d.From != nil || (d.By != "" && d.By != r.Principal) {
				f.Untrusted = true
			}
		}
	}
	f.Trigger = hex.EncodeToString(trigger.Sum(nil)[:8])
	return f
}

// cachedFacts returns the facts of a run that the service holds at the
// revision of its summary.
func (s *Service) cachedFacts(e fleet.ArchiveEntry) (runFacts, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.facts[e.Run.ID]
	return f, ok && f.updated.Equal(e.Updated)
}

// keepFacts keeps the facts of a run.
func (s *Service) keepFacts(id string, f runFacts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.facts) >= maxFacts {
		s.facts = map[string]runFacts{}
	}
	s.facts[id] = f
}

// factsOf returns the facts of a run, reading its events when the service
// holds no facts at the revision of its summary.
func (s *Service) factsOf(ctx context.Context, e fleet.ArchiveEntry) (runFacts, error) {
	if f, ok := s.cachedFacts(e); ok {
		return f, nil
	}
	f, _, err := s.readRun(ctx, e)
	return f, err
}

// readRun reads the events of a run of the archive and keeps its facts.
func (s *Service) readRun(ctx context.Context, e fleet.ArchiveEntry) (runFacts, []fw.Event, error) {
	if s.arch == nil {
		return runFacts{}, nil, nil
	}
	events, err := s.arch.Events(ctx, e.Run.ID)
	if err != nil {
		return runFacts{}, nil, err
	}
	f := factsFrom(e, events)
	s.keepFacts(e.Run.ID, f)
	return f, events, nil
}

// archive returns the runs of the archive, or none without one.
func (s *Service) archive(ctx context.Context) []fleet.ArchiveEntry {
	if s.arch == nil {
		return nil
	}
	list, _ := s.arch.All(ctx)
	return list
}

// cut cuts a text to n bytes at a rune, with "…" after a cut.
func cut(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for n > 0 && (text[n]&0xC0) == 0x80 {
		n--
	}
	return text[:n] + "…"
}
