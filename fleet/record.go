package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// cutMark ends a text that the record cut.
const cutMark = "…"

// record builds the record of this process. It reads the runs and the
// profiles with no principal, so it sees all of them. The kernel returns
// copies, and record changes only its own copies.
func (f *Fleet) record() (Record, error) {
	cfg := f.a.Config()
	rec := Record{
		Version:  Version,
		Name:     f.name,
		URL:      f.url,
		Atespace: f.atespace,
		Interval: f.interval,
		Access:   Access{Mode: f.a.AccessMode()},
	}
	if rec.Access.Mode == fw.AccessModeRules {
		rec.Access.Default = "open"
		if strings.EqualFold(strings.TrimSpace(cfg.Access.Default), "closed") {
			rec.Access.Default = "closed"
		}
		rec.Access.Admins = append([]string(nil), cfg.Access.Admins...)
	}
	// A context without a principal reads every profile and every run.
	bg := context.Background()
	profiles, err := f.profiles(bg)
	if err != nil {
		return rec, err
	}
	rec.Agents = make([]RecordAgent, 0, len(profiles))
	for _, p := range profiles {
		rec.Agents = append(rec.Agents, RecordAgent{Name: p.Name, Description: p.Description, Access: p.Access, Teams: teams.OfAgent(teams.Patterns(f.a.Config()), p.Access)})
	}
	runs, err := f.a.List(bg)
	if err != nil {
		return rec, fmt.Errorf("fleet: list the runs: %w", err)
	}
	rec.Runs, rec.Omitted = pickRuns(runs)
	if err := fit(&rec); err != nil {
		return rec, err
	}
	return rec, nil
}

// pickRuns keeps every run that is not finished and the newest finished
// runs, each cut to size. runs is newest first, and so is the result. The
// second value counts the finished runs that it leaves out.
func pickRuns(runs []*fw.Run) ([]*fw.Run, int) {
	out := make([]*fw.Run, 0, len(runs))
	finished, omitted := 0, 0
	for _, r := range runs {
		if r.State.Terminal() {
			if finished >= FinishedRuns {
				omitted++
				continue
			}
			finished++
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

// fit drops runs until the record is at most RecordLimit bytes:
// the oldest finished runs first, then the oldest runs of any state. Each
// dropped run counts in Omitted.
func fit(rec *Record) error {
	for {
		b, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("fleet: encode the record: %w", err)
		}
		if len(b) <= RecordLimit || len(rec.Runs) == 0 {
			return nil
		}
		drop := len(rec.Runs) - 1
		for i := len(rec.Runs) - 1; i >= 0; i-- {
			if rec.Runs[i].State.Terminal() {
				drop = i
				break
			}
		}
		rec.Runs = append(rec.Runs[:drop:drop], rec.Runs[drop+1:]...)
		rec.Omitted++
	}
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
