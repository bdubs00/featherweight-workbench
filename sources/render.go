package sources

// This file turns the JSON of an outside system into run requests: it
// finds the events in a body or in the answer of a poll, and it renders
// the templates of a source over each event. A template is Go
// text/template. A field that an event does not have is an error, so a
// key never renders as an empty or partial value by accident; a template
// reads an optional field with index, such as {{index . "assignee"}}.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/template"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// maxRender is the largest text that one template renders.
const maxRender = 1 << 20

// maxTitleRunes is the longest title of a run of a source. A longer title
// is cut.
const maxTitleRunes = 200

// noValue is what text/template prints for a JSON null.
const noValue = "<no value>"

// funcs are the functions that a template can call beside the built-in
// ones.
var funcs = template.FuncMap{
	// json writes a value as compact JSON, such as {{json .}} for the
	// whole event.
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
	"lower": func(v any) string { return strings.ToLower(text(v)) },
	"upper": func(v any) string { return strings.ToUpper(text(v)) },
	"trim":  func(v any) string { return strings.TrimSpace(text(v)) },
	// default gives def when the value is missing or empty, such as
	// {{index . "priority" | default "normal"}}.
	"default": func(def string, v any) any {
		if v == nil || text(v) == "" {
			return def
		}
		return v
	},
}

// text writes a value as a template prints it, with "" for nil.
func text(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// templates are the compiled templates of one source. input is nil when
// the source sets none, and the input is then the event as indented JSON.
type templates struct {
	input, title, key *template.Template
	metadata          map[string]*template.Template
}

// compileTemplates parses every template of a source.
func compileTemplates(s Source) (*templates, error) {
	var t templates
	var err error
	parse := func(name, src string) *template.Template {
		if err != nil || strings.TrimSpace(src) == "" {
			return nil
		}
		var tp *template.Template
		tp, err = template.New(name).Option("missingkey=error").Funcs(funcs).Parse(src)
		if err != nil {
			err = fmt.Errorf("the template %s does not parse: %w", name, err)
		}
		return tp
	}
	t.input = parse("input", s.Input)
	t.title = parse("title", s.Title)
	t.key = parse("key", s.Key)
	for k, v := range s.Metadata {
		if t.metadata == nil {
			t.metadata = map[string]*template.Template{}
		}
		if tp := parse("metadata."+k, v); tp != nil {
			t.metadata[k] = tp
		}
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// rendered is one event of a source as a run request.
type rendered struct {
	index int // the position of the event, from 1
	key   string
	req   fw.RunRequest
}

// render turns one event into a run request of a source. The caller sets
// nothing more: the agent, the person, the batch, the metadata, and the
// idempotency key are in the request.
func (src *source) render(index int, event map[string]any) (rendered, error) {
	t := src.tpl
	key, err := execute(t.key, event)
	if err != nil {
		return rendered{}, err
	}
	key = strings.TrimSpace(key)
	switch {
	case key == "":
		return rendered{}, errors.New("the key template made an empty key")
	case strings.Contains(key, noValue):
		return rendered{}, errors.New("the key template met a field that is null")
	case len(key) > MaxKeyBytes:
		return rendered{}, fmt.Errorf("the key is longer than %d bytes", MaxKeyBytes)
	}
	var input string
	if t.input == nil {
		b, err := json.MarshalIndent(event, "", "  ")
		if err != nil {
			return rendered{}, fmt.Errorf("write the event as JSON: %w", err)
		}
		input = string(b)
	} else if input, err = execute(t.input, event); err != nil {
		return rendered{}, err
	}
	if strings.TrimSpace(input) == "" {
		return rendered{}, errors.New("the input template made an empty input")
	}
	md := map[string]string{}
	for k, tp := range t.metadata {
		v, err := execute(tp, event)
		if err != nil {
			return rendered{}, err
		}
		md[k] = v
	}
	if t.title != nil {
		title, err := execute(t.title, event)
		if err != nil {
			return rendered{}, err
		}
		if title = cutRunes(strings.Join(strings.Fields(title), " "), maxTitleRunes); title != "" {
			md[fw.MetaTitle] = title
		}
	}
	md[fw.MetaBatch] = src.cfg.Name
	md[MetaSource] = src.cfg.Name
	md[MetaKey] = key
	// The claim adds the generation after the render, so it needs an entry.
	if src.cfg.Redrive != nil && len(md) >= fw.MaxMetadataEntries {
		return rendered{}, errors.New("the metadata leave no room for the source generation")
	}
	if err := fw.CheckMetadata(md); err != nil {
		return rendered{}, fmt.Errorf("the metadata do not fit: %s", strings.TrimPrefix(err.Error(), "fw: invalid request: "))
	}
	return rendered{index: index, key: key, req: fw.RunRequest{
		Agent:          src.cfg.Agent,
		Input:          input,
		Metadata:       md,
		Principal:      src.cfg.RunAs,
		PrincipalKind:  src.cfg.RunAsKind,
		IdempotencyKey: src.cfg.Name + "/" + key,
	}}, nil
}

// execute renders one template over an event, up to maxRender bytes.
func execute(t *template.Template, event map[string]any) (string, error) {
	var b bytes.Buffer
	if err := t.Execute(&limitWriter{w: &b, n: maxRender}, event); err != nil {
		return "", fmt.Errorf("the template %s failed: %w", t.Name(), err)
	}
	return b.String(), nil
}

// errTooLong means that a template rendered more than maxRender bytes.
var errTooLong = fmt.Errorf("the text is longer than %d bytes", maxRender)

// limitWriter fails once more than n bytes went through it.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, errTooLong
	}
	l.n -= len(p)
	return l.w.Write(p)
}

// cutRunes cuts s to at most n runes.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n]))
}

// decodeEvents reads the events of a JSON document: the value at the
// dotted path items, or the document itself when items is empty. An
// array holds one event per element, and an object is one event. Each
// event must be an object. Numbers stay as they are written, so a large
// id keeps every digit.
func decodeEvents(body []byte, items string) ([]map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("the body is not JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("the body holds more than one JSON value")
	}
	v, err := at(doc, items)
	if err != nil {
		return nil, err
	}
	switch v := v.(type) {
	case map[string]any:
		return []map[string]any{v}, nil
	case []any:
		out := make([]map[string]any, 0, len(v))
		for i, e := range v {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("event %d is not a JSON object", i+1)
			}
			out = append(out, m)
		}
		return out, nil
	default:
		return nil, errors.New("the events are not a JSON object or an array of objects")
	}
}

// at follows a dotted path of object keys from v.
func at(v any, path string) (any, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return v, nil
	}
	walked := ""
	for _, k := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("the items path %q stops at %q, which is not an object", path, strings.TrimPrefix(walked, "."))
		}
		walked += "." + k
		if v, ok = m[k]; !ok {
			return nil, fmt.Errorf("the items path %q finds no %q", path, strings.TrimPrefix(walked, "."))
		}
	}
	return v, nil
}
