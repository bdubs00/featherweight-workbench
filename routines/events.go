package routines

// This file reads the events of a routine that the kernel records
// (fw.RoutineDecidedData, fw.RoutineAppliedData, fw.RoutineComparedData)
// into what the evidence needs.

import (
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// routineOf returns the routine and the version that an event of a
// routine names.
func routineOf(e fw.Event) (string, int, bool) {
	var d struct {
		Routine string `json:"routine"`
		Version int    `json:"version"`
	}
	if e.Decode(&d) != nil || d.Routine == "" {
		return "", 0, false
	}
	return d.Routine, d.Version, true
}

// decisionOf reads fw.routine.decided. The probability of each question is
// the probability of the chosen answer: for a choice or a score the
// probability of the option, and for a yes or no question the larger of
// P(yes) and P(no).
func decisionOf(e fw.Event, routine string) (decision, bool) {
	var d fw.RoutineDecidedData
	if e.Decode(&d) != nil {
		return decision{}, false
	}
	out := decision{mode: d.Mode, applies: d.Applies}
	for q, a := range d.Answers {
		q = strings.TrimPrefix(q, routine+".")
		p := a.Probability
		if a.Type == fw.QuestionNoul {
			p = max(p, 1-p)
		} else if v, ok := a.Probabilities[a.Choice]; ok {
			p = v
		}
		if out.probability == nil {
			out.probability = map[string]float64{}
		}
		out.probability[q] = p
	}
	return out, true
}

// appliedOK reads fw.routine.applied: whether the routine completed.
func appliedOK(e fw.Event) (bool, bool) {
	var d fw.RoutineAppliedData
	if e.Decode(&d) != nil {
		return false, false
	}
	return d.OK, true
}

// comparisonOf reads fw.routine.compared.
func comparisonOf(e fw.Event, routine string) (comparison, bool) {
	var d fw.RoutineComparedData
	if e.Decode(&d) != nil || d.Question == "" {
		return comparison{}, false
	}
	return comparison{question: strings.TrimPrefix(d.Question, routine+"."), said: d.Said, observed: d.Observed, agree: d.Agree}, true
}

// verificationOf reads the verification of a quality receipt from
// fw.run.annotated, or "".
func verificationOf(e fw.Event) string {
	var d fw.QualityAnnotatedData
	if e.Decode(&d) != nil || d.Quality == nil {
		return ""
	}
	return d.Quality.Verification
}
