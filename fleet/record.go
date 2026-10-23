package fleet

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// This file cuts the runs of a member to the size that a record holds
// (platform.go) and lists the profiles that a process offers.

// cutMark ends a text that the record cut.
const cutMark = "…"

// pickRuns keeps every run that is not finished, the newest FinishedRuns
// finished runs that the lists show, and the newest HiddenFinishedRuns
// finished runs that they leave out, each cut to size. runs is newest
// first, and so is the result. The second value counts the finished runs
// that it leaves out.
func pickRuns(runs []*fw.Run) ([]*fw.Run, int) {
	out := make([]*fw.Run, 0, len(runs))
	finished, hidden, omitted := 0, 0, 0
	for _, r := range runs {
		if r.State.Terminal() {
			count, limit := &finished, FinishedRuns
			if !Listed(r) {
				count, limit = &hidden, HiddenFinishedRuns
			}
			if *count >= limit {
				omitted++
				continue
			}
			*count++
		}
		out = append(out, shorten(r))
	}
	return out, omitted
}

// shorten returns a copy of a run with its texts cut to TextLimit
// and without the input of a pending call that is larger than
// PendingInputLimit.
func shorten(r *fw.Run) *fw.Run {
	c := *r
	c.Input = cut(c.Input, TextLimit)
	c.Output = cut(c.Output, TextLimit)
	c.Error = cut(c.Error, TextLimit)
	c.Member = ""
	if r.Pending != nil {
		p := *r.Pending
		if len(p.Input) > PendingInputLimit {
			p.Input = nil
		}
		c.Pending = &p
	}
	return &c
}

// cut shortens s to at most n bytes at a rune boundary. A cut text ends with
// cutMark, inside the n bytes.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n - len(cutMark)
	if i < 0 {
		i = 0
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + cutMark
}

// profiles lists the agent profiles that the principal in ctx can view. It
// leaves out the implicit "default" profile of a process that configures
// none, such as a front that only serves the view. Such a process has no
// agent to offer the fleet, so it shows only its runs, if any.
func (f *Fleet) profiles(ctx context.Context) ([]fw.AgentProfile, error) {
	all, err := f.a.ListProfiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("fleet: list the profiles: %w", err)
	}
	if _, ok := f.a.ProfileNamed("default"); ok {
		return all, nil
	}
	out := all[:0:0]
	for _, p := range all {
		if p.Name != "default" {
			out = append(out, p)
		}
	}
	return out, nil
}
