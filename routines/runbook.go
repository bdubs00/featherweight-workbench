package routines

// This file writes the runbook of a routine (docs/ARCHITECTURE.md section
// 21.5): on a move to live, the page agents/<agent>/routines/<name>.md of
// the wiki says in words what the routine decides, the steps with the
// names of their tools, the check afterward, where the routine comes
// from, and who signed it off. The front writes it as pages.ByFront, with
// no run.

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pages"
)

// RunbookPath returns the path of the runbook of a routine in the wiki.
func RunbookPath(agent, name string) string {
	return "agents/" + agent + "/routines/" + name + ".md"
}

// methodOf finds the method of a step, such as add_labels in
// {"method": "add_labels", ...}, which says more than the tool alone.
var methodOf = regexp.MustCompile(`"method"\s*:\s*"([A-Za-z0-9_.-]{1,64})"`)

// Runbook renders the runbook of a routine as Markdown.
func Runbook(rec Record) string {
	r := rec.Routine
	var b strings.Builder
	fmt.Fprintf(&b, "---\ntitle: %s\ntags: routine, %s\n---\n\n# %s\n\n", r.Name, rec.Agent, r.Name)
	fmt.Fprintf(&b, "A routine of %s, version %d.", rec.Agent, r.Version)
	if so := rec.SignedOff; so != nil {
		fmt.Fprintf(&b, " Signed off by %s on %s.", so.By, so.At.UTC().Format("2 January 2006"))
	}
	if rec.Standing == Live {
		fmt.Fprintf(&b, " It acts on its own. The agent still handles %s of the runs, to compare.", share(r.CanaryShare()))
	}
	b.WriteString("\n\n## What it decides\n\n")
	if len(r.Questions) == 0 {
		b.WriteString("It decides nothing. It acts on every run.\n")
	}
	for _, name := range r.QuestionNames() {
		q := r.Questions[name]
		fmt.Fprintf(&b, "- **%s**", name)
		if q.Instructions != "" {
			fmt.Fprintf(&b, ": %s", oneLine(q.Instructions))
		}
		b.WriteString(" " + actsWhen(q) + "\n")
	}
	if len(r.Questions) > 0 {
		b.WriteString("\nOtherwise the agent decides, with these answers in hand.\n")
	}
	b.WriteString("\n## Steps\n\n")
	for i, s := range r.Steps {
		fmt.Fprintf(&b, "%d. `%s`", i+1, s.Tool)
		if m := methodOf.FindStringSubmatch(s.Input); m != nil {
			fmt.Fprintf(&b, " (%s)", m[1])
		}
		if s.As != "" {
			fmt.Fprintf(&b, ", read as %s", s.As)
		}
		b.WriteString("\n")
	}
	if len(r.After) > 0 {
		b.WriteString("\n## Afterward\n\n")
		for _, c := range r.After {
			fmt.Fprintf(&b, "- It reads with `%s`.", c.Tool)
			if len(c.Contains) > 0 {
				fmt.Fprintf(&b, " The result holds %s.", quoted(c.Contains))
			}
			if len(c.Lacks) > 0 {
				fmt.Fprintf(&b, " The result holds no %s.", quoted(c.Lacks))
			}
			b.WriteString("\n")
		}
		b.WriteString("\nIf a check does not hold, the agent takes over.\n")
	}
	b.WriteString("\n## Where it comes from\n\n")
	if src := rec.Source; src != nil && src.Runs > 0 {
		fmt.Fprintf(&b, "Made from %d runs of %s", src.Runs, rec.Agent)
		if src.From != "" && src.To != "" {
			fmt.Fprintf(&b, " between %s and %s", src.From, src.To)
		}
		if src.ProposedBy != "" {
			fmt.Fprintf(&b, ", proposed by %s", src.ProposedBy)
		}
		b.WriteString(".")
	} else {
		b.WriteString("A person wrote it.")
	}
	if ev := rec.Evidence; ev != nil && ev.Totals.Acted > 0 {
		fmt.Fprintf(&b, " It rehearsed beside the agent and would have acted on %d runs, with %s agreed.", ev.Totals.Acted, share(float64(ev.Totals.Agreed)/float64(ev.Totals.Acted)))
	}
	if ev := rec.Evidence; ev != nil && ev.Replay != nil && ev.Replay.Total > 0 {
		fmt.Fprintf(&b, " %d of %d past runs, handled the same way.", ev.Replay.Matched, ev.Replay.Total)
	}
	b.WriteString("\n")
	return b.String()
}

// actsWhen words the band and the expected answers of a question.
func actsWhen(q fw.RoutineQuestion) string {
	sure := share(q.Act)
	if q.Type == fw.QuestionNoul {
		switch {
		case len(q.Expect) == 1:
			return fmt.Sprintf("It acts when it is at least %s sure that the answer is %s.", sure, q.Expect[0])
		default:
			return fmt.Sprintf("It acts when it is at least %s sure of yes or of no.", sure)
		}
	}
	options, _ := fw.QuestionOptions(q.Question)
	s := ""
	if len(options) > 0 {
		s = "Answers: " + strings.Join(options, ", ") + ". "
	}
	s += fmt.Sprintf("It acts when it is at least %s sure", sure)
	if len(q.Expect) > 0 {
		s += ", and only for " + strings.Join(q.Expect, ", ")
	}
	return s + "."
}

// share words a share as a percentage, such as 85%.
func share(f float64) string {
	p := f * 100
	if p == math.Trunc(p) {
		return fmt.Sprintf("%d%%", int(p))
	}
	return fmt.Sprintf("%.1f%%", p)
}

// quoted words a list of texts as code, with and before the last.
func quoted(list []string) string {
	q := make([]string, len(list))
	for i, s := range list {
		q[i] = "`" + strings.ReplaceAll(s, "`", "'") + "`"
	}
	if len(q) == 1 {
		return q[0]
	}
	return strings.Join(q[:len(q)-1], ", ") + " and " + q[len(q)-1]
}

// oneLine joins the lines of a text.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// writeRunbook writes the runbook of a live routine to the wiki of the
// agent a, as the front, when the process has a wiki. It returns the path,
// or "" without a wiki.
func writeRunbook(ctx context.Context, a *fw.Agent, rec Record) (string, error) {
	w := pages.Of(a)
	if w == nil {
		return "", nil
	}
	path := RunbookPath(rec.Agent, rec.Name())
	summary := fmt.Sprintf("signed off version %d", rec.Version())
	if rec.SignedOff != nil {
		summary += " by " + rec.SignedOff.By
	}
	if _, err := w.Write(ctx, path, Runbook(rec), pages.ByFront, summary, pages.IfAny); err != nil {
		return "", fmt.Errorf("routines: write the runbook %s: %w", path, err)
	}
	return path, nil
}
