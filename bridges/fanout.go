package bridges

// This file appends the messages of people and agents to a channel and
// fans each one out (docs/ARCHITECTURE.md section 18.5). The addressed
// members, or every active member when a message names nobody, get the
// message as a follow-up on the newest run of the member in the bridge,
// with the sender in from (the kernel architecture, section 5.5). A member
// whose run the fleet lost, and a member that nothing seats, get a new run
// instead, which reads the channel first. The final answer of a member
// fans out in the same way when its first words address members
// (addressees), from the watch. The change of the record that
// gives the message its seq also decides the deliveries and the seatings
// under the caps, so two replicas never deliver past a cap, and the
// replica that took the post delivers, so a message is delivered once. A
// delivery runs on a bounded worker, never on the request of the poster,
// and a delivery that fails is tried again with a pause that doubles.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/skills"
)

// Bounds of a message.
const (
	// maxTextBytes bounds the text of a message that a person or an agent
	// posts. The run that gets it holds the whole text.
	maxTextBytes = 64 << 10
	// entryTextBytes is where the channel cuts a text, with cut true.
	entryTextBytes = 4 << 10
	// maxTo bounds the handles that one message addresses.
	maxTo = MaxMembersLimit
)

// sender is who posts a message: a person with a subject, or an agent with
// a handle and the run that posts.
type sender struct {
	subject string // a person
	display string
	handle  string // an agent
	run     string
}

// from is the sender as an entry names it.
func (x sender) from() From {
	if x.handle != "" {
		return From{Kind: KindAgent, Handle: x.handle, Run: x.run}
	}
	return From{Kind: KindPerson, Name: x.subject, Display: x.display}
}

// name is the sender in the first line of a follow-up: the handle of an
// agent, or operator for a person, because the agents address the people
// of a bridge as operator.
func (x sender) name() string {
	if x.handle != "" {
		return x.handle
	}
	return HandleOperator
}

// party is the sender as the kernel records it on the follow-up for the
// recipient member.
func (x sender) party(member string) fw.MessageFrom {
	if x.handle != "" {
		return fw.MessageFrom{Kind: fw.FromAgent, Name: x.handle, Run: x.run, Member: member}
	}
	return fw.MessageFrom{Kind: fw.FromPerson, Name: orOperator(x.subject), Member: member}
}

// PostSpec is a message of a person or an agent: the text, the handles
// that it addresses, and the entry that it replies to. Pinned is for a
// message of a person alone: each delivery of the message is a pinned
// follow-up (fw.WithMessagePinned), so the instruction stays in the
// context of the members through every compaction. The tool bridge_post
// does not take it. Skill makes the message a slash command of a person:
// the front expands the skill of the catalog (skills.Prepare), and Text,
// which is optional then, follows the skill (docs/ARCHITECTURE.md section
// 20.3). A slash command is always pinned.
type PostSpec struct {
	Text    string          `json:"text"`
	To      []string        `json:"to,omitempty"`
	ReplyTo *uint64         `json:"replyTo,omitempty"`
	Pinned  bool            `json:"pinned,omitempty"`
	Skill   *skills.Command `json:"skill,omitempty"`
}

// delivery is one follow-up of a message to the newest run of a member.
// pinned makes the follow-up survive every compaction of the run. A
// delivery without a run waits for newest, the newest run of the member,
// which the view did not hold when the message came (deferred).
type delivery struct {
	bridge  string
	seq     uint64
	handle  string
	run     *fw.Run
	newest  string
	content string
	from    fw.MessageFrom
	pinned  bool // a pinned message or a slash command
}

// wakeInput is the input of the new run of a seat whose earlier run the
// fleet lost, when a message seats it again. It sends the run to the whole
// channel, because a run that reads only from the message on misses its
// earlier work and what people told it, and names the message to answer.
// The text of the message never becomes the input, which has the
// authority of the operator.
func wakeInput(seq uint64) string {
	return fmt.Sprintf("The fleet lost your earlier run of this seat, so this is a new run on the same seat. Your task is in your instructions. First read the channel with bridge_read, without since, so you see your earlier work and every message for you. Do not repeat work that the channel shows as done. Then answer entry %d and any later message for you. Your final answer reaches the channel by itself, so do not post it with bridge_post.", seq)
}

// header is the first line of a follow-up, which names the bridge, so a
// run that reads several bridges tells them apart:
// [bridge "<title>" <id>] @reviewer from migrator-1: ...
func header(rec *record, to []string, from string) string {
	if len(to) == 0 {
		return fmt.Sprintf("[bridge %q %s] to everyone from %s: ", rec.Title, rec.ID, from)
	}
	return fmt.Sprintf("[bridge %q %s] @%s from %s: ", rec.Title, rec.ID, strings.Join(to, " @"), from)
}

// limited reports whether a run stopped at a limit of its budget, so a
// follow-up without a larger budget fails at once.
func limited(r *fw.Run) bool {
	return r.State == fw.StateFailed && strings.HasPrefix(r.Error, "budget exceeded")
}

// cannotTake is the cap line of a member whose newest run failed, with the
// reason from the run: a stop at a limit of its budget, or another
// failure. The line states the fact and no advice, and the page words it.
// At a limit the member needs a larger budget, and after another failure
// its task seats it again on a new run (giveTask).
func cannotTake(handle string, r *fw.Run) string {
	reason := oneLine(orWords(r.Error, "no error text"), 300)
	if limited(r) {
		return fmt.Sprintf("%s cannot take messages: its run stopped at a limit (%s)", handle, reason)
	}
	return fmt.Sprintf("%s cannot take messages: its run failed (%s)", handle, reason)
}

// post appends a message of a person or an agent to the channel and fans
// it out. It returns the entry.
func (s *Store) post(ctx context.Context, id string, snd sender, spec PostSpec) (Message, error) {
	text := clean(spec.Text)
	var cmd *skills.Expansion
	if spec.Skill != nil {
		if snd.handle != "" {
			return Message{}, invalid("a run hands no skill to its teammates; a person sends a skill through the route")
		}
		if len(text) > maxTextBytes {
			return Message{}, invalid("a message holds at most %d bytes", maxTextBytes)
		}
		x, err := skills.Prepare(s.a, *spec.Skill, text)
		if err != nil {
			return Message{}, skillError(err)
		}
		cmd, text = &x, x.Message
	}
	switch {
	case text == "":
		return Message{}, invalid("a message needs a text or a skill")
	case len(text) > maxTextBytes:
		return Message{}, invalid("a message holds at most %d bytes", maxTextBytes)
	case len(spec.To) > maxTo:
		return Message{}, invalid("a message addresses at most %d members", maxTo)
	case spec.Pinned && snd.handle != "":
		return Message{}, invalid("only a person pins a message")
	}
	var to []string
	seen := map[string]bool{}
	for _, h := range spec.To {
		h = strings.TrimPrefix(strings.TrimSpace(h), "@")
		if h != "" && !seen[h] {
			seen[h] = true
			to = append(to, h)
		}
	}
	fctx := s.asFront(ctx)
	runs, err := s.runsOf(fctx, id)
	if err != nil {
		return Message{}, err
	}
	now := s.now().UTC()
	var msg Message
	var lines []Message
	var plan []delivery
	var seats []Member
	var inputs map[string]string
	rec, err := s.b.update(ctx, id, func(rec *record) error {
		lines, plan, seats, inputs = nil, nil, nil, nil
		if rec.State != StateOpen {
			return fmt.Errorf("%w: the bridge %s is closed, so it takes no message", ErrConflict, id)
		}
		for _, h := range to {
			if h == HandleOperator && snd.handle != "" {
				continue
			}
			m := rec.member(h)
			switch {
			case m == nil:
				return invalid("no member %s in this bridge", h)
			case m.Fired:
				return invalid("%s is fired and gets no message", h)
			}
		}
		if spec.ReplyTo != nil && (*spec.ReplyTo == 0 || *spec.ReplyTo > rec.LastSeq) {
			return invalid("no entry %d to reply to", *spec.ReplyTo)
		}
		claim(rec, runs)
		shown := text
		if cmd != nil {
			// The channel holds the line Skill and a summary, and the run
			// holds the whole message.
			shown = cmd.Summary
		}
		cut, isCut := cutText(shown, entryTextBytes)
		msg = newMessage(snd.from(), cut)
		// A slash command is pinned, so its entry says so, and fanPlan
		// pins each delivery of a pinned entry.
		msg.Seq, msg.At, msg.Cut, msg.ReplyTo, msg.Pinned = rec.alloc(), now, isCut || cmd != nil, spec.ReplyTo, spec.Pinned || cmd != nil
		if msg.Cut {
			msg.whole = text
		}
		if to != nil {
			msg.To = to
		}
		if cmd != nil {
			c := cmd.Command
			msg.Skill = &c
		}
		lines, plan, seats, inputs = fanPlan(rec, snd, msg, text, runs, now)
		return nil
	})
	if err != nil {
		return Message{}, err
	}
	s.land(ctx, id, msg)
	s.land(ctx, id, lines...)
	for _, d := range plan {
		s.submit(s.deliveryJob(d))
	}
	if len(seats) > 0 {
		s.submit(s.seatJob(rec, seats, inputs, msg.Seq))
	}
	if snd.subject != "" {
		if err := s.b.setRead(fctx, id, snd.subject, msg.Seq); err != nil {
			s.log.Warn("bridges: move the read mark of the poster", "bridge", id, "error", err)
		}
	}
	s.log.Info("bridges: a message reached the channel", "bridge", id, "seq", msg.Seq, "from", snd.name(), "deliveries", len(plan), "seatings", len(seats))
	return msg, nil
}

// fanPlan decides the deliveries and the seatings of a message under the
// caps of the bridge, inside the change of the record that gives the
// message its seq, and returns the system lines that the caps post, with
// their seqs. A cap delivers nothing more, posts one line, and waits: the
// channel holds the message in every case.
//
//   - maxMessages counts the messages of people and agents. Past it, no
//     message fans out until the operator raises the cap.
//   - maxFanoutPerMinute counts the deliveries of the current minute. Past
//     it, the rest of the minute delivers nothing.
//   - maxPairMessages counts the messages between two agents since the
//     last message of a person. Past it, the two get nothing more from
//     each other until a person writes or the operator raises the cap.
//
// Each member gets the message by the state of its newest run:
//
//   - A run that the view holds gets a follow-up. A canceled run gets no
//     message of an agent, because a person stopped it, and a message of
//     a person starts a new turn of it.
//   - A run that failed gets nothing, and the channel says so once for
//     that run with the reason from the run (cannotTake).
//   - A run that no worker of the fleet holds, as the channel said with
//     the line lost (goneLines), gets nothing: the message seats the
//     member again on a new run, whose input sends it to the channel
//     (wakeInput).
//   - A run that the view does not hold yet, while the channel did not
//     say that the fleet lost it, gets a deferred delivery, which reads
//     the record and the run again at each try (deferred).
//   - A member without a run that something seats (willSeat) gets
//     nothing, and its instructions tell it to read the channel first. A
//     member without a run that nothing seats is seated by the message on
//     a new run with its task.
//
// A seating counts as one delivery under the caps, and it moves the marks
// Again and AgainAt of the seat as seatAgain does, so a second message
// within seatAgainWait seats nothing more (seatingOut): the new run reads
// the channel first. fanPlan returns the seats to seat with the input of
// each new run by handle (inputOf), and the caller places them together
// after the change (seatJob).
func fanPlan(rec *record, snd sender, msg Message, text string, runs map[string]*fw.Run, now time.Time) (lines []Message, plan []delivery, seats []Member, inputs map[string]string) {
	f := &rec.Fan
	if snd.handle == "" {
		// A message of a person ends every exchange between two agents.
		f.Pairs, f.PairCapped = nil, nil
	}
	f.Messages++
	if f.Messages > rec.Caps.MaxMessages {
		if !f.Capped {
			f.Capped = true
			lines = append(lines, system(EventCap, fmt.Sprintf("The bridge reached its cap of %d messages", rec.Caps.MaxMessages)))
		}
		return seqLines(rec, lines, now), nil, nil, nil
	}
	var targets []*Member
	if len(msg.To) > 0 {
		for _, h := range msg.To {
			if m := rec.member(h); m != nil && !m.Fired {
				targets = append(targets, m)
			}
		}
	} else {
		for i := range rec.Members {
			if m := &rec.Members[i]; !m.Fired && !m.Muted {
				targets = append(targets, m)
			}
		}
	}
	minute := now.Truncate(time.Minute)
	if !f.Minute.Equal(minute) {
		f.Minute, f.Count = minute, 0
	}
	var minuteMissed []string
	head := header(rec, msg.To, snd.name())
	for _, m := range targets {
		if m.Handle == snd.handle {
			continue // a message of a member to itself
		}
		newest := m.newest()
		r := runs[newest]
		sm := rec.Seats[m.Handle]
		seat, input := false, ""
		switch {
		case newest == "":
			if willSeat(rec, runs[managerRun(rec)]) || seatingOut(sm, *m, now) {
				continue // its run comes and reads the channel first
			}
			seat = true
		case r == nil && sm != nil && sm.Gone == newest:
			if seatingOut(sm, *m, now) {
				continue // a seating for the lost run went out, and its run reads the channel first
			}
			seat, input = true, wakeInput(msg.Seq)
		case r == nil:
			// The view does not hold the run yet: a deferred delivery.
		case r.State == fw.StateFailed:
			if mk := rec.mark(r.ID, m.Handle); !mk.Limit {
				mk.Limit = true
				lines = append(lines, system(EventCap, cannotTake(m.Handle, r)))
			}
			continue
		case r.State == fw.StateCanceled && snd.handle != "":
			continue // a person stopped the run, and only a person starts it again
		}
		pair := ""
		if snd.handle != "" {
			pair = pairKey(snd.handle, m.Handle)
			if f.Pairs[pair] >= rec.Caps.MaxPairMessages {
				if !f.PairCapped[pair] {
					if f.PairCapped == nil {
						f.PairCapped = map[string]bool{}
					}
					f.PairCapped[pair] = true
					lines = append(lines, system(EventCap, fmt.Sprintf("%s and %s exchanged %d messages without a person", snd.handle, m.Handle, rec.Caps.MaxPairMessages)))
				}
				continue
			}
		}
		if f.Count >= rec.Caps.MaxFanoutPerMinute {
			minuteMissed = append(minuteMissed, m.Handle)
			continue
		}
		f.Count++
		if pair != "" {
			if f.Pairs == nil {
				f.Pairs = map[string]int{}
			}
			f.Pairs[pair]++
		}
		if seat {
			sm := rec.seat(m.Handle)
			sm.Seatings++
			sm.Again, sm.AgainAt = newest, &now
			seats = append(seats, *m)
			if input != "" {
				if inputs == nil {
					inputs = map[string]string{}
				}
				inputs[m.Handle] = input
			}
			continue
		}
		d := delivery{bridge: rec.ID, seq: msg.Seq, handle: m.Handle, run: r, content: head + text, from: snd.party(m.Handle), pinned: msg.Pinned}
		if r == nil {
			d.newest = newest
		}
		plan = append(plan, d)
	}
	if len(minuteMissed) > 0 && !f.MinuteCapped.Equal(minute) {
		f.MinuteCapped = minute
		lines = append(lines, system(EventCap, fmt.Sprintf("The bridge reached its cap of %d deliveries a minute, and %s did not get message %d", rec.Caps.MaxFanoutPerMinute, strings.Join(minuteMissed, ", "), msg.Seq)))
	}
	return seqLines(rec, lines, now), plan, seats, inputs
}

// fanOut is what fanPlan decided for one message: the deliveries, and
// the seats that the message seats on new runs with their inputs.
type fanOut struct {
	seq    uint64
	plan   []delivery
	seats  []Member
	inputs map[string]string
}

// addressees reads the handles that the first words of the final answer
// of a member address, such as "@reviewer please apply these fixes" or
// "**@writer @operator:** done". It returns the handles in their order,
// each once: the members that are not fired, other than the sender self,
// and operator. An @ word that names no such handle is left out, and the
// first word without an @ ends the list. member is true when the list
// holds a member, so the answer reaches that member (docs/ARCHITECTURE.md
// section 18.5).
func addressees(rec *record, text, self string) (to []string, member bool) {
	seen := map[string]bool{}
	rest := text
	for {
		rest = strings.TrimLeft(rest, " \t\r\n*_>")
		if !strings.HasPrefix(rest, "@") {
			return to, member
		}
		rest = rest[1:]
		n := 0
		for n < len(rest) && handleByte(rest[n]) {
			n++
		}
		word := strings.ToLower(rest[:n])
		rest = strings.TrimLeft(rest[n:], ",:;*_")
		h := word
		if m := rec.member(h); m == nil && h != HandleOperator {
			// A sentence can end right after the handle.
			h = strings.TrimRight(h, ".")
		}
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		if h == HandleOperator {
			to = append(to, h)
			continue
		}
		if m := rec.member(h); m != nil && !m.Fired && h != self {
			to, member = append(to, h), true
		}
	}
}

// handleByte reports whether a byte can be part of a handle, in either
// case, because a model can write the handle with a capital first.
func handleByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}

// seqLines gives lines their seqs and their time.
func seqLines(rec *record, lines []Message, now time.Time) []Message {
	for i := range lines {
		lines[i].Seq, lines[i].At = rec.alloc(), now
	}
	return lines
}

// postLines appends entries to the channel in one change of the record.
func (s *Store) postLines(ctx context.Context, id string, msgs ...Message) error {
	if len(msgs) == 0 {
		return nil
	}
	now := s.now().UTC()
	var out []Message
	_, err := s.b.update(ctx, id, func(rec *record) error {
		out = append(out[:0], msgs...)
		seqLines(rec, out, now)
		return nil
	})
	if err != nil {
		s.log.Warn("bridges: a line did not reach the channel", "bridge", id, "error", err)
		return err
	}
	s.land(ctx, id, out...)
	return nil
}

// job is one piece of background work of the fan-out, which the worker
// tries again with a pause that doubles while do fails with an error that
// can go away (retryable), up to retryFor. fail runs once when the job
// gives up.
type job struct {
	name  string
	do    func(ctx context.Context) error
	fail  func(err error)
	first time.Time
	pause time.Duration
}

// deliveryJob is the job of one delivery. A delivery without a run is
// deferred: each try reads the record and the run again.
func (s *Store) deliveryJob(d delivery) *job {
	run := d.newest
	do := func(ctx context.Context) error { return s.deferred(ctx, d) }
	if d.run != nil {
		run = d.run.ID
		do = func(ctx context.Context) error { return s.send(ctx, d.run, d.content, d.from, d.pinned) }
	}
	return &job{
		name: fmt.Sprintf("message %d of %s to %s", d.seq, d.bridge, d.handle),
		do:   do,
		fail: func(err error) {
			s.log.Warn("bridges: a member did not get a message", "bridge", d.bridge, "seq", d.seq, "member", d.handle, "run", run, "error", err)
			_ = s.postLines(s.ctx, d.bridge, system(EventFailed, fmt.Sprintf("%s did not get message %d: %s", d.handle, d.seq, oneLine(strings.TrimPrefix(err.Error(), "bridges: "), 300))))
		},
	}
}

// deferred tries a delivery whose run the view did not hold when the
// message came. When the view holds the run now, the run gets the
// follow-up, by the rules of fanPlan. When the channel said that the fleet
// lost the run, the message seats the member again (seatLost). When the
// seat has a newer run, which started after the message and reads the
// channel first, or the seat is fired, nothing happens. Otherwise it fails
// with an error that can go away, so the job tries again.
func (s *Store) deferred(ctx context.Context, d delivery) error {
	fctx := s.asFront(ctx)
	rec, _, err := s.b.get(fctx, d.bridge)
	if err != nil {
		return err
	}
	m := rec.member(d.handle)
	if m == nil || m.Fired || m.newest() != d.newest {
		return nil
	}
	if r := s.runOf(fctx, d.newest); r != nil {
		switch {
		case r.State == fw.StateFailed:
			s.cannotTakeLine(ctx, d.bridge, d.handle, r)
			return nil
		case r.State == fw.StateCanceled && d.from.Kind == fw.FromAgent:
			return nil
		}
		return s.send(ctx, r, d.content, d.from, d.pinned)
	}
	if sm := rec.Seats[d.handle]; sm != nil && sm.Gone == d.newest {
		s.seatLost(ctx, d.bridge, d.handle, d.newest, d.seq)
		return nil
	}
	return fmt.Errorf("bridges: the view of the fleet does not hold the run %s", d.newest)
}

// cannotTakeLine posts the cap line of a member whose newest run failed
// (cannotTake), once for that run, as fanPlan does.
func (s *Store) cannotTakeLine(ctx context.Context, id, handle string, r *fw.Run) {
	var lines []Message
	_, err := s.b.update(ctx, id, func(rec *record) error {
		lines = nil
		mk := rec.mark(r.ID, handle)
		if mk.Limit {
			return errNoChange
		}
		mk.Limit = true
		lines = seqLines(rec, []Message{system(EventCap, cannotTake(handle, r))}, s.now().UTC())
		return nil
	})
	if err != nil {
		s.log.Warn("bridges: a line did not reach the channel", "bridge", id, "error", err)
		return
	}
	s.land(ctx, id, lines...)
}

// seatLost seats a member again for the message seq once the channel said
// that the fleet lost its newest run lost, in a change of the record of
// its own, as fanPlan seats it, unless a seating for that run went out
// within seatAgainWait (seatingOut).
func (s *Store) seatLost(ctx context.Context, id, handle, lost string, seq uint64) {
	now := s.now().UTC()
	seated := false
	rec, err := s.b.update(ctx, id, func(rec *record) error {
		seated = false
		m := rec.member(handle)
		if m == nil || m.Fired || m.newest() != lost || rec.Seats[handle] == nil || rec.Seats[handle].Gone != lost || seatingOut(rec.Seats[handle], *m, now) {
			return errNoChange
		}
		sm := rec.seat(handle)
		sm.Seatings++
		sm.Again, sm.AgainAt = lost, &now
		seated = true
		return nil
	})
	if err != nil {
		s.log.Warn("bridges: seat a member again for a message", "bridge", id, "member", handle, "error", err)
		return
	}
	if seated {
		s.placeAgain(ctx, rec, []Member{*rec.member(handle)}, map[string]string{handle: wakeInput(seq)})
	}
}

// seatJob is the job that seats the members that a message seats on new
// runs (fanPlan), together. It runs once: the change of the record that
// decided the seatings moved the marks of the seats, so a seating that
// fails gets its line and no second try.
func (s *Store) seatJob(rec *record, seats []Member, inputs map[string]string, seq uint64) *job {
	return &job{
		name: fmt.Sprintf("the seatings of message %d of %s", seq, rec.ID),
		do: func(ctx context.Context) error {
			s.placeAgain(ctx, rec, seats, inputs)
			return nil
		},
		fail: func(err error) { s.seatFailed(rec.ID, seats, err) },
	}
}

// placeAgain places seats that the bridge seats again, and gives each
// seat a line when the placement fails.
func (s *Store) placeAgain(ctx context.Context, rec *record, seats []Member, inputs map[string]string) {
	if err := s.place(ctx, rec, seats, inputs); err != nil {
		s.seatFailed(rec.ID, seats, err)
	}
}

// seatFailed posts the line of each seat that the front failed to seat
// again.
func (s *Store) seatFailed(id string, seats []Member, err error) {
	lines := make([]Message, 0, len(seats))
	for _, m := range seats {
		lines = append(lines, system(EventFailed, fmt.Sprintf("The front failed to seat %s again: %s", m.Handle, oneLine(strings.TrimPrefix(err.Error(), "bridges: "), 300))))
	}
	_ = s.postLines(s.ctx, id, lines...)
}

// submit queues a job for the workers. A full queue fails the job at once.
func (s *Store) submit(j *job) {
	if j.first.IsZero() {
		j.first = s.now()
	}
	if s.ctx.Err() != nil {
		return
	}
	select {
	case s.jobs <- j:
	default:
		if j.fail != nil {
			j.fail(errors.New("the fan-out of the front is full; the channel keeps the message"))
		}
	}
}

// work runs jobs until the store closes.
func (s *Store) work() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case j := <-s.jobs:
			s.attempt(j)
		}
	}
}

// attempt runs a job once, and plans the next try when it fails with an
// error that can go away.
func (s *Store) attempt(j *job) {
	err := j.do(s.ctx)
	if err == nil || s.ctx.Err() != nil {
		return
	}
	if !retryable(err) || s.now().Sub(j.first) >= s.retryFor {
		if j.fail != nil {
			j.fail(err)
		}
		return
	}
	if j.pause == 0 {
		j.pause = s.retryFirst
	} else {
		j.pause = min(2*j.pause, s.retryMax)
	}
	s.log.Debug("bridges: a job failed and runs again", "job", j.name, "pause", j.pause, "error", err)
	time.AfterFunc(j.pause, func() { s.submit(j) })
}

// skillError words an error of a slash command for the routes: an unknown
// skill is not found, a skill that is not for people is an invalid
// request, and a message over its bound is too large.
func skillError(err error) error {
	msg := err.Error()
	for _, e := range []error{skills.ErrUnknown, skills.ErrNotInvocable, skills.ErrTooLarge, skills.ErrUnavailable} {
		msg = strings.TrimPrefix(msg, e.Error()+": ")
	}
	switch {
	case errors.Is(err, skills.ErrUnknown):
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	case errors.Is(err, skills.ErrNotInvocable):
		return invalid("%s", msg)
	case errors.Is(err, skills.ErrTooLarge):
		return fmt.Errorf("%w: %s", ErrTooLarge, msg)
	case errors.Is(err, skills.ErrUnavailable):
		return fmt.Errorf("%w: %s", ErrUnavailable, msg)
	}
	return err
}
