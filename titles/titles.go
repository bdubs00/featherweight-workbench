// Package titles names the work. A run gets a title from its request or from
// the first sentence of its input (fw.TitleFrom). This package asks a model
// for a title of a few words the first time that the workbench shows a run
// that has none, and records it on the run through fw.Agent.Annotate, with
// the usage of the call. The kernel makes no title call of its own. See
// docs/ARCHITECTURE.md section 9.
package titles

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bdubs00/featherweight-kernel/fw"
)

const (
	// Block is the top-level key of fw.yaml that the workbench claims.
	Block = "workbench"
	// EnvTitles is the variable that sets the mode. It wins over the file.
	EnvTitles = "FW_WORKBENCH_TITLES"
	// ModeModel asks a model for a title, the default. ModeInput keeps the
	// title of the input and makes no call.
	ModeModel = "model"
	ModeInput = "input"
)

// Config is the part of the block that this package reads.
type Config struct {
	Titles string `yaml:"titles"`
}

func init() { fw.RegisterExtension(Block) }

// Mode returns the mode of the configuration: EnvTitles when it is set,
// else the block of the file, else ModeModel.
func Mode(cfg *fw.Config) string {
	if raw, ok := os.LookupEnv(EnvTitles); ok && strings.TrimSpace(raw) != "" {
		return strings.ToLower(strings.TrimSpace(raw))
	}
	var c Config
	if cfg != nil {
		_, _ = cfg.Extension(Block, &c)
	}
	if m := strings.ToLower(strings.TrimSpace(c.Titles)); m != "" {
		return m
	}
	return ModeModel
}

// System is the system prompt of a title call.
const System = "Write a title for the task below in at most six words, in the language of the task. " +
	"Name the work, not the worker. Use sentence case, as in \"Fix checkout retries\". Answer with the title only: no quotes, no final period, no Markdown."

// Bounds of a title call.
const (
	timeout   = 60 * time.Second
	maxTokens = 512 // room for a model that reasons before it answers
	maxInput  = 2000
	parallel  = 4 // title calls in flight at once, per agent
	remember  = 8192
)

// titler writes the titles of one agent. It remembers each run that it
// handled, so a run gets one call per process.
type titler struct {
	mu   sync.Mutex
	seen map[string]bool
	sem  chan struct{}
}

var titlers sync.Map // *fw.Agent to *titler

func titlerOf(a *fw.Agent) *titler {
	if t, ok := titlers.Load(a); ok {
		return t.(*titler)
	}
	t, _ := titlers.LoadOrStore(a, &titler{seen: map[string]bool{}, sem: make(chan struct{}, parallel)})
	return t.(*titler)
}

// Want asks for the title of each run that has none, in the background,
// and returns at once. A run gets a title when it is a top-level run of
// this process, its request set no title, its input has text, and it is
// not a replay. In the mode input, Want does nothing. Each run gets one
// call per process, whatever the outcome.
func Want(a *fw.Agent, runs ...*fw.Run) {
	if a == nil || Mode(a.Config()) != ModeModel {
		return
	}
	t := titlerOf(a)
	for _, r := range runs {
		if !wants(r) || !t.claim(r.ID) {
			continue
		}
		go t.write(a, r)
	}
}

// wants reports whether a run needs a title.
func wants(r *fw.Run) bool {
	return r != nil && r.Parent == "" && r.Member == "" && strings.TrimSpace(r.Input) != "" &&
		strings.TrimSpace(r.Metadata[fw.MetaTitle]) == "" && r.Metadata["replayOf"] == ""
}

// claim records a run and reports whether it was new.
func (t *titler) claim(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen[id] {
		return false
	}
	if len(t.seen) >= remember {
		clear(t.seen)
	}
	t.seen[id] = true
	return true
}

// write asks the model for a title and records it on the run, with the
// usage of the call. A call that yields no title still records its usage,
// because the tokens cost. The call goes to compaction.model when it is
// set, else to the model of the agent of the run.
func (t *titler) write(a *fw.Agent, r *fw.Run) {
	t.sem <- struct{}{}
	defer func() { <-t.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	log := a.Logger().With("run", r.ID)
	cfg := a.Config()
	name := cfg.Compaction.Model
	if name == "" {
		name = cfg.Agents[r.Agent].Model
	}
	m, err := a.ModelFor(ctx, name)
	if err != nil {
		log.Debug("titles: no model for the title", "error", err)
		return
	}
	input := r.Input
	if rs := []rune(input); len(rs) > maxInput {
		input = string(rs[:maxInput])
	}
	resp, err := m.Generate(ctx, &fw.Request{
		System:    System,
		Messages:  []fw.Message{{Role: fw.RoleUser, Content: []fw.Block{fw.Text(input)}}},
		Effort:    "low",
		MaxTokens: maxTokens,
	})
	if err != nil {
		log.Debug("titles: the model wrote no title", "error", err)
		return
	}
	d := fw.RunAnnotatedData{Provider: resp.Provider, Model: resp.Model}
	if resp.Usage != (fw.Usage{}) {
		u := resp.Usage
		d.Usage = &u
	}
	if title := Clean(fw.TextOf(resp.Content)); title != "" {
		d.Metadata = map[string]string{fw.MetaTitle: title}
	} else if d.Usage == nil {
		return
	}
	if err := a.Annotate(ctx, r.ID, d); err != nil {
		log.Debug("titles: the title was not recorded", "error", err)
	}
}

// Clean turns the answer of a title call into a title: its last line with
// text, without Markdown marks, quotes, a "Title:" label, or a final
// period, folded to one line and cut to fw.MaxTitleRunes. It returns ""
// when nothing is left.
func Clean(s string) string {
	var line string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			line = l
		}
	}
	line = strings.TrimLeft(line, "#>*-+ \t")
	if i := strings.IndexByte(line, ':'); i >= 0 && strings.EqualFold(strings.TrimSpace(line[:i]), "title") {
		line = line[i+1:]
	}
	line = strings.Trim(strings.TrimSpace(line), "\"'`*_“”‘’")
	line = strings.TrimRight(line, ".。")
	line = strings.Join(strings.FieldsFunc(line, unicode.IsSpace), " ")
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, line)
	if r := []rune(line); len(r) > fw.MaxTitleRunes {
		line = strings.TrimSpace(string(r[:fw.MaxTitleRunes])) + "…"
	}
	return line
}
