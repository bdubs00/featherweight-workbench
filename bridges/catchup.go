package bridges

// This file makes each replica of the front give its own streams every new
// entry of a channel, also an entry that another replica wrote
// (docs/ARCHITECTURE.md section 18.12). A hook runs on the replica that
// wrote an entry, and the stream of bossmode relays it to the other
// replicas over the bus, without waiting and without a second try: a full
// queue, a bus that does not answer, or a subscription that failed when a
// replica started each lose the entry on the page of the other replica,
// until the page reads the channel again. The watch of each replica reads
// every open bridge anyway, so after each read it gives the hook OnCatchUp
// each entry after the last one that it gave its streams, which this
// replica did not write itself. The relay stays the fast path, and the
// page takes an entry that it already holds as it is.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// catchUpHold is how long the catch-up waits for an entry whose seq a
// change of the record gave and whose write has not landed yet, such as
// the write of another replica a moment after its change. Past it, the
// seq counts as a gap, such as a write that a crash cut.
const catchUpHold = 30 * time.Second

// catchUpMax bounds the entries of one bridge that one read gives.
const catchUpMax = 200

// feedMark is what this replica gave its streams of one channel: every
// entry before next, and the entries at or after next in fed. missingAt
// is when the catch-up first found no entry at next.
type feedMark struct {
	next      uint64
	fed       map[uint64]bool
	missingAt time.Time
}

// feeds holds the feed marks of a store, by bridge.
type feeds struct {
	mu sync.Mutex
	by map[string]*feedMark
}

// fed notes that this replica gave its streams the entry seq of a bridge
// that it wrote (land, and the entries that it changes).
func (s *Store) fed(bridge string, seq uint64) {
	s.feeds.mu.Lock()
	defer s.feeds.mu.Unlock()
	fm := s.feeds.by[bridge]
	if fm == nil || seq < fm.next {
		return
	}
	fm.fed[seq] = true
}

// catchUp gives the hook OnCatchUp the entries of a bridge up to the last
// seq of its record that this replica neither wrote nor gave before. The
// first read of a bridge starts at its end, so a start gives no history.
// An entry whose write has not landed yet waits for the next read, up to
// catchUpHold.
func (s *Store) catchUp(ctx context.Context, rec *record) {
	s.feeds.mu.Lock()
	fm := s.feeds.by[rec.ID]
	if fm == nil {
		s.feeds.by[rec.ID] = &feedMark{next: rec.LastSeq + 1, fed: map[uint64]bool{}}
		s.feeds.mu.Unlock()
		return
	}
	var want []uint64
	for seq := fm.next; seq <= rec.LastSeq && len(want) < catchUpMax; seq++ {
		if !fm.fed[seq] {
			want = append(want, seq)
		}
	}
	s.feeds.mu.Unlock()
	got := map[uint64]*Message{}
	failed := uint64(0) // the first seq whose read failed, which waits for the next read
	for _, seq := range want {
		m, _, err := s.b.entry(s.asFront(ctx), rec.ID, seq)
		if errors.Is(err, ErrNotFound) {
			continue // a write that has not landed yet, or a gap
		}
		if err != nil {
			s.log.Debug("bridges: read an entry for the catch-up", "bridge", rec.ID, "seq", seq, "error", err)
			failed = seq
			break
		}
		got[seq] = &m
	}
	var out []Message
	s.feeds.mu.Lock()
	now := s.now()
	for fm.next <= rec.LastSeq {
		seq := fm.next
		switch m := got[seq]; {
		case fm.fed[seq]:
		case m != nil:
			out = append(out, *m)
		case seq == failed:
			s.feeds.mu.Unlock()
			s.emitCatchUp(rec.ID, out)
			return
		case fm.missingAt.IsZero():
			fm.missingAt = now
			s.feeds.mu.Unlock()
			s.emitCatchUp(rec.ID, out)
			return
		case now.Sub(fm.missingAt) < catchUpHold:
			s.feeds.mu.Unlock()
			s.emitCatchUp(rec.ID, out)
			return
		}
		// The entry went out, or the seq is a gap past the hold.
		delete(fm.fed, seq)
		fm.next++
		fm.missingAt = time.Time{}
	}
	s.feeds.mu.Unlock()
	s.emitCatchUp(rec.ID, out)
}

// forgetFeeds drops the feed marks of the bridges that the watch no longer
// reads.
func (s *Store) forgetFeeds(watched map[string]bool) {
	s.feeds.mu.Lock()
	defer s.feeds.mu.Unlock()
	for id := range s.feeds.by {
		if !watched[id] {
			delete(s.feeds.by, id)
		}
	}
}

// emitCatchUp gives the OnCatchUp hooks the entries, in order.
func (s *Store) emitCatchUp(bridge string, msgs []Message) {
	if len(msgs) == 0 {
		return
	}
	hooks.mu.Lock()
	list := append([]func(*fw.Agent, string, Message){}, hooks.catchUp...)
	hooks.mu.Unlock()
	for _, m := range msgs {
		for _, fn := range list {
			fn(s.a, bridge, m)
		}
	}
}

// OnCatchUp registers a function that gets each entry of a channel that
// another replica wrote, once the watch of this replica finds it, with the
// agent of the store whose watch found it. The live stream of bossmode
// registers one in init and sends the entry to the streams of that agent
// alone, because every replica finds the entry on its own. An entry can
// reach a stream twice, by the relay of the replica that wrote it and by
// this catch-up, and the page takes the same entry as it is. The function
// must not block.
func OnCatchUp(fn func(a *fw.Agent, bridge string, m Message)) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.catchUp = append(hooks.catchUp, fn)
}
