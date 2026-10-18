# Product direction

## 1. The thesis

Featherweight turns sandboxed containers on Google AX and Agent Substrate into durable, capability-bounded actors that work for a person and cost nothing while they wait.

Featherweight is not a chatbot and not a general workflow engine. It is an operating layer between an agent loop and a snapshotting container substrate. It splits the system into two binaries with a strict trust boundary:

1. **The worker (`fw`, in `featherweight-kernel`)** runs as PID 1 inside one AX Task. It runs the turn loop, enforces the budget and the policy, and writes every event to an append-only CloudEvents ledger on `/workspace`. It holds no OAuth refresh token, no team rule, and no view of other Tasks.
2. **The front (`fw-workbench`, in `featherweight-workbench`)** runs outside the sandboxes as a standard Deployment. It authenticates people, keeps their OAuth grants in an encrypted store, serves the MCP server (`/mcp`) and the web page (`/ui`), puts work on the bus intake queues, projects fleet state from the AX API and the bus egress stream, and routes interactive requests to Tasks through the `atenet-router`.

The platforms hold the state. AX holds the Tasks. Substrate snapshots and restores `/workspace`. NATS carries work in and lifecycle events out. The ledger inside each Task holds the run. Prometheus, an OpenTelemetry collector, and Grafana hold the metrics, logs, and traces. The front holds the OAuth grants of people and nothing else of its own.

## 2. Why this layer exists

Google AX and Agent Substrate give an agent a gVisor sandbox, a persistent `/workspace` volume across snapshots, wake-on-request routing through `atenet-router`, and network egress policies. AX treats the container inside a Task as a black box. It does not know what a model turn is, whether a process is burning tokens or waiting on a person, how to resume an interrupted model stream without breaking the prompt cache, or how to limit a tree of delegated child Tasks.

Most agent frameworks sit at the other extreme. They hold conversation state in memory or in an external database, run subagents in one shared process with one service credential, and treat sandboxes as stateless command runners. When a run needs a human decision, it either blocks a running container and burns compute or drops the container and loses the filesystem state.

Featherweight bridges the two. Because the kernel owns both PID 1 in the sandbox and the append-only ledger of the run, it maps every turn, tool call, approval, and budget onto Substrate's snapshot, suspend, and wake lifecycle.

## 3. Three core capabilities

### 3.1 Zero-cost waiting (stateful suspend on block)

A long task often blocks on a person or an external system: a policy approval before `git push`, a clarifying question through `ask_user`, an OAuth sign-in for an MCP server, or a slow child Task.

In Featherweight, a blocked run costs neither compute nor deadline time:

1. When a run enters `input-required`, the kernel records `fw.approval.requested` in `ledger.jsonl`, syncs the file to `/workspace`, and parks the run driver.
2. When no run in the Task works, the worker reports `idle` on `/v1/status` and on the egress stream.
3. The front suspends the AX Task. Substrate snapshots `/workspace` and frees the compute.
4. Hours or days later, the person approves the action, answers the question, or finishes the OAuth connection through an MCP host or `/ui`.
5. The front forwards the decision through `atenet-router` (or publishes the connection notice on the bus). The router wakes the Task, Substrate restores `/workspace`, `fw` rebuilds the run from `ledger.jsonl`, and the run continues from the exact point where it stopped.

Because the system prompt, the `Briefer` sections (`fw.run.started`), and the raw provider `thinking` blocks live in the ledger unchanged, the woken container keeps its provider prompt cache prefix. Because `fw.WorkingTime` counts only intervals when the run works, time spent waiting or suspended never consumes the run deadline.

### 3.2 Bounded fleet delegation under a person's identity

A person in Claude Code, an IDE, or the browser can split a large job across many sandboxes without surrendering control or sharing a broad service account.

Two paths fan work out across the fleet:

- **Batch jobs (`enqueue` and `batch`):** An MCP client sends a list of items to the front for a named profile. The front places one run request per item on `fw.jobs.<agent>` with a shared `fw.batch` id and the caller as the principal. Workers pull items from the queue as capacity allows.
- **Hierarchical delegation (`spawn_task` with `spawn.driver: ax`):** A running agent spawns child runs. The AX spawner creates a separate AX Task for each child, passes the parent's principal and batch id, and drives the child through `atenet-router`.

Every delegated run obeys four invariants:

1. **Monotonic attenuation:** A child run can never gain authority. It receives a subset of the parent's tools, the stricter of the parent's and its own policy rules, a budget capped by the parent's budget, and a deadline capped by the parent's remaining working time.
2. **Acting as the person without exposing grants:** When an MCP server sets `auth.oauth.grant: user`, it acts with the permissions of the person who started the run. The worker requests short-lived access tokens from the front's token broker (`POST /v1/tokens`). The refresh token stays in the front's encrypted store and never enters a sandbox or a `/workspace` snapshot.
3. **Single point of supervision:** When a child Task needs an approval or asks a question, `spawn_task` relays the request to the parent run. The person watches one run (or one `Needs you` list on the front) for the whole tree.
4. **Hard guardrails at the token boundary:** Every worker enforces seven budget limits (`maxTurns`, `maxTokens`, `maxCostUSD`, `deadline`, `maxToolCalls`, `maxCallsPerTurn`, `maxRepeats`), per-tool byte and time bounds, cycle detection in the loop guard, and automatic secret redaction before any event reaches the ledger or the bus.

### 3.3 Ledger replay and speculative branching (`fw replay` and `fw fork`)

Because a run is completely defined by its append-only ledger and its `/workspace` directory, execution history is an object that the kernel can replay and branch.

- **Regression testing (`fw replay`):** `fw replay <ledger> --model <selector>` re-runs a recorded ledger against a new model or prompt while serving the recorded `fw.tool.responded` outputs. A diff of the two ledgers tests a model upgrade without touching external services.
- **Speculative execution (`fw fork`):** A person or an orchestrator can fork a run at sequence `N` into parallel child Tasks, try two different approaches or models against copies of the workspace, compare the resulting `fw.file.changed` diffs and test results, and apply the winner. Because the forked ledgers share the event prefix up to `N`, they share the provider prompt cache up to the branch point.

## 4. Surfaces and boundaries

### 4.1 MCP first, calm web workbench second

The primary interface for power users and developers is **MCP** (`/mcp` over Streamable HTTP and `fw-workbench mcp` over stdio). A person in Claude Code or an IDE uses Featherweight as a remote execution fleet: `enqueue`, `batch`, `run`, `run_status`, `run_result`, `run_file`, `approve`, and `connections`.

The browser page at `/ui` and the MCP App (`dashboard`) provide the human control plane for reviewing results, answering schema-driven questions, approving destructive actions, and completing OAuth connections. Both `/mcp` and `/ui` read the same server-side projections in `dashboard/`:

- **Home (`GET /v1/home`):** Leads with a composer and three lists across the fleet: `Needs you`, `In progress`, and `Recent results`.
- **Result (`GET /v1/runs/{id}/result`):** Shows the deliverable first: the pending decision, the summary, the files changed in `/workspace`, the recorded steps, and the budget limits. It never claims that a task passed verification merely because the model ended its turn.
- **Builder (`POST /v1/builder`):** Proposes a least-privilege agent profile (`propose_agent`) from a description or from a completed run (`fromRun`). The model never saves a profile; the person reviews the plain-English abilities card (`On its own`, `Asks you first`, `Cannot`) and saves it under their own identity.

### 4.2 Export operational data, do not rebuild Grafana

Featherweight does not ship an operational monitoring console or a billing dashboard. Workers push Prometheus metrics, CloudEvents ledger logs, and OpenTelemetry spans over OTLP (`telemetry/otlp.go`) to an external collector. Sleeping Tasks stop pushing, and nothing scrapes a sleeping Task through the wake-on-request router. The ledger remains the exact record of token usage and tool execution for audit and budget enforcement.
