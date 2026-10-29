package knowhow

// This file finds the candidates of the after-work review by code, with no
// model (docs/KNOWHOW.md section 7.1). A pass reads the finished runs of
// the archive behind a durable cursor, the entry mark.candidates: the
// time of the newest change of the archive that it read, and the runs of
// that time. A run whose summary changed later, such as a run with a
// receipt that came after its end, is read again, and a candidate counts
// each run once. A candidate is one entry, cand.<agent>.<id>, whose id is
// a hash of its kind, its scope, and what it found, so the same finding of
// two runs is one candidate.
//
// The kinds:
//   - gotcha: a tool call that failed, and a later call of the same tool
//     in the same run that passed with another input
//   - place: the same read call, tool and input, in two runs or more of
//     an agent, with the same result
//   - outcome: a run with a receipt, a verdict of its grader, or a thumbs
//     up or down
//   - recheck: a note that went out of date
//
// A candidate belongs to the scope of the agent, because the runs of the
// agent read the notes of that scope: the review keeps its notes where
// the next run finds them. So the same finding in runs of two principals
// is one candidate. A recheck keeps the scope of its note. A candidate
// gets the source run:<id> of each of its runs, and the source
// page:<path> with the hash of each page of the wiki that the call of a
// place read, or that a run of an outcome read, so its note goes out of
// date when the page changes. The text
// of a candidate is untrusted when it came from a block found, event, or
// source, from a tool that reads the open world (an MCP tool or a peer,
// bash, and http), or from a message of another principal. An untrusted
// candidate waits for two runs with different triggers before the review
// sees it. A candidate drops 14 days after it was found.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// The kinds of a candidate.
const (
	KindGotcha  = "gotcha"
	KindPlace   = "place"
	KindOutcome = "outcome"
	KindRecheck = "recheck"
)

// The origins of a candidate.
const (
	OriginTrusted   = "trusted"
	OriginUntrusted = "untrusted"
)

// The states of a candidate.
const (
	CandGathering = "gathering"
	CandOpen      = "open"
	CandKept      = "kept"
	CandSkipped   = "skipped"
)

// Bounds of the candidates.
const (
	CandidateDays = 14
	maxCandRuns   = 8
	evidenceCut   = 1024
	inputCut      = 512
	pageCut       = 4096
	passBatch     = 200
	settle        = 2 * time.Minute
	maxPlaces     = 500
	readsPerRun   = 8
	// MetaReview is the metadata key of a run of the after-work review: the
	// agent whose work it reviews.
	MetaReview = "workbench.knowhow.review"
)

// Candidate is one thing that the after-work review can turn into a note.
type Candidate struct {
	ID       string         `json:"id"`
	Agent    string         `json:"agent"`
	Kind     string         `json:"kind"`
	Scope    Scope          `json:"scope"`
	Origin   string         `json:"origin"`
	State    string         `json:"state"`
	Summary  string         `json:"summary"`
	Note     string         `json:"note,omitempty"`
	Evidence []Evidence     `json:"evidence"`
	Runs     []CandidateRun `json:"runs"`
	Sources  []Source       `json:"sources"`
	First    time.Time      `json:"first"`
	Updated  time.Time      `json:"updated"`
	Decided  *Decision      `json:"decided,omitempty"`

	// reopen makes a new run with new evidence open a decided candidate
	// again (add). The store keeps no such field.
	reopen bool
}

// CandidateRun is one run of a candidate, with its trigger.
type CandidateRun struct {
	Run     string    `json:"run"`
	Trigger string    `json:"trigger,omitempty"`
	At      time.Time `json:"at"`
}

// Evidence is the text of a candidate from one run. It is data that a run
// recorded, never an instruction.
type Evidence struct {
	Run  string `json:"run,omitempty"`
	Text string `json:"text"`
}

// Decision is what the review decided on a candidate.
type Decision struct {
	Op   string    `json:"op"`
	Note string    `json:"note,omitempty"`
	By   string    `json:"by"`
	Run  string    `json:"run,omitempty"`
	At   time.Time `json:"at"`
}

// ready reports whether the review can see the candidate: a recheck at
// once, a trusted candidate with one run, and an untrusted one with two
// runs of different triggers.
func (c Candidate) ready() bool {
	switch {
	case c.Kind == KindRecheck:
		return true
	case c.Origin != OriginUntrusted:
		return len(c.Runs) > 0
	}
	triggers := map[string]bool{}
	for _, r := range c.Runs {
		triggers[r.Trigger] = true
	}
	return len(triggers) >= 2
}

// runIDs returns the runs of a candidate.
func (c Candidate) runIDs() []string {
	out := make([]string, 0, len(c.Runs))
	for _, r := range c.Runs {
		out = append(out, r.Run)
	}
	return out
}

// candKey is the key of the candidate of the id.
func candKey(agent, id string) string {
	h, _, _ := strings.Cut(id, "-")
	return "cand." + agentKey(agent) + "." + h
}

// candID is the id of a candidate: a hash of its kind, its agent, its
// scope, and what it found, then a dash and the agent in base64url, so a
// tool finds the candidate by its id alone.
func candID(kind, agent string, sc Scope, sig string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + agent + "\x00" + sc.Kind + "\x00" + sc.Principal + "\x00" + sig))
	return hex.EncodeToString(sum[:10]) + "-" + agentKey(agent)
}

// candAgent returns the agent of the id of a candidate.
func candAgent(id string) (string, error) {
	h, enc, ok := strings.Cut(id, "-")
	if !ok || len(h) != 20 {
		return "", fmt.Errorf("%w: %q is not the id of a candidate", ErrNotFound, id)
	}
	agent, err := decodeToken(enc)
	if err != nil || CheckAgent(agent) != nil {
		return "", fmt.Errorf("%w: %q is not the id of a candidate", ErrNotFound, id)
	}
	return agent, nil
}

// add adds one run and its evidence to the candidate c, or creates it. A
// run that the candidate holds changes only the text of its evidence, such
// as a thumb with a note that came after the run was read. A decided
// candidate stays decided, unless c.reopen: then a new run with new
// evidence opens it again, with that run alone, because the review decided
// on the runs before it.
func (s *Service) add(ctx context.Context, c Candidate, run *CandidateRun, ev Evidence) error {
	now := s.now().UTC()
	wrote, _, _, err := s.modify(ctx, candKey(c.Agent, c.ID), func(value []byte, exists bool, _ uint64) ([]byte, error) {
		cur := c
		if exists {
			// A fresh value, so the decode shares no slice with c.
			var got Candidate
			if err := json.Unmarshal(value, &got); err != nil {
				exists = false
			} else {
				cur = got
			}
		}
		if exists && (cur.State == CandKept || cur.State == CandSkipped) {
			if !c.reopen || run == nil || ev.Text == "" || cur.holds(run.Run, ev.Text) {
				return nil, nil
			}
			// The 14 days of the candidate start again with this run.
			exists = false
			cur.Decided, cur.Origin, cur.Sources = nil, "", nil
		}
		if !exists {
			cur.First, cur.Runs, cur.Evidence, cur.State = now, nil, nil, CandGathering
		}
		if run != nil {
			if slices.ContainsFunc(cur.Runs, func(r CandidateRun) bool { return r.Run == run.Run }) {
				i := slices.IndexFunc(cur.Evidence, func(e Evidence) bool { return e.Run == run.Run })
				if ev.Text == "" || i < 0 || cur.Evidence[i].Text == ev.Text {
					return nil, nil
				}
				cur.Evidence[i].Text = ev.Text
				cur.Updated = now
				return encode(cur)
			}
			if len(cur.Runs) >= maxCandRuns {
				return nil, nil
			}
			cur.Runs = append(cur.Runs, *run)
			cur.Sources = appendSource(cur.Sources, Source{Ref: "run:" + run.Run})
		} else if exists {
			return nil, nil
		}
		for _, src := range c.Sources {
			cur.Sources = appendSource(cur.Sources, src)
		}
		if ev.Text != "" && len(cur.Evidence) < maxCandRuns {
			cur.Evidence = append(cur.Evidence, ev)
		}
		if c.Origin == OriginUntrusted {
			cur.Origin = OriginUntrusted
		}
		if cur.Origin == "" {
			cur.Origin = OriginTrusted
		}
		if cur.State == CandGathering && cur.ready() {
			cur.State = CandOpen
		}
		cur.Updated = now
		return encode(cur)
	})
	if err == nil && wrote {
		emitChange(c.Agent)
	}
	return err
}

// holds reports whether the candidate holds the run with this text of
// evidence.
func (c Candidate) holds(run, text string) bool {
	return slices.ContainsFunc(c.Evidence, func(e Evidence) bool { return e.Run == run && e.Text == text })
}

// appendSource adds a source to a list. A page that the list names already
// takes the version of the newer run, so a note never names two versions
// of one page.
func appendSource(list []Source, src Source) []Source {
	if slices.Contains(list, src) {
		return list
	}
	if strings.HasPrefix(src.Ref, "page:") {
		for i, old := range list {
			if old.Ref == src.Ref {
				out := slices.Clone(list)
				out[i] = src
				return out
			}
		}
	}
	if len(list) >= MaxSources {
		return list
	}
	return append(list, src)
}

// Candidates returns the candidates of an agent in the states that keep
// allows, the oldest first. A nil keep keeps every candidate.
func (s *Service) Candidates(ctx context.Context, agent string, keep func(Candidate) bool) ([]Candidate, error) {
	if err := CheckAgent(agent); err != nil {
		return nil, err
	}
	keys, err := bucket.EntriesUnder(ctx, s.b, "cand."+agentKey(agent))
	if err != nil {
		return nil, fmt.Errorf("knowhow: list the candidates: %w", err)
	}
	out := []Candidate{}
	for _, k := range keys {
		got, err := s.b.Get(ctx, k.Key)
		if err != nil {
			continue
		}
		var c Candidate
		if json.Unmarshal(got.Value, &c) != nil {
			continue
		}
		if keep == nil || keep(c) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].First.Before(out[j].First) })
	return out, nil
}

// candidate reads one candidate by its id, which names its agent.
func (s *Service) candidate(ctx context.Context, agent, id string) (Candidate, uint64, error) {
	got, err := s.b.Get(ctx, candKey(agent, id))
	if errors.Is(err, bucket.ErrNotFound) {
		return Candidate{}, 0, fmt.Errorf("%w: no candidate of %s has the id %s", ErrNotFound, agent, id)
	}
	if err != nil {
		return Candidate{}, 0, err
	}
	var c Candidate
	if err := json.Unmarshal(got.Value, &c); err != nil {
		return Candidate{}, 0, err
	}
	return c, got.Revision, nil
}

// dropCandidates removes the candidates found more than CandidateDays ago.
func (s *Service) dropCandidates(ctx context.Context) (int, error) {
	keys, err := bucket.EntriesUnder(ctx, s.b, "cand")
	if err != nil {
		return 0, err
	}
	cut := s.now().UTC().AddDate(0, 0, -CandidateDays)
	n := 0
	for _, k := range keys {
		got, err := s.b.Get(ctx, k.Key)
		if err != nil {
			continue
		}
		var c Candidate
		if json.Unmarshal(got.Value, &c) == nil && !c.First.Before(cut) {
			continue
		}
		if err := s.b.Delete(ctx, k.Key, got.Revision); err == nil {
			n++
		}
	}
	return n, nil
}

// cursor is the place of the pass of the candidates in the archive: the
// newest change that it read, and the runs of that time.
type cursor struct {
	At  time.Time `json:"at"`
	IDs []string  `json:"ids,omitempty"`
}

const cursorKey = "mark.candidates"

// CandidatePass reads the finished runs of the archive that changed since
// the cursor, at most passBatch of them, finds their candidates, and moves
// the cursor. A run whose summary changed within the last two minutes
// waits for the next pass, so its last events arrive first. Two replicas
// can read the same runs; a candidate counts each run once. It returns the
// number of runs that it read.
func (s *Service) CandidatePass(ctx context.Context) (int, error) {
	if s.arch == nil {
		return 0, nil
	}
	var cur cursor
	got, err := s.b.Get(ctx, cursorKey)
	switch {
	case err == nil:
		_ = json.Unmarshal(got.Value, &cur)
	case !errors.Is(err, bucket.ErrNotFound):
		return 0, err
	}
	list := s.archive(ctx)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Updated.Before(list[j].Updated) })
	cutoff := s.now().UTC().Add(-settle)
	var todo []fleet.ArchiveEntry
	for _, e := range list {
		if e.Run == nil || !e.Run.State.Terminal() || e.Updated.After(cutoff) || e.Updated.Before(cur.At) {
			continue
		}
		if e.Updated.Equal(cur.At) && slices.Contains(cur.IDs, e.Run.ID) {
			continue
		}
		todo = append(todo, e)
		if len(todo) >= passBatch {
			break
		}
	}
	if len(todo) == 0 {
		return 0, nil
	}
	next := cur
	for _, e := range todo {
		if ctx.Err() != nil {
			break
		}
		if err := s.learnFrom(ctx, e); err != nil {
			s.log.Warn("knowhow: a run gave no candidates; the next change of the run reads it again", "run", e.Run.ID, "error", err)
		}
		if !e.Updated.Equal(next.At) {
			next = cursor{At: e.Updated}
		}
		next.IDs = append(next.IDs, e.Run.ID)
	}
	raw, err := encode(next)
	if err != nil {
		return len(todo), err
	}
	if got.Revision == 0 {
		_, err = s.b.Create(ctx, cursorKey, raw)
	} else {
		_, err = s.b.Update(ctx, cursorKey, raw, got.Revision)
	}
	if errors.Is(err, bucket.ErrConflict) {
		// Another replica moved the cursor; both read the same runs.
		err = nil
	}
	return len(todo), err
}

// maxPassRounds bounds the passes of a start of the review by a person,
// each of at most passBatch runs.
const maxPassRounds = 10

// readUpToNow reads the finished runs of the archive up to now for a
// person who starts the review of an agent: the pass behind the cursor
// until no run waits, and then the runs of the agent that changed within
// the last two minutes, which the pass leaves for later. Those it reads
// without the cursor, which another replica can move only past runs that
// it knows: the pass reads them again once they settle, and a candidate
// counts each run once.
func (s *Service) readUpToNow(ctx context.Context, agent string) error {
	// The other replica can have applied a thumb of a moment ago, and this
	// one reads the changes of the archive every few seconds alone, so
	// Review now two seconds after a thumbs-down missed it (live walk).
	if r, ok := s.arch.(refresher); ok {
		r.Refresh(ctx)
	}
	for range maxPassRounds {
		n, err := s.CandidatePass(ctx)
		if err != nil {
			return err
		}
		if n < passBatch {
			break
		}
	}
	cutoff := s.now().UTC().Add(-settle)
	var errs []error
	for _, e := range s.archive(ctx) {
		if ctx.Err() != nil {
			break
		}
		if e.Run == nil || e.Run.Agent != agent || !e.Run.State.Terminal() || !e.Updated.After(cutoff) {
			continue
		}
		errs = append(errs, s.learnFrom(ctx, e))
	}
	return errors.Join(errs...)
}

// call is one tool call of the agent in a run, with its result.
type call struct {
	tool    string
	source  string
	input   json.RawMessage
	result  string
	isError bool
}

// callsOf reads the tool calls of the agent from the events of a run. The
// calls of the routine runner are not calls of the agent.
func callsOf(events []fw.Event) []call {
	var out []call
	byID := map[string]int{}
	for _, e := range events {
		switch e.Type {
		case fw.EvToolRequested:
			var d fw.ToolRequestedData
			if e.Decode(&d) != nil || d.Routine != "" {
				continue
			}
			byID[d.CallID] = len(out)
			out = append(out, call{tool: d.Tool, source: d.Source, input: d.Input})
		case fw.EvToolResponded:
			var d fw.ToolRespondedData
			if e.Decode(&d) != nil {
				continue
			}
			if i, ok := byID[d.CallID]; ok {
				out[i].result = fw.TextOf(d.Content)
				out[i].isError = d.IsError || (d.ExitCode != nil && *d.ExitCode != 0)
			}
		}
	}
	return out
}

// openWorld names the built-in tools that read the open world.
var openWorld = map[string]bool{"bash": true, "http": true, "http_request": true, "fetch": true, "web_fetch": true, "web_search": true}

// untrustedTool reports whether the text of a call came from the open
// world: an MCP tool or a peer of another service, or a built-in tool
// that reads the open world. The stream carries no annotations of a tool,
// so the source of the call and its name decide.
func untrustedTool(c call) bool {
	if c.source != "" && c.source != "builtin" {
		return true
	}
	name := c.tool
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return true
	}
	return openWorld[name]
}

// pageHead matches the first line of a result of pages_read: the path, the
// version, and the hash of the page that the call read.
var pageHead = regexp.MustCompile(`^Page (\S.*?), version [0-9]+, hash ([0-9a-f]{64}),`)

// maxPagesRead bounds the pages of one run that an outcome names.
const maxPagesRead = 8

// pageOf returns the source of a call that read the newest version of a
// page of the wiki (pages_read of the front), with the hash that its
// result names as the version. A call of an older version gives none,
// because a note must name the page as it is.
func pageOf(c call) (Source, bool) {
	name := c.tool
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	if name != "pages_read" || c.isError {
		return Source{}, false
	}
	var in struct {
		Path    string          `json:"path"`
		Version json.RawMessage `json:"version"`
	}
	if json.Unmarshal(c.input, &in) != nil || in.Path == "" || (len(in.Version) > 0 && string(in.Version) != "null") {
		return Source{}, false
	}
	m := pageHead.FindStringSubmatch(c.result)
	if m == nil || m[1] != in.Path {
		return Source{}, false
	}
	return Source{Ref: "page:" + in.Path, Version: m[2]}, true
}

// pagesRead returns the sources of the pages of the wiki that the calls of
// a run read, at most maxPagesRead, with the version of the newest read.
func pagesRead(calls []call) []Source {
	var out []Source
	for _, c := range calls {
		src, ok := pageOf(c)
		if !ok {
			continue
		}
		if !slices.ContainsFunc(out, func(x Source) bool { return x.Ref == src.Ref }) && len(out) >= maxPagesRead {
			continue
		}
		out = appendSource(out, src)
	}
	return out
}

// readVerbs are the words that name a call that reads.
var readVerbs = []string{"read", "get", "list", "search", "find", "view", "query", "show", "fetch", "lookup"}

// readCall reports whether a call reads: its name, or the method of its
// input, says read, get, list, search, and the like. The stream carries
// no annotation readOnly, so the words decide.
func readCall(c call) bool {
	name := strings.ToLower(c.tool)
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	for _, w := range strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' }) {
		if slices.Contains(readVerbs, w) {
			return true
		}
	}
	var in struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(c.input, &in) == nil && in.Method != "" {
		m := strings.ToLower(in.Method)
		for _, w := range readVerbs {
			if strings.HasPrefix(m, w) {
				return true
			}
		}
	}
	return false
}

// canonical returns the input of a call as canonical JSON.
func canonical(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(bytes.TrimSpace(raw))
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// digits matches the numbers of an error, which the signature of a gotcha
// leaves out, so the same failure of two runs is one candidate.
var digits = regexp.MustCompile(`[0-9]+`)

// errorSignature is the first line of an error with its numbers folded.
func errorSignature(text string) string {
	line := strings.TrimSpace(text)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return digits.ReplaceAllString(cut(line, 200), "#")
}

func short(h string) string {
	sum := sha256.Sum256([]byte(h))
	return hex.EncodeToString(sum[:8])
}

// learnFrom finds the candidates of one finished run.
func (s *Service) learnFrom(ctx context.Context, e fleet.ArchiveEntry) error {
	r := e.Run
	if r.Principal == "" || r.Metadata[MetaReview] != "" || (s.cfg.Review.Agent != "" && r.Agent == s.cfg.Review.Agent) || CheckAgent(r.Agent) != nil {
		return nil
	}
	f, events, err := s.readRun(ctx, e)
	if err != nil {
		return err
	}
	// The candidate belongs to the agent, whose notes every run reads.
	sc := Scope{Kind: ScopeAgent}
	at := e.Ended
	if at.IsZero() {
		at = r.UpdatedAt
	}
	run := CandidateRun{Run: r.ID, Trigger: f.Trigger, At: at}
	origin := func(untrusted bool) string {
		if untrusted || f.Untrusted {
			return OriginUntrusted
		}
		return OriginTrusted
	}
	calls := callsOf(events)
	var errs []error
	// gotcha: a failed call, and a later call of the same tool that passed
	// with another input.
	seen := map[string]bool{}
	for i, c := range calls {
		if !c.isError {
			continue
		}
		sig := c.tool + "\x00" + errorSignature(c.result)
		if seen[sig] {
			continue
		}
		for _, later := range calls[i+1:] {
			if later.tool != c.tool || later.isError || canonical(later.input) == canonical(c.input) {
				continue
			}
			seen[sig] = true
			text := fmt.Sprintf("The call %s failed with this result:\n%s\nIts input was:\n%s\nA later call of %s in the same run passed with this input:\n%s",
				c.tool, cut(c.result, evidenceCut), cut(canonical(c.input), inputCut), c.tool, cut(canonical(later.input), inputCut))
			cand := Candidate{ID: candID(KindGotcha, r.Agent, sc, sig), Agent: r.Agent, Kind: KindGotcha, Scope: sc,
				Origin:  origin(untrustedTool(c) || untrustedTool(later)),
				Summary: fmt.Sprintf("%s failed, and a later call with another input passed: %s", c.tool, cut(errorSignature(c.result), 160))}
			errs = append(errs, s.add(ctx, cand, &run, Evidence{Run: r.ID, Text: text}))
			break
		}
	}
	// place: the same read call with the same result in two runs.
	errs = append(errs, s.places(ctx, r.Agent, sc, run, f, calls))
	// outcome: what a system or a person recorded about the run.
	if polarity, what := outcomeOf(f); polarity != "" {
		duty := dutyOf(r)
		label := "runs of the Duty " + duty
		if duty == "" {
			label = "runs without a Duty"
		}
		text := fmt.Sprintf("What was recorded: %s.\n%sThe input of the run was:\n%s\nThe answer of the run was:\n%s", what, thumbsText(f.Thumbs), cut(r.Input, evidenceCut), cut(r.Output, evidenceCut))
		// A new run that went badly, or that a person gave a thumb, opens a
		// decided outcome again. A run that only a check of the system
		// verified does not, so a Duty with checks does not start a review
		// every day.
		cand := Candidate{ID: candID(KindOutcome, r.Agent, sc, duty+"\x00"+polarity), Agent: r.Agent, Kind: KindOutcome, Scope: sc, Origin: origin(false),
			Summary: fmt.Sprintf("%s that went %s", label, polarity), Sources: pagesRead(calls), reopen: polarity == "badly" || f.Up || f.Down}
		errs = append(errs, s.add(ctx, cand, &run, Evidence{Run: r.ID, Text: text}))
	}
	return errors.Join(errs...)
}

// thumbsText words the notes that people wrote with a thumb on a run, for
// the evidence of an outcome, each on lines of its own.
func thumbsText(thumbs []thumbNote) string {
	var b strings.Builder
	for _, t := range thumbs {
		fmt.Fprintf(&b, "A person wrote with a thumbs %s:\n%s\n", t.Reaction, cut(t.Note, evidenceCut))
	}
	return b.String()
}

// outcomeOf returns well or badly for a run that a system or a person
// judged, with what was recorded, or "" for a run without a judgment.
func outcomeOf(f runFacts) (string, string) {
	var said []string
	if f.Verification != "" {
		said = append(said, "a receipt of "+f.Verification)
	}
	if f.Grade != "" && f.Grade != fw.GradeSkipped && f.Grade != fw.GradeUnparsed {
		said = append(said, "a verdict of the grader of "+f.Grade)
	}
	if f.Checked {
		said = append(said, "a definition of done that passed")
	}
	if f.Up {
		said = append(said, "a thumbs up")
	}
	if f.Down {
		said = append(said, "a thumbs down")
	}
	if len(said) == 0 {
		return "", ""
	}
	if f.rejected() || f.Down || f.failedGrade() {
		return "badly", strings.Join(said, ", ")
	}
	if f.verified() {
		return "well", strings.Join(said, ", ")
	}
	return "", ""
}

// placeSeen is one read call that the runs of a scope made, with the runs
// that made it.
type placeSeen struct {
	Tool      string         `json:"tool"`
	Input     string         `json:"input"`
	Result    string         `json:"result"`
	Page      *Source        `json:"page,omitempty"`
	Untrusted bool           `json:"untrusted,omitempty"`
	Runs      []CandidateRun `json:"runs"`
	Last      time.Time      `json:"last"`
}

func placesKey(agent string, sc Scope) string {
	return "places." + agentKey(agent) + "." + scopeKey(sc)
}

// places keeps the read calls of a run in the entry of its scope, and
// opens a candidate place for each call that a second run made with the
// same result. The entry keeps at most maxPlaces calls, the newest ones.
func (s *Service) places(ctx context.Context, agent string, sc Scope, run CandidateRun, f runFacts, calls []call) error {
	type read struct {
		sig  string
		seen placeSeen
	}
	var reads []read
	got := map[string]bool{}
	for _, c := range calls {
		if c.isError || strings.TrimSpace(c.result) == "" || !readCall(c) {
			continue
		}
		in := canonical(c.input)
		sig := short(c.tool + "\x00" + in + "\x00" + c.result)
		if got[sig] {
			continue
		}
		got[sig] = true
		seen := placeSeen{Tool: c.tool, Input: cut(in, inputCut), Result: cut(c.result, evidenceCut), Untrusted: untrustedTool(c) || f.Untrusted}
		if src, ok := pageOf(c); ok {
			seen.Page = &src
		}
		reads = append(reads, read{sig: sig, seen: seen})
		if len(reads) >= readsPerRun {
			break
		}
	}
	if len(reads) == 0 {
		return nil
	}
	now := s.now().UTC()
	var open []read
	_, _, _, err := s.modify(ctx, placesKey(agent, sc), func(value []byte, exists bool, _ uint64) ([]byte, error) {
		open = nil
		m := map[string]placeSeen{}
		if exists {
			_ = json.Unmarshal(value, &m)
		}
		changed := false
		for _, rd := range reads {
			p, ok := m[rd.sig]
			if !ok {
				p = rd.seen
			}
			if slices.ContainsFunc(p.Runs, func(x CandidateRun) bool { return x.Run == run.Run }) {
				continue
			}
			p.Runs = append(p.Runs, run)
			if len(p.Runs) > 3 {
				p.Runs = p.Runs[len(p.Runs)-3:]
			}
			p.Untrusted = p.Untrusted || rd.seen.Untrusted
			p.Last = now
			m[rd.sig] = p
			changed = true
			if len(p.Runs) >= 2 {
				open = append(open, read{sig: rd.sig, seen: p})
			}
		}
		if !changed {
			return nil, nil
		}
		if len(m) > maxPlaces {
			type aged struct {
				sig  string
				last time.Time
			}
			var all []aged
			for sig, p := range m {
				all = append(all, aged{sig, p.Last})
			}
			sort.Slice(all, func(i, j int) bool { return all[i].last.Before(all[j].last) })
			for _, x := range all[:len(m)-maxPlaces] {
				delete(m, x.sig)
			}
		}
		return encode(m)
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, rd := range open {
		p := rd.seen
		origin := OriginTrusted
		if p.Untrusted {
			origin = OriginUntrusted
		}
		text := fmt.Sprintf("The call %s with this input:\n%s\ngave the same result in %d runs:\n%s", p.Tool, p.Input, len(p.Runs), p.Result)
		cand := Candidate{ID: candID(KindPlace, agent, sc, rd.sig), Agent: agent, Kind: KindPlace, Scope: sc, Origin: origin,
			Summary: fmt.Sprintf("%s gave the same result in several runs", p.Tool)}
		if p.Page != nil {
			cand.Sources = []Source{*p.Page}
		}
		for i := range p.Runs {
			ev := Evidence{}
			if i == 0 {
				ev = Evidence{Run: p.Runs[i].Run, Text: text}
			}
			errs = append(errs, s.add(ctx, cand, &p.Runs[i], ev))
		}
	}
	return errors.Join(errs...)
}

// recheck opens a candidate recheck for a note that went out of date, with
// the sources of the note at the new version of the source that changed.
// The candidate keeps the scope of its note. A note that a person wrote
// changes only by a person, so it gets no candidate.
func (s *Service) recheck(ctx context.Context, n Note, ref, version string) error {
	if n.ByPerson() {
		return nil
	}
	full, err := s.get(ctx, noteKey(n.Agent, n.Scope, n.Name))
	if err != nil {
		return err
	}
	var sources []Source
	for _, src := range full.Sources {
		if src.Ref == ref {
			src.Version = version
		}
		sources = append(sources, src)
	}
	origin := full.Origin
	if origin == "" {
		origin = OriginTrusted
	}
	c := Candidate{ID: candID(KindRecheck, n.Agent, n.Scope, n.Name+"\x00"+ref+"\x00"+version), Agent: n.Agent, Kind: KindRecheck, Scope: n.Scope,
		Origin: origin, Note: n.Name, Sources: sources, Summary: fmt.Sprintf("The note %s is out of date: %s changed", n.Name, ref)}
	return s.add(ctx, c, nil, Evidence{Text: s.recheckEvidence(ctx, full, ref)})
}

// recheckEvidence words the evidence of a recheck from the note and its
// source as they are now: the text of the note, and for a page of the wiki
// the text of the page now, cut to pageCut, so the review can correct the
// note from the evidence alone. The list of the candidates words it again
// each time (OpenCandidates), so the review never reads an old text.
func (s *Service) recheckEvidence(ctx context.Context, n Note, ref string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The note %s says now:\n%s\n", n.Name, cut(n.Text, evidenceCut))
	path, ok := strings.CutPrefix(ref, "page:")
	if !ok {
		fmt.Fprintf(&b, "Its source %s changed since the note was written.", ref)
		return b.String()
	}
	text, _, err := s.pageAt(ctx, path)
	switch {
	case errors.Is(err, errNoPage):
		fmt.Fprintf(&b, "Its source, the page %s of the wiki, was removed since the note was written.", path)
	case err != nil:
		fmt.Fprintf(&b, "Its source, the page %s of the wiki, changed since the note was written. The page does not read now.", path)
	default:
		fmt.Fprintf(&b, "Its source, the page %s of the wiki, changed since the note was written. The page says now:\n%s", path, cut(text, pageCut))
	}
	return b.String()
}

// recheckNote returns the note of a recheck as it is now, and whether the
// recheck still stands: the note is out of date, and the review can change
// it. A note that a person or the agent corrected, that was put away, or
// that a person wrote ends its recheck.
func (s *Service) recheckNote(ctx context.Context, c Candidate) (Note, bool, error) {
	n, err := s.get(ctx, noteKey(c.Agent, c.Scope, c.Note))
	switch {
	case errors.Is(err, ErrNotFound):
		return Note{}, false, nil
	case err != nil:
		return Note{}, false, err
	}
	return n, n.State != StatePutAway && !n.ByPerson() && n.OutOfDate != nil, nil
}

// openCandidates returns the candidates of an agent that the review can
// decide now, the oldest first. A recheck that no longer stands
// (recheckNote) is decided on the spot and left out, so no review starts
// for it.
func (s *Service) openCandidates(ctx context.Context, agent string) ([]Candidate, error) {
	list, err := s.Candidates(ctx, agent, func(c Candidate) bool { return c.State == CandOpen && c.ready() })
	if err != nil {
		return nil, err
	}
	out := list[:0]
	for _, c := range list {
		if c.Kind == KindRecheck {
			_, stands, err := s.recheckNote(ctx, c)
			if err != nil {
				return nil, err
			}
			if !stands {
				if err := s.decided(ctx, c, CandSkipped, Decision{Op: OpSkip, Note: c.Note, By: ByKnowhow}); err != nil {
					s.log.Warn("knowhow: a recheck whose note was corrected stays open", "agent", c.Agent, "candidate", c.ID, "error", err)
				}
				continue
			}
		}
		out = append(out, c)
	}
	return out, nil
}
