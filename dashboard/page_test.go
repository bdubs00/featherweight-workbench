package dashboard

import (
	"regexp"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// block returns the part of the page from start to the first end after it.
func block(t *testing.T, page, start, end string) string {
	t.Helper()
	i := strings.Index(page, start)
	if i < 0 {
		t.Fatalf("the page has no %q", start)
	}
	j := strings.Index(page[i:], end)
	if j < 0 {
		t.Fatalf("the block %q has no end %q", start, end)
	}
	return page[i : i+j]
}

// TestPageAdapters checks that the MCP adapter and the native adapter of
// the page carry the same calls, with the calls of the workbench, and that
// neither reads usage any more.
func TestPageAdapters(t *testing.T) {
	page := string(Page())
	calls := []string{"whoami", "home", "agents", "agent", "putAgent", "deleteAgent", "runs", "result", "items", "activity",
		"file", "download", "start", "builder", "previewAgent", "send", "decide", "cancel", "fleet",
		"connections", "connect", "disconnect"}
	for _, name := range []string{"const mcp = (() => {", "const native = (() => {"} {
		b := block(t, page, name, "\n})();")
		for _, c := range calls {
			if !regexp.MustCompile(`\n\s+` + c + `: `).MatchString(b) {
				t.Errorf("%s has no call %s", name, c)
			}
		}
		if regexp.MustCompile(`\n\s+usage: `).MatchString(b) {
			t.Errorf("%s still reads usage", name)
		}
	}
	for _, want := range []string{"'/v1/home'", "/result'", "'/v1/builder'", "?download=1", "'run_result'", "'agent_builder'", "{view: 'home'}", "raw: true"} {
		if !strings.Contains(page, want) {
			t.Errorf("the adapters do not use %s", want)
		}
	}
}

// TestPageWorkbench checks the shape of the workbench: no usage screen,
// no opening animation, the builder prompt on the server only, a status
// word for each status, the next actions of a result, and no external
// asset.
func TestPageWorkbench(t *testing.T) {
	page := string(Page())
	for _, gone := range []string{"screens.usage", "/v1/usage", "api.usage", "intro.start", "'opening'", ".reveal", "class=\"meter\"", "BUILDER", "builderRun", "draftProfile", "hud("} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still has %q", gone)
		}
	}
	if strings.Contains(page, BuilderPrompt) {
		t.Error("the page holds the builder prompt; it belongs to the server")
	}
	if !strings.Contains(page, "What would you like to move forward?") {
		t.Error("home has no composer")
	}
	words := block(t, page, "const STATUS = {", "};")
	for _, s := range []string{StatusNeedsYou, StatusWorking, StatusReady, StatusEnded, StatusFailed, StatusCanceled} {
		if !regexp.MustCompile(`'?` + regexp.QuoteMeta(s) + `'?: '[A-Z]`).MatchString(words) {
			t.Errorf("the status %s has no word", s)
		}
	}
	for _, k := range []string{NextDecide, NextAnswer, NextConnect, NextReview, NextReply, NextSaveAgent, NextRetry} {
		if !strings.Contains(page, "'"+k+"'") {
			t.Errorf("the page does not handle the next action %s", k)
		}
	}
	external := regexp.MustCompile(`(?i)(<(script|link|img|iframe)[^>]+(src|href)="(https?:)?//)|@import|url\(\s*['"]?https?:`)
	if m := external.FindString(page); m != "" {
		t.Errorf("the page loads an external asset: %s", m)
	}
}

func TestPageURL(t *testing.T) {
	cfg := fw.DefaultConfig()
	if got := PageURL(cfg); got != "" {
		t.Errorf("no URL = %q", got)
	}
	cfg.A2A.URL = "https://agent.example.com/a2a"
	if got := PageURL(cfg); got != "https://agent.example.com/ui" {
		t.Errorf("a2a URL = %q", got)
	}
	cfg.MCP.Auth.Audience = "https://fleet.example.com/mcp"
	if got := PageURL(cfg); got != "https://fleet.example.com/ui" {
		t.Errorf("audience = %q", got)
	}
	cfg.MCP.Auth.Audience = "fleet-audience"
	if got := PageURL(cfg); got != "https://agent.example.com/ui" {
		t.Errorf("a name that is no URL = %q", got)
	}
	if got := PageURL(nil); got != "" {
		t.Errorf("nil = %q", got)
	}
}

// TestPageDependable checks the parts of the page that keep the work of a
// person: drafts per place, updates in place, paging back, the sign-in
// screen, the keyboard, and plain words in the conversation.
func TestPageDependable(t *testing.T) {
	page := string(Page())
	for _, want := range []struct{ what, text string }{
		{"text drafts in sessionStorage", "sessionStorage.setItem('fw.draft.'"},
		{"small files in sessionStorage", "sessionStorage.setItem('fw.files.'"},
		{"a draft for home", `data-draft="home"`},
		{"a draft for each run", `data-draft="run:`},
		{"a draft for the builder form", `data-draft="create"`},
		{"rows that change in place", "morph(box, this.draw(h))"},
		{"a decision form that a poll leaves alone", "f.dataset.dirty = '1'"},
		{"a run that opens at its recent end", "api.items(s.id, {limit: PAGE})"},
		{"earlier items on demand", "{before: ctx.prev, limit: PAGE}"},
		{"a control for earlier items", "Load earlier messages"},
		{"the sign-in route of a 401", "data.signin"},
		{"the way back after sign-in", "'next=' + enc(next)"},
		{"the token form behind a disclosure", "Use an access token"},
		{"the lines of the native events page", "(r && r.lines)"},
		{"the slash key", "ev.key === '/'"},
		{"Cmd or Ctrl+Enter", "(ev.metaKey || ev.ctrlKey)"},
		{"the arrow keys and j and k", "['ArrowDown', 'ArrowUp', 'j', 'k']"},
		{"plain words for a step that waits", "'Waits for you to allow '"},
		{"the display mode of the host", "ctx.displayMode"},
	} {
		if !strings.Contains(page, want.text) {
			t.Errorf("the page has no %s: %q", want.what, want.text)
		}
	}
	// The conversation names no tool where a step waits, and the page no
	// longer words raw events itself.
	for _, gone := range []string{"waits for an approval", "${name(waiting)}", "const line = e =>"} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still has %q", gone)
		}
	}
	// Every use of browser storage sits in a try block that opens on its
	// line or on the line before.
	for _, m := range regexp.MustCompile(`(sessionStorage|localStorage)\.(get|set|remove)Item`).FindAllStringIndex(page, -1) {
		from := strings.LastIndex(page[:m[0]], "\n")
		if prev := strings.LastIndex(page[:max(from, 0)], "\n"); prev >= 0 {
			from = prev
		}
		if !strings.Contains(page[max(from, 0):m[0]], "try {") {
			t.Errorf("storage outside a try block near %q", page[m[0]:min(m[1]+40, len(page))])
		}
	}
}

// TestPageConnect checks the connection screens and the connect card: the
// routes of the spec, Connect and Skip but no Allow on the card, the link
// through the host, and the menu entry for a person only.
func TestPageConnect(t *testing.T) {
	page := string(Page())
	for _, want := range []string{
		"screens.connections = {", "screens.connect = {", "screens.connected = {",
		"'/v1/connections'", "'/v1/connections/' + enc(server)", "call('connections')", "call('disconnect', {server})",
		"'ui/open-link'", "location.assign(r.url)", "'#run/' + d.run", `data-next="#connections"`,
		"data-view=\"connections\">Connections</button>' : ''", "principal.kind === 'user'",
		"'Connected ' : 'Skipped '", "Waits for you to connect ",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page has no %q", want)
		}
	}
	card := block(t, page, "function connectCard(run, d) {", "\n}\n")
	for _, want := range []string{`data-action="skip"`, `data-action="connect"`, ">Skip</button>", ">Connect</button>"} {
		if !strings.Contains(card, want) {
			t.Errorf("the connect card has no %q", want)
		}
	}
	if strings.Contains(card, "Allow") || strings.Contains(card, `value="allow"`) {
		t.Error("the connect card offers Allow; a click is not a grant")
	}
}
