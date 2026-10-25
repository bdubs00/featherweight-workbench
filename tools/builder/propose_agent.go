// Package builder holds the tool propose_agent, with which a model proposes
// a new agent to a person. The workbench turns a proposal into a card with
// a Create agent button. Nothing is created until the person taps it.
package builder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/tools/builtin"
)

// workerTools holds the check of the tools of a new agent, which the fleet
// registers in init.
var workerTools struct {
	mu sync.Mutex
	fn func(ctx context.Context, a *fw.Agent, name string, tools []string) string
}

// RegisterToolsCheck sets how propose_agent learns whether the workers of
// the agent offer the tools of a proposal. fn says in words what they do
// not offer, or "" when they offer each tool or it cannot tell. Without it,
// the tool knows the tools of this process alone, and a pool can refuse
// the proposal when the person taps Create agent.
func RegisterToolsCheck(fn func(ctx context.Context, a *fw.Agent, name string, tools []string) string) {
	workerTools.mu.Lock()
	defer workerTools.mu.Unlock()
	workerTools.fn = fn
}

// Name is the name of the tool.
const Name = "propose_agent"

// title is the title of the tool, in the words of the other built-in tools.
const title = "Propose new agents"

func init() {
	fw.RegisterTool(Name, newProposeAgent)
	// The workbench offers the tool by default. A profile still has to name
	// it, because the tool is opt-in.
	fw.RegisterDefaults(func(c *fw.Config) {
		c.Tools.Builtin = append(c.Tools.Builtin, Name)
	})
}

const proposeAgentSchema = `{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "pattern": "^[a-z0-9][a-z0-9-]{0,39}$",
      "description": "A short name in lowercase with hyphens, such as pr-reviewer."
    },
    "description": {
      "type": "string",
      "description": "One sentence that says what the agent handles, for the person."
    },
    "instructions": {
      "type": "string",
      "description": "The system prompt of the new agent: what it does, how it works, how it reports, and what it never does. Write to the agent as you."
    },
    "tools": {
      "type": "array",
      "items": {"type": "string"},
      "description": "The names of the tools that the agent needs, and no other."
    },
    "asks_first": {
      "type": "array",
      "items": {"type": "string"},
      "description": "The names of tools from tools that wait for the person to allow each call."
    },
    "never_runs": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Shell commands that the agent can never run, as patterns such as \"git push*\". It needs bash in tools."
    },
    "minutes": {
      "type": "integer",
      "minimum": 1,
      "maximum": 1440,
      "description": "The most minutes of work for one run."
    }
  },
  "required": ["name", "description", "instructions", "tools"],
  "additionalProperties": false
}`

const proposeAgentDescription = `Propose a new agent to the person. The person sees the proposal as a card with a Create agent button. Nothing is created until the person taps it.
Before you call it, make sure that you know what the agent handles and what it must never do. If one of them is unclear, ask the person in a short message and end your turn. Ask at most two questions.
Give the agent each tool that it needs and no other. Put a tool that changes things in asks_first, unless the person said that the agent can act alone. Use never_runs for commands that the person forbids.
After the call, tell the person in one sentence to tap Create agent or to say what to change. If the person asks for a change, call this tool again with the whole new proposal.`

// proposeAgentTool checks a proposal for a new agent and returns it. It
// creates nothing: the person creates the agent from the proposal, through
// the surfaces, with the rights of the person. The tool is opt-in, so only a
// profile that names it gets it.
type proposeAgentTool struct{ catalog string }

// newProposeAgent words the tools of the configuration for the description:
// each built-in tool with its title, and the MCP servers whose tools the
// proposal can name.
func newProposeAgent(cfg *fw.Config) (fw.Tool, error) {
	var b strings.Builder
	b.WriteString("\nThe built-in tools:")
	for _, name := range cfg.Tools.Builtin {
		if name == "propose_agent" || name == "await_task" {
			continue
		}
		fmt.Fprintf(&b, "\n- %s: %s", name, builtin.Title(name))
	}
	if len(cfg.Tools.MCP) > 0 {
		var servers []string
		for _, s := range cfg.Tools.MCP {
			servers = append(servers, s.Name)
		}
		fmt.Fprintf(&b, "\nThe tools of the MCP servers %s have names such as %s.example. If a name does not exist, the result lists every tool.", strings.Join(servers, ", "), servers[0])
	}
	return proposeAgentTool{catalog: b.String()}, nil
}

// Spec implements fw.Tool.
func (t proposeAgentTool) Spec() fw.ToolSpec {
	s := fw.ToolSpec{
		Name:        Name,
		Title:       title,
		Description: proposeAgentDescription + t.catalog,
		InputSchema: json.RawMessage(proposeAgentSchema),
		Source:      builtin.Source,
		Trusted:     true,
	}
	s.ReadOnly = true
	s.Idempotent = true
	s.OptIn = true
	return s
}

// proposal is the input of propose_agent. The dashboard turns it into a
// profile: a tool in AsksFirst becomes an ask rule, a command in NeverRuns
// a deny rule for bash, and Minutes the deadline of the budget.
type proposal struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Instructions string   `json:"instructions"`
	Tools        []string `json:"tools"`
	AsksFirst    []string `json:"asks_first,omitempty"`
	NeverRuns    []string `json:"never_runs,omitempty"`
	Minutes      int      `json:"minutes,omitempty"`
}

var proposalName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Call implements fw.Tool.
func (proposeAgentTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	var in proposal
	if err := decode(input, &in); err != nil {
		return badInput("propose_agent", err), nil
	}
	var problems []string
	if !proposalName.MatchString(in.Name) {
		problems = append(problems, fmt.Sprintf("the name %q is not lowercase letters, digits, and hyphens, such as pr-reviewer", in.Name))
	}
	if strings.TrimSpace(in.Description) == "" {
		problems = append(problems, "the description is empty")
	}
	if strings.TrimSpace(in.Instructions) == "" {
		problems = append(problems, "the instructions are empty")
	}
	// Without an agent in the context, as in a unit test, no name is checked.
	a := fw.AgentFrom(ctx)
	known := map[string]fw.ToolSpec{}
	if a != nil {
		specs, err := a.ToolSpecs(ctx)
		if err != nil {
			return fw.ErrorResult(fmt.Sprintf("propose_agent: list the tools: %v", err)), nil
		}
		for _, s := range specs {
			if !s.OptIn {
				known[s.Name] = s
			}
		}
	}
	// An empty tool list means every tool, so a proposal names at least one.
	if len(in.Tools) == 0 {
		problems = append(problems, "tools is empty; name at least one tool, such as files for an agent that only writes")
	}
	inTools := map[string]bool{}
	for _, t := range in.Tools {
		inTools[t] = true
		if _, ok := known[t]; !ok && a != nil {
			problems = append(problems, fmt.Sprintf("no tool is named %q", t))
		}
	}
	workerTools.mu.Lock()
	check := workerTools.fn
	workerTools.mu.Unlock()
	if a != nil && check != nil && len(in.Tools) > 0 {
		// The workers of the pool of the agent run it, and they can offer
		// fewer tools than this process.
		if why := check(ctx, a, in.Name, in.Tools); why != "" {
			problems = append(problems, why)
		}
	}
	for _, t := range in.AsksFirst {
		if !inTools[t] {
			problems = append(problems, fmt.Sprintf("asks_first names %q, which is not in tools", t))
		}
	}
	for _, c := range in.NeverRuns {
		if !inTools["bash"] {
			problems = append(problems, "never_runs needs bash in tools")
			break
		}
		if strings.TrimSpace(c) == "" {
			problems = append(problems, "never_runs has an empty command")
			break
		}
	}
	if len(problems) > 0 {
		return fw.ErrorResult("propose_agent: " + strings.Join(problems, "; ") + ".\n" + toolList(known)), nil
	}
	return fw.TextResult(fmt.Sprintf("The person now sees the proposal for %s with a Create agent button. The agent does not exist until the person taps it. Tell the person to tap Create agent or to say what to change.", in.Name)), nil
}

// toolList names the tools that a proposal can use, with their titles.
func toolList(known map[string]fw.ToolSpec) string {
	if len(known) == 0 {
		return "No tool exists here."
	}
	names := make([]string, 0, len(known))
	for n := range known {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("The tools that exist:")
	for _, n := range names {
		s := known[n]
		what := s.Title
		if what == "" {
			what, _, _ = strings.Cut(s.Description, "\n")
		}
		fmt.Fprintf(&b, "\n- %s: %s", n, what)
	}
	return b.String()
}

// decode reads the tool input into v. An empty input is an empty object.
func decode(input json.RawMessage, v any) error {
	if len(bytes.TrimSpace(input)) == 0 {
		return nil
	}
	return json.Unmarshal(input, v)
}

// badInput is the error result for input that does not decode.
func badInput(tool string, err error) fw.ToolResult {
	return fw.ErrorResult(fmt.Sprintf("%s: the input is not valid JSON for this tool: %v", tool, err))
}
