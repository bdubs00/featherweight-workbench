package duties

// This file runs the check of a duty (docs/ARCHITECTURE.md section 22.6).
// A check reads a JSON document, finds the items at a dotted path, keeps
// the items that pass the filters, and decides whether they wake the
// agent: new items, any item, or a number of items. It costs no model
// call. A check of an HTTP address runs in the front with the poll of the
// sources, a GET with the secret of a variable of the front. A check with
// a tool of the agent needs the check run of the kernel, which a later
// package registers (RegisterLook).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-workbench/sources"
)

// Bounds of a check.
const (
	// CheckTimeout bounds one look.
	CheckTimeout = 30 * time.Second
	// MaxCheckBytes bounds the answer of one look.
	MaxCheckBytes = 1 << 20
	// maxFoundBytes bounds the text of {found}.
	maxFoundBytes = 8 << 10
	// maxTestItems is the most items that a test of a check shows.
	maxTestItems = 20
)

// checkResult is what one check found. Matched holds the items that pass
// the filters, Wake those that wake the agent, and New the keys of the
// new items, which the duty remembers once their run is on the queue.
// Why says why the agent does not wake, and Text is {found}.
type checkResult struct {
	Matched []map[string]any
	Wake    []map[string]any
	New     []string
	Wakes   bool
	Why     string
	Text    string
}

// check looks once and judges the answer.
func (s *Service) check(ctx context.Context, d *Duty) (checkResult, error) {
	body, err := s.look(ctx, d)
	if err != nil {
		return checkResult{}, err
	}
	return s.judge(ctx, d, body)
}

// look reads the document of the check of a duty.
func (s *Service) look(ctx context.Context, d *Duty) ([]byte, error) {
	c := d.CheckFirst
	if c.Look.Tool != "" {
		fn := lookOf("tool")
		if fn == nil {
			return nil, fmt.Errorf("%w: a check with a tool of the agent needs the check run of the kernel, which this build does not have. Use an HTTP address for now", ErrNotAvailable)
		}
		return fn(ctx, s.a, d)
	}
	h := c.Look.HTTP
	if h == nil {
		return nil, fmt.Errorf("the check names no address")
	}
	host := hostOf(h.URL)
	if p := s.cfg.lookProblems(h); len(p) > 0 {
		return nil, fmt.Errorf("%s", strings.TrimSuffix(problemText(p[0]), "."))
	}
	value := ""
	if h.Secret != "" {
		v, err := s.secret(ctx, h.Secret)
		if err != nil {
			return nil, fmt.Errorf("the variable %s holds no secret in this front", h.Secret)
		}
		value = v
	}
	body, err := sources.Fetch(ctx, s.client, h.URL, h.Header, value, CheckTimeout, MaxCheckBytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", host, strings.TrimPrefix(err.Error(), "Get \""))
	}
	return body, nil
}

// judge finds the items in a document, applies the filters, and decides
// whether the items wake the agent.
func (s *Service) judge(ctx context.Context, d *Duty, body []byte) (checkResult, error) {
	c := d.CheckFirst
	items, err := sources.Events(body, c.Items)
	if err != nil {
		return checkResult{}, fmt.Errorf("the answer does not read: %s", strings.TrimPrefix(err.Error(), "the items path"))
	}
	var res checkResult
	for _, it := range items {
		if c.matches(it) {
			res.Matched = append(res.Matched, it)
		}
	}
	switch c.Wake.When {
	case WakeAny:
		res.Wake, res.Wakes = res.Matched, len(res.Matched) > 0
		if !res.Wakes {
			res.Why = "The check found no item, so the agent did not wake."
		}
	case WakeAtLeast:
		res.Wake, res.Wakes = res.Matched, len(res.Matched) >= c.Wake.Count
		if !res.Wakes {
			res.Why = fmt.Sprintf("The check found %s, fewer than %d, so the agent did not wake.", itemsWords(len(res.Matched)), c.Wake.Count)
		}
	default:
		seen, err := s.st.seenOf(ctx, d.Name)
		if err != nil {
			return checkResult{}, err
		}
		taken := map[string]bool{}
		for _, it := range res.Matched {
			key, ok := keyOf(it, c.Key)
			if !ok || taken[key] {
				continue
			}
			if _, old := seen[key]; old {
				continue
			}
			taken[key] = true
			res.Wake = append(res.Wake, it)
			res.New = append(res.New, key)
		}
		res.Wakes = len(res.Wake) > 0
		if !res.Wakes {
			res.Why = fmt.Sprintf("The check found %s, and none is new, so the agent did not wake.", itemsWords(len(res.Matched)))
		}
	}
	if res.Wakes {
		res.Text = foundText(res.Wake)
	}
	return res, nil
}

// itemsWords counts items in words.
func itemsWords(n int) string {
	if n == 1 {
		return "1 item"
	}
	return strconv.Itoa(n) + " items"
}

// matches reports whether an item passes every filter of the check.
func (c *CheckFirst) matches(item map[string]any) bool {
	for _, w := range c.Where {
		if !w.matches(item) {
			return false
		}
	}
	return true
}

// matches reports whether an item passes the filter.
func (w Where) matches(item map[string]any) bool {
	v, ok := field(item, w.Field)
	switch {
	case len(w.Is) > 0:
		return ok && equal(v, w.Is)
	case len(w.IsNot) > 0:
		return !ok || !equal(v, w.IsNot)
	case w.OneOf != nil:
		for _, o := range w.OneOf {
			if ok && equal(v, o) {
				return true
			}
		}
		return false
	case len(w.Contains) > 0:
		if !ok {
			return false
		}
		if list, isList := v.([]any); isList {
			for _, x := range list {
				if equal(x, w.Contains) {
					return true
				}
			}
			return false
		}
		want := decode(w.Contains)
		return strings.Contains(strings.ToLower(text(v)), strings.ToLower(text(want)))
	case len(w.AtLeast) > 0:
		return ok && compare(v, decode(w.AtLeast)) >= 0
	case len(w.AtMost) > 0:
		return ok && compare(v, decode(w.AtMost)) <= 0
	}
	return true
}

// field follows a dotted path of keys in an item.
func field(item map[string]any, path string) (any, bool) {
	var v any = item
	for _, k := range strings.Split(strings.TrimSpace(path), ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[k]; !ok {
			return nil, false
		}
	}
	return v, true
}

// keyOf returns the key of an item at the dotted path, as text.
func keyOf(item map[string]any, path string) (string, bool) {
	v, ok := field(item, path)
	if !ok || v == nil {
		return "", false
	}
	k := text(v)
	return k, k != ""
}

// decode reads a value of a filter, with numbers as json.Number.
func decode(raw json.RawMessage) any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return string(raw)
	}
	return v
}

// number reads a value as a number: a JSON number, or a text that holds
// one, as a ticket system often writes a priority.
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// text writes a value as text: a text as it is, a number with its digits,
// and any other value as compact JSON.
func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// equal reports whether a value of an item equals a value of a filter.
// Two numbers compare as numbers, also when one is a text, and any other
// pair compares as text.
func equal(v any, raw json.RawMessage) bool {
	want := decode(raw)
	a, okA := number(v)
	b, okB := number(want)
	if okA && okB {
		return a == b
	}
	if want == nil || v == nil {
		return want == nil && v == nil
	}
	return text(v) == text(want)
}

// compare orders a value of an item against a value of a filter: as
// numbers when both are numbers, else as text, so dates in the form
// 2006-01-02 order too.
func compare(v, want any) int {
	a, okA := number(v)
	b, okB := number(want)
	if okA && okB {
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		}
		return 0
	}
	return strings.Compare(text(v), text(want))
}

// foundText writes the items that woke the agent for {found}: one item of
// compact JSON per line, within maxFoundBytes, with a line that counts the
// items that do not fit.
func foundText(items []map[string]any) string {
	var b strings.Builder
	for i, it := range items {
		line, _ := json.Marshal(it)
		if b.Len()+len(line)+1 > maxFoundBytes {
			fmt.Fprintf(&b, "(%s more %s not shown)", strconv.Itoa(len(items)-i), plural(len(items)-i, "item is", "items are"))
			break
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// plural picks the word for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// TestAnswer is what a test of a check found: the items that wake the
// agent, at most 20, their count, whether they wake it, the count of the
// items that pass the filters, and why the agent does not wake.
type TestAnswer struct {
	Found   []map[string]any `json:"found"`
	Count   int              `json:"count"`
	Wakes   bool             `json:"wakes"`
	Matched int              `json:"matched"`
	Reason  string           `json:"reason,omitempty"`
}

// testCheck runs the check of a duty once, for a dry run. It writes
// nothing: a new item stays new.
func (s *Service) testCheck(ctx context.Context, d *Duty) (*TestAnswer, error) {
	res, err := s.check(ctx, d)
	if err != nil {
		return nil, err
	}
	found := res.Wake
	if len(found) > maxTestItems {
		found = found[:maxTestItems]
	}
	if found == nil {
		found = []map[string]any{}
	}
	return &TestAnswer{Found: found, Count: len(res.Wake), Wakes: res.Wakes, Matched: len(res.Matched), Reason: res.Why}, nil
}
