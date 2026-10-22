package bridges

// This file keeps the records and the channels in two key-value buckets
// (internal/bucket). Every write is a compare-and-swap on the revision that
// the writer read, so two replicas of the front never lose a change: the
// loser reads again and decides again. A new entry takes its seq from a
// change of the record, which moves LastSeq, and then lands under its own
// key, so two replicas never give two entries one seq.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// Errors of the package. A route answers ErrNotFound with 404 and
// ErrConflict with 409; an error that wraps fw.ErrInvalidRequest answers
// 400, and fw.ErrForbidden 403.
var (
	ErrNotFound = errors.New("bridges: not found")
	ErrConflict = errors.New("bridges: conflict")
	// ErrTooLarge is a slash command whose message passes its bound.
	ErrTooLarge = errors.New("bridges: too large")
)

// casAttempts bounds the tries of a change that other writers race.
const casAttempts = 16

// idPattern is the shape of the id of a bridge: a key of a bucket.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// checkID reports whether the id is the id of a bridge.
func checkID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: bridges: %q is not the id of a bridge", fw.ErrInvalidRequest, id)
	}
	return nil
}

// buckets holds the two buckets of the bridges.
type buckets struct {
	records  bucket.Bucket
	channels bucket.Bucket

	// summaries holds what never changes of an entry, the kind and the
	// name of the sender, by key, so a count of the unread entries reads
	// an entry once per process.
	mu        sync.Mutex
	summaries map[string]summary
}

// summary is what never changes of an entry.
type summary struct {
	kind, name string
}

// maxSummaries bounds the summaries that a process keeps.
const maxSummaries = 100000

// get reads a record with its revision.
func (b *buckets) get(ctx context.Context, id string) (*record, uint64, error) {
	if err := checkID(id); err != nil {
		return nil, 0, err
	}
	e, err := b.records.Get(ctx, id)
	if errors.Is(err, bucket.ErrNotFound) {
		return nil, 0, fmt.Errorf("%w: no bridge has the id %q", ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("bridges: read the bridge %s: %w", id, err)
	}
	var rec record
	if err := json.Unmarshal(e.Value, &rec); err != nil {
		return nil, 0, fmt.Errorf("bridges: the record of the bridge %s does not decode: %w", id, err)
	}
	return &rec, e.Revision, nil
}

// create writes a new record.
func (b *buckets) create(ctx context.Context, rec *record) error {
	v, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("bridges: encode the bridge %s: %w", rec.ID, err)
	}
	if _, err := b.records.Create(ctx, rec.ID, v); err != nil {
		if errors.Is(err, bucket.ErrConflict) {
			return fmt.Errorf("%w: a bridge with the id %s exists", ErrConflict, rec.ID)
		}
		return fmt.Errorf("bridges: write the bridge %s: %w", rec.ID, err)
	}
	return nil
}

// errNoChange tells update that the change wants no write.
var errNoChange = errors.New("bridges: no change")

// update changes a record with a compare-and-swap. fn changes the record
// that it gets, and it can run more than once when another writer comes
// first, each time on a fresh read. When fn returns errNoChange, update
// writes nothing and returns the record as it is with no error.
func (b *buckets) update(ctx context.Context, id string, fn func(rec *record) error) (*record, error) {
	for try := 0; try < casAttempts; try++ {
		rec, rev, err := b.get(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := fn(rec); err != nil {
			if errors.Is(err, errNoChange) {
				return rec, nil
			}
			return nil, err
		}
		v, err := json.Marshal(rec)
		if err != nil {
			return nil, fmt.Errorf("bridges: encode the bridge %s: %w", id, err)
		}
		_, err = b.records.Update(ctx, id, v, rev)
		if errors.Is(err, bucket.ErrConflict) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("bridges: write the bridge %s: %w", id, err)
		}
		return rec, nil
	}
	return nil, fmt.Errorf("bridges: the bridge %s changed %d times while the front wrote it; try again", id, casAttempts)
}

// list reads every record, in no order.
func (b *buckets) list(ctx context.Context) ([]*record, error) {
	keys, err := b.records.Keys(ctx)
	if err != nil {
		return nil, fmt.Errorf("bridges: list the bridges: %w", err)
	}
	out := make([]*record, 0, len(keys))
	for _, k := range keys {
		rec, _, err := b.get(ctx, k.Key)
		if errors.Is(err, ErrNotFound) {
			continue // removed between the list and the read
		}
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// putEntry writes a new entry of the channel under its seq.
func (b *buckets) putEntry(ctx context.Context, id string, m Message) error {
	v, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("bridges: encode entry %d of the bridge %s: %w", m.Seq, id, err)
	}
	if _, err := b.channels.Create(ctx, seqKey(id, m.Seq), v); err != nil {
		return fmt.Errorf("bridges: write entry %d of the bridge %s: %w", m.Seq, id, err)
	}
	b.remember(seqKey(id, m.Seq), m)
	return nil
}

// entry reads one entry with its revision. A seq without an entry, such
// as the seq of a write that a crash cut, is ErrNotFound.
func (b *buckets) entry(ctx context.Context, id string, seq uint64) (Message, uint64, error) {
	e, err := b.channels.Get(ctx, seqKey(id, seq))
	if errors.Is(err, bucket.ErrNotFound) {
		return Message{}, 0, fmt.Errorf("%w: the bridge %s has no entry %d", ErrNotFound, id, seq)
	}
	if err != nil {
		return Message{}, 0, fmt.Errorf("bridges: read entry %d of the bridge %s: %w", seq, id, err)
	}
	var m Message
	if err := json.Unmarshal(e.Value, &m); err != nil {
		return Message{}, 0, fmt.Errorf("bridges: entry %d of the bridge %s does not decode: %w", seq, id, err)
	}
	if m.Reactions == nil {
		m.Reactions = emptyReactions()
	}
	if m.To == nil {
		m.To = []string{}
	}
	b.remember(e.Key, m)
	return m, e.Revision, nil
}

// changeEntry changes an entry with a compare-and-swap. fn returns false
// when it changes nothing, and then changeEntry writes nothing.
func (b *buckets) changeEntry(ctx context.Context, id string, seq uint64, fn func(m *Message) bool) (Message, bool, error) {
	for try := 0; try < casAttempts; try++ {
		m, rev, err := b.entry(ctx, id, seq)
		if err != nil {
			return Message{}, false, err
		}
		if !fn(&m) {
			return m, false, nil
		}
		v, err := json.Marshal(m)
		if err != nil {
			return Message{}, false, fmt.Errorf("bridges: encode entry %d of the bridge %s: %w", seq, id, err)
		}
		_, err = b.channels.Update(ctx, seqKey(id, seq), v, rev)
		if errors.Is(err, bucket.ErrConflict) {
			continue
		}
		if err != nil {
			return Message{}, false, fmt.Errorf("bridges: write entry %d of the bridge %s: %w", seq, id, err)
		}
		return m, true, nil
	}
	return Message{}, false, fmt.Errorf("bridges: entry %d of the bridge %s changed %d times while the front wrote it; try again", seq, id, casAttempts)
}

// remember keeps the summary of an entry.
func (b *buckets) remember(key string, m Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.summaries == nil || len(b.summaries) >= maxSummaries {
		b.summaries = map[string]summary{}
	}
	b.summaries[key] = summary{kind: m.From.Kind, name: m.From.Name}
}

// summaryOf returns the summary of an entry, from memory or from the
// bucket. A seq without an entry reports false.
func (b *buckets) summaryOf(ctx context.Context, id string, seq uint64) (summary, bool, error) {
	key := seqKey(id, seq)
	b.mu.Lock()
	s, ok := b.summaries[key]
	b.mu.Unlock()
	if ok {
		return s, true, nil
	}
	m, _, err := b.entry(ctx, id, seq)
	if errors.Is(err, ErrNotFound) {
		return summary{}, false, nil
	}
	if err != nil {
		return summary{}, false, err
	}
	return summary{kind: m.From.Kind, name: m.From.Name}, true, nil
}

// Bounds of a page of the channel.
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 500
)

// Page is one page of the channel, as GET /v1/bridges/{id}/messages
// answers it. Prev is the value of before for the page in front, and 0 at
// the first page. Next is the seq to read on from, and 0 when no entry
// follows the page.
type Page struct {
	Bridge   string    `json:"bridge"`
	Messages []Message `json:"messages"`
	Next     uint64    `json:"next"`
	Prev     uint64    `json:"prev"`
}

// page reads one page of the channel of a record, as the transcript route
// pages a run (docs/ARCHITECTURE.md section 2.2). Without from and before,
// it holds the last limit entries. before reads the limit entries before
// that seq, and from reads forward from that seq. A seq without an entry
// is left out.
func (b *buckets) page(ctx context.Context, rec *record, from, before uint64, limit int) (Page, error) {
	if from > 0 && before > 0 {
		return Page{}, fmt.Errorf("%w: from and before together are not allowed", fw.ErrInvalidRequest)
	}
	if limit <= 0 {
		limit = DefaultPageLimit
	}
	limit = min(limit, MaxPageLimit)
	last := rec.LastSeq
	out := Page{Bridge: rec.ID, Messages: []Message{}}
	var lo, hi uint64 // the seqs of the page, lo to hi inclusive
	switch {
	case from > 0:
		lo, hi = from, min(last, from+uint64(limit)-1)
		if hi < last {
			out.Next = hi + 1
		}
	default:
		end := last
		if before > 0 {
			end = min(last, before-1)
			if before <= last {
				out.Next = before
			}
		}
		hi = end
		if end >= uint64(limit) {
			lo = end - uint64(limit) + 1
		} else {
			lo = 1
		}
	}
	if lo == 0 {
		lo = 1
	}
	for seq := lo; seq <= hi && hi > 0; seq++ {
		m, _, err := b.entry(ctx, rec.ID, seq)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return Page{}, err
		}
		out.Messages = append(out.Messages, m)
	}
	if lo > 1 && hi > 0 {
		out.Prev = lo
	}
	return out, nil
}

// tail returns the last limit entries of a record after the seq since.
func (b *buckets) tail(ctx context.Context, rec *record, since uint64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = DefaultPageLimit
	}
	limit = min(limit, MaxPageLimit)
	lo := since + 1
	if rec.LastSeq >= uint64(limit) && rec.LastSeq-uint64(limit)+1 > lo {
		lo = rec.LastSeq - uint64(limit) + 1
	}
	out := []Message{}
	for seq := lo; seq <= rec.LastSeq; seq++ {
		m, _, err := b.entry(ctx, rec.ID, seq)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// readKey is the key of the read mark of a person: <bridge>.read.<the
// SHA-256 of the subject>, so no subject enters a key.
func readKey(id, subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return id + ".read." + hex.EncodeToString(sum[:])
}

// readMark is the value of a read mark.
type readMark struct {
	Seq uint64 `json:"seq"`
}

// readOf returns the read mark of a person, 0 without one.
func (b *buckets) readOf(ctx context.Context, id, subject string) (uint64, error) {
	if subject == "" {
		return 0, nil
	}
	e, err := b.channels.Get(ctx, readKey(id, subject))
	if errors.Is(err, bucket.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("bridges: read the read mark of the bridge %s: %w", id, err)
	}
	var m readMark
	if json.Unmarshal(e.Value, &m) != nil {
		return 0, nil
	}
	return m.Seq, nil
}

// setRead moves the read mark of a person to seq. A smaller seq than the
// mark leaves the mark.
func (b *buckets) setRead(ctx context.Context, id, subject string, seq uint64) error {
	if subject == "" {
		return nil
	}
	key := readKey(id, subject)
	v, _ := json.Marshal(readMark{Seq: seq})
	for try := 0; try < casAttempts; try++ {
		e, err := b.channels.Get(ctx, key)
		switch {
		case errors.Is(err, bucket.ErrNotFound):
			_, err = b.channels.Create(ctx, key, v)
		case err != nil:
			return fmt.Errorf("bridges: read the read mark of the bridge %s: %w", id, err)
		default:
			var m readMark
			if json.Unmarshal(e.Value, &m) == nil && m.Seq >= seq {
				return nil
			}
			_, err = b.channels.Update(ctx, key, v, e.Revision)
		}
		if errors.Is(err, bucket.ErrConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("bridges: write the read mark of the bridge %s: %w", id, err)
		}
		return nil
	}
	return fmt.Errorf("bridges: the read mark of the bridge %s changed %d times while the front wrote it", id, casAttempts)
}

// unreadScan bounds the entries that one count of the unread entries of a
// bridge reads, newest first. Entries further back count as unread.
const unreadScan = 2000

// unread counts the entries after read that count for the person subject:
// a message of a person or an agent other than the caller, and a card that
// waits for a decision. A message of the caller and a plain system line do
// not count.
func (b *buckets) unread(ctx context.Context, rec *record, subject string, read uint64) (int, error) {
	n := 0
	scanned := 0
	for seq := rec.LastSeq; seq > read; seq-- {
		if scanned >= unreadScan {
			return n + int(seq-read), nil
		}
		scanned++
		s, ok, err := b.summaryOf(ctx, rec.ID, seq)
		if err != nil {
			return 0, err
		}
		switch {
		case !ok:
		case s.kind == KindSystem:
			if rec.waits(seq) {
				n++
			}
		case s.kind == KindPerson && s.name == subject:
		default:
			n++
		}
	}
	return n, nil
}

// sortItems puts the open bridges first, the newest first in each group.
func sortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		oi, oj := items[i].State == StateOpen, items[j].State == StateOpen
		if oi != oj {
			return oi
		}
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID > items[j].ID
	})
}

// cutText cuts a text to n bytes at a rune boundary and reports whether
// it cut.
func cutText(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	i := n
	for i > 0 && !utf8Start(s[i]) {
		i--
	}
	return s[:i], true
}

func utf8Start(c byte) bool { return c&0xC0 != 0x80 }

// oneLine turns a text into one line of at most n bytes, for a system
// line.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if t, cut := cutText(s, n); cut {
		return t + "…"
	}
	return s
}
