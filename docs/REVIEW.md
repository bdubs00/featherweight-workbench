> Moved on 2026-10-17 from the root of featherweight-agent. The text below is unchanged. Its file paths name that repository at the time of writing. The code it describes now lives in featherweight-workbench and, for the kernel, in featherweight-kernel.

# Featherweight Kernel review

## Bottom line

**The execution kernel is substantially ahead of the product experience.** Much of your agent architecture already exists. What is missing is the layer that turns user intent into understandable, reusable, coordinated work.

I would not start with a visual redesign alone. I would define the product objects and UI/MCP contract together, then build a conversational agent-creation flow on that foundation.

This covers the outline you sent; it ends mid-sentence.

## 1. What already exists

I checked the implementation, not just the architecture document.

| Your idea | Status | What exists or remains |
|---|---|---|
| Natural-language work requests | **Present** | Users give an agent a task through CLI, HTTP, MCP, or A2A. |
| All major components configurable through language | **Partial** | An MCP host can translate language into management calls. The application has no first-party conversational setup flow. |
| Missions | **Missing** | No mission object, lifecycle, completion criteria, or mission-level view. |
| Goals | **Missing** | No tracked outcomes, dependencies, or acceptance evidence. The ax bootstrap “goal” is a different concept. |
| Tasks | **Partial** | Runs and child runs execute assignments. There is no separate durable task with assignment, attempts, dependencies, and acceptance. |
| Reusable agents | **Present** | Named profiles, runtime creation/editing/deletion, and persistent profile storage. |
| Specialized roles | **Partial** | Responsibilities live in descriptions, prompts, and instructions. No structured specialization or capability matching. |
| Scoped tools and permissions | **Present** | Tool allowlists, policy rules, approvals, ownership roles, and child restrictions. |
| Skills | **Present** | Skill discovery, allowlists, on-demand reading, and MCP resources. |
| SPIFFE identity | **Partial** | Workload authentication and outbound JWT-SVIDs exist. Each profile does **not** receive its own provisioned identity. |
| Owner | **Present** | Owners, operators, viewers, creator permissions, and automatic ownership on creation. |
| Task awareness | **Present, bounded** | Runs receive their assignment. Children require self-contained briefs rather than inheriting the conversation. |
| Mission awareness | **Missing structurally** | A prompt can carry context, but there is no mission context object or automatic propagation. |
| User awareness | **Partial** | Caller identity and ownership are tracked. User preferences and relevant personal context are not automatically assembled for the model. |
| Workspaces | **Partial** | Filesystem boundaries, file-change tracking, and ax workspace preparation exist. No end-user workspace lifecycle or per-profile isolation. |
| Resources that create constructs | **Missing as described** | Skills and profiles are useful ingredients, but there is no versioned blueprint/template system for creating agents or workflows. |

The foundation also includes memory, compaction, budgets, restart/resume, replay, child delegation, fleet visibility, telemetry, and approval handling.

The central definitions are in [`fw/run.go`](fw/run.go), with profile management in [`fw/profiles.go`](fw/profiles.go).

### Two important boundaries

- **A profile is not an isolated worker.** Profiles currently share the process workspace and built-in bash tool instance. A tool allowlist is not a filesystem, process, or identity sandbox.
- **Spawning a child run is not creating a persistent agent.** Both capabilities exist, but the UX needs to explain their different lifetimes.

## 2. Clarify the product model before expanding it

I would slightly revise your hierarchy:

```text
Mission
  └─ Goals
       └─ Tasks
            └─ Runs / attempts

Tasks are assigned to agents.
Agents use capabilities and workspaces.
```

This distinction matters:

- A **task** describes the outcome to achieve.
- A **run** records one execution attempt.
- An **agent** is reusable across many tasks.
- A model finishing a run does not necessarily mean the task passed acceptance.

For your “Resources,” I suggest the product term **Blueprints**. MCP already uses “resources” to mean readable context, which is different from a reusable recipe for constructing something.

These new objects need canonical state and lifecycle events. They cannot be produced solely by adding dashboard projections. They can, however, sit above the existing run engine without making the turn loop mission-specific.

## 3. Why agent creation feels wrong

The current editor in [`surface/dashboard/index.html:708`](surface/dashboard/index.html#L708) is essentially a configuration editor.

It asks users to understand:

- Model selectors and reasoning effort.
- System prompts.
- Tool and skill globs.
- Identity subjects and access patterns.
- Instructions, policy, and budget expressed as JSON.

That is a valid expert escape hatch, but a poor primary creation experience.

**The user’s question is “What do I want this agent to handle?” The form’s question is “How do I configure the runtime?”**

### A better creation flow

Start with:

> **What would you like this agent to handle?**

For example:

> “Review our pull requests for security issues. Explain findings clearly, but never push changes.”

Then show an editable agent brief:

| Section | User-facing summary |
|---|---|
| Responsibility | Review pull requests for security issues |
| Capabilities | Read repository files and inspect pull requests |
| Boundaries | Cannot push, merge, or deploy |
| Deliverable | Findings with severity, evidence, and suggested fixes |
| Ownership | You |
| Limits | Visible working-time and spending limits |

The flow becomes:

**Describe → clarify → review → try → publish**

Important details:

- Ask only questions that materially affect behavior.
- Select capabilities from a real catalog; do not invent unavailable integrations.
- Show effective permissions, not just generated configuration.
- Make test runs explicitly bounded; “try” must not imply harmless execution.
- Keep raw configuration under **Advanced**.
- Let users refine the draft through conversation.

Also, **do not require agent creation before useful work**. Offer “Do this once,” then “Save this setup as an agent” after a successful run.

That is a bigger ergonomic improvement than a more elaborate wizard.

### The broader UI direction

Keep the existing conversation and tool-step presentation. Add an outcome-oriented workbench:

- **Mission:** objective, outcomes, plan, and evidence.
- **Needs you:** approvals, questions, blockers, and scope changes.
- **Work:** tasks, dependencies, assignments, and attempts.
- **Agents:** reusable responsibilities and capability boundaries.

The inventive part is making delegation and consequences understandable—not adding more animation.

## 4. MCP already has considerable surface area

The server currently exposes:

- **Execution:** `run`, `run_status`, `send`, `cancel`, `approve`.
- **Agent management:** `agents_list`, `agent_get`, `agent_put`, `agent_delete`.
- **Inspection:** `whoami`, `runs_list`, `run_events`, `run_file`, `usage`.
- **Presentation:** `dashboard`, plus `fleet` when enabled.
- **Resources:** run objects, ledgers, skills, and the dashboard.
- **Prompts:** one per profile.
- **Gateway:** configured upstream tools can be re-exported.

Both the browser dashboard and MCP App already share presentation code. **That is worth preserving.**

### What the next contract needs

| Area | Recommended addition |
|---|---|
| Discovery | Available capabilities, tools, skills, models, blueprints, and connection readiness |
| Agent creation | Draft, validate, preview effective permissions, and publish |
| Safe editing | Revisions, diffs, and protection against overwriting concurrent edits |
| Missions | Create, plan, revise, inspect, and manage goals/tasks |
| Context | Explicit links between mission, task, agent revision, workspace, and run |
| Approvals | Schema-driven forms instead of asking users to enter structured answers into a textarea |

These are proposed additions, not existing features.

**Natural language belongs above typed, validated operations.** It proposes changes; the domain layer validates and authorizes them. It must not become a shortcut around ownership, policy, or infrastructure permissions.

## 5. Concrete issues to address first

I reproduced three contract problems using a local fake-model probe:

1. **Private profile information leaks through MCP discovery and prompts.**
   A caller denied by `agent_get` can still receive the profile’s first prompt line in server instructions and retrieve its system prompt through `prompts/get`.
   See [`surface/mcp/mcp.go:255`](surface/mcp/mcp.go#L255) and [`surface/mcp/resources.go:161`](surface/mcp/resources.go#L161). This is the highest-priority finding.

2. **Budget data does not round-trip through MCP.**
   `agent_get` returns deadlines as numeric Go durations; `agent_put` requires duration strings. Saving an unchanged deadline through the MCP editor fails validation.

3. **New profiles do not appear in the existing server’s prompt list.**
   Prompts are registered when the MCP server is constructed, rather than following runtime profile changes.

Other relevant gaps:

- An empty tools list means **every tool**, while an empty skills list means **no skills**. A least-privilege builder needs explicit selection semantics without silently breaking existing clients.
- Schema-bearing questions currently render as a generic answer textarea.
- Configuration-defined profiles cannot be edited through the profile store, but the editor does not expose that distinction upfront.
- Browser sign-in is token pasting; authentication support exists, but the standalone end-user login experience is incomplete.
- The MCP Tasks extension is not served today. Existing run IDs and polling are the usable fallback.

## 6. Recommended next leap

I would deliver this in three slices:

### 1. Harden the shared contract

Fix the reproduced issues, normalize wire formats, make discovery caller-scoped, and expose effective capabilities and configuration provenance.

### 2. Build the conversational agent builder

Deliver one complete path in both browser and MCP:

> Describe a read-only reviewer → review its boundaries → run a sample → publish → use it.

Acceptance criterion: **the user never needs JSON, tool globs, or an identity subject to complete that path.**

### 3. Add one mission vertical slice

Support one mission with goals, assigned tasks, run attempts, and acceptance evidence. Build the mission screen around those real objects—not a cosmetic grouping of runs.

**My recommendation: make “intent to a trustworthy, reusable agent” the next milestone.** Define the mission model now, but avoid implementing a broad workflow engine before that basic experience is excellent.

---

**Verification:** `go build ./...`, `go vet ./...`, and `go test -short ./...` passed. No real model providers were called. This was a code and local-protocol survey, not a browser usability test. The survey was read-only.
