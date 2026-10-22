package bridges

// This file watches the view of the fleet for the runs of each bridge and
// mirrors their life into the channel (docs/ARCHITECTURE.md section 18.6):
// a seat started, finished with an answer, needs a person, asked a
// question, got a decision, failed, or was canceled, and its Task went to
// sleep or woke. Each fact gets one entry, never two: the record keeps a
// mark of what the channel holds of each run, and the change of the record
// that moves a mark also gives the seq of the entry, so of two replicas
// that see the same fact one posts it. The watch reads the view at each
// change of the fleet that can concern a bridge (follow), and every
// sweepEvery beside them; without a fleet that sends its changes, it reads
// the view every pollEvery, two seconds.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pages"
)

// Bounds of the reads of the watch.
const (
	// endEvents is how many of the last events of a finished run the
	// watch reads to find its end.
	endEvents = 20
	// decisionEvents bounds the events after a card that the watch reads
	// to find its decision.
	decisionEvents = 200
	// decisionTries bounds the reads of a decision that fail before the
	// watch writes the card again with the state unknown. The watch then
	// reads the run again every lostEvery, so a read that works restores
	// the real state of the card.
	decisionTries = 30
)

// watch reads the view until the store closes: at each Wake, at most once
// per pollGap, and after a pause without one, which is sweepEvery while
// the service follows the changes of the fleet and pollEvery otherwise.
func (s *Store) watch() {
	defer s.wg.Done()
	for {
		s.poll(s.ctx)
		last := time.Now()
		every := s.pollEvery
		if s.following.Load() {
			every = s.sweepEvery
		}
		t := time.NewTimer(every)
		select {
		case <-s.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-s.wake:
			t.Stop()
			// Changes come in bursts: the wait lets the rest of a burst
			// land, so the burst costs one read.
			if gap := s.pollGap - time.Since(last); gap > 0 {
				g := time.NewTimer(gap)
				select {
				case <-s.ctx.Done():
					g.Stop()
					return
				case <-g.C:
				}
			}
		}
	}
}

// follow wakes the watch for each change of the fleet view that can
// concern a bridge (concerns), for as long as the store lives. It follows
// the fleet of the agent when that fleet sends its changes
// (fleet.Watcher), and it looks for one every followRetry, so it follows a
// fleet that starts after the bridges, or that takes the place of another.
// While it follows, the watch reads the view only every sweepEvery beside
// the changes. When the watch of the fleet ends, the poll of every
// pollEvery takes over at once.
func (s *Store) follow() {
	defer s.wg.Done()
	for {
		if w, ok := fleet.Of(s.a).(fleet.Watcher); ok {
			changes := w.Watch(s.ctx)
			s.following.Store(true)
			// The view changed before the watch began, and a read finds it.
			s.Wake()
			for c := range changes {
				if concerns(c) {
					s.Wake()
				}
			}
			s.following.Store(false)
			s.Wake()
		}
		t := time.NewTimer(s.followRetry)
		select {
		case <-s.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// concerns reports whether a change of the view can concern a bridge: a
// change of a run whose batch is the batch of a bridge, which the seats,
// their helpers, and the manager run of a spawn bridge carry, and every
// change of a member or of the phase of a Task, which moves the presence
// of a seat and the lines of its sleep.
func concerns(c fleet.Change) bool {
	if c.Kind != fleet.ChangeRun || c.Run == nil {
		return true
	}
	return strings.HasPrefix(c.Run.Metadata[fw.MetaBatch], BatchPrefix)
}

// poll looks at every bridge that the watch follows once: the open ones,
// and the closed ones for reportWait after the close, so the end of the
// work and the report still reach the channel.
func (s *Store) poll(ctx context.Context) {
	s.polls.Add(1)
	fctx := s.asFront(ctx)
	recs, err := s.b.list(fctx)
	if err != nil {
		s.log.Warn("bridges: read the bridges", "error", err)
		return
	}
	states := s.taskStates(fctx)
	live := map[string]bool{}
	for _, rec := range recs {
		if ctx.Err() != nil {
			return
		}
		if rec.State != StateOpen && (rec.ClosedAt == nil || s.now().Sub(*rec.ClosedAt) > s.reportWait) {
			continue
		}
		for id := range s.observe(fctx, rec, states) {
			live[id] = true
		}
	}
	s.mu.Lock()
	for id := range s.checked {
		if !live[id] {
			delete(s.checked, id)
		}
	}
	for id := range s.tries {
		if !live[id] {
			delete(s.tries, id)
		}
	}
	for id := range s.lostAt {
		if !live[id] {
			delete(s.lostAt, id)
		}
	}
	s.mu.Unlock()
}

// ending is the last end of a run, read from its owner.
type ending struct {
	seq     uint64
	at      time.Time
	outcome string
	output  string
	err     string
}

// verdict is the decision of a card, read from the owner of the run.
type verdict struct {
	found bool // the ledger holds the decision
	ended bool // the run ended without a decision
	seq   uint64
	at    time.Time
	d     fw.ApprovalDecidedData
}

// observe mirrors the runs of one bridge into its channel and gives the
// hooks the members whose presence changed. It returns the ids of the runs
// of the bridge.
func (s *Store) observe(ctx context.Context, rec *record, states map[string]string) map[string]*fw.Run {
	runs, err := s.runsOf(ctx, rec.ID)
	if err != nil {
		s.log.Warn("bridges: read the runs of a bridge", "bridge", rec.ID, "error", err)
		return nil
	}
	// The reads of the owners come before the change of the record, which
	// can run more than once.
	claim(rec, runs)
	ends := map[string]*ending{}
	verdicts := map[string]*verdict{}
	lost := map[string]map[string]*verdict{} // by run and approval: the decisions of lost cards
	for _, r := range runs {
		seat := mirrored(rec, r)
		if seat == "" {
			continue
		}
		mk := rec.Marks[r.ID]
		if mk != nil && mk.Pending != "" && (r.Pending == nil || r.Pending.ID != mk.Pending) {
			if v := s.readVerdict(ctx, r, mk); v != nil {
				verdicts[r.ID] = v
			}
		}
		if mk != nil && len(mk.Lost) > 0 && s.lostDue(r.ID) {
			for _, lc := range mk.Lost {
				if v := s.readLost(ctx, r, lc); v != nil {
					if lost[r.ID] == nil {
						lost[r.ID] = map[string]*verdict{}
					}
					lost[r.ID][lc.Pending] = v
				}
			}
		}
		if r.State.Terminal() && s.unchecked(r) {
			if e := s.readEnd(ctx, r); e != nil {
				ends[r.ID] = e
			}
		}
	}
	now := s.now().UTC()
	var lines []Message
	var cards []Message // cards whose decision landed, written again
	fresh, err := s.b.update(ctx, rec.ID, func(rec *record) error {
		lines, cards = nil, nil
		add := func(l Message) {
			l.Seq, l.At = rec.alloc(), now
			lines = append(lines, l)
		}
		changed := claim(rec, runs)
		for _, r := range sorted(runs) {
			seat := mirrored(rec, r)
			if seat == "" {
				continue
			}
			cards = append(cards, s.facts(ctx, rec, r, seat, ends[r.ID], verdicts[r.ID], lost[r.ID], add)...)
		}
		slept, marked := sleepLines(rec, runs, states, now, s.sleepHold)
		for _, l := range slept {
			add(l)
		}
		if !changed && !marked && len(lines) == 0 && len(cards) == 0 {
			return errNoChange
		}
		return nil
	})
	if err != nil {
		s.log.Warn("bridges: mirror the runs of a bridge", "bridge", rec.ID, "error", err)
		return runs
	}
	for _, c := range cards {
		s.rewriteCard(ctx, fresh.ID, c)
	}
	// A job of a fired seat that waited in the queue starts a run after
	// the fire; the watch stops it.
	for _, r := range runs {
		if m := fresh.member(r.Metadata[MetaMember]); m != nil && m.Fired && !r.State.Terminal() {
			s.submitCancel(fresh.ID, m.Handle, r)
		}
	}
	s.land(ctx, fresh.ID, lines...)
	s.reportCheck(ctx, fresh)
	s.show(fresh, runs, states)
	return runs
}

// mirrored names the seat of a run that the channel mirrors: a seat run,
// or HandleManager for the manager run of a spawn bridge, which carries
// the batch of the bridge. A helper that a seat started, and the
// conversation of the manager that opened a queue bridge, are not
// mirrored.
func mirrored(rec *record, r *fw.Run) string {
	if h := rec.seatOfRun(r.ID); h != "" {
		return h
	}
	if rec.Manager != nil && r.ID == rec.Manager.Run && r.Metadata[fw.MetaBatch] == BatchOf(rec.ID) {
		return HandleManager
	}
	return ""
}

// unchecked reports whether the watch has not read the end of a finished
// run at its current seq.
func (s *Store) unchecked(r *fw.Run) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checked[r.ID] != r.Seq
}

// readEnd reads the last end of a finished run from its owner, with the
// whole output. It returns nil when the read fails; the next poll tries
// again.
func (s *Store) readEnd(ctx context.Context, r *fw.Run) *ending {
	events, err := s.events(ctx, r, 0, endEvents)
	if err != nil {
		s.log.Debug("bridges: read the end of a run", "run", r.ID, "error", err)
		return nil
	}
	s.mu.Lock()
	s.checked[r.ID] = r.Seq
	s.mu.Unlock()
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != fw.EvRunFinished {
			continue
		}
		var d fw.RunFinishedData
		if e.Decode(&d) != nil {
			return nil
		}
		return &ending{seq: e.Seq, at: e.Time, outcome: d.Outcome, output: d.Output, err: d.Error}
	}
	return nil
}

// readVerdict reads the decision of the card of a run from its owner. It
// returns nil when the read failed and the watch tries again, and an empty
// verdict after decisionTries failed reads, which writes the card again
// with the state unknown.
func (s *Store) readVerdict(ctx context.Context, r *fw.Run, mk *runMark) *verdict {
	v, err := s.findVerdict(ctx, r, mk.Pending, mk.AskSeq)
	if err != nil {
		s.mu.Lock()
		s.tries[r.ID]++
		n := s.tries[r.ID]
		s.mu.Unlock()
		s.log.Debug("bridges: read the decision of a card", "run", r.ID, "tries", n, "error", err)
		if n >= decisionTries {
			return &verdict{}
		}
		return nil
	}
	return v
}

// readLost reads the decision of a card that shows the state unknown. It
// returns nil when the read fails again; the card stays unknown, and the
// watch reads again after lostEvery.
func (s *Store) readLost(ctx context.Context, r *fw.Run, lc lostCard) *verdict {
	v, err := s.findVerdict(ctx, r, lc.Pending, lc.AskSeq)
	if err != nil {
		s.log.Debug("bridges: read the decision of a card that shows unknown", "run", r.ID, "card", lc.Card, "error", err)
		return nil
	}
	return v
}

// lostDue reports whether the watch reads a run for its lost cards now: at
// most once per lostEvery for each run.
func (s *Store) lostDue(run string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if at, ok := s.lostAt[run]; ok && now.Sub(at) < s.lostEvery {
		return false
	}
	s.lostAt[run] = now
	return true
}

// findVerdict reads the events of a run from the seq from on, and finds
// the decision of the approval: a verdict that found it, or a verdict
// that the run ended without it. It returns the error of a read that
// failed.
func (s *Store) findVerdict(ctx context.Context, r *fw.Run, approval string, from uint64) (*verdict, error) {
	if from == 0 {
		from = 1
	}
	events, err := s.events(ctx, r, from, decisionEvents)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		if e.Type != fw.EvApprovalDecided {
			continue
		}
		var d fw.ApprovalDecidedData
		if e.Decode(&d) == nil && d.ApprovalID == approval {
			return &verdict{found: true, seq: e.Seq, at: e.Time, d: d}, nil
		}
	}
	// The ledger holds no decision of the approval, and the run no longer
	// waits for it: the run ended, or a resume asked again under a new id.
	return &verdict{ended: true}, nil
}

// facts adds the lines that one run brings to the channel since its mark
// with add, which gives each line its seq, moves the mark, and returns the
// cards whose decision landed. lost holds the decisions that the watch
// read for the cards of the run that show the state unknown, by approval.
// It runs inside the change of the record.
func (s *Store) facts(ctx context.Context, rec *record, r *fw.Run, seat string, end *ending, v *verdict, lost map[string]*verdict, add func(Message)) (cards []Message) {
	mk := rec.mark(r.ID, seat)
	task := ""
	if m := rec.member(seat); m != nil {
		task = m.Task
	}
	if !mk.Started {
		mk.Started = true
		text := fmt.Sprintf("%s started: %s", seat, oneLine(task, 300))
		if seat == HandleManager {
			text = "The manager started: it seats the members of the bridge."
		}
		l := system(EventStarted, text)
		l.Mirrors = &Mirror{Run: r.ID, Seq: 1, Type: fw.EvRunQueued}
		add(l)
	}
	// A card that shows the state unknown, whose run the watch read
	// again: the real decision.
	if len(mk.Lost) > 0 && len(lost) > 0 {
		keep := make([]lostCard, 0, len(mk.Lost))
		for _, lc := range mk.Lost {
			lv := lost[lc.Pending]
			if lv == nil || (!lv.found && !lv.ended) {
				keep = append(keep, lc)
				continue
			}
			card, line := decidedCard(r.ID, seat, lc.Card, lc.Question, lv)
			if card != nil {
				cards = append(cards, *card)
			}
			if line != nil {
				add(*line)
			}
		}
		mk.Lost = keep
	}
	// A card whose decision landed, from any surface, or whose run the
	// front failed to read after decisionTries tries.
	if mk.Pending != "" && (r.Pending == nil || r.Pending.ID != mk.Pending) && v != nil {
		card, line := decidedCard(r.ID, seat, mk.Card, mk.Question, v)
		if card != nil {
			cards = append(cards, *card)
		}
		if line != nil {
			add(*line)
		}
		if !v.found && !v.ended && mk.Card != 0 {
			mk.Lost = append(mk.Lost, lostCard{Pending: mk.Pending, Card: mk.Card, AskSeq: mk.AskSeq, Question: mk.Question})
			if len(mk.Lost) > maxLost {
				mk.Lost = mk.Lost[len(mk.Lost)-maxLost:]
			}
		}
		rec.dropCard(mk.Card)
		mk.Pending, mk.Card, mk.AskSeq, mk.Question = "", 0, 0, false
		s.mu.Lock()
		delete(s.tries, r.ID)
		s.mu.Unlock()
	}
	// A new approval or question.
	if r.State == fw.StateInputRequired && r.Pending != nil && mk.Pending == "" {
		c := s.cardOf(ctx, r, seat)
		add(c)
		mk.Pending, mk.AskSeq, mk.Card, mk.Question = r.Pending.ID, r.Seq, rec.LastSeq, c.Decision.Question
		rec.Cards = append(rec.Cards, rec.LastSeq)
	}
	// A new end of the run.
	if end != nil && end.seq > mk.Done {
		mk.Done, mk.Limit = end.seq, false
		mirror := &Mirror{Run: r.ID, Seq: end.seq, Type: fw.EvRunFinished}
		switch end.outcome {
		case fw.OutcomeCompleted:
			if strings.TrimSpace(end.output) == "" {
				l := system(EventFinished, seat+" finished with no answer.")
				l.Mirrors = mirror
				add(l)
				break
			}
			text, cut := cutText(end.output, entryTextBytes)
			l := newMessage(From{Kind: KindAgent, Handle: seat, Run: r.ID}, text)
			l.Cut, l.Mirrors = cut, mirror
			add(l)
		case fw.OutcomeFailed:
			l := system(EventFailed, fmt.Sprintf("%s failed: %s", seat, oneLine(orWords(end.err, "no error text"), 500)))
			l.Mirrors = mirror
			add(l)
		default:
			l := system(EventCanceled, fmt.Sprintf("The run of %s was canceled.", seat))
			l.Mirrors = mirror
			add(l)
		}
	}
	return cards
}

// orWords returns s, or the words w when s is empty.
func orWords(s, w string) string {
	if strings.TrimSpace(s) == "" {
		return w
	}
	return s
}

// cardOf is the card of the approval or the question that a run waits for,
// worded as the result of a run words a pending decision
// (dashboard.Decision).
func (s *Store) cardOf(ctx context.Context, r *fw.Run, seat string) Message {
	d := Decision{Run: r.ID, State: DecisionPending}
	if res, err := dashboard.ResultFrom(ctx, s.a, r, nil); err == nil && res.Pending != nil {
		d.Decision = *res.Pending
	} else {
		d.Decision = dashboard.Decision{ID: r.Pending.ID, Tool: r.Pending.Tool, Label: "Allow", Reason: r.Pending.Reason, Input: r.Pending.Input}
	}
	d.Always = alwaysOffered(r, d.Decision)
	event, text := EventNeedsYou, ""
	switch p := d.Decision; {
	case p.Connect != nil:
		text = fmt.Sprintf("%s waits for you to connect a service: %s.", seat, strings.TrimPrefix(p.Label, "Connect "))
	case p.Question:
		event = EventAsked
		text = seat + " asks a question."
		if q := questionOf(p.Input); q != "" {
			text = fmt.Sprintf("%s asks: %s", seat, oneLine(q, 500))
		}
	default:
		text = fmt.Sprintf("%s wants to %s.", seat, wantOf(p.Tool))
	}
	l := system(event, text)
	l.Decision = &d
	l.Mirrors = &Mirror{Run: r.ID, Seq: r.Seq, Type: fw.EvApprovalRequested}
	return l
}

// wantOf words what a tool call does, for a card.
func wantOf(tool string) string {
	switch tool {
	case "bash":
		return "run a command"
	case "files":
		return "change a file"
	case "":
		return "go on"
	}
	return "use " + tool
}

// questionOf reads the question of the input of a question tool.
func questionOf(input json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	for _, k := range []string{"question", "message", "prompt"} {
		if q, ok := in[k].(string); ok && strings.TrimSpace(q) != "" {
			return q
		}
	}
	return ""
}

// alwaysOffered reports whether the card of a decision can offer Always
// allow, by the rule of a pool (docs/ARCHITECTURE.md section 14.9): a run
// of a worker, a call of one tool, and an ask of the default or of a rule
// that names the tool, not a flag and not a pattern. A question and a
// connection are no permission to act.
func alwaysOffered(r *fw.Run, d dashboard.Decision) bool {
	if r.Member == "" || d.Tool == "" || d.Question || d.Connect != nil {
		return false
	}
	if d.Reason == "default" {
		return true
	}
	entry, ok := strings.CutPrefix(d.Reason, "rule: ask ")
	if !ok {
		return false
	}
	entry = strings.TrimSpace(entry)
	switch strings.ToLower(entry) {
	case "readonly", "read-only", "idempotent", "destructive", "parallel", "parallelsafe":
		return false
	}
	return !strings.ContainsAny(entry, ":*?[")
}

// decidedCard returns the card with the seq card written again with its
// decision, and the line that says who decided. question is true for a
// card that asks a question. A card whose run no longer waits for it
// without a decision, because the run ended or asked again, is written
// again as denied, with no line. A card whose run the front failed to
// read after decisionTries tries is written again with the state unknown
// and the note unknownNote, and the channel gets a line with the event
// failed that says so.
func decidedCard(run, seat string, card uint64, question bool, v *verdict) (*Message, *Message) {
	switch {
	case v.found:
		at := v.at.UTC()
		d := Decision{State: v.d.Decision, DecidedBy: v.d.Decider, DecidedAt: &at, Note: v.d.Note}
		if question {
			d.Answer = answerOf(v.d)
		}
		line := system(EventDecided, decidedText(seat, v.d, question, d.Answer))
		line.Mirrors = &Mirror{Run: run, Seq: v.seq, Type: fw.EvApprovalDecided}
		if card == 0 {
			return nil, &line
		}
		return &Message{Seq: card, Decision: &d}, &line
	case v.ended:
		if card == 0 {
			return nil, nil
		}
		now := time.Now().UTC()
		return &Message{Seq: card, Decision: &Decision{State: DecisionDeny, DecidedAt: &now, Note: "The run no longer waits for this decision."}}, nil
	}
	line := system(EventFailed, fmt.Sprintf("%s no longer waits for its decision, and the front failed to read the run, so it cannot tell who decided. The card shows the decision once the front reads the run again.", seat))
	if card == 0 {
		return nil, &line
	}
	return &Message{Seq: card, Decision: &Decision{State: DecisionUnknown, Note: unknownNote}}, &line
}

// unknownNote is the note of a card with the state unknown.
const unknownNote = "The front failed to read the run, so it cannot tell whether anyone decided this card. Nobody can decide it here."

// answerOf words the answer to a question: the value of a form with one
// field, Yes or No for a boolean, the fields of a larger form, or the
// note.
func answerOf(d fw.ApprovalDecidedData) string {
	if len(d.Content) == 0 {
		if d.Decision == DecisionAllow {
			return d.Note
		}
		return ""
	}
	var v map[string]any
	if json.Unmarshal(d.Content, &v) != nil {
		return oneLine(string(d.Content), 500)
	}
	words := func(x any) string {
		switch t := x.(type) {
		case bool:
			if t {
				return "Yes"
			}
			return "No"
		case string:
			return t
		}
		b, _ := json.Marshal(x)
		return string(b)
	}
	if len(v) == 1 {
		for _, x := range v {
			return oneLine(words(x), 500)
		}
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+words(v[k]))
	}
	return oneLine(strings.Join(parts, ", "), 500)
}

// decidedText is the line of a decision, of a question when question is
// true.
func decidedText(seat string, d fw.ApprovalDecidedData, question bool, answer string) string {
	who := d.Decider
	switch who {
	case "":
		who = "somebody"
	case fw.KindToken:
		who = "the token of the fleet"
	}
	note := ""
	if strings.TrimSpace(d.Note) != "" {
		note = ", with the note: " + oneLine(d.Note, 300)
	}
	if question {
		if d.Decision == DecisionAllow {
			return fmt.Sprintf("%s answered the question of %s: %s.", who, seat, oneLine(answer, 300))
		}
		return fmt.Sprintf("%s declined the question of %s%s.", who, seat, note)
	}
	verb := "allowed"
	if d.Decision != DecisionAllow {
		verb = "denied"
	}
	return fmt.Sprintf("%s %s the call of %s%s.", who, verb, seat, note)
}

// rewriteCard writes a card again with its decision, under its seq, and
// gives the hook the changed entry. A card that waits takes any state, and
// a card with the state unknown takes its real decision.
func (s *Store) rewriteCard(ctx context.Context, id string, c Message) {
	if c.Decision == nil {
		return
	}
	got, changed, err := s.b.changeEntry(ctx, id, c.Seq, func(m *Message) bool {
		if m.Decision == nil {
			return false
		}
		switch m.Decision.State {
		case DecisionPending:
		case DecisionUnknown:
			if c.Decision.State == DecisionUnknown {
				return false
			}
		default:
			return false
		}
		d := *m.Decision
		d.State, d.DecidedBy, d.DecidedAt, d.Note, d.Answer = c.Decision.State, c.Decision.DecidedBy, c.Decision.DecidedAt, c.Decision.Note, c.Decision.Answer
		m.Decision = &d
		return true
	})
	if err != nil {
		s.log.Warn("bridges: write a card again with its decision", "bridge", id, "seq", c.Seq, "error", err)
		return
	}
	if changed {
		emitMessage(id, got)
	}
}

// sleepLines returns the lines of the seats whose Task went to sleep or
// woke since the channel last said so, moves the marks, and reports
// whether it changed the record. The lines follow the record, which every
// replica shares, and not the view of one replica: the first replica that
// sees the Task of the newest run of a seat quiet marks the time, a
// replica that sees the Task live, stopped, or blocked clears the mark,
// and the line asleep comes once the mark is hold old. The line woke up
// comes once per line asleep, when a replica sees the Task live again.
// The change of the record that moves a mark gives the line its seq, so
// of two replicas one posts it. A Task that sleeps and wakes within hold
// posts nothing. A replica whose view does not know the Task changes
// nothing.
func sleepLines(rec *record, runs map[string]*fw.Run, states map[string]string, now time.Time, hold time.Duration) ([]Message, bool) {
	var out []Message
	changed := false
	for _, m := range rec.Members {
		if m.Fired {
			continue
		}
		r := runs[m.newest()]
		if r == nil {
			continue
		}
		sm := rec.seat(m.Handle)
		switch taskOf(r, states) {
		case "":
		case fleet.MemberQuiet:
			if sm.Asleep {
				continue
			}
			if sm.QuietAt == nil || sm.QuietOn != r.Member {
				at := now
				sm.QuietAt, sm.QuietOn = &at, r.Member
				changed = true
			}
			if now.Sub(*sm.QuietAt) >= hold {
				sm.Asleep, sm.QuietAt, sm.QuietOn = true, nil, ""
				changed = true
				out = append(out, system(EventAsleep, m.Handle+" went to sleep. A message wakes its Task."))
			}
		default:
			if sm.QuietAt != nil {
				sm.QuietAt, sm.QuietOn = nil, ""
				changed = true
			}
			if taskOf(r, states) == fleet.MemberLive && sm.Asleep {
				sm.Asleep = false
				changed = true
				out = append(out, system(EventStarted, m.Handle+" woke up."))
			}
		}
	}
	return out, changed
}

// show gives the OnMember hooks every member whose presence, activity, or
// seat changed since this replica last gave it.
func (s *Store) show(rec *record, runs map[string]*fw.Run, states map[string]string) {
	for _, m := range decorate(rec, runs, states) {
		key := rec.ID + "/" + m.Handle
		k := shownKey(m)
		s.mu.Lock()
		same := s.shown[key] == k
		s.shown[key] = k
		s.mu.Unlock()
		if !same {
			emitMember(rec.ID, m)
		}
	}
}

// reportCheck posts the line of the report of a closed bridge once the
// page bridges/<id>/report.md appears, up to reportWait after the ask. The
// line names the page in its field page, and its text holds the path
// without a link.
func (s *Store) reportCheck(ctx context.Context, rec *record) {
	r := rec.Report
	if rec.State != StateClosed || r == nil || r.Posted || s.now().Sub(r.AskedAt) > s.reportWait {
		return
	}
	w := pages.Of(s.a)
	if w == nil {
		return
	}
	page, err := w.Read(ctx, reportPath(rec.ID), 0)
	if err != nil || page.At.Before(r.AskedAt.Add(-time.Minute)) {
		return
	}
	var line []Message
	_, err = s.b.update(ctx, rec.ID, func(rec *record) error {
		line = nil
		if rec.Report == nil || rec.Report.Posted {
			return errNoChange
		}
		rec.Report.Posted = true
		l := system(EventChanged, fmt.Sprintf("The report is in the wiki: %s.", reportPath(rec.ID)))
		l.Page = reportPath(rec.ID)
		line = seqLines(rec, []Message{l}, s.now().UTC())
		return nil
	})
	if err != nil {
		s.log.Warn("bridges: post the link to the report", "bridge", rec.ID, "error", err)
		return
	}
	s.land(ctx, rec.ID, line...)
}

// reportPath is the page of the report of a bridge.
func reportPath(id string) string { return "bridges/" + id + "/report.md" }
