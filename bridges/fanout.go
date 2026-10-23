package bridges

// This file appends the messages of people and agents to a channel and
// fans each one out (docs/ARCHITECTURE.md section 18.5). The addressed
// members, or every active member when a message names nobody, get the
// message as a follow-up on the newest run of the member in the bridge,
// with the sender in from (the kernel architecture, section 5.5). The
// change of the record that gives the message its seq also decides the
// deliveries under the caps, so two replicas never deliver past a cap, and
// the replica that took the post delivers, so a message is delivered once.
// A delivery runs on a bounded worker, never on the request of the poster,
// and a delivery that fails is tried again with a pause that doubles.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
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

// name is the sender in the first line of a follow-up.
func (x sender) name() string {
	if x.handle != "" {
		return x.handle
	}
	return orOperator(x.subject)
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
// that it addresses, and the entry that it replies to.
type PostSpec struct {
	Text    string            `json:"text"`
	To      []string          `json:"to,omitempty"`
	ReplyTo *uint64           `json:"replyTo,omitempty"`
	Files   []native.FileBody `json:"files,omitempty"`
}

// delivery is one follow-up of a message to the newest run of a member.
type delivery struct {
	bridge  string
	seq     uint64
	handle  string
	run     *fw.Run
	content string
	from    fw.MessageFrom
	files   []native.FileBody
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

// post appends a message of a person or an agent to the channel and fans
// it out. It returns the entry.
func (s *Store) post(ctx context.Context, id string, snd sender, spec PostSpec) (Message, error) {
	files, err := describeFiles(spec.Files)
	if err != nil {
		return Message{}, err
	}
	text := clean(spec.Text)
	switch {
	case text == "" && len(files) == 0:
		return Message{}, invalid("a message needs text or files")
	case len(text) > maxTextBytes:
		return Message{}, invalid("a message holds at most %d bytes", maxTextBytes)
	case len(spec.To) > maxTo:
		return Message{}, invalid("a message addresses at most %d members", maxTo)
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
	rec, err := s.b.update(ctx, id, func(rec *record) error {
		lines, plan = nil, nil
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
		cut, isCut := cutText(text, entryTextBytes)
		msg = newMessage(snd.from(), cut)
		msg.Files = files
		msg.Seq, msg.At, msg.Cut, msg.ReplyTo = rec.alloc(), now, isCut, spec.ReplyTo
		if to != nil {
			msg.To = to
		}
		lines, plan = fanPlan(rec, snd, msg, text, runs, now)
		if len(files) > 0 {
			// A channel keeps metadata alone. Refuse before the post when
			// a recipient cannot take the bytes, so the UI keeps its draft.
			targets := 0
			for _, m := range rec.Members {
				if m.Fired || m.Handle == snd.handle || (len(to) == 0 && m.Muted) || (len(to) > 0 && !seen[m.Handle]) {
					continue
				}
				targets++
			}
			if targets == 0 || len(plan) != targets {
				return fmt.Errorf("%w: files need a ready run for every recipient and room under the bridge caps; wait for the runs, raise the caps, or address a ready member", ErrConflict)
			}
			for i := range plan {
				plan[i].files = spec.Files
			}
		}
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
	if snd.subject != "" {
		if err := s.b.setRead(fctx, id, snd.subject, msg.Seq); err != nil {
			s.log.Warn("bridges: move the read mark of the poster", "bridge", id, "error", err)
		}
	}
	s.log.Info("bridges: a message reached the channel", "bridge", rec.ID, "seq", msg.Seq, "from", snd.name(), "deliveries", len(plan))
	return msg, nil
}

// describeFiles validates the upload before a message reaches the channel.
// The recipient kernel applies its workspace rules and sniffs the bytes too.
func describeFiles(files []native.FileBody) ([]AttachedFile, error) {
	if len(files) > fw.MaxAttachments {
		return nil, invalid("a message holds at most %d files", fw.MaxAttachments)
	}
	var out []AttachedFile
	total := 0
	for _, f := range files {
		if strings.TrimSpace(f.Name) == "" || len(f.Name) > 255 || strings.ContainsAny(f.Name, "/\\") || strings.IndexFunc(f.Name, unicode.IsControl) >= 0 {
			return nil, invalid("a file needs a name of at most 255 bytes without a path or control characters")
		}
		if len(f.Data) == 0 {
			return nil, invalid("the file %q has no bytes", f.Name)
		}
		total += len(f.Data)
		if total > fw.MaxAttachmentBytes {
			return nil, invalid("the files of a message hold at most %d bytes", fw.MaxAttachmentBytes)
		}
		out = append(out, AttachedFile{Name: f.Name, Size: int64(len(f.Data)), MediaType: fw.SniffMediaType(f.Data)})
	}
	return out, nil
}

// fanPlan decides the deliveries of a message under the caps of the
// bridge, inside the change of the record that gives the message its seq,
// and returns the system lines that the caps post, with their seqs. A cap
// delivers nothing more, posts one line, and waits: the channel holds the
// message in every case.
//
//   - maxMessages counts the messages of people and agents. Past it, no
//     message fans out until the operator raises the cap.
//   - maxFanoutPerMinute counts the deliveries of the current minute. Past
//     it, the rest of the minute delivers nothing.
//   - maxPairMessages counts the messages between two agents since the
//     last message of a person. Past it, the two get nothing more from
//     each other until a person writes or the operator raises the cap.
//
// A member whose newest run stopped at a limit of its budget gets nothing,
// and the channel says so once for that run. A member without a run, whose
// job waits in the queue, gets nothing; its instructions tell it to read
// the channel first.
func fanPlan(rec *record, snd sender, msg Message, text string, runs map[string]*fw.Run, now time.Time) (lines []Message, plan []delivery) {
	f := &rec.Fan
	if snd.handle == "" {
		// A message of a person ends every exchange between two agents.
		f.Pairs, f.PairCapped = nil, nil
	}
	f.Messages++
	if f.Messages > rec.Caps.MaxMessages {
		if !f.Capped {
			f.Capped = true
			lines = append(lines, system(EventCap, fmt.Sprintf("The bridge reached its cap of %d messages, so the front delivers no more messages to the members. Raise caps.maxMessages to go on; the channel keeps every message.", rec.Caps.MaxMessages)))
		}
		return seqLines(rec, lines, now), nil
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
		r := runs[m.newest()]
		if r == nil {
			continue // the job waits in the queue; the channel holds the message
		}
		if limited(r) {
			if mk := rec.mark(r.ID, m.Handle); !mk.Limit {
				mk.Limit = true
				lines = append(lines, system(EventCap, fmt.Sprintf("%s cannot take messages: its run stopped at a limit (%s). It needs a larger budget.", m.Handle, r.Error)))
			}
			continue
		}
		if r.State == fw.StateCanceled {
			continue // a message starts a canceled run again
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
					lines = append(lines, system(EventCap, fmt.Sprintf("%s and %s exchanged %d messages without a message of a person, so the front delivers no more between them. Write in the channel to go on, or raise caps.maxPairMessages.", snd.handle, m.Handle, rec.Caps.MaxPairMessages)))
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
		plan = append(plan, delivery{bridge: rec.ID, seq: msg.Seq, handle: m.Handle, run: r, content: head + text, from: snd.party(m.Handle)})
	}
	if len(minuteMissed) > 0 && !f.MinuteCapped.Equal(minute) {
		f.MinuteCapped = minute
		lines = append(lines, system(EventCap, fmt.Sprintf("The bridge reached its cap of %d deliveries a minute, so %s did not get message %d. The channel keeps it; the members read it with bridge_read. Raise caps.maxFanoutPerMinute to deliver more.", rec.Caps.MaxFanoutPerMinute, strings.Join(minuteMissed, ", "), msg.Seq)))
	}
	return seqLines(rec, lines, now), plan
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

// deliveryJob is the job of one delivery.
func (s *Store) deliveryJob(d delivery) *job {
	return &job{
		name: fmt.Sprintf("message %d of %s to %s", d.seq, d.bridge, d.handle),
		do:   func(ctx context.Context) error { return s.send(ctx, d.run, d.content, d.from, d.files...) },
		fail: func(err error) {
			s.log.Warn("bridges: a member did not get a message", "bridge", d.bridge, "seq", d.seq, "member", d.handle, "run", d.run.ID, "error", err)
			_ = s.postLines(s.ctx, d.bridge, system(EventFailed, fmt.Sprintf("%s did not get message %d: %s.", d.handle, d.seq, oneLine(strings.TrimPrefix(err.Error(), "bridges: "), 300))))
		},
	}
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
