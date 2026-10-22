package faces

// This file is the store of the faces. Each agent with a face, a display
// name, or a tagline is one entry of a bucket (package internal/bucket),
// keyed by its name in base64url. The value is one line of JSON with the
// display name, the tagline, the hash, the writer, and the time, a line
// break, and the SVG that the sanitizer wrote. A write reads the entry and
// writes at the revision that it read, so two replicas of the front never
// lose a write: the loser reads again and writes again.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/memory"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// Limits of a face.
const (
	MaxDisplayNameRunes = 64
	MaxTaglineRunes     = 140
	// History is how many values of each face the store keeps.
	History = 10
	// writeAttempts bounds the tries of a write that other writers race.
	writeAttempts = 8
)

// Errors of the store beside ErrRefused of the sanitizer.
var (
	// ErrInvalid means that the name of the agent, the display name, or
	// the tagline does not fit.
	ErrInvalid = errors.New("faces: the request is not valid")
	// ErrConflict means that other writers changed the face each time a
	// write tried.
	ErrConflict = errors.New("faces: the face changed")
)

// Face is the face of an agent: the SVG, the display name, the tagline,
// and the hash of the three, which changes when one of them changes.
// Custom is false for the identicon, which the store draws for an agent
// without a stored SVG. As JSON, a face is the entry of GET /v1/faces:
// agent, displayName, tagline, hash, and custom, and by, at, and version
// for an agent with an entry.
type Face struct {
	Agent       string    `json:"agent"`
	DisplayName string    `json:"displayName"`
	Tagline     string    `json:"tagline"`
	Hash        string    `json:"hash"`
	Custom      bool      `json:"custom"`
	By          string    `json:"by,omitempty"`
	At          time.Time `json:"at,omitzero"`
	// Version is the revision of the entry in the store, zero for an
	// agent without an entry.
	Version uint64 `json:"version,omitempty"`
	SVG     []byte `json:"-"`
}

// record is the first line of the value of an entry.
type record struct {
	Hash        string    `json:"hash,omitempty"`
	DisplayName string    `json:"displayName,omitempty"`
	Tagline     string    `json:"tagline,omitempty"`
	By          string    `json:"by"`
	At          time.Time `json:"at"`
}

// Store keeps the faces of the agents. Build it with New.
type Store struct {
	b   bucket.Bucket
	log *slog.Logger
	now func() time.Time
}

// New returns the store of the faces on the bucket b.
func New(b bucket.Bucket, log *slog.Logger) *Store {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Store{b: b, log: log, now: time.Now}
}

// Describe names the place of the store for a person.
func (s *Store) Describe() string { return s.b.Describe() }

func keyOf(agent string) string {
	return "face." + base64.RawURLEncoding.EncodeToString([]byte(agent))
}

// CheckAgent checks the name of an agent as the kernel checks the name of
// a profile for its memory: 1 to 64 letters, digits, '-', '_', or '.', not
// starting with a dot.
func CheckAgent(agent string) error {
	if !memory.ValidAgentName(agent) {
		return fmt.Errorf("%w: %q is not the name of an agent profile; a name holds 1 to 64 letters, digits, '-', '_', or '.'", ErrInvalid, agent)
	}
	return nil
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func encode(rec record, svg []byte) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return nil, fmt.Errorf("faces: encode a record: %w", err)
	}
	buf.Write(svg)
	return buf.Bytes(), nil
}

func decode(value []byte) (record, []byte, error) {
	var rec record
	head, svg, ok := bytes.Cut(value, []byte("\n"))
	if !ok {
		return rec, nil, errors.New("faces: an entry has no record line")
	}
	if err := json.Unmarshal(head, &rec); err != nil {
		return rec, nil, fmt.Errorf("faces: decode a record: %w", err)
	}
	return rec, svg, nil
}

// faceOf builds the face of an agent from its record, or the identicon
// when the record holds no SVG.
func faceOf(agent string, rec record, svg []byte, rev uint64) Face {
	f := Face{Agent: agent, DisplayName: rec.DisplayName, Tagline: rec.Tagline, By: rec.By, At: rec.At, Version: rev}
	if len(svg) > 0 {
		f.SVG, f.Custom = svg, true
	} else {
		f.SVG = Identicon(agent)
	}
	f.Hash = faceHash(f.SVG, f.DisplayName, f.Tagline)
	return f
}

// faceHash is the hash of a face: the SHA-256 of the SVG, the display
// name, and the tagline, with a zero byte between them, in hex.
func faceHash(svg []byte, displayName, tagline string) string {
	h := sha256.New()
	h.Write(svg)
	h.Write([]byte{0})
	h.Write([]byte(displayName))
	h.Write([]byte{0})
	h.Write([]byte(tagline))
	return hex.EncodeToString(h.Sum(nil))
}

// Get returns the face of an agent: the stored SVG, else the identicon,
// with the display name and the tagline. The stored SVG passes the
// sanitizer again on each read, so a value that something else wrote into
// the bucket never reaches a browser: such a face, and an entry that does
// not decode, read as the identicon, and the log says so.
func (s *Store) Get(ctx context.Context, agent string) (Face, error) {
	if err := CheckAgent(agent); err != nil {
		return Face{}, err
	}
	e, err := s.b.Get(ctx, keyOf(agent))
	if errors.Is(err, bucket.ErrNotFound) {
		return faceOf(agent, record{}, nil, 0), nil
	}
	if err != nil {
		return Face{}, fmt.Errorf("faces: read the face of %s: %w", agent, err)
	}
	rec, svg, err := decode(e.Value)
	if err != nil {
		s.log.Warn("faces: an entry does not decode; the agent reads as its identicon", "agent", agent, "error", err)
		return faceOf(agent, record{}, nil, e.Revision), nil
	}
	if len(svg) > 0 {
		clean, err := Sanitize(svg)
		if err != nil {
			s.log.Warn("faces: a stored face does not pass the sanitizer; the agent reads as its identicon", "agent", agent, "error", err)
			svg = nil
		} else {
			svg = clean
		}
	}
	return faceOf(agent, rec, svg, e.Revision), nil
}

// Put sanitizes svg and stores what the sanitizer wrote as the face of an
// agent, by the writer by. The display name and the tagline stay. A face
// that the sanitizer refuses is a *Refusal, and the store keeps the face
// that it had.
func (s *Store) Put(ctx context.Context, agent string, svg []byte, by string) (Face, error) {
	if err := CheckAgent(agent); err != nil {
		return Face{}, err
	}
	clean, err := Sanitize(svg)
	if err != nil {
		return Face{}, err
	}
	return s.change(ctx, agent, by, func(rec *record, have []byte) []byte {
		rec.Hash = hashOf(clean)
		return clean
	})
}

// Meta sets the display name, at most 64 characters, and the tagline, at
// most 140, of an agent, by the writer by. An empty value clears it. The
// face stays.
func (s *Store) Meta(ctx context.Context, agent, displayName, tagline, by string) (Face, error) {
	return s.Patch(ctx, agent, &displayName, &tagline, by)
}

// Patch is Meta for the values that are not nil: a nil value stays as it
// is.
func (s *Store) Patch(ctx context.Context, agent string, displayName, tagline *string, by string) (Face, error) {
	if err := CheckAgent(agent); err != nil {
		return Face{}, err
	}
	var name, line string
	var err error
	if displayName != nil {
		if name, err = checkLine("displayName", *displayName, MaxDisplayNameRunes); err != nil {
			return Face{}, err
		}
	}
	if tagline != nil {
		if line, err = checkLine("tagline", *tagline, MaxTaglineRunes); err != nil {
			return Face{}, err
		}
	}
	return s.change(ctx, agent, by, func(rec *record, have []byte) []byte {
		if displayName != nil {
			rec.DisplayName = name
		}
		if tagline != nil {
			rec.Tagline = line
		}
		return have
	})
}

// Delete removes the stored SVG of an agent, so the agent has its
// identicon again, and returns the face after the change. The display
// name and the tagline stay; an agent with neither loses its entry.
func (s *Store) Delete(ctx context.Context, agent, by string) (Face, error) {
	return s.change(ctx, agent, by, func(rec *record, have []byte) []byte {
		rec.Hash = ""
		return nil
	})
}

// change applies f to the entry of an agent and writes the result at the
// revision that it read. An entry that holds nothing after f goes away.
// After a write or a removal, it calls the OnChange hooks.
func (s *Store) change(ctx context.Context, agent, by string, f func(rec *record, have []byte) []byte) (Face, error) {
	if err := CheckAgent(agent); err != nil {
		return Face{}, err
	}
	key := keyOf(agent)
	for range writeAttempts {
		var rec record
		var have []byte
		cur, err := s.b.Get(ctx, key)
		exists, corrupt := err == nil, false
		switch {
		case exists:
			if rec, have, err = decode(cur.Value); err != nil {
				// A write repairs an entry that does not decode.
				s.log.Warn("faces: an entry does not decode; the write replaces it", "agent", agent, "error", err)
				rec, have, corrupt = record{}, nil, true
			}
		case !errors.Is(err, bucket.ErrNotFound):
			return Face{}, fmt.Errorf("faces: read the face of %s: %w", agent, err)
		}
		before := rec
		svg := f(&rec, have)
		if exists && !corrupt && rec == before && bytes.Equal(svg, have) {
			return faceOf(agent, rec, have, cur.Revision), nil
		}
		if len(svg) == 0 && rec.DisplayName == "" && rec.Tagline == "" {
			if !exists {
				return faceOf(agent, record{}, nil, 0), nil
			}
			err := s.b.Delete(ctx, key, cur.Revision)
			if errors.Is(err, bucket.ErrConflict) {
				continue
			}
			if err != nil {
				return Face{}, fmt.Errorf("faces: remove the face of %s: %w", agent, err)
			}
			f := faceOf(agent, record{}, nil, 0)
			emitChange(f)
			return f, nil
		}
		rec.By = oneLine(by, 256)
		rec.At = s.now().UTC()
		value, err := encode(rec, svg)
		if err != nil {
			return Face{}, err
		}
		var rev uint64
		if exists {
			rev, err = s.b.Update(ctx, key, value, cur.Revision)
		} else {
			rev, err = s.b.Create(ctx, key, value)
		}
		if errors.Is(err, bucket.ErrConflict) {
			continue
		}
		if err != nil {
			return Face{}, fmt.Errorf("faces: write the face of %s: %w", agent, err)
		}
		f := faceOf(agent, rec, svg, rev)
		emitChange(f)
		return f, nil
	}
	return Face{}, fmt.Errorf("%w: other writers changed the face of %s %d times while this write tried; try again", ErrConflict, agent, writeAttempts)
}

// checkLine checks a display name or a tagline: one line without control
// characters, of at most n characters after the spaces at its ends go.
func checkLine(field, v string, n int) (string, error) {
	v = strings.TrimSpace(v)
	if !utf8.ValidString(v) || strings.ContainsFunc(v, unicode.IsControl) {
		return "", fmt.Errorf("%w: %s must be one line of text without control characters", ErrInvalid, field)
	}
	if c := utf8.RuneCountInString(v); c > n {
		return "", fmt.Errorf("%w: %s holds at most %d characters, and this one has %d", ErrInvalid, field, n, c)
	}
	return v, nil
}

// oneLine turns s into one line of at most n bytes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)), " ")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
