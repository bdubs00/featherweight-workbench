package duties

// This file copies the entries of workbench.sources into the store of the
// duties (docs/ARCHITECTURE.md section 22.8). At its start, the server
// imports each entry once: the entry becomes an ordinary duty that a
// person can change on the page. The store records each imported name, so
// a duty that a person deletes does not come back at the next start, and
// a later change of the entry in the file does not change the duty. An
// entry with locked: true makes a locked duty, which follows the file at
// each start and refuses every change.
//
// The duty keeps the templates of its entry in a hidden part of its
// record. The engine renders them for the runs, through the delivery of
// the sources, so the runs stay as they were: one run per event, with the
// same input, title, metadata, key, and redrive. No route and no tool
// shows a template. A message that a person writes replaces the
// templates, and the agent then gets the event as data, as with any
// webhook duty.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties/schedule"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/sources"
)

// Origin says where a duty comes from. Kind OriginConfig is an entry of
// workbench.sources that the server imported, and Name is its name.
type Origin struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// The kind of the origin of an imported duty, and the words of a read.
const (
	OriginConfig = "config"
	OriginText   = "First set up in the configuration of the server."
	// LockedText is the detail of the answer 409 to a change, a pause, or
	// a delete of a locked duty.
	LockedText = "This duty is locked by the setup of this server. The person who runs the server can change it."
)

// importedBy names the writer of an imported duty in its record.
const importedBy = "the configuration of the server"

// imported is the part of an entry of workbench.sources that a duty keeps
// out of every read. Templated is true while the templates make the runs:
// Input, Title, Key, and Metadata are the templates, Items the path of the
// events in the body of a call, DedupFor the window of a key, and Redrive
// the redrive of the entry. URL, Secret, and Header are the poll of the
// entry, and HookSecret the secret of its webhook: the operator set them
// in the file, so the rules of workbench.duties do not refuse them.
type imported struct {
	Templated  bool              `json:"templated"`
	Input      string            `json:"input,omitempty"`
	Title      string            `json:"title,omitempty"`
	Key        string            `json:"key,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Items      string            `json:"items,omitempty"`
	DedupFor   time.Duration     `json:"dedupFor,omitempty"`
	Redrive    *sources.Redrive  `json:"redrive,omitempty"`
	URL        string            `json:"url,omitempty"`
	Secret     string            `json:"secret,omitempty"`
	Header     string            `json:"header,omitempty"`
	HookSecret string            `json:"hookSecret,omitempty"`
}

// templated reports whether the templates of an entry make the runs of
// the duty.
func (st *stored) templated() bool { return st.From != nil && st.From.Templated }

// retries returns the retries of the runs of a duty. The runs of the
// templates retry through the redrive of the entry, so they take none.
func (st *stored) retries() int {
	if st.templated() {
		return 0
	}
	return st.Duty.retries()
}

// importKey is the key of the record of an imported name in the bucket
// of the duties.
func importKey(name string) string { return "import." + name }

// importRecord is the value of the record of an imported name.
type importRecord struct {
	At time.Time `json:"at"`
	By string    `json:"by"`
}

// keyPathPattern matches a key template that names one field, such as
// {{.id}} or {{ .issue.number }}.
var keyPathPattern = regexp.MustCompile(`^\{\{-?\s*\.([A-Za-z0-9_]+(?:\.[A-Za-z0-9_]+)*)\s*-?\}\}$`)

// keyPath returns the dotted path of a key template that names one field,
// or "".
func keyPath(tpl string) string {
	if m := keyPathPattern.FindStringSubmatch(strings.TrimSpace(tpl)); m != nil {
		return m[1]
	}
	return ""
}

// dutyOfSource returns the duty of an entry of workbench.sources and its
// hidden part. A webhook becomes a duty with when.webhook, with the mode,
// the secret, and the header of its login. A poll becomes a duty with a
// schedule of its interval and an HTTP check with the address, the
// secret, the header, the items, and the key of the entry. The agent and
// the pool stay. The runs of the entry ran as runAs, so the duty runs as
// its owner, which is that principal.
func dutyOfSource(src sources.Source) (*stored, error) {
	if strings.TrimSpace(src.RunAs) == "" {
		src.RunAs = "source:" + src.Name
	}
	if strings.TrimSpace(src.RunAsKind) == "" {
		src.RunAsKind = sources.DefaultKind
	}
	zero := 0
	d := Duty{Name: src.Name, Agent: src.Agent, Pool: src.Pool, Owner: src.RunAs, RunAs: RunAsOwner,
		Do: Do{Ask: &Ask{}}, Limits: Limits{Overlap: OverlapAllow, Retries: &zero}}
	from := &imported{Templated: true, Input: src.Input, Title: src.Title, Key: src.Key, Metadata: src.Metadata,
		Items: src.Items, DedupFor: src.DedupFor, Redrive: src.Redrive}
	key := keyPath(src.Key)
	if p := src.Poll; p != nil {
		d.When.Schedule = &schedule.Schedule{Repeat: schedule.RepeatEvery, Every: durText(p.Every)}
		d.CheckFirst = &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: p.URL, Secret: p.Secret, Header: p.Header}}, Items: src.Items, Key: key, Wake: Wake{When: WakeNew}}
		from.URL, from.Secret, from.Header, from.Items = p.URL, p.Secret, p.Header, ""
	} else {
		d.When.Webhook = &Webhook{Auth: WebhookAuth{Mode: src.Auth.Mode, Secret: src.Auth.Secret, Header: src.Auth.Header}, Key: key}
		from.HookSecret = src.Auth.Secret
	}
	d.normalize(schedule.DefaultZone)
	return &stored{Duty: d, OwnerKind: src.RunAsKind, Origin: &Origin{Kind: OriginConfig, Name: src.Name}, Locked: src.Locked, From: from}, nil
}

// sourceOf returns the entry of workbench.sources that the delivery of
// the sources renders for a duty of the templates: the agent, the pool,
// and the principal of the duty now, with the templates of the entry, the
// name of the duty for workbench.duty, and the share of comparison runs.
func (st *stored) sourceOf(comparisonRuns float64) sources.Source {
	d, f := st.Duty, st.From
	src := sources.Source{Name: d.Name, Agent: d.Agent, Pool: d.Pool, Input: f.Input, Title: f.Title, Metadata: f.Metadata, Key: f.Key,
		Items: f.Items, DedupFor: f.DedupFor, Redrive: f.Redrive, RunAs: "duty:" + d.Name, RunAsKind: fw.KindWorkload,
		Duty: d.Name, ComparisonRuns: comparisonRuns}
	if d.RunAs == RunAsOwner && d.Owner != "" {
		src.RunAs, src.RunAsKind = d.Owner, st.OwnerKind
		if src.RunAsKind == "" {
			src.RunAsKind = fw.KindUser
		}
	}
	if c := d.CheckFirst; c != nil {
		src.Items = c.Items
	}
	return src
}

// importSources copies the entries of workbench.sources into the store,
// once each, and tells the sources which entries the duties run now. Two
// replicas that import at the same time make one duty of each entry: the
// create of a duty and the record of its name decide by the revision of
// the bucket.
func (s *Service) importSources(ctx context.Context) {
	list, err := sources.Load(s.a.Config())
	if err != nil || len(list) == 0 {
		return
	}
	var adopted []string
	for _, src := range list {
		ok, err := s.importOne(ctx, src)
		if err != nil {
			s.log.Warn("duties: an entry of workbench.sources did not import; the sources run it", "source", src.Name, "error", err)
			continue
		}
		if ok {
			adopted = append(adopted, src.Name)
		}
	}
	sources.Adopt(s.a, adopted)
	if len(adopted) > 0 {
		s.log.Info("duties: the duties run the entries of workbench.sources", "sources", strings.Join(adopted, ","))
	}
}

// importOne imports one entry and reports whether the duties run it: a
// duty of the store holds it now, or a person deleted that duty. A duty
// that a person made under the same name keeps the name, and the sources
// run the entry as before.
func (s *Service) importOne(ctx context.Context, src sources.Source) (bool, error) {
	fresh, err := dutyOfSource(src)
	if err != nil {
		return false, err
	}
	cur, err := s.st.get(ctx, src.Name)
	switch {
	case errors.Is(err, ErrNotFound):
		cur = nil
	case err != nil:
		return false, err
	}
	if cur != nil && (cur.Origin == nil || cur.Origin.Kind != OriginConfig || cur.Origin.Name != src.Name) {
		s.log.Warn("duties: a duty of a person has the name of an entry of workbench.sources, so the sources run the entry", "source", src.Name)
		return false, nil
	}
	marked, err := s.st.isImported(ctx, src.Name)
	if err != nil {
		return false, err
	}
	now := s.now().UTC()
	switch {
	case src.Locked:
		// A locked duty follows the file at each start.
		if err := s.followFile(ctx, cur, fresh, now); err != nil {
			return false, err
		}
	case cur != nil && cur.Locked:
		// The entry lost its lock: the duty keeps its record and takes
		// changes from now on.
		if _, err := s.st.update(ctx, src.Name, 0, func(x *stored) error {
			if !x.Locked {
				return errUnchanged
			}
			x.Locked, x.Changed, x.ChangedBy = false, now, importedBy
			return nil
		}); err != nil && !errors.Is(err, errUnchanged) {
			return false, err
		}
	case marked || cur != nil:
		// The entry was imported before. A duty that a person deleted
		// stays deleted, and a change of the entry changes no duty.
	default:
		fresh.Since, fresh.Created, fresh.Changed, fresh.ChangedBy = now, now, now, importedBy
		if err := s.st.create(ctx, fresh); err != nil && !errors.Is(err, ErrExists) {
			return false, err
		} else if err == nil {
			s.log.Info("duties: imported an entry of workbench.sources as a duty", "duty", src.Name, "agent", src.Agent)
			changed(src.Name)
		}
	}
	if !marked {
		if err := s.st.markImported(ctx, src.Name, importRecord{At: now, By: s.replica}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// followFile writes a locked entry as its duty: it creates the duty, or
// replaces the record and its hidden part with the entry. The history, the
// pause, and the time from which the duty counts stay.
func (s *Service) followFile(ctx context.Context, cur, fresh *stored, now time.Time) error {
	if cur == nil {
		fresh.Since, fresh.Created, fresh.Changed, fresh.ChangedBy = now, now, now, importedBy
		err := s.st.create(ctx, fresh)
		if err == nil {
			s.log.Info("duties: imported a locked entry of workbench.sources as a duty", "duty", fresh.Duty.Name)
			changed(fresh.Duty.Name)
			return nil
		}
		if !errors.Is(err, ErrExists) {
			return err
		}
	}
	_, err := s.st.update(ctx, fresh.Duty.Name, 0, func(x *stored) error {
		d := fresh.Duty
		d.Paused = x.Duty.Paused
		if x.Locked && reflect.DeepEqual(x.Duty, d) && reflect.DeepEqual(x.From, fresh.From) && x.OwnerKind == fresh.OwnerKind {
			return errUnchanged
		}
		if !reflect.DeepEqual(x.Duty.When, d.When) {
			x.Since = now
		}
		x.Duty, x.From, x.OwnerKind, x.Origin, x.Locked = d, fresh.From, fresh.OwnerKind, fresh.Origin, true
		x.Changed, x.ChangedBy = now, importedBy
		return nil
	})
	if errors.Is(err, errUnchanged) {
		return nil
	}
	if err == nil {
		changed(fresh.Duty.Name)
	}
	return err
}

// isImported reports whether the store records the name as imported.
func (s *Store) isImported(ctx context.Context, name string) (bool, error) {
	_, err := s.duties.Get(ctx, importKey(name))
	switch {
	case errors.Is(err, bucket.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("duties: read the record of the import of %s: %w", name, err)
	}
	return true, nil
}

// markImported records an imported name. A record that exists stays.
func (s *Store) markImported(ctx context.Context, name string, rec importRecord) error {
	value, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := s.duties.Create(ctx, importKey(name), value); err != nil && !errors.Is(err, bucket.ErrConflict) {
		return fmt.Errorf("duties: record the import of %s: %w", name, err)
	}
	return nil
}

// takeover answers the old address of an entry that the duties run,
// POST /v1/sources/{name}, as POST /v1/duties/{name}/call (call.go), and
// reports whether it answered.
func takeover(a *fw.Agent, w http.ResponseWriter, r *http.Request) bool {
	s := Of(a)
	if s == nil || !sources.Adopted(a, r.PathValue("name")) {
		return false
	}
	s.serveCall(w, r)
	return true
}

// templateAllows reports whether a duty of the templates can keep a
// problem of its record: an empty message, no budget, no key of the
// check, and the interval of its poll, because the templates make its
// runs as the entry made them.
func templateAllows(d *Duty, p string) bool {
	switch {
	case strings.HasPrefix(p, "do.ask.message: "), strings.HasPrefix(p, "limits.budget"), strings.HasPrefix(p, "checkFirst.key: A key is required"):
		return true
	case strings.HasPrefix(p, "when.schedule.every: "):
		sch := d.When.Schedule
		step, err := time.ParseDuration(strings.TrimSpace(sch.Every))
		return sch.Repeat == schedule.RepeatEvery && err == nil && step >= minPoll && step <= schedule.MaxEvery
	}
	return false
}

// maskedURL returns an address without its query and its fragment, as a
// read of an imported duty shows the address of its entry.
func maskedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.RawQuery == "" && u.Fragment == "") {
		return raw
	}
	u.RawQuery, u.Fragment, u.ForceQuery = "", "", false
	return u.String()
}

// pollWords says the start of a duty of the templates that polls.
func pollWords(d *Duty, step time.Duration) string {
	where := "its address"
	if c := d.CheckFirst; c != nil && c.Look.HTTP != nil {
		where = addressWords(c.Look.HTTP.URL)
	}
	return fmt.Sprintf("Every %s, it looks at %s, and each new item starts a run of %s.", durText(step), where, d.Agent)
}

// fromFile reports whether an HTTP check names the address, the secret,
// and the header that the configuration of the server gave the entry of
// an imported duty.
func (d *Duty) fromFile(h *LookHTTP) bool {
	f := d.from
	return f != nil && f.URL != "" && h.URL == f.URL && h.Secret == f.Secret && h.Header == f.Header
}

// hookFromFile reports whether a webhook names the secret that the
// configuration of the server gave the entry of an imported duty.
func (d *Duty) hookFromFile(w *Webhook) bool {
	f := d.from
	return f != nil && f.HookSecret != "" && w.Auth.Secret == f.HookSecret
}

// lockedError is the refusal of a change of a locked duty.
func lockedError() error { return fmt.Errorf("%w: %s", ErrReadOnly, LockedText) }

// Bounds of a poll of the templates.
const (
	// minPoll is the shortest step of the poll of a duty of the
	// templates, as for a poll source.
	minPoll = sources.MinEvery
	// pollTimeout bounds one poll and its delivery.
	pollTimeout = time.Minute
)

// pollStep returns the step of the poll of a duty of the templates whose
// schedule repeats every step, and false for any other duty. The schedule
// of such a duty keeps the interval of its entry, which can be shorter
// than the shortest step of a schedule.
func (st *stored) pollStep() (time.Duration, bool) {
	sch := st.Duty.When.Schedule
	if !st.templated() || sch == nil || sch.Repeat != schedule.RepeatEvery || st.Duty.CheckFirst == nil {
		return 0, false
	}
	d, err := time.ParseDuration(strings.TrimSpace(sch.Every))
	if err != nil || d < minPoll {
		return 0, false
	}
	return d, true
}

// poll starts a poll of a duty of the templates when its step passed
// since the last poll of this replica, and returns the time of the next
// poll. Every replica polls, as every replica of the sources did: the
// claims of the sources keep each run single. A paused duty polls not.
func (s *Service) poll(ctx context.Context, st *stored, step time.Duration, sw Switch, now time.Time) time.Time {
	name := st.Duty.Name
	if st.Duty.Paused || sw.Paused {
		return now.Add(step)
	}
	s.mu.Lock()
	if s.polled == nil {
		s.polled, s.polling = map[string]time.Time{}, map[string]bool{}
	}
	last, busy := s.polled[name], s.polling[name]
	due := !busy && !now.Before(last.Add(step))
	if due {
		s.polled[name], s.polling[name] = now, true
	}
	s.mu.Unlock()
	if !due {
		return last.Add(step)
	}
	s.pollers.Add(1)
	go func() {
		defer s.pollers.Done()
		defer func() {
			s.mu.Lock()
			delete(s.polling, name)
			s.mu.Unlock()
		}()
		pctx, cancel := context.WithTimeout(ctx, pollTimeout)
		defer cancel()
		s.pollOnce(pctx, st)
	}()
	return now.Add(step)
}

// pollOnce reads the address of the check of a duty of the templates and
// delivers its items. A start with runs goes into the history of the duty.
func (s *Service) pollOnce(ctx context.Context, st *stored) {
	body, err := s.look(ctx, &st.Duty)
	if err != nil {
		s.warnOnce(st.Duty.Name, "the poll failed: "+err.Error())
		return
	}
	got, err := sources.DeliverDocument(ctx, s.a, st.sourceOf(s.comparisonRuns), body)
	if err != nil {
		s.warnOnce(st.Duty.Name, "the poll did not deliver: "+err.Error())
		return
	}
	s.mu.Lock()
	delete(s.warned, st.Duty.Name)
	s.mu.Unlock()
	if len(got.Runs) > 0 {
		s.recordDelivery(ctx, st, StartSchedule, got)
	}
}

// deliveryWords says what a delivery of the templates did.
func deliveryWords(got sources.Delivered) string {
	runs := len(got.Runs)
	s := fmt.Sprintf("%s started %s.", itemsOrEvents(got.Events), plural(runs, "1 run", fmt.Sprintf("%d runs", runs)))
	if got.Duplicates > 0 {
		s += fmt.Sprintf(" %d made a run before.", got.Duplicates)
	}
	if got.Refused > 0 {
		s += fmt.Sprintf(" %d did not fit the setup of the server.", got.Refused)
	}
	return s
}

// itemsOrEvents counts the events of a delivery in words.
func itemsOrEvents(n int) string {
	if n == 1 {
		return "1 event"
	}
	return fmt.Sprintf("%d events", n)
}

// recordDelivery writes a start of a duty of the templates into its
// history: the first run of the delivery, which the engine follows to its
// end, and what the delivery did. A start that another replica wrote
// stays one start.
func (s *Service) recordDelivery(ctx context.Context, st *stored, kind string, got sources.Delivered) {
	d := &st.Duty
	now := s.now()
	e := s.newEntry(Trigger{Kind: kind, Slot: now.UTC()}, now)
	e.State, e.Run, e.Title = StateQueued, got.Runs[0].ID, got.Runs[0].Title
	e.Found, e.Woke, e.Reason = got.Events, true, deliveryWords(got)
	date := dateOf(e.Slot, d.zone())
	if _, err := s.st.claim(ctx, d.Name, date, e); err != nil {
		s.log.Warn("duties: the history of a duty did not take a delivery", "duty", d.Name, "error", err)
		return
	}
	s.noteOpen(d.Name, date)
	changed(d.Name)
}

// carryTemplated carries a start of a duty of the templates that the
// engine claimed: a time of a schedule that does not repeat every step,
// or a run now. It reads the address of the check and delivers its items.
// A duty of the templates without a check starts on the calls of another
// app only, so a run now of it starts nothing.
func (s *Service) carryTemplated(ctx context.Context, st *stored, date string, e entry) Outcome {
	d := &st.Duty
	if d.CheckFirst == nil {
		reason := "It waits for a call of the other app."
		s.endEntry(ctx, st, date, e, []string{StateStarting}, StateSkipped, reason)
		return Outcome{Skipped: reason}
	}
	body, err := s.look(ctx, d)
	if err != nil {
		reason := "The check failed, so no run started: " + strings.TrimSuffix(err.Error(), ".") + "."
		s.endEntry(ctx, st, date, e, []string{StateStarting}, StateCheckFailed, reason)
		s.checkFailed(ctx, st, reason)
		return Outcome{Failed: reason}
	}
	s.checkPassed(ctx, d.Name)
	got, err := sources.DeliverDocument(ctx, s.a, st.sourceOf(s.comparisonRuns), body)
	if err == nil && got.Err != nil && len(got.Runs) == 0 {
		err = got.Err
	}
	if err != nil {
		reason := "The items did not reach the queue: " + reasonOf(err) + "."
		s.endEntry(ctx, st, date, e, []string{StateStarting}, StateFailed, reason)
		return Outcome{Failed: reason}
	}
	if len(got.Runs) == 0 {
		reason := "The check found no new item, so no run started."
		if got.Events > 0 {
			reason = deliveryWords(got)
		}
		_, wrote, err := s.st.setEntry(ctx, d.Name, date, e.ID, []string{StateStarting}, func(x *entry) bool {
			x.State, x.Found, x.Reason = StateQuiet, got.Events, reason
			return true
		})
		if err == nil && wrote {
			changed(d.Name)
		}
		return Outcome{Skipped: reason}
	}
	first := got.Runs[0]
	_, wrote, err := s.st.setEntry(ctx, d.Name, date, e.ID, []string{StateStarting}, func(x *entry) bool {
		x.State, x.Run, x.Title = StateQueued, first.ID, first.Title
		x.Found, x.Woke, x.Reason = got.Events, true, deliveryWords(got)
		return true
	})
	if err != nil {
		s.log.Warn("duties: the history did not take a run that went out", "duty", d.Name, "run", first.ID, "error", err)
	}
	if wrote {
		changed(d.Name)
	}
	return Outcome{Run: first.ID}
}
