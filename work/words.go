package work

// This file says a tool in plain words for a person: what a call of it
// does, such as "writes a page of the wiki" for front.pages_write. The
// readback of a plan says when a task asks the person ("Before it writes
// a page of the wiki"), and a card says what its run does now ("Runs a
// shell command."). Neither names a tool or the word front (walk findings
// L8 and L17).

import "strings"

// toolDoes says what a call of a tool does, by the name of the tool
// without its server, as a phrase after "it".
var toolDoes = map[string]string{
	"bash":          "runs a shell command",
	"shell":         "runs a shell command",
	"exec":          "runs a shell command",
	"files":         "works with files",
	"http":          "fetches a web page",
	"web_fetch":     "fetches a web page",
	"env":           "changes its packages",
	"memory":        "uses its notes",
	"recall":        "searches its notes",
	"pages_write":   "writes a page of the wiki",
	"pages_read":    "reads a page of the wiki",
	"pages_search":  "searches the wiki",
	"pages_list":    "lists the pages of the wiki",
	"pages_history": "reads the history of a page of the wiki",
	"bridge_open":   "opens a team",
	"bridge_post":   "posts in the channel of its team",
	"bridge_read":   "reads the channel of its team",
	"bridge_close":  "closes a team",
	"spawn_task":    "hands a task to another agent",
	"send":          "sends a message to another agent",
	"ask_user":      "asks you a question",
}

// frontServer is the server of the tools of the front, which a person
// knows as the workbench, not by that name.
const frontServer = "front"

// toolWords says what a call of the tool does, as a phrase after "it",
// such as "runs a shell command" for bash. A tool that it does not know is
// "calls <its words> in <its server>", with the underscores of its name as
// spaces, as in "calls update incident in servicenow".
func toolWords(tool string) string {
	tool = strings.TrimSpace(tool)
	server, base := "", tool
	if i := strings.LastIndexAny(tool, "./"); i >= 0 {
		server, base = tool[:i], tool[i+1:]
	}
	if w, ok := toolDoes[base]; ok {
		return w
	}
	switch {
	case strings.HasPrefix(base, "pages_"):
		return "uses the wiki"
	case strings.HasPrefix(base, "bridge_"):
		return "uses a team"
	case base == "":
		return "uses a tool"
	}
	w := "calls " + strings.Join(strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(base)), " ")
	if server != "" && server != frontServer {
		w += " in " + server
	}
	return w
}

// serverWords names the server of a pattern such as github.*, for "a tool
// of ...": the front is "this server".
func serverWords(server string) string {
	if server == frontServer {
		return "this server"
	}
	return server
}

// capital writes the first letter of s as a capital.
func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
