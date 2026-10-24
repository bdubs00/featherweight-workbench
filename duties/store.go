package duties

// This file is the store of the duties (docs/ARCHITECTURE.md section
// 22.4). It keeps three buckets (package internal/bucket), so on a NATS
// bus every replica of the front reads one truth, and on the bus mem they
// live in the memory of the process:
//
//   - <stream>_DUTIES holds each duty under duty.<name>, the counters of
//     its engine under state.<name>, and the switch that pauses every duty
//     under switch. Every write of a duty is a compare-and-swap on the
//     revision that the writer read, as for the runtime pools.
//   - <stream>_DUTY_RUNS holds the history: one entry per duty and day,
//     <name>.<date>, with each start of that day. The bucket forgets an
//     entry 30 days after its last write. A replica claims a time of a
//     duty by adding it to the entry of its day with a compare-and-swap,
//     so of two replicas that claim one time, one wins.
//   - <stream>_DUTY_SEEN holds the keys that the checks of a duty saw,
//     under check.<name>, and the keys of the calls of its webhook, under
//     call.<name>.<hash>, for 30 days.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// HistoryFor is how long the history and the seen keys of a duty stay.
const HistoryFor = 30 * 24 * time.Hour

// The buckets of the duties.
var (
	dutiesBucket = bucket.Config{
		Name:        "DUTIES",
		Description: "The duties of a featherweight fleet: each duty under duty.<name>, the counters of its engine under state.<name>, and the switch that pauses every duty.",
		History:     10,
		MaxValue:    256 << 10,
	}
	runsBucket = bucket.Config{
		Name:        "DUTY_RUNS",
		Description: "The history of the duties of a featherweight fleet: one entry per duty and day under <name>.<date>, with each start of that day, for 30 days.",
		History:     1,
		MaxValue:    768 << 10,
		TTL:         HistoryFor,
	}
	seenBucket = bucket.Config{
		Name:        "DUTY_SEEN",
		Description: "The keys that the checks of the duties of a featherweight fleet saw, under check.<name>, and the keys of the calls of their webhooks, under call.<name>.<hash>, for 30 days.",
		History:     1,
		MaxValue:    768 << 10,
		TTL:         HistoryFor,
	}
)

// Bounds of the store.
const (
	// storeAttempts bounds the tries of a write that other writers race.
	storeAttempts = 8
	// maxDayEntries is the most starts that the entry of one day keeps.
	// Beyond it, the oldest ended starts without a run go first.
	maxDayEntries = 400
	// maxSeen is the most keys that the checks of one duty keep.
	maxSeen = 20000
)

// Errors of the store.
var (
	// ErrNotFound means that no duty has the name.
	ErrNotFound = errors.New("duties: not found")
	// ErrExists means that a duty of the name exists.
	ErrExists = errors.New("duties: exists")
	// ErrConflict means that the duty changed since the writer read it.
	ErrConflict = errors.New("duties: the duty changed since it was read")
)

// stored is the entry of a duty in the bucket: the record and what the
// front keeps beside it. OwnerKind is the kind of the principal of the
// owner. PausedReason says why the duty paused itself. Since is the moment
// from which the engine counts the times of the schedule: the create, the
// last change of when, or the last resume. rev is the revision of the
// entry.
type stored struct {
	Duty         Duty      `json:"duty"`
	OwnerKind    string    `json:"ownerKind,omitempty"`
	PausedReason string    `json:"pausedReason,omitempty"`
	Since        time.Time `json:"since"`
	Created      time.Time `json:"created"`
	Changed      time.Time `json:"changed"`
	ChangedBy    string    `json:"changedBy,omitempty"`

	rev uint64
}

// Switch is the kill switch: Paused pauses every duty. At is the time of
// the last change, by By, with Reason.
type Switch struct {
	Paused bool      `json:"paused"`
	Reason string    `json:"reason,omitempty"`
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at,omitzero"`
}

// counters are the counters of the engine of one duty.
type counters struct {
	FailuresInRow int    `json:"failuresInRow"`
	ChecksFailed  int    `json:"checksFailed"`
	CapDay        string `json:"capDay,omitempty"` // the day whose cap the owner heard of
}

// RunEntry is one start of a duty in its history: a run, or a time that
// started none and why. At is when the front handled the start, Slot the
// time of the schedule or of the start, Run the run of the agent, State
// one of the State constants, CostUSD the cost of every attempt, Found the
// items that the check found, Woke whether they woke the agent, Reason the
// why in words, Start the kind of start, Attempt the attempt of the run,
// Title the title of the run, and Decided the decisions about the
// approvals and the questions of its run, oldest first.
type RunEntry struct {
	At      time.Time `json:"at"`
	Slot    time.Time `json:"slot"`
	Run     string    `json:"run"`
	State   string    `json:"state"`
	CostUSD float64   `json:"costUSD"`
	Found   int       `json:"found"`
	Woke    bool      `json:"woke"`
	Reason  string    `json:"reason"`
	Start   string    `json:"start,omitempty"`
	Attempt int       `json:"attempt,omitempty"`
	Title   string    `json:"title,omitempty"`
	Decided []Decided `json:"decided,omitempty"`
}

// Decided is one decision about an approval or a question of the run of a
// start: when, by whom (DecidedByManager, DecidedByTimeout, or the subject
// of a person), the verdict (allow, deny, or cancel), the tool of the
// call, and the reason in words.
type Decided struct {
	At      time.Time `json:"at"`
	By      string    `json:"by"`
	Verdict string    `json:"verdict"`
	Tool    string    `json:"tool,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

// Deciders of a decision that no person made.
const (
	DecidedByManager = "manager" // the front applied the verdict of the manager
	DecidedByTimeout = "timeout" // nobody decided within whileAway.timeout
)

// maxDecided is the most decisions that one start keeps.
const maxDecided = 20

// States of a start. A start that works is starting, waiting, retrying,
// or in a state of its run; the others have ended.
const (
	StateStarting    = "starting"     // a replica claimed it and starts it
	StateWaiting     = "waiting"      // it waits for the run that works, with overlap queue
	StateRetrying    = "retrying"     // its run failed, and it goes again at RetryAt
	StateQueued      = "queued"       // its run is on the queue
	StateCompleted   = "completed"    // its run completed
	StateFailed      = "failed"       // its run failed after its retries, or it did not start
	StateCanceled    = "canceled"     // somebody canceled its run
	StateSkipped     = "skipped"      // it did not start, and Reason says why
	StateMissed      = "missed"       // the front did not run at the time, older than catchUp
	StateQuiet       = "quiet"        // the check found no work
	StateCheckFailed = "check-failed" // the check failed, so the model did not wake
	StateLost        = "lost"         // no worker reported its run, and the front stopped waiting
)

// ended reports whether a state is final.
func ended(state string) bool {
	switch state {
	case StateCompleted, StateFailed, StateCanceled, StateSkipped, StateMissed, StateQuiet, StateCheckFailed, StateLost:
		return true
	}
	return false
}

// working reports whether a start holds a run that works: it has not
// ended, and it does not wait.
func working(state string) bool { return !ended(state) && state != StateWaiting }

// entry is a start in the entry of its day: the public part and what the
// engine needs. ID is the slot in RFC 3339 with nanoseconds, unique in the
// day. SpentUSD is the cost of the earlier attempts. FoundText and Event
// hold the text of {found} and {event} while a retry can need them.
// Person ran it now. By is the replica that started its last attempt.
// Asked is true once the owner heard of a question of its run. RetryAt is
// the time of the next attempt. Chain is the depth of a start in a chain
// of duties that events started, for the metadata MetaChain of each
// attempt.
type entry struct {
	RunEntry
	ID        string    `json:"id"`
	SpentUSD  float64   `json:"spentUSD,omitempty"`
	FoundText string    `json:"foundText,omitempty"`
	Event     string    `json:"event,omitempty"`
	Person    string    `json:"person,omitempty"`
	By        string    `json:"by,omitempty"`
	Asked     bool      `json:"asked,omitempty"`
	Chain     int       `json:"chain,omitempty"`
	RetryAt   time.Time `json:"retryAt,omitzero"`
	Updated   time.Time `json:"updated"`
}

// day is the history of one duty on one day, oldest first. Dropped counts
// the starts that the day left out for room.
type day struct {
	Entries []entry `json:"entries"`
	Dropped int     `json:"dropped,omitempty"`
}

// find returns the start of the id, or nil.
func (d *day) find(id string) *entry {
	for i := range d.Entries {
		if d.Entries[i].ID == id {
			return &d.Entries[i]
		}
	}
	return nil
}

// add appends a start and keeps the day within maxDayEntries: the oldest
// ended start without a run goes first, then the oldest ended start.
func (d *day) add(e entry) {
	d.Entries = append(d.Entries, e)
	for len(d.Entries) > maxDayEntries {
		drop := -1
		for i, x := range d.Entries {
			if ended(x.State) && x.Run == "" {
				drop = i
				break
			}
		}
		if drop < 0 {
			for i, x := range d.Entries {
				if ended(x.State) {
					drop = i
					break
				}
			}
		}
		if drop < 0 {
			drop = 0
		}
		d.Entries = slices.Delete(d.Entries, drop, drop+1)
		d.Dropped++
	}
}

// spent returns the cost of the starts of the day.
func (d *day) spent() float64 {
	sum := 0.0
	for _, e := range d.Entries {
		sum += e.CostUSD
	}
	return usd(sum)
}

// seenKeys are the keys that the checks of a duty saw, with the time of
// the check that saw each last. By names the start that woke the agent
// for each key, so a start that fails for good gives its keys back.
type seenKeys struct {
	Keys map[string]time.Time `json:"keys"`
	By   map[string]string    `json:"by,omitempty"`
}

// Store keeps the duties in their buckets. Build it with NewStore.
type Store struct {
	duties, runs, seen bucket.Bucket
	now                func() time.Time
	log                *slog.Logger
}

// NewStore returns the store on the three buckets.
func NewStore(duties, runs, seen bucket.Bucket, log *slog.Logger) *Store {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Store{duties: duties, runs: runs, seen: seen, now: time.Now, log: log}
}

// Describe names the place of the store for a person.
func (s *Store) Describe() string { return s.duties.Describe() }

func dutyKey(name string) string  { return "duty." + name }
func stateKey(name string) string { return "state." + name }
func dayKey(name, date string) string {
	return name + "." + date
}

const switchKey = "switch"

// dateOf returns the date of t in loc, as the key of a day.
func dateOf(t time.Time, loc *time.Location) string { return t.In(loc).Format(time.DateOnly) }

// get returns the duty of the name, or an error that wraps ErrNotFound.
func (s *Store) get(ctx context.Context, name string) (*stored, error) {
	e, err := s.duties.Get(ctx, dutyKey(name))
	if errors.Is(err, bucket.ErrNotFound) {
		return nil, fmt.Errorf("%w: no duty is named %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("duties: read the duty %s: %w", name, err)
	}
	return decodeStored(e)
}

func decodeStored(e bucket.Entry) (*stored, error) {
	var st stored
	if err := json.Unmarshal(e.Value, &st); err != nil {
		return nil, fmt.Errorf("duties: decode the duty %s: %w", strings.TrimPrefix(e.Key, "duty."), err)
	}
	st.rev = e.Revision
	return &st, nil
}

// list returns every duty, sorted by name. An entry that does not decode
// is left out, and the log names it.
func (s *Store) list(ctx context.Context) ([]*stored, error) {
	keys, err := bucket.KeysUnder(ctx, s.duties, "duty")
	if err != nil {
		return nil, fmt.Errorf("duties: list the duties in %s: %w", s.duties.Describe(), err)
	}
	out := []*stored{}
	for _, k := range keys {
		e, err := s.duties.Get(ctx, k)
		if errors.Is(err, bucket.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("duties: read a duty: %w", err)
		}
		st, err := decodeStored(e)
		if err != nil {
			s.log.Warn("duties: an entry of the duties does not decode; the front leaves it out", "key", k, "error", err)
			continue
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Duty.Name < out[j].Duty.Name })
	return out, nil
}

// create writes a new duty, or an error that wraps ErrExists.
func (s *Store) create(ctx context.Context, st *stored) error {
	value, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("duties: encode the duty %s: %w", st.Duty.Name, err)
	}
	rev, err := s.duties.Create(ctx, dutyKey(st.Duty.Name), value)
	if errors.Is(err, bucket.ErrConflict) {
		return fmt.Errorf("%w: a duty is named %s already", ErrExists, st.Duty.Name)
	}
	if err != nil {
		return fmt.Errorf("duties: write the duty %s: %w", st.Duty.Name, err)
	}
	st.rev = rev
	return nil
}

// update reads the duty, applies f, and writes the result at the revision
// that it read. With rev, the duty must still have that revision, else the
// error wraps ErrConflict. Without rev, a write that another writer beat
// reads again and applies f again. An error of f comes back as it is.
func (s *Store) update(ctx context.Context, name string, rev uint64, f func(*stored) error) (*stored, error) {
	for range storeAttempts {
		cur, err := s.get(ctx, name)
		if err != nil {
			return nil, err
		}
		if rev != 0 && cur.rev != rev {
			return nil, fmt.Errorf("%w: the duty %s has the revision %d, and the write names %d; read it again", ErrConflict, name, cur.rev, rev)
		}
		if err := f(cur); err != nil {
			return nil, err
		}
		value, err := json.Marshal(cur)
		if err != nil {
			return nil, fmt.Errorf("duties: encode the duty %s: %w", name, err)
		}
		next, err := s.duties.Update(ctx, dutyKey(name), value, cur.rev)
		if errors.Is(err, bucket.ErrConflict) {
			if rev != 0 {
				return nil, fmt.Errorf("%w: another writer changed the duty %s; read it again", ErrConflict, name)
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("duties: write the duty %s: %w", name, err)
		}
		cur.rev = next
		return cur, nil
	}
	return nil, fmt.Errorf("%w: other writers changed the duty %s %d times while this write tried; try again", ErrConflict, name, storeAttempts)
}

// remove removes the duty at the revision rev, with its counters and the
// keys that its checks saw. Its history stays until the bucket forgets it.
func (s *Store) remove(ctx context.Context, name string, rev uint64) error {
	err := s.duties.Delete(ctx, dutyKey(name), rev)
	if errors.Is(err, bucket.ErrConflict) {
		if _, gerr := s.get(ctx, name); errors.Is(gerr, ErrNotFound) {
			return fmt.Errorf("%w: no duty is named %s", ErrNotFound, name)
		}
		return fmt.Errorf("%w: the duty %s changed since the front read it; try again", ErrConflict, name)
	}
	if err != nil {
		return fmt.Errorf("duties: remove the duty %s: %w", name, err)
	}
	s.drop(ctx, s.duties, stateKey(name))
	s.drop(ctx, s.seen, "check."+name)
	return nil
}

// drop deletes a key of a bucket when it holds a value.
func (s *Store) drop(ctx context.Context, b bucket.Bucket, key string) {
	e, err := b.Get(ctx, key)
	if err != nil {
		return
	}
	if err := b.Delete(ctx, key, e.Revision); err != nil {
		s.log.Warn("duties: a key of a removed duty stays", "key", key, "error", err)
	}
}

// killSwitch reads the switch that pauses every duty. A store without the
// switch has it off.
func (s *Store) killSwitch(ctx context.Context) (Switch, error) {
	var sw Switch
	e, err := s.duties.Get(ctx, switchKey)
	if errors.Is(err, bucket.ErrNotFound) {
		return sw, nil
	}
	if err != nil {
		return sw, fmt.Errorf("duties: read the switch: %w", err)
	}
	if err := json.Unmarshal(e.Value, &sw); err != nil {
		return sw, fmt.Errorf("duties: decode the switch: %w", err)
	}
	return sw, nil
}

// setSwitch writes the switch.
func (s *Store) setSwitch(ctx context.Context, sw Switch) error {
	value, err := json.Marshal(sw)
	if err != nil {
		return err
	}
	_, err = bucket.Modify(ctx, s.duties, switchKey, storeAttempts, func([]byte, bool) ([]byte, error) { return value, nil })
	if err != nil {
		return fmt.Errorf("duties: write the switch: %w", err)
	}
	return nil
}

// counters reads the counters of a duty.
func (s *Store) counters(ctx context.Context, name string) (counters, error) {
	var c counters
	e, err := s.duties.Get(ctx, stateKey(name))
	if errors.Is(err, bucket.ErrNotFound) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("duties: read the counters of %s: %w", name, err)
	}
	if err := json.Unmarshal(e.Value, &c); err != nil {
		return counters{}, nil
	}
	return c, nil
}

// modifyCounters applies f to the counters of a duty and writes them when
// f reports a change. It returns the counters after f.
func (s *Store) modifyCounters(ctx context.Context, name string, f func(*counters) bool) (counters, error) {
	var out counters
	_, err := bucket.Modify(ctx, s.duties, stateKey(name), storeAttempts, func(value []byte, exists bool) ([]byte, error) {
		var c counters
		if exists {
			_ = json.Unmarshal(value, &c)
		}
		changed := f(&c)
		out = c
		if !changed {
			return nil, nil
		}
		return json.Marshal(c)
	})
	if err != nil {
		return out, fmt.Errorf("duties: write the counters of %s: %w", name, err)
	}
	return out, nil
}

// day reads the history of a duty on a date. A date without history has
// none.
func (s *Store) day(ctx context.Context, name, date string) (day, error) {
	var d day
	e, err := s.runs.Get(ctx, dayKey(name, date))
	if errors.Is(err, bucket.ErrNotFound) {
		return d, nil
	}
	if err != nil {
		return d, fmt.Errorf("duties: read the history of %s on %s: %w", name, date, err)
	}
	if err := json.Unmarshal(e.Value, &d); err != nil {
		return day{}, fmt.Errorf("duties: decode the history of %s on %s: %w", name, date, err)
	}
	return d, nil
}

// modifyDay applies f to the history of a duty on a date and writes it
// when f reports a change, at the revision that it read. It reports
// whether it wrote.
func (s *Store) modifyDay(ctx context.Context, name, date string, f func(*day) bool) (bool, error) {
	wrote, err := bucket.Modify(ctx, s.runs, dayKey(name, date), storeAttempts, func(value []byte, exists bool) ([]byte, error) {
		var d day
		if exists {
			if err := json.Unmarshal(value, &d); err != nil {
				return nil, fmt.Errorf("decode the history: %w", err)
			}
		}
		if !f(&d) {
			return nil, nil
		}
		return json.Marshal(d)
	})
	if err != nil {
		return false, fmt.Errorf("duties: write the history of %s on %s: %w", name, date, err)
	}
	return wrote, nil
}

// claim adds a start to the history of its day when the day has no start
// of its id. It reports false when another replica claimed the id first.
func (s *Store) claim(ctx context.Context, name, date string, e entry) (bool, error) {
	return s.modifyDay(ctx, name, date, func(d *day) bool {
		if d.find(e.ID) != nil {
			return false
		}
		d.add(e)
		return true
	})
}

// setEntry applies f to the start id of a day while the start is in one
// of the states from; f reports whether it changed the start. It returns
// the start after f and whether it wrote. A start that left those states
// in the meantime is left as it is, so only one replica moves a start.
func (s *Store) setEntry(ctx context.Context, name, date, id string, from []string, f func(*entry) bool) (entry, bool, error) {
	var out entry
	wrote, err := s.modifyDay(ctx, name, date, func(d *day) bool {
		e := d.find(id)
		if e == nil || (from != nil && !slices.Contains(from, e.State)) {
			return false
		}
		if !f(e) {
			return false
		}
		e.Updated = s.now().UTC()
		out = *e
		return true
	})
	return out, wrote, err
}

// addDecided adds a decision to the start of a duty whose run is run, in
// the newest of the dates that holds that start. It reports false when no
// start of those dates has the run. A start keeps its newest maxDecided
// decisions.
func (s *Store) addDecided(ctx context.Context, name, run string, dates []string, d Decided) (bool, error) {
	for i := len(dates) - 1; i >= 0; i-- {
		wrote, err := s.modifyDay(ctx, name, dates[i], func(x *day) bool {
			for j := range x.Entries {
				e := &x.Entries[j]
				if e.Run != run {
					continue
				}
				e.Decided = append(e.Decided, d)
				if over := len(e.Decided) - maxDecided; over > 0 {
					e.Decided = slices.Delete(e.Decided, 0, over)
				}
				e.Updated = s.now().UTC()
				return true
			}
			return false
		})
		if err != nil {
			return false, err
		}
		if wrote {
			return true, nil
		}
	}
	return false, nil
}

// dates returns the dates of the history of a duty, oldest first.
func (s *Store) dates(ctx context.Context, name string) ([]string, error) {
	keys, err := bucket.KeysUnder(ctx, s.runs, name)
	if err != nil {
		return nil, fmt.Errorf("duties: list the history of %s: %w", name, err)
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, strings.TrimPrefix(k, name+"."))
	}
	sort.Strings(out)
	return out, nil
}

// history returns the newest n starts of a duty, newest first.
func (s *Store) history(ctx context.Context, name string, n int) ([]RunEntry, error) {
	dates, err := s.dates(ctx, name)
	if err != nil {
		return nil, err
	}
	out := []RunEntry{}
	for i := len(dates) - 1; i >= 0 && len(out) < n; i-- {
		d, err := s.day(ctx, name, dates[i])
		if err != nil {
			return nil, err
		}
		list := slices.Clone(d.Entries)
		sort.SliceStable(list, func(a, b int) bool { return list[a].Slot.Before(list[b].Slot) })
		for j := len(list) - 1; j >= 0 && len(out) < n; j-- {
			out = append(out, list[j].RunEntry)
		}
	}
	return out, nil
}

// seenOf returns the keys that the checks of a duty saw.
func (s *Store) seenOf(ctx context.Context, name string) (map[string]time.Time, error) {
	e, err := s.seen.Get(ctx, "check."+name)
	if errors.Is(err, bucket.ErrNotFound) {
		return map[string]time.Time{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("duties: read the seen keys of %s: %w", name, err)
	}
	var sk seenKeys
	if err := json.Unmarshal(e.Value, &sk); err != nil || sk.Keys == nil {
		return map[string]time.Time{}, nil
	}
	return sk.Keys, nil
}

// remember adds the keys to the seen keys of a duty, for the start that
// woke the agent for them. A key that no check saw for HistoryFor goes,
// and beyond maxSeen the oldest keys go.
func (s *Store) remember(ctx context.Context, name string, keys []string, start string) error {
	if len(keys) == 0 {
		return nil
	}
	now := s.now().UTC()
	_, err := bucket.Modify(ctx, s.seen, "check."+name, storeAttempts, func(value []byte, exists bool) ([]byte, error) {
		var sk seenKeys
		if exists {
			_ = json.Unmarshal(value, &sk)
		}
		if sk.Keys == nil {
			sk.Keys = map[string]time.Time{}
		}
		if sk.By == nil {
			sk.By = map[string]string{}
		}
		for _, k := range keys {
			sk.Keys[k] = now
			if start != "" {
				sk.By[k] = start
			}
		}
		for k, at := range sk.Keys {
			if now.Sub(at) > HistoryFor {
				delete(sk.Keys, k)
			}
		}
		if over := len(sk.Keys) - maxSeen; over > 0 {
			type kv struct {
				k  string
				at time.Time
			}
			all := make([]kv, 0, len(sk.Keys))
			for k, at := range sk.Keys {
				all = append(all, kv{k, at})
			}
			sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
			for _, x := range all[:over] {
				delete(sk.Keys, x.k)
			}
		}
		for k := range sk.By {
			if _, ok := sk.Keys[k]; !ok {
				delete(sk.By, k)
			}
		}
		return json.Marshal(sk)
	})
	if err != nil {
		return fmt.Errorf("duties: write the seen keys of %s: %w", name, err)
	}
	return nil
}

// forget gives back the keys of a start that failed for good, so the next
// check wakes the agent for its items again.
func (s *Store) forget(ctx context.Context, name, start string) error {
	if start == "" {
		return nil
	}
	_, err := bucket.Modify(ctx, s.seen, "check."+name, storeAttempts, func(value []byte, exists bool) ([]byte, error) {
		if !exists {
			return nil, nil
		}
		var sk seenKeys
		_ = json.Unmarshal(value, &sk)
		gone := 0
		for k, by := range sk.By {
			if by == start {
				delete(sk.Keys, k)
				delete(sk.By, k)
				gone++
			}
		}
		if gone == 0 {
			return nil, nil
		}
		return json.Marshal(sk)
	})
	if err != nil {
		return fmt.Errorf("duties: give back the seen keys of %s: %w", name, err)
	}
	return nil
}

// callKey is the key of a call of the webhook of a duty in the bucket of
// the seen keys: the SHA-256 of the key of the call, in hex.
func callKey(name, key string) string {
	sum := sha256.Sum256([]byte(key))
	return "call." + name + "." + hex.EncodeToString(sum[:])
}

// takeCall claims the key of a call of the webhook of a duty for the
// window. It reports false for a key that a call claimed within the
// window. release removes the claim, after a call that started nothing
// because the store or the queue failed.
func (s *Store) takeCall(ctx context.Context, name, key string, window time.Duration) (bool, func(), error) {
	id := callKey(name, key)
	now := s.now().UTC()
	value, _ := json.Marshal(map[string]time.Time{"at": now})
	for range storeAttempts {
		rev, err := s.seen.Create(ctx, id, value)
		if err == nil {
			return true, s.releaser(id, rev), nil
		}
		if !errors.Is(err, bucket.ErrConflict) {
			return false, nil, fmt.Errorf("duties: claim a call: %w", err)
		}
		cur, err := s.seen.Get(ctx, id)
		if errors.Is(err, bucket.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, nil, fmt.Errorf("duties: read a call: %w", err)
		}
		var at struct {
			At time.Time `json:"at"`
		}
		_ = json.Unmarshal(cur.Value, &at)
		if now.Sub(at.At) < window {
			return false, nil, nil
		}
		rev, err = s.seen.Update(ctx, id, value, cur.Revision)
		if err == nil {
			return true, s.releaser(id, rev), nil
		}
		if !errors.Is(err, bucket.ErrConflict) {
			return false, nil, fmt.Errorf("duties: claim a call: %w", err)
		}
	}
	return false, nil, nil
}

// releaser returns the function that removes the claim of a call.
func (s *Store) releaser(id string, rev uint64) func() {
	return func() {
		if err := s.seen.Delete(context.Background(), id, rev); err != nil {
			s.log.Warn("duties: the claim of a call stays after a call that started nothing", "error", err)
		}
	}
}
