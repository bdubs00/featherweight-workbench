package routines

// This file writes the runbook of a routine (docs/ARCHITECTURE.md section
// 21.5): the page agents/<agent>/routines/<name>.md of the wiki says in
// words what the routine decides, the steps with the names of their
// tools, the check afterward, where the routine comes from, who signed it
// off, and its standing now. A sign-off writes it, and every later move
// writes it again, so the page never says that a paused or retired
// routine acts. The front writes it as pages.ByFront, with no run.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pages"
)

// The words of the runbook for the runs of a routine, as the page says
// them.
const (
	runWord  = "ticket"
	runsWord = "tickets"
)

// RunbookPath returns the path of the runbook of a routine in the wiki.
func RunbookPath(agent, name string) string {
	return "agents/" + agent + "/routines/" + name + ".md"
}

// TitleOf returns the name of a routine as the page shows it: the hyphens
// as spaces and the first letter in upper case, such as "Acknowledge a
// new ticket" for acknowledge-a-new-ticket.
func TitleOf(name string) string {
	s := strings.ReplaceAll(name, "-", " ")
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// methodOf finds the method of a step, such as add_labels in
// {"method": "add_labels", ...}, which says more than the tool alone.
var methodOf = regexp.MustCompile(`"method"\s*:\s*"([A-Za-z0-9_.-]{1,64})"`)

// holes finds the actions of a text of a routine, such as {{ .type }}.
var holes = regexp.MustCompile(`\{\{-?\s*(.*?)\s*-?\}\}`)

// field finds the field that an action reads, such as type in .type.
var field = regexp.MustCompile(`^\.([a-z][a-z0-9_]*)$`)

// Runbook renders the runbook of a routine as Markdown.
func Runbook(rec Record) string {
	r := rec.Routine
	title := TitleOf(r.Name)
	var b strings.Builder
	fmt.Fprintf(&b, "---\ntitle: %s\ntags: routine, %s\n---\n\n# %s\n\n", title, rec.Agent, title)
	fmt.Fprintf(&b, "A routine of %s, version %d. %s\n", rec.Agent, r.Version, standingLine(rec))

	b.WriteString("\n## What it decides\n\n")
	if len(r.Questions) == 0 {
		fmt.Fprintf(&b, "It decides nothing. It acts on every %s.\n", runWord)
	}
	for _, name := range r.QuestionNames() {
		q := r.Questions[name]
		fmt.Fprintf(&b, "- **%s**", TitleOf(strings.ReplaceAll(name, "_", " ")))
		if text := oneLine(q.Instructions); text != "" {
			fmt.Fprintf(&b, ": %s", sentence(text))
		} else {
			b.WriteString(".")
		}
		b.WriteString(" " + actsWhen(q) + "\n")
	}
	if len(r.Questions) > 0 {
		b.WriteString("\nOtherwise the agent decides, with these answers in hand.\n")
	}

	b.WriteString("\n## Steps\n\n")
	for i, s := range r.Steps {
		fmt.Fprintf(&b, "%d. It calls `%s`", i+1, s.Tool)
		if m := methodOf.FindStringSubmatch(s.Input); m != nil {
			fmt.Fprintf(&b, " with %s", m[1])
		}
		if s.As != "" {
			fmt.Fprintf(&b, ", and keeps the result as the %s", words(s.As))
		}
		b.WriteString(".\n")
	}

	if len(r.After) > 0 {
		b.WriteString("\n## Afterward\n\n")
		for _, c := range r.After {
			parts := []string{fmt.Sprintf("It reads with `%s`", c.Tool)}
			if len(c.Contains) > 0 {
				parts = append(parts, "The result holds "+texts(c.Contains, r))
			}
			if len(c.Lacks) > 0 {
				parts = append(parts, "The result holds no "+texts(c.Lacks, r))
			}
			b.WriteString("- " + strings.Join(parts, ". ") + ".\n")
		}
		b.WriteString("\nIf a check does not hold, the agent takes over.\n")
	}

	b.WriteString("\n## Where it comes from\n\n")
	var from []string
	if src := rec.Source; src != nil && src.Runs > 0 {
		s := fmt.Sprintf("Made from %s %s that %s handled", count(src.Runs), plural(src.Runs), rec.Agent)
		if src.From != "" && src.To != "" {
			s += fmt.Sprintf(" between %s and %s", day(src.From), day(src.To))
		}
		if src.ProposedBy != "" {
			s += ", proposed by " + src.ProposedBy
		}
		from = append(from, s)
	} else {
		from = append(from, "A person wrote it")
	}
	if ev := rec.Evidence; ev != nil && ev.Totals.Acted > 0 {
		from = append(from, fmt.Sprintf("It rehearsed beside the agent and would have acted on %s %s, with %s agreed", count(ev.Totals.Acted), plural(ev.Totals.Acted), share(float64(ev.Totals.Agreed)/float64(ev.Totals.Acted))))
	}
	if ev := rec.Evidence; ev != nil && ev.Replay != nil && ev.Replay.Total > 0 {
		from = append(from, fmt.Sprintf("%s of %s past %s, handled the same way", count(ev.Replay.Matched), count(ev.Replay.Total), plural(ev.Replay.Total)))
	}
	b.WriteString(strings.Join(from, ". ") + ".\n")
	return b.String()
}

// standingLine says what the routine does now, in one or two sentences.
func standingLine(rec Record) string {
	signed := ""
	if so := rec.SignedOff; so != nil {
		signed = fmt.Sprintf("Signed off by %s on %s. ", so.By, so.At.UTC().Format("2 January 2006"))
	}
	switch rec.Standing {
	case Live:
		return signed + fmt.Sprintf("It is live: it acts on its own, and the agent still handles %s of the %s, to compare.", share(rec.Routine.CanaryShare()), runsWord)
	case Rehearsing:
		return "It is rehearsing: it decides beside the agent and does not act."
	case Paused:
		return "It is paused: it does not run, and the agent handles every " + runWord + "."
	case Retired:
		return "It is retired: it no longer runs."
	}
	return "It is proposed: it does not run yet."
}

// texts words the texts of a check, each as code, with the holes of the
// routine as an ellipsis and their words after it, such as `type/…`, with
// the type it decided.
func texts(list []string, r fw.Routine) string {
	out := make([]string, len(list))
	for i, s := range list {
		var said []string
		code := holes.ReplaceAllStringFunc(s, func(h string) string {
			said = append(said, holeWords(holes.FindStringSubmatch(h)[1], r))
			return "…"
		})
		out[i] = "`" + strings.ReplaceAll(code, "`", "'") + "`"
		if len(said) > 0 {
			out[i] += ", with " + and(said)
		}
	}
	return and(out)
}

// holeWords words one action of a text: a question as what the routine
// decided, a step result as what it read, and anything else as a value of
// the run.
func holeWords(action string, r fw.Routine) string {
	if m := field.FindStringSubmatch(action); m != nil {
		if _, ok := r.Questions[m[1]]; ok {
			return "the " + words(m[1]) + " it decided"
		}
		for _, s := range r.Steps {
			if s.As == m[1] {
				return "the " + words(m[1]) + " it read"
			}
		}
	}
	return "a value of the " + runWord
}

// actsWhen words the band and the expected answers of a question.
func actsWhen(q fw.RoutineQuestion) string {
	sure := share(q.Act)
	if q.Type == fw.QuestionNoul {
		if len(q.Expect) == 1 {
			return fmt.Sprintf("It acts when it is at least %s sure that the answer is %s.", sure, q.Expect[0])
		}
		return fmt.Sprintf("It acts when it is at least %s sure of yes or of no.", sure)
	}
	options, _ := fw.QuestionOptions(q.Question)
	s := ""
	if len(options) > 0 {
		s = "Answers: " + strings.Join(options, ", ") + ". "
	}
	s += fmt.Sprintf("It acts when it is at least %s sure", sure)
	if len(q.Expect) > 0 {
		s += ", and only for " + or(q.Expect)
	}
	return s + "."
}

// share words a share as a percentage, such as 85% or 97.6%.
func share(f float64) string {
	p := f * 100
	if p == math.Trunc(p) {
		return fmt.Sprintf("%d%%", int(p))
	}
	return strconv.FormatFloat(math.Round(p*10)/10, 'f', -1, 64) + "%"
}

// count words a count with a comma between thousands, such as 1,480.
func count(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// plural words the runs of a count.
func plural(n int) string {
	if n == 1 {
		return runWord
	}
	return runsWord
}

// day words a day such as 2026-09-14 as 14 September 2026, or leaves it.
func day(s string) string {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return s
	}
	return t.Format("2 January 2006")
}

// words turns a name such as first_label into words.
func words(name string) string { return strings.ReplaceAll(name, "_", " ") }

// sentence ends a text with one period.
func sentence(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), ".")
	if strings.HasSuffix(s, "?") || strings.HasSuffix(s, "!") {
		return s
	}
	return s + "."
}

// and joins words with commas and "and" before the last.
func and(list []string) string { return join(list, "and") }

// or joins words with commas and "or" before the last.
func or(list []string) string { return join(list, "or") }

func join(list []string, word string) string {
	switch len(list) {
	case 0:
		return ""
	case 1:
		return list[0]
	}
	return strings.Join(list[:len(list)-1], ", ") + " " + word + " " + list[len(list)-1]
}

// oneLine joins the lines of a text.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// writeRunbook writes the runbook of a routine to the wiki of the agent a,
// as the front, when the process has a wiki. A sign-off writes it, and any
// other move writes it only when the page exists, so a routine that was
// never signed off has no page. It returns the path, or "" when it wrote
// nothing.
func writeRunbook(ctx context.Context, a *fw.Agent, rec Record) (string, error) {
	w := pages.Of(a)
	if w == nil {
		return "", nil
	}
	path := RunbookPath(rec.Agent, rec.Name())
	if rec.Standing != Live {
		if _, err := w.Read(ctx, path, 0); errors.Is(err, pages.ErrNotFound) {
			return "", nil
		} else if err != nil {
			return "", fmt.Errorf("routines: read the runbook %s: %w", path, err)
		}
	}
	summary := fmt.Sprintf("%s, version %d", rec.Standing, rec.Version())
	if rec.Standing == Live && rec.SignedOff != nil {
		summary = fmt.Sprintf("signed off version %d by %s", rec.Version(), rec.SignedOff.By)
	}
	if _, err := w.Write(ctx, path, Runbook(rec), pages.ByFront, summary, pages.IfAny); err != nil {
		return "", fmt.Errorf("routines: write the runbook %s: %w", path, err)
	}
	return path, nil
}
