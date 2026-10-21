# Featherweight Workbench architecture

## 1. Summary

Featherweight Workbench is the workbench for people on top of the Featherweight Kernel. Its binary, `fw-workbench`, is the kernel binary `fw` plus the parts that people use. These parts are the page at `/ui`, the browser sign-in, the routes for people, the MCP server, the fleet, and the agent builder. The module is `github.com/bdubs00/featherweight-workbench`.

The kernel is a separate repository, featherweight-kernel. Its document `docs/ARCHITECTURE.md` describes the agent runtime for one google/ax Task: the ledger, the turn loop, the models, the tools, the native API for runs, and ax. This document calls it the kernel architecture. This document describes only what the workbench adds. In both documents, `fw` names the agent. Here, `fw-workbench` names the binary.

### 1.1 How the workbench builds on the kernel

The workbench changes no kernel code. It plugs in through the extension points of the kernel:

- `native.Extend` adds routes to the native surface of the kernel, on the same listener and behind the same authentication. The package `surface/web` registers one extension in `init`. Its routes get a `*native.Kit`, so they answer errors and forward the runs of other fleet members as the run routes do.
- `fw.RegisterDefaults` changes the default configuration in `init`. The package `surface/mcp` adds `mcp` to `surfaces`, and the package `tools/builder` adds `propose_agent` to the built-in tools. A file or a variable still overrides each default.
- The package `cli` of the kernel holds the command line. `cmd/fw-workbench` calls `cli.Run`, so `fw-workbench` takes every verb of `fw`. It sets `cli.ServeMCPStdio`, so the verb `mcp` serves the MCP server over stdio. The verb `pools` belongs to the workbench, so `cmd/fw-workbench` runs it before `cli.Run` (section 14.2).
- `fw.RegisterExtension` claims a top-level block of `fw.yaml` for the workbench. The package `teams` claims the block `workbench`, and `fw.Config.Extension` decodes it (section 8).
- `fw.RegisterRunAnnotator` adds metadata to every run that the kernel queues. The package `teams` records the teams of the person who starts a run this way (section 8).
- `fw.Agent.SetProfiles` replaces the profiles of the kernel at runtime, and `cli.AgentsPut` and `cli.AgentsDelete` give the verb `agents` its writes. The package `agents` uses them (section 10).
- `fw.RegisterService` runs work beside the agent for as long as it lives, `Kit.SetRelay` of the native surface answers a request about a run of another process, `metrics.RegisterCollector` adds series to the metrics surface, and `cli.RegisterDoctorCheck` adds a line to `fw doctor`. The package `fleet` uses the service, the relay, the collector, and the doctor check (section 4). Its collector adds the series of the queues, the ends of the runs of the workers, and the approvals that wait (section 4.14). A worker exports the facts of its own runs, and the front shows the fleet as the egress stream and the server of the bus tell it.
- `fw.Agent.SetPersonTokens` gives the tool sources of the kernel the access tokens of people, and `fw.Agent.Connected` resumes the calls that wait for a connection. The package `connections` uses both (section 11).
- The package `sources` uses `fw.RegisterService`, `native.Extend`, and `cli.RegisterDoctorCheck` too: its service runs the sources, its route takes the events of webhooks, and its doctor line names the sources (section 13).

Each driver of the workbench registers itself in `init` through `fw.Register*`, as the kernel drivers do. `fleet` registers the fleet driver, and `surface/mcp` registers the surface `mcp`. A blank import in `cmd/fw-workbench/drivers.go` links each one, beside every driver of the kernel binary, the `oidc` authentication mode of the kernel included, because the front verifies the tokens of people.

### 1.2 Build

The build needs Go 1.27 or newer and a kernel checkout next to the workbench checkout:

```
featherweight-kernel/
featherweight-workbench/
```

The `replace` directive in `go.mod` points the module `github.com/bdubs00/featherweight-kernel` at `../featherweight-kernel`. When the kernel has a release, `go.mod` names its tag and the directive goes away. Run this command in the workbench checkout:

```
go build ./... && go vet ./... && go test ./...
```

To build an image, pass the kernel checkout as the build context `kernel`:

```
docker buildx build --build-context kernel=../featherweight-kernel \
  -t ghcr.io/bdubs00/featherweight-workbench:static .
```

### 1.3 Layout

```
cmd/fw-workbench/       the binary and the drivers that it links
dashboard/              the page index.html and the views for people: home, result,
                        builder, transcript items, usage report, fleet member text
surface/web/            the routes for people on the native surface (section 2)
surface/mcp/            the MCP server over HTTP and over stdio (section 3)
fleet/                  the fleet: the member record, the view, forwarding, the queues,
                        the wake, and the fleet metrics (section 4)
pools/                  the pools: the configuration, the rendered Tasks, and the reconciler (section 14)
sources/                the event sources: webhooks and polls that turn the events of
                        outside systems into runs (section 13)
tools/builder/          the tool propose_agent (section 6.4)
examples/agentgateway/  a fleet behind agentgateway on one public URL
examples/pools/         a front that keeps a pool of three workers (section 14)
examples/sources/       a configuration with a webhook source and a poll source
docs/ARCHITECTURE.md    this document
Dockerfile              fw-workbench:static on distroless
Dockerfile.toolbox      fw-workbench:toolbox on Debian with git
```

## 2. Routes for people

The package `surface/web` mounts the routes for people on the native surface of the kernel through `native.Extend`. They share the listener, the authentication, and the problem documents of the run routes (the kernel architecture, sections 8.1 and 8.2). The MCP server at `/mcp` is a surface of its own (section 3).

| Path | Surface | Present |
|---|---|---|
| `/mcp` | MCP server over Streamable HTTP, POST only | default on |
| `/ui` | Dashboard page (section 6.2) | always |
| `/ui/signin`, `/ui/callback` | Browser sign-in (section 5) | with `auth.oidc.clientId` |
| `/ui/connect/callback` | The end of a connection (section 2.1) | always |
| `/.well-known/oauth-protected-resource` | Protected resource metadata (RFC 9728) | when `auth.mode` includes `oidc` |
| `/v1/sources/{name}` | Webhook sources, POST only, with the credential of each source (section 13) | with a webhook source in `workbench.sources` |

| Method and path | Purpose |
|---|---|
| `GET /v1/whoami` | The caller: the principal, its role on every agent that it can see, and its teams (section 8). |
| `PUT /v1/agents/{name}` | Create or replace an agent profile. The body is an `fw.AgentProfile`. `?dryRun=true` checks it and saves nothing (section 10). |
| `DELETE /v1/agents/{name}` | Remove an agent profile (section 10). |
| `GET /v1/home` | The home of the caller: the runs that need the caller, the runs in progress, and the recent results (section 6.3). |
| `GET /v1/home/others` | The top-level runs of the people outside the teams of the caller, newest first, at most 200, as `{"runs": [...]}` (section 8). |
| `GET /v1/runs/{id}/result` | What the run produced and what it needs next (section 6.3). The read authorizes the caller as `GET /v1/runs/{id}` does. |
| `GET /v1/runs/{id}/transcript` | The conversation of a run as chat items, one page at a time. See below. |
| `POST /v1/builder` | Start the conversation that creates an agent. Body fields: `input` or `fromRun`, and `files`. See below. |
| `GET /v1/fleet` | The members of the fleet that the caller can see (section 4.7). |
| `GET /v1/usage` | Deprecated. See below. |
| `GET /v1/connections` | The services that act for the caller, connected or not, as `{"connections": [...]}` (section 11). |
| `POST /v1/connections/{server}` | Start a connection. Body field: `next`, a place in the page to return to. The answer holds `url`, the authorization request, and sets the cookie of the callback. |
| `DELETE /v1/connections/{server}` | Remove the grant of the caller for the service. |

### 2.1 Connections

The three routes under `/v1/connections` need a person, a principal of the kind `user`. Any other caller gets a 403 with this text: `Only a signed-in person can connect a service.` Each `connections.Connection` has the server, its title, whether the caller is connected, the account name at the service, and the times of the grant. It never holds a token.

`POST /v1/connections/{server}` sets the cookie `fw_connect`. The cookie holds the start of the connection: the state, the PKCE verifier, the redirect address, and `next`. It also holds the subject and the name of the person. It is signed with the key of the sign-in cookie (section 5), under a label of its own. It is HttpOnly, SameSite=Lax, limited to the path `/ui/connect`, and Secure over https. It lasts 10 minutes.

The authorization server sends the browser to `GET /ui/connect/callback` with `code` and `state`, or with `error`. The route opens the cookie, compares the state, checks the expiry, and deletes the cookie. Then it exchanges the code and stores the grant. On success, it answers 303 to `/ui` plus `next`, or to `/ui#connected/<server>`. On failure, it answers a small page with one plain sentence and a link to `/ui#connections`.

The redirect address is `/ui/connect/callback` on the origin of `auth.oidc.redirectUrl`. Without that key, it is the same path on the public origin of the request. Register the address at each authorization server, beside `/ui/callback`.

### 2.2 Transcript pages

`GET /v1/runs/{id}/transcript` answers `{"run", "items", "next", "prev"}`. The items are the chat items of the dashboard, built from the ledger. `limit` sets the size of a page, 200 by default and 1000 at most.

- Without `from` and `before`, the page holds the last `limit` events. A long conversation therefore opens at its recent end.
- `before=<seq>` reads the last `limit` events with a smaller sequence number. If events follow the page, `next` is `before`.
- `from=<seq>` reads forward from that sequence number. If more events follow, `next` is the sequence number to ask from.
- `prev` is the value of `before` for the page in front. It is 0 at the first page of the run.

`from` and `before` together answer 400, and `before=0` is the same as no `before`. To read the whole history, ask for the recent end first. Then ask with `before=<prev>` until `prev` is 0.

### 2.3 The builder route

`POST /v1/builder` starts the conversation that creates an agent (section 6.4). The body holds exactly one of `input` and `fromRun`, else the route answers 400.

- `input` is the first message of the person.
- `fromRun` names a run to turn into an agent. This is Save as agent, and its terms are in section 6.3. The first message then holds what the person asked, the last answer, and the tools that the run used. An unknown run answers 404.
- `files` attaches files to the first message, as on `POST /v1/runs`.

The start is the start of `POST /v1/runs`, so the principal, the origin `http`, and `Idempotency-Key` apply. The answer is 201 with the run object and a `Location` header. The builder runs as the default agent with an inline profile, so the caller needs the operator role on the default agent. A `fromRun` of another fleet member is read from that member, with the credential of the caller, because a worker serves the native API of the kernel alone and has no builder route. The route reads the run and every event of its ledger, applies the terms of section 6.3 to that run, and starts the builder in this process.

### 2.4 Deprecated usage

`GET /v1/usage` still answers with the same body, but it is deprecated. The answer carries `Deprecation: true` and a `Link` header with `rel="deprecation"` that points to section 10 of the kernel architecture, at the anchor `#observability`. The MCP tool `usage` is deprecated too. So are the Go view `AgentView.Usage` of the kernel package `surface/view` and the views `Report` and `Usages` of `dashboard/`. The dashboard shows no usage. Operators read the Prometheus metrics of `/metrics` instead. The usage of these views carries `unpricedTokens`, the tokens whose model has no price, and its line reads "at least $X" or "cost unknown" when there are such tokens, so a partial cost does not read as a complete one.

## 3. MCP server: work intake

The MCP server makes `fw` a tool for any MCP client, for example Claude Code, an IDE, or another agent. It is a work intake equal to the native API, A2A, and the bus. The transport is Streamable HTTP at `/mcp`, with POST only, as the 2026-07-28 revision requires. The command `fw-workbench mcp` serves the same server over stdio for desktop clients. The server is dual-era. A request that carries modern `_meta` is served without a session, and an `initialize` request selects the legacy 2025-11-25 behavior with `Mcp-Session-Id`.

### 3.1 What the server offers

- `server/discover` returns `supportedVersions`, `capabilities`, and protocol instructions with no profile names or prompt text. The capabilities include `tools` with `listChanged`, `resources`, and `prompts`. The Tasks extension waits for SDK support.
- Tools: `run`, `run_status`, `approve`, `cancel`, `send`, `fork`, `run_budget`, the management tools of section 6.1, and every tool listed under `mcp.expose`. `fork` branches a run (the kernel architecture, section 4.7): the branch keeps the events of the run up to `seq` and goes on with `input`, with `model`, or the same way again, and with `spawn` it runs in a Task of its own. The answer is the branch; with `spawn` it is queued on the new Task until that Task takes it, and the client follows it with `run_status`.
- Resources: `fw://runs/{id}` (the run object), `fw://runs/{id}/ledger` (JSONL), the template `fw://runs/{id}/files/{+path}` (a file that the run changed, see the kernel architecture, section 7.9), and `fw://skills/{name}`. The three run resources read the runs of this process only. For a run of another fleet member, use the tools `run_status`, `run_events`, `run_result`, and `run_file`, which reach that member (section 4.5).
- Prompts: one per agent profile that the caller can view. Lists apply access rules before pagination, and reads check access again. Both use the current profiles, so a create, edit, deletion, or access change takes effect on the next request. Prompt lists are private with a zero cache lifetime. The server does not advertise prompt list-change notifications.

The `run` tool takes `input`, `title`, `agent`, `instructions`, `profile`, `wait` (default `true`), `budget`, and `requestId`. `title` names the work, as on the native API (the kernel architecture, section 8.2). `instructions` and `profile` have the shape of the native API fields. An MCP budget accepts `deadline` as a duration string, such as `30m`, or as positive integer nanoseconds, the form that profile reads return. This applies to `agent_put` and to both budget fields of `run`. `requestId` is the idempotency key. A retried `run` with the same `requestId` returns the same run instead of starting a second one. `metadata` labels the run, as it does on the native API, and the run object returns it. `run_status` is marked `readOnlyHint`. `run` and `send` are marked `destructiveHint`, because they start work that acts on the world.

`send` takes `runId`, `content`, `files`, and `budget`. With `budget`, the kernel changes the budget of the run before the message (the kernel architecture, section 5.2): each limit that is set replaces the same limit for the work that follows, so a person continues a run that failed at a limit with more room. `run_budget` takes `runId` and `budget` and changes the budget of a run without a message, also of a run that is running; the run reads the new budget at its next step. Both budgets have the shape of the `budget` of `run`. Every limit counts the whole run, so the turns, the tool calls, the tokens, the cost, and the working time before the change count against the new limits. A change needs the operator role on the run, as a decision does. The result of `run_budget` is a text with the budget of the run after the change, and `structuredContent` holds `budget` and `run`, as `PATCH /v1/runs/{id}/budget` answers. `run_budget` is marked `destructiveHint`, because a larger budget lets a run spend more.

### 3.2 How a run flows through MCP

`fw` picks one of three shapes per request, from the capabilities that the client declares in `_meta`. The Go MCP SDK v1.8.0 does not implement the Tasks extension yet. The first shape therefore waits for the SDK, and the server offers the other two today:

1. The client declares `io.modelcontextprotocol/tasks`. `run` returns a `CreateTaskResult` with `taskId` equal to the run id, status `working`, `pollIntervalMs` of 1000, and `ttlMs` from `mcp.taskTTL`. The client polls `tasks/get`. A client that opens `subscriptions/listen` receives `notifications/tasks` on every state change instead.
2. The client declares `elicitation` but not tasks, and `wait` is true. `run` streams one `notifications/progress` per turn on the response stream, with `message` from the latest ledger event. When the run finishes, the result is a `CallToolResult` with the output in `content` and the run object in `structuredContent`. `isError` is true for the outcome `failed`.
3. `wait` is false. `run` returns the run object at once, and the client polls `run_status` or reads `fw://runs/{id}`.

Run states map to task statuses as follows:

| Run state | Task status |
|---|---|
| `queued`, `running`, `suspended` | `working` |
| `input-required` | `input_required` |
| `completed` | `completed` with `result` |
| `failed` | `failed` with `error` |
| `canceled` | `cancelled` |

### 3.3 Approvals over MCP

An approval is an input request. In the task shape, `tasks/get` returns `input_required` with an `inputRequests` map that holds one `elicitation/create` per pending approval. The form schema has two fields: `decision` with the values `allow` and `deny`, and `note`. The client answers through `tasks/update` with `inputResponses`, and `fw` appends `fw.approval.decided` with the principal of the token as the decider.

In the waiting shape, the `run` call returns an `InputRequiredResult` with the same `elicitation/create` request and a `requestState`. The `requestState` is an HMAC-signed blob that holds the run id, the approval id, the principal, and an expiry. The client retries `run` with `inputResponses` and the unchanged `requestState`. `fw` verifies the blob, rejects a mismatch of principal or an expired blob, records the decision, and continues the same run. The retry never starts a second run.

The key of the HMAC is the SHA-256 of the secret `FW_MCP_STATE_KEY`. Without that secret, the server reads the key from the file `$FW_HOME/mcp-state.key`. If the file does not exist, the server creates it with 32 random bytes and mode 0600. A signed retry therefore survives a restart of one process. Two processes that create the file at the same time agree on one key. A file of another length is never used and never replaced, and the log says that the file is damaged.

At start without the secret, the server logs one warning. Each replica has its own home and so its own file. Replicas behind one endpoint therefore must share `FW_MCP_STATE_KEY`. If the server cannot read or write the file, the key lives in memory only, and a second warning names the file and the cause. No log line holds the key. `fw-workbench mcp` over stdio uses the same rule. Its home is `FW_HOME`, else `.fw` under the working directory. The same secret also signs the cookie of the browser sign-in (section 5).

If the client declares neither tasks nor elicitation, `run` returns the run object in the state `input-required`. The text content names the native approval endpoint. The run waits for a decision from any surface.

The tool `approve` takes `rule` with the decision `allow`: allow this call, and allow the tool for the agent from now on. The front first adds the tool to the policy of the pool whose worker runs the run, with the owner role on the profile (section 14.9), and then decides the approval as before. A refusal decides nothing and says why. Without a pool, the tool says that a rule needs a pool.

### 3.4 Connections over MCP

A run can wait for the person to connect a service (section 11). MCP revision 2025-11-25 added URL-mode elicitation for such flows outside the client, such as an OAuth sign-in. The client opens a URL in a browser, and the server learns the result on its own. The URL is `<base>/ui#connect/<server>/<run>`. The base is the public address of the page, from `mcp.auth.audience` or `a2a.url`. Without it, the base is the origin of the request, as a proxy forwards it. The URL holds no secret.

`fw` asks in the shape that the client declares:

- A modern client that declares `elicitation.url` gets `elicitation/create` with `mode: "url"` in `inputRequests`. The `elicitationId` is the approval id. A legacy session gets the same request on the open call.
- Accept means that the person opened the link. The `run` call keeps waiting. When the connection ends, the call of the agent goes on, and a legacy session gets `notifications/elicitation/complete`.
- Decline and cancel skip the service.
- A client with form elicitation alone gets a form. Its message holds the link, and the choices are Done and Skip. Done keeps waiting.
- A client without elicitation gets the run in `input-required`. The text gives the link.

The tool `approve` cannot allow a connect request. Its answer says to connect the service. Over stdio without a public address of the page, the text says to connect in the dashboard. The tools `connections` and `disconnect` (section 6.1) list and remove the connections of the caller.

### 3.5 Cancellation and disconnects

Closing the response stream cancels the `run` call but not the run. An MCP client that loses its connection must not kill a long job. The run continues, and the client picks it up again through `run_status` or `tasks/get`. The `cancel` tool and `tasks/cancel` cancel the run itself. This departs from the default reading of the revision, where closing the stream stops the work, and the server instructions say so.

### 3.6 Why the stateless revision fits

Revision 2026-07-28 removed protocol sessions. Every request carries its version, its client identity, and its capabilities in `_meta`, and a server never opens a request of its own. This matches rule 3, because `fw` keeps no connection state at all. The ledger is the only state, and every MCP request reads or writes it.

Three things follow. First, a request to a suspended ax Task is complete on its own. The atenet router wakes the actor, the fresh container reads the ledger, and it answers with no session to rebuild (the kernel architecture, section 11.4). Second, several `fw` replicas can serve one MCP endpoint behind one Service with no session affinity. They need a shared ledger driver such as NATS or Redis. A read for any run works on any replica from the shared ledger. A write for a run that another replica owns, such as an approval or a follow-up, goes to the run mailbox on the bus. The owner applies it. Third, the approval context travels in `requestState` instead of in server memory. A retry after a disconnect, a restart, or a resume carries everything `fw` needs, and the HMAC keeps it honest.

The `server/discover` and `tools/list` results carry `ttlMs` and `cacheScope`, so a gateway in front of `fw` caches discovery for every client. `fw` marks discovery `public` and the tool list `private`, because the tool list depends on the scopes of the token. Shared instructions and tool schemas contain no profile names or prompt text. Clients discover the profiles that they can view through `agents_list` or `prompts/list`.

### 3.7 Security

- `fw` checks the `Origin` header against `mcp.origins` and answers 403 on a mismatch. A request without `Origin` is allowed, because non-browser clients send none.
- The MCP server uses the same `Authn` plug as the native API. In `token` mode, a missing or wrong token gets a 401 with `WWW-Authenticate: Bearer`.
- In `oauth` mode, `fw` is an OAuth 2.1 resource server. It serves `/.well-known/oauth-protected-resource` (RFC 9728) with the servers from `mcp.auth.authorizationServers`. It checks the token signature through the JWKS of the issuer, and it checks that the audience equals its own canonical URL.
- The principal from the token is stored in the `fw.run.queued` metadata and in every approval decision.
- Because `Mcp-Method` and `Mcp-Name` mirror the body, a gateway in front of `fw` can rate-limit or route calls to `run` without parsing JSON.

### 3.8 Gateway

`mcp.expose` re-exports upstream tools under their prefixed names, for example `git.commit`. A call to an exposed tool passes through the same policy as a call from the model. The gateway can therefore require an approval for an upstream tool. This lets one `fw` process stand in front of several MCP servers and present one endpoint with one authentication mode.

## 4. Fleet

A fleet is a set of workers and one front. A worker is a bare kernel, `fw`, in one ax Task: it takes run requests from its intake queue, publishes the lifecycle events of its runs on the egress stream, and publishes nothing about itself. The front is `fw-workbench` outside the sandboxes: it reads the Task list from the ax API and the state of every run from the egress stream, projects both into one view, and serves people through its native API, its MCP server, and its page. The platforms keep the state, and the front keeps nothing of its own but the OAuth grants of people (section 11). No member leads the others, and no election takes place.

A fleet is not a set of replicas. Replicas of one agent share one ledger and serve the same runs (section 3.6). Each worker keeps its own ledger. The front knows the runs of a worker from the stream, and it acts on such a run through the Task that owns it.

### 4.1 The sources

The front reads two sources, which the configuration names (section 4.6). `workbench.fleet.atespaces` names the atespaces whose Tasks are members: at every interval the front lists them on the ax API at `ax.server`, and each Task is a member named `<atespace>/<task>` with the address `ax:<atespace>/<task>` and the state of its phase, live when it runs, quiet when it is suspended or pending, stopped otherwise. A Task that leaves the list leaves the view. `workbench.fleet.egress` names the subject prefix of the egress stream of the workers, `bus.egress` on each of them. The front subscribes to that stream and projects every run from its events with the projection of the kernel, so the state, the title, the pending approval, the turns, and the usage of a run are those of the ledger of the worker. The usage comes from the totals that `fw.run.finished` and `fw.run.suspended` carry (the kernel architecture, section 9.4). The stream therefore needs no `fw.model.responded`, which holds the content of a run, and the front knows the usage of a run each time it finishes or is suspended. A lost event costs nothing once a later `fw.run.finished` or `fw.run.suspended` of the run arrives. A lost final `fw.run.finished` has no later event, so the view keeps the state and the usage of the event before it until the front reads the Task again (below). The cost is the cost at the prices of the worker. The totals carry no breakdown by model, so the front cannot price them again: it loses its own pricing at every totals event, its price table prices only the increments that the stream carries after the totals, such as the title call of `fw.run.annotated`, and the cost that it shows can go down at an end of a run, to $0 with every token unpriced when the worker has no price table. A breakdown by model in the totals, a later change, can let the front price them with its own table. The stream keeps the order of the events of a run, unless two processes publish for the run or the bus is the mem bus (the kernel architecture, section 9.4). For a reorder, the view applies the first `fw.run.queued` of a run even when it arrives after later events, because only that event names the agent, the person, and the metadata of the run, and it keeps the state of the later events; a repeated `fw.run.queued` changes nothing. A late end of a run, `fw.run.finished` or `fw.run.suspended`, applies when no event that changes the state of the run came after it, as after a title annotation, and the usage that the later events added stays on top of its totals. Any other late event changes nothing. When two processes publish for one branch, because its source took its start for failed while its process took it after all, the `fw.run.queued` with the larger `kept` applies too: the record of the source holds no copy and no work, so the copies of the branch never count as its own work. Each event names its node (the kernel architecture, section 9.4), so the front knows which Task holds the run. A `fw.run.child` event of a run tells the view where a child of that run lives, a child of `spawn_task` or a branch in a Task of its own (the kernel architecture, sections 4.7 and 11.9); each child sends it twice, a child of `spawn_task` before its Task exists and again once its run started, a branch when it is placed and again when it starts, and the view keeps one link. The view shows the child under its parent. Until the events of the child arrive, because its Task is still starting, the view shows it as a queued run of the member of the parent, so a request for it goes to the parent, which answers for it.

Each time a Task becomes live, the front reads its runs and its agents through the router, with the static token of the fleet, so the runs that started before the front listened are in the view and the front knows which profiles the Task serves. A Task becomes live when the Task list shows its phase as Running after another phase, or when the router woke it for a request (section 4.12). A running Task is awake, so the reads wake nobody, and the front never reads a Task that sleeps. The front subscribes to the stream before it reads the Tasks, one after another, so the stream can reach a run before the read of its Task, without the `fw.run.queued` that named the run before the front listened. The read then gives the run its identity: a read at the sequence number of the view or later replaces the run, and a read that is older gives the agent, the person, the input, and the batch, while the newer state of the stream and the usage that it added after the read stay. A read of the runs that fails is tried again after a pause of one interval, which doubles up to five minutes, so a run stays without identity only briefly. After that read the front lives on the stream, with one exception. A worker that a suspend cuts can hold back its final events: the bus is out of reach, and the process is gone before the bus answers. A kernel with the file ledger sends them at its next boot (the kernel architecture, section 9.4), but a Task that sleeps does not boot, and a worker with another ledger loses them. Without a new read, the view holds the run as running for good, and the suspend controller never puts the Task to sleep. So while the view holds a run of a live Task as queued or running, the front reads the runs of that Task again once a minute, and a read at a later sequence number ends the run in the view. A short suspend and resume between two reads of the Task list leaves no trace in the phase, and this read repairs that case too. Each replica of the front reads on its own. A worker outside ax that publishes events is visible too, under its node name, without an address, and it leaves the view when no event arrived for `workbench.fleet.expire`. The view drops a finished run after `workbench.fleet.expire` without an event, and it keeps a copy of the identity of the run for as long again: a later event of the run, such as the `fw.run.child` of a branch of a finished run, brings the run back with its agent, its person, and its batch. A run that the view knows from events alone, because no `fw.run.queued`, no read of its Task, and no such copy named it, has no identity: the view lists it nowhere, and it leaves the view after `workbench.fleet.expire` without an event, whatever its state.

### 4.2 The record of a member

The front computes a record for each member from the sources: the name, the address, the atespace, the state, the agents that its runs name, and its runs. Every run that is not finished and the 50 newest finished runs are in it, as run objects whose input, output, and error are cut to 2 KB; a pending tool input larger than 4 KB is left out. A large job does not fit in a record, so the tool `batch` reads every run of a job from the view instead (section 4.9). When a person opens a run, the view reads the full run from its owner. The record names the authorization of the front, not of the worker: the front runs the same `fw.yaml` as the workers and decides for every worker with its own `access` block and agent profiles. A process that configures no agent profile offers no agent, so a person never sees the front itself as a member with agents.

### 4.3 The view

The view gives each member one of three states. A Task is `live` when its phase is Running, `quiet` when it is Suspended or Pending, and `stopped` otherwise. A worker that only the stream knows is `live` within three intervals of its last event and `quiet` after that. Under ax, a quiet member is usually a Task that sleeps, and the suspend controller (section 4.12) is what put it to sleep.

### 4.4 Who sees what

The view decides as the front decides, with its own configuration:

- In `rules` mode, the view applies the access rules (the kernel architecture, section 13.2) with its own `access.default`, `access.admins`, and the access blocks of its agent profiles, to the agents and the runs of every member. Whoever started a run sees that run. Whoever sees a run sees its children.
- In `external` or `custom` mode, the view asks the `Authz` of its own process. With `workbench.access.external` (section 12), that is the control plane, and each check carries `member`, the access block of the agent, and the lineage of the run. A view without a decision point shows no run of another member. The view has no local fallback.

Every action goes to the owner of the run, and the owner decides with its current rules.

### 4.5 Acting on a run of another member

Run ids are unique across the fleet. A request can name a run that the process does not have but the view knows. Then the process forwards the request to the Task that owns it. This applies to the native routes under `/v1/runs/{id}` that the kernel serves. It also applies to the MCP tools `run_status`, `run_events`, `run_file`, `approve`, `cancel`, `send`, `fork`, and `run_budget`, which goes to `PATCH /v1/runs/{id}/budget` of the owner. The views of a run that only the front serves are not forwarded: `GET /v1/runs/{id}/result`, `GET /v1/runs/{id}/transcript`, and the MCP tool `run_result`. A worker serves the native API of the kernel alone, so the front reads the run and the events of the owner through the same forward, with the credential of the caller, and builds the view here (`fleet.MemberEvents`, `fleet.MemberLedger`, `dashboard.ResultFrom`). `POST /v1/builder` and the MCP tool `agent_builder` with a `fromRun` of another member read the run and its events from the owner in the same way, apply the terms of Save as agent, and start the builder here (`dashboard.SaveAsAgentInputOn`). `POST /v1/runs?member=<name>` and the `member` argument of the MCP tool `run` start a run on another member. Through MCP, such a run does not wait, and the client follows it with `run_status`.

The forwarded request carries the bearer credential of the caller. The owner authenticates the caller again and authorizes the action in its kernel. The view therefore never acts under an identity of its own, and the owner records the real decider in its ledger. A credential goes to one kind of address only: an ax address, through the router at `ax.router` with the header `ate-target-actor`. The router wakes a Task that sleeps, so an approval reaches a sleeping member, and the front then makes the phase of the Task follow (section 4.12). A worker that only the stream knows has no address, so no credential can reach it.

The relay of the native surface, which the package registers with `Kit.SetRelay`, does the forwarding for the run routes of the kernel, and the MCP server does it for its work tools. For the owner to accept the credential, the front and the workers trust the same issuer and the same `auth.oidc.audience`, or share the static token. A caller with an X.509 client certificate, or over stdio, carries no bearer credential. Such a caller sees the fleet but acts only on the runs of the front. If the owner refuses the credential, the view answers 403, and if the owner does not answer, the view answers 502. A forwarded request carries the header `Fw-Fleet-Via` with the name of the front, and a member never forwards such a request again.

### 4.6 Configuration

The fleet reads the block `fleet` under the key `workbench` of `fw.yaml`, which the workbench claims (section 8). Each variable wins over the file.

| Key | Variable | Meaning | Default |
|---|---|---|---|
| `workbench.fleet.name` | `FW_WORKBENCH_FLEET_NAME` | The name of this process in the view | `<atespace>/<task>` under ax, else `<host>:<port>` |
| `workbench.fleet.interval` | `FW_WORKBENCH_FLEET_INTERVAL` | The time between two reads of the Task list | `30s` |
| `workbench.fleet.expire` | `FW_WORKBENCH_FLEET_EXPIRE` | The age after which the view forgets a finished run and a worker that only the stream knows | `168h` |
| `workbench.fleet.atespaces` | `FW_WORKBENCH_FLEET_ATESPACES` | The atespaces whose Tasks are members, read from the ax API at `ax.server`, with commas between them (section 4.1). This or `egress` turns the fleet on. | none |
| `workbench.fleet.egress` | `FW_WORKBENCH_FLEET_EGRESS` | The subject prefix of the egress stream of the workers, `bus.egress` on each of them (section 4.1) | none |
| `workbench.fleet.intake` | `FW_WORKBENCH_FLEET_INTAKE` | The subject prefix of the intake queues of the workers: a worker that runs the profile `<agent>` sets `bus.intake` to `<intake>.<agent>` (section 4.9) | none |
| `workbench.fleet.suspendAfter` | `FW_WORKBENCH_FLEET_SUSPEND_AFTER` | How long a Task stays idle before the front suspends it, a duration such as `10m`, or `off` (section 4.12) | `10m` with atespaces, else off |
| `workbench.fleet.wakeOnQueue` | `FW_WORKBENCH_FLEET_WAKE_ON_QUEUE` | Whether the front wakes the sleeping Tasks of a profile when jobs wait in its intake queue, `on` or `off` (section 4.13). It needs `atespaces` and `intake`. | on with atespaces and intake, else off |
| `workbench.pools` | none | The pools of Tasks that the front keeps, each with its image, the configuration of its workers, and its replica count (section 14) | none |

A worker needs no key of this block: it sets `bus.driver`, `bus.url`, `bus.egress`, `bus.intake`, and `tokens.url` of the kernel. On a NATS bus, the front and the workers must name the same JetStream stream in `bus.options.stream`, `FW` by default, because the front reads the queues from it (section 4.13). The front names its sources:

```yaml
bus: { driver: nats, url: "nats://nats.agents.svc:4222" }
ax: { server: "ax-server.ax-system.svc.cluster.local:8080" }
workbench:
  fleet:
    atespaces: [default]
    egress: fw.events
    intake: fw.jobs
    suspendAfter: 10m
```

The fleet is a service of the kernel (`fw.RegisterService`), and the commands that do not serve, for example `fw-workbench run`, `fw-workbench mcp`, and `fw-workbench agents`, start no service, so they never join a fleet. `fw-workbench doctor` has a line about the fleet.

The directory `examples/agentgateway` puts a fleet behind agentgateway on one public URL. Its README lists the rules that keep the dashboard and the login working through the gateway.

### 4.7 Surfaces

- The native API serves `GET /v1/fleet`. It lists the members that the caller can see. Each member has its state, the agents that the caller can see, and its counts of active runs and of runs that wait for input. A member that a storage failure blocked also has `blockedReason` until its next event (section 4.12). `GET /v1/fleet/runs` lists the runs of the whole fleet in the shape of `GET /v1/runs` of the kernel, which covers this process only. Its parameter `scope` is `fleet`, the default, or `local`, `member` narrows the list to one member, and `parent` lists the children of a run of any member. `agent`, `state`, `principal`, and `limit` narrow it as on `GET /v1/runs`. Each run of another member carries the field `member`. `POST /v1/runs?member=<name>` starts a run on that member.
- The MCP server adds the tool `fleet`, and `runs_list` takes the arguments `scope` and `member`: the scope `fleet`, the default, covers every member, and `local` covers this process only.
- With pools, `GET /v1/fleet` carries `pools`, the MCP tool `fleet` lists them after the members, and the Agents place of the page shows one line for each pool (section 14.7).
- The home of the dashboard lists the work of the whole fleet that the caller can see. The Agents place groups the agents by member (section 6.2). Approve, deny, cancel, and a follow-up work in the same way on every run.

The A2A server and the MCP resources `fw://runs/{id}`, `fw://runs/{id}/ledger`, and `fw://runs/{id}/files/{+path}` serve the runs of their own process only.

### 4.8 The platforms as sources

Section 4.1 describes the two sources. The front reads the Task list from the ax API and the runs from the egress stream, reads the runs of a Task when it becomes live and once a minute while the view holds one of its runs as queued or running, and lives on the stream between those reads; it never reads a Task that sleeps, because a request wakes it. The gauges that the collector carries, the forwarding, the token broker, the suspend controller, and the pools read the same view. The suspend controller and the pools read a Task through the router only right before they decide whether to act on it (sections 4.12 and 13.4).

### 4.9 Jobs

A job is many pieces of the same work, one run each, spread over the workers that run one profile. The front sends a job with the MCP tool `enqueue`: it takes the profile, the items, and a batch name, and it puts one run request per item on the intake queue of the profile, `<workbench.fleet.intake>.<agent>`. Each worker that runs that profile sets `bus.intake` to that queue and takes requests from it, as many at a time as its `maxRuns` allows, so the fleet scales with the number of workers and nobody assigns work by hand. The call returns at once with the batch name, the queue, and the count.

Every request names the person who sent the job, so the run on the worker acts as that person, and it carries the batch name in the metadata key `fw.batch`, which a child run inherits in this process or in a child Task. The idempotency key of each request is the batch name and the position of the item, so a job that is sent twice starts each run once. The tool `batch` tells how a job goes: the jobs that wait in the queue because no Task took them yet, the runs of the batch by state, the members that hold them, the runs that wait for the person, and the runs that failed with their errors. The tool counts every run of the job that the view holds, not the cut records of section 4.2, and it says how many runs of the job the view forgot: a finished run leaves the view after `workbench.fleet.expire`, and every run of a Task leaves with the Task. The view keeps a copy of each forgotten run for `workbench.fleet.expire`, with the facts that the authorization reads (the id, the agent, the person, the parent, the state, and the origin), its usage, and its batch, and with no title, no input, no output, and no other metadata, and it counts only the forgotten runs that the caller could see, decided as for the runs, so two people who use one batch name learn nothing of the runs of the other. The view knows the runs that it saw since the front started, so after a restart of the front the counts are those of the stream since then and of the Tasks that it read. It also sums the usage of the runs: the tokens, the cache tokens, the turns, the tool calls, and the cost, with the tokens that have no price apart, because their cost is unknown. The sum counts each run once. A child run carries the batch of its parent, so it counts as a run of its own, and its usage is not also in its parent: the sum adds the `usage` of each run and never its `treeUsage`, which holds the usage of its child runs too (the kernel architecture, section 8.2). A branch counts its own work only, not the events that it kept from its source (the kernel architecture, section 4.7). A run of the front itself has the usage of its whole ledger. A run of a worker has a known usage once its kernel recorded the totals of the run on `fw.run.finished` or `fw.run.suspended`. Tokens without totals do not count, because on the lifecycle stream they come from `fw.run.annotated` alone, such as a title call. A run without a known usage counts as missing, such as a run that works for the first time or a run of an older kernel. A known usage is final when the run is finished; a run that works or waits again after its totals, after a follow-up, a resume, or a decision, has the usage of its totals so far. The sum reads as partial when a run is missing, when a usage is not final, or when the view forgot runs of the job. While the job works, it also reads as partial when the view holds runs that have no identity yet on a worker that holds a run of the job that the caller sees, because such a run can belong to the job (section 4.1). The tool counts no such run on a worker where the caller has no run of the job, and once every run of the job that the caller sees is finished, no such run changes the result, so a run of someone else keeps no finished job from being finished. `runs_list` and `GET /v1/fleet/runs` take `batch` as a filter, and `GET /v1/runs` of a worker takes it too. The view of the front sees the runs of a job through the egress stream or the records, as section 4.8 describes, so a run appears when a worker takes it.

A job that no worker took yet has no run, so the view alone cannot tell how much of a job is left. The front published the requests, so it records them. The job record holds, for one batch name and one person, the idempotency keys of the requests that went on each queue, so a job that is sent twice counts once. On a NATS bus, the record lives in the key-value bucket `fw_workbench_jobs` of the server, and each record expires after `workbench.fleet.expire`. Every replica of the front then reads the same record. On another bus, or while the bucket does not answer, each replica keeps the records of the jobs that it sent, in memory, and another replica counts no queued job of them. The field `jobsShared` of the answer says which case applies.

The answer of `batch` holds `jobs`, the jobs that the caller sent under the batch name, and `queued`, the jobs of the caller that no Task took yet. `queued` is `jobs` less the top-level runs of the caller in the batch that a queue started, including the runs that the view forgot. A child run is not a job. After the front read the queues once after the last enqueue of the job, `queued` is never more than the jobs that the queues of the job still hold, pending and ack-pending (section 4.13): a request that a worker cannot start leaves the queue without a run, so it waits nowhere, and its record is on the dead-letter subject (section 4.14). A run in the state `queued` is a job that a Task took and has not started, and `queued` never counts it. The record is per person, so two people who use one batch name learn nothing of the jobs of the other. Without a principal, as over stdio, the answer has no `queued`. The job is finished only when no job of the caller waits in the queue, every run of the job that the caller sees ended, and no run without identity works on a member of the job.

A batch name is at most 128 characters without spaces. Without one, `enqueue` makes an id. One call takes at most 500 items; a larger job goes in several calls under one batch name. A worker that sleeps reads no queue, so after the publish the front wakes every suspended Task that serves the profile, with one read of its status through the router (section 4.12). A job that another process publishes on a queue, and a job that waits after the awake Tasks are full, wake the sleeping Tasks through the queue depth (section 4.13).

An event source (section 13) puts its runs on the same queues through the same publish, so its runs wake the sleeping Tasks in the same way. The batch of each run of a source is the name of the source.

### 4.10 Security

The events of the stream carry the inputs, the outputs, and the tool inputs of runs, cut to size. The `Redact` middleware removes secrets before an event enters the ledger, so no secret reaches the stream. Limit publish and subscribe on the egress and the intake subjects to `fw` workloads, for example with NATS accounts. A process that can publish on the egress subject can show false runs in a view, and a process that can publish on an intake queue can give a worker work; it cannot act on a real run, because the owner decides, and a credential goes to the router only. The dead-letter subjects `<queue>.dead` carry the records of jobs, and the bucket `fw_workbench_jobs` holds batch names, the subjects of people, and the positions of their jobs, with no input. Limit them in the same way. The buckets `<stream>_POOL_<pool>` hold the profiles and the allow rules that the front adds to its pools (section 14.9). A process that can write there changes what every worker of a pool runs, so allow the writes to the front alone. The front logs the id, the type, and the run of a dead letter, never its data.

### 4.11 What the fleet does not do

The fleet shares no ledger, elects no leader, and schedules no work. Bus intake (the kernel architecture, section 9.3) spreads work across the processes that consume one queue. A2A peers (the kernel architecture, section 7.4) let one agent ask another.

### 4.12 The suspend controller

A Task that waits for a person costs compute while it runs, and nothing in it changes until the person answers. The front suspends such a Task. At every interval, after the Task list, the controller looks at every Task of the atespaces whose phase is Running. A Task is a candidate when the view holds no run of it that is queued or running, and when its newest run update, or its last start when it has no run, is older than `workbench.fleet.suspendAfter`. For each candidate the front reads `GET /v1/status` of the Task once through the router, which wakes nobody because the Task runs, and it suspends the Task through `SuspendTask` on the ax API only when the body says `idle` and the idle time is at least the grace period. The body of the Task decides, so a view that missed an event suspends nobody who works. A child Task of the spawner is a Task like any other.

A storage failure, such as a full `/workspace`, blocks the kernel of a Task (the kernel architecture, section 5.6). The kernel then writes nothing and sends no event. It keeps its open runs in their last state, such as `running`, and `/readyz` answers 503. The view therefore shows those runs as running, and the rule above never reads the Task. So the controller has a second rule. A Task whose phase is Running is also a candidate when the view holds a run of it that is queued or running, and when its newest run update, or its last start, is older than `workbench.fleet.suspendAfter`. The front reads `GET /v1/status` of such a busy Task at most once per interval. When the body says `blocked`, the front suspends the Task through `SuspendTask` at once. It logs one line with the member and the reason, and it marks the member in the view as blocked with that reason until the next event of the member. A suspend and a resume restart the process under ax, and the fresh process rebuilds from its ledger. When the body does not say `blocked`, the runs can be slow, so the front leaves the Task alone and logs nothing above Debug. A body that says `blocked` for an idle candidate also suspends the Task at once, whatever its `idleSince`.

The front reads the runs of a Task once a minute while it holds one of them as queued or running (section 4.8). The kernel of a blocked Task shows the field `blocked` on each open run of such a read. Such a run does no work, whatever its last state, so it does not make its Task busy. The next event of the member clears the field on all its runs in the view.

The worker removes the secrets from `blockedReason`, and the front removes its own secrets too and cuts the reason to 2 KiB. `GET /v1/fleet` and the MCP tool `fleet` show the reason as `blockedReason` of the member, so a person sees why the Task went to sleep, and the gauge `fw_fleet_members_blocked` counts the marked members (section 4.14). The front does not resume the Task itself. The next request through the router restarts it: a wake for a job (section 4.13), an approval, a message, or a run. If the storage is still full, the fresh process can block again, and the controller then suspends it again after the grace period. Repair the storage of the Task before you wake it.

A suspended Task costs nothing. The router wakes it on the next request through the front: an approval, a message, a follow-up, or a run, and the run continues from its ledger. The ax API does not learn of such a wake, and the phase of the Task would stay Suspended, so the front calls `ResumeTask` after every answer of a Task whose phase is not Running. The phase follows, and the controller can suspend the Task again when it idles. A Task that sleeps reads no queue, so `enqueue` wakes the suspended Tasks that serve the profile of the job, with one status read each, and so do the jobs that wait in a queue (section 4.13); the front learns the profiles of a Task from `GET /v1/agents` when the Task first runs and from the runs on the stream. When no sleeping Task is known to serve the profile, the front wakes the sleeping Tasks whose profiles it does not know yet, such as the Tasks that slept since before the front started, and the first wake teaches it what each one serves. A Task whose wake still runs gets no second wake. The grace period defaults to ten minutes when the configuration names atespaces. `off` turns the controller off, and the controller needs atespaces, because the ax API is what suspends a Task. The log of the front names every Task that it suspends and wakes, and each wake names its cause: `enqueue` or `queue`.

The ax API records no reason for a suspend. The front therefore cannot tell a Task that a person suspended on purpose from a Task that the controller suspended, and it wakes both for a job. To keep a Task asleep while its profile has jobs, stop the Task. To keep the front from waking Tasks for the jobs that wait in a queue, set `workbench.fleet.wakeOnQueue` to `off`; the wake after an enqueue of the front stays.

### 4.13 Queue depth

A job waits in the intake queue of its profile until a worker takes it, and a worker that sleeps reads no queue. The wake after `enqueue` (section 4.12) covers the jobs that the front itself sends. It misses a job that another process publishes on a queue, and a job that still waits after the awake Tasks are full. The front therefore reads the queues at every interval of `workbench.fleet.interval`.

The front reads the queue of every profile that it knows: the profiles of its own configuration, which is the configuration of the workers, and the profiles that the members of the view serve. The queue of the profile `<agent>` is `<workbench.fleet.intake>.<agent>`. On a NATS bus, the front opens one connection of its own to `bus.url`, with the keys `credentials` and `token` of `bus.options`, because the kernel driver does not share its connection. A server that is not reachable yet does not stop the start; the reads fail until the client connects, and the log names the first failure and the first read that works again. Each read asks the JetStream stream of `bus.options.stream`, `FW` by default, for three facts:

- The durable consumers of the stream. The consumer of a queue is the consumer whose filter is the queue alone. The kernel makes one such consumer per queue, and every worker of the profile shares it. The front takes two numbers from it: pending, the jobs that no worker took yet, and ack-pending, the jobs that a worker took and did not acknowledge yet. A worker acknowledges a job once the run of the job is queued in its ledger (the kernel architecture, section 9.3). So ack-pending counts only the jobs that a worker took and has not started yet, and a run that works or waits holds no job.
- The messages that the stream stores for each subject under the intake prefix. A queue without a consumer never had a worker, so every message that the stream stores for it waits.
- The time of the last dead letter of a queue, only when the number of dead letters changed (section 4.14).

A read sends no request to a Task, so it wakes nobody. With another bus driver, the front reads no queue depth, and the wake after an enqueue of the front is the only wake for a job.

After each read, the front looks at each queue in which jobs wait, with pending above zero. It wakes the sleeping Tasks that serve the profile of the queue when no awake Task that serves the profile has room. An awake Task is a Task whose phase is Running, or a worker that only the stream knows and that sent an event within three intervals. It has room when the view holds no run of it that is queued or running, because a Task under ax runs one run at a time by default. The view does not know how many jobs a worker fetches at a time (`bus.options.concurrency` of the worker, 4 by default). A Task can therefore seem to have room and still take no job, for example when four of its runs wait for people. So the front also wakes the sleeping Tasks when no job left the queue for three intervals, whatever the room of the awake Tasks.

The wake is the wake of `enqueue` (section 4.12): one read of `GET /v1/status` through the router for each sleeping Task that is known to serve the profile, or, when none is known, for each sleeping Task whose profiles the front does not know yet. A Task that a wake for a queue reached stays out of the next wakes for a queue for four intervals, so it has time to connect to the bus and take a job. The log line `fleet: jobs wait in a queue; the front wakes the sleeping Tasks that serve it` names the queue, the profile, the jobs that wait, the number of Tasks, and whether the queue stalled.

Every replica of the front reads the queues and wakes the Tasks, because no member of a fleet leads the others (section 4). A wake is idempotent and cheap: a second wake of a Task that woke a moment ago is one more status read of an awake Task, and a `ResumeTask` call that changes nothing for a running Task. Two replicas can therefore wake one Task in the same interval, and the Task still wakes once.

`workbench.fleet.wakeOnQueue` is on by default when the configuration names atespaces and an intake prefix. A value of `on` without both stops the start with a message that names the key. `fw-workbench doctor` says whether the front reads the queues and wakes the Tasks for them.

### 4.14 Metrics and alerts

The package `fleet` adds the series of the fleet to the registry of the front through `metrics.RegisterCollector` (the kernel architecture, section 10.1). The collector reads, at scrape time, what the front already holds: the last read of the queues (section 4.13), the dead letters, and the view of the egress stream. A scrape therefore sends no request to a Task or to the server of the bus, and it wakes nobody. The series go out like every series of the registry: through `/metrics` to Prometheus, and through the OTLP push of the front to a collector (the kernel architecture, section 10.5). A worker exports the facts of its own runs; the series of this section show the fleet from the front, so an alert needs no metric of a Task.

| Metric | Type | Labels | What it counts |
|---|---|---|---|
| `fw_fleet_queue_pending` | gauge | `queue` | The jobs on the queue that no worker took yet |
| `fw_fleet_queue_ack_pending` | gauge | `queue` | The jobs of the queue that a worker took and did not acknowledge yet: their runs are not queued yet |
| `fw_fleet_dead_letters_total` | counter | `queue` | The dead-letter records of the queue on `<queue>.dead` |
| `fw_fleet_dead_letter_last_timestamp_seconds` | gauge | `queue` | The Unix time of the last dead-letter record. A queue without one has no series. |
| `fw_fleet_runs_finished_total` | counter | `agent`, `member`, `outcome` (`completed`, `failed`, `canceled`, `other`) | The runs of the workers that finished, from `fw.run.finished` on the egress stream |
| `fw_fleet_runs_budget_exceeded_total` | counter | `agent`, `member`, `limit` (`maxTurns`, `maxTokens`, `maxCostUSD`, `maxToolCalls`, `deadline`, `maxRepeats`, `other`) | The runs of the workers that a budget limit stopped. Each one also counts as `failed`. |
| `fw_fleet_approvals_pending` | gauge | `agent` | The runs of the workers that wait for a person: an approval, a question, or a connection |
| `fw_fleet_approvals_oldest_timestamp_seconds` | gauge | `agent` | The Unix time of the request of the oldest approval that waits. An agent without one has no series. |
| `fw_fleet_members_blocked` | gauge | none | The members whose status said `blocked` after a storage failure when the suspend controller read it, until the next event of each (section 4.12) |

The labels hold names that the deployment bounds. `queue` is `<workbench.fleet.intake>.<agent>` for each profile that the front knows. `member` is `<atespace>/<task>` for a Task and the node name for a worker that only the stream knows. `agent` names a profile. A process that can publish on the egress subject can name any agent, so each run counter keeps at most 1024 pairs of agent and member, and the pairs past that bound count under the agent and the member `other`. No label holds a run id, a person, a batch, or a text. The queue gauges have a series only while the last read of the queues worked, so a front that cannot read them shows no stale depth.

Some rules apply when you read the series:

- The run counters count the ends that the front saw since it started. Each replica of the front receives the whole egress stream, so each replica counts every end. Take the maximum over the replicas, not the sum.
- A run that finishes again after a follow-up counts again, as `fw_runs_total` of the kernel counts it. A repeated event of the stream counts once.
- A budget stop comes from the error of `fw.run.finished`, `budget exceeded: <limit>`, because the egress stream of a worker carries no `fw.budget.exceeded`.
- When the stream captures `<queue>.dead`, as the default filter `fw.>` does, `fw_fleet_dead_letters_total` is the number of records that the stream holds. Every replica then reports the same value, and a restart of the front keeps it. A purge of the stream makes the value fall, and Prometheus reads that as a counter reset. When the stream does not capture the subject, the front counts the records that its subscription receives since it started, and each replica counts on its own. The kernel gives the dead letter of a job the same event id at every try, so a record that two processes sent, as before and after a suspend, counts once while its id is among the last 4,096 ids of the queue. A record without an id counts at each delivery. The front logs each dead letter that reaches its subscription with its id, its type, and its run, never its data.
- `fw_fleet_approvals_pending` counts the runs of other members only. The kernel series `fw_approvals_pending` of the front counts the runs of the front itself.
- Each replica of the front runs the suspend controller, and only a replica that read the status marks the member. Once one replica suspends the Task, the other replicas see a Task that sleeps and read it no more, so `fw_fleet_members_blocked` and `blockedReason` can show the mark on one replica only. Take the maximum over the replicas. A replica that starts after the suspend does not know the mark.

These alert rules are examples to start from. Change the thresholds to fit your agents, and load them beside the rules of `examples/observability` of the kernel. Each rule takes the maximum over the replicas of the front.

```yaml
groups:
  - name: fw-fleet
    rules:
      - alert: FwFleetRunFailuresRising
        expr: |
          (
            max by (agent, member) (increase(fw_fleet_runs_finished_total{outcome="failed"}[30m]))
              / max by (agent, member) (sum without (outcome) (increase(fw_fleet_runs_finished_total[30m])))
          ) > 0.25
          and max by (agent, member) (increase(fw_fleet_runs_finished_total{outcome="failed"}[30m])) >= 3
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "More than 25% of the runs of agent {{ $labels.agent }} on {{ $labels.member }} failed in 30 minutes"
          description: "Read the error of a failed run with the tool batch or in the dashboard. fw_fleet_runs_budget_exceeded_total tells whether a budget limit stopped them."

      - alert: FwFleetQueueDeep
        expr: max by (queue) (fw_fleet_queue_pending) > 20
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "More than 20 jobs wait in {{ $labels.queue }} for 15 minutes"
          description: "No worker takes them. Look for a Task that sleeps and does not wake, a Task that cannot reach the bus, or a profile that no Task serves."

      - alert: FwFleetApprovalWaiting
        expr: time() - min by (agent) (fw_fleet_approvals_oldest_timestamp_seconds) > 30 * 60
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "An approval of agent {{ $labels.agent }} waits for more than 30 minutes"
          description: "The run does nothing until a person decides. Open the dashboard, or call batch and approve."

      - alert: FwFleetMemberBlocked
        expr: max(fw_fleet_members_blocked) > 0
        labels:
          severity: warning
        annotations:
          summary: "A storage failure blocked {{ $value }} fleet members"
          description: "The front suspended each such Task. GET /v1/fleet or the tool fleet shows the reason as blockedReason. Repair the storage, such as a full /workspace, before the next request restarts the Task."

      - alert: FwFleetDeadLetter
        expr: max by (queue) (increase(fw_fleet_dead_letters_total[15m])) > 0
        labels:
          severity: warning
        annotations:
          summary: "A job of {{ $labels.queue }} went to its dead-letter subject"
          description: "A worker did not start the job, or its run failed on its last attempt. The record is on {{ $labels.queue }}.dead."
```

The rules keep only the labels that they name, so the label of the replica goes, and they work for the series of a scrape and of the OTLP push alike.

## 5. Browser sign-in

A person signs in to the dashboard at `/ui` through the OpenID Connect issuer of `auth.oidc`. Three keys turn the sign-in on and shape it:

| Key | Variable | Meaning |
|---|---|---|
| `auth.oidc.clientId` | `FW_AUTH_OIDC_CLIENT_ID` | The client id at the issuer. A value turns browser sign-in on and adds the routes `GET /ui/signin` and `GET /ui/callback`. |
| `auth.oidc.clientSecret` | `FW_AUTH_OIDC_CLIENT_SECRET` | The `Secrets` name of the client secret, such as `FW_OIDC_CLIENT_SECRET`. Empty means a public client. |
| `auth.oidc.redirectUrl` | `FW_AUTH_OIDC_REDIRECT_URL` | The callback address that the issuer knows |

The sign-in also reads `auth.oidc.issuer`, `auth.oidc.scopes`, `auth.oidc.ca`, and `auth.oidc.insecure`. The scope of the request is `openid` plus `auth.oidc.scopes`. Without `auth.oidc.redirectUrl`, the redirect URI is `/ui/callback` on the public address of the page, from `mcp.auth.audience` or `a2a.url`. Without those, it is `/ui/callback` on the address of the request, as a proxy forwards it in `X-Forwarded-Proto` and `X-Forwarded-Host`. Only the first value of each header counts. Register that address at the issuer. A connection (the kernel architecture, section 7.11) returns to `/ui/connect/callback` on the origin of `auth.oidc.redirectUrl`, else on the origin of the request. Register that address too.

A configuration that cannot work stops the start. These are the cases:

- The issuer is not an https URL, and `auth.oidc.insecure` is not set.
- The client id has no issuer.
- The redirect URL is not absolute.
- The CA bundle holds no certificate.

The flow has these steps:

1. The page sends the person to `GET /ui/signin?next=<fragment>`. `next` is the place in the page to return to. If it does not start with `#`, or if it holds a control character, the page returns to `/ui`.
2. The person can open `/ui` on another origin than the redirect URI. Then the route first answers 302 to `/ui/signin` on the host of the redirect URI. A default port counts as the same origin. The new address keeps `next` and adds `hop=1`. The cookie therefore lands on the host that receives the callback. A prefix before `/callback` in the redirect URI stays in the path. With `hop`, the route never moves again, so a proxy that hides the host causes no loop.
3. The route reads the discovery document of the issuer and keeps it for an hour. It sends the person to the authorization endpoint with the authorization code flow, PKCE with S256, a `state`, and a `nonce`.
4. The cookie `fw_signin` holds the PKCE verifier, the state, the nonce, `next`, and the redirect URI. It lasts 10 minutes. It is signed with HMAC-SHA256, HttpOnly, SameSite=Lax, and limited to the path `/ui`. Over https, it is also Secure.
5. The issuer sends the person to `GET /ui/callback`. The route compares the state with the cookie and exchanges the code at the token endpoint. A confidential client authenticates with HTTP Basic. If the issuer lists only `client_secret_post`, the client sends the secret in the form.
6. If the answer holds an ID token, its nonce must match the cookie.
7. If the `Authn` of the agent accepts the access token, the route keeps it. If the `Authn` refuses the access token and accepts the ID token, the route keeps the ID token. A token that gives an anonymous principal signs nobody in. To accept ID tokens in `oidc` mode, put the client id in `auth.oidc.audience`.
8. The answer is a small page with a strict content security policy and one script. The script stores the token in `sessionStorage["fw.token"]` and replaces the location with `/ui` plus `next`. The callback therefore leaves the history of the browser.

Every answer of `/ui/callback` deletes the cookie. A failure answers a plain page that says in one sentence what failed. No token, code, verifier, or secret enters a log line or an error message. When browser sign-in is on, every 401 problem document of the native API has the member `"signin": "/ui/signin"`. The content security policy of `GET /ui` allows the move to `/ui/signin` and still blocks forms.

The page keeps one token, the access token or else the ID token, in the `sessionStorage` of its tab. It sends that token as a bearer token to `/v1`. The callback hands the page no refresh token, and no cookie holds the token. Pasting a token stays as the path for a developer.

The key that signs the cookie comes from the first source in this list:

1. The secret `FW_MCP_STATE_KEY`, read through `Secrets`. This is the secret that signs the approval retries of the MCP server (section 3.3).
2. The client secret of `auth.oidc.clientSecret`.
3. A random 32-byte key in the file `$FW_HOME/signin.key`. The surface creates the file with mode 0600, and processes that share the home directory share the key.
4. A random key in memory, with one warning in the log.

If the key comes from the key file and the agent likely has replicas, the log holds one warning at start. Replicas are likely with a fleet, with a ledger driver other than `mem` or `file`, or with a bus driver other than `mem`. Each replica then has its own home and so its own key file.

The key is an HMAC-SHA256 of its source with a label for this purpose. It therefore never equals the MCP state key. Replicas behind one address without session affinity set `FW_MCP_STATE_KEY` to the same value. Otherwise a callback that reaches another replica fails. A confidential client works without it, because every replica has the client secret.

## 6. Managing agents

The package `agents` creates, changes, and removes a profile (section 10). The workbench adds the MCP tools, the page, the shared views, and the agent builder on top.

### 6.1 The MCP tools

The MCP server carries these management tools beside the work tools:

| Tool | What it does |
|---|---|
| `whoami` | The principal of the caller, its role on every agent that it can see, and its teams (section 8) |
| `agents_list`, `agent_get` | The agents that the caller can see, and one agent with the words of what it can do. With pools, the profiles that the workers of each pool run, each with `pool` (section 14.9). |
| `agent_put`, `agent_delete` | Create, change, or remove an agent. `dryRun` checks a profile and changes nothing. With pools, `pool` names the pool (section 14.9). |
| `runs_list` | The runs that the caller can see, with `scope`, `member`, `batch`, and `by` (section 8) |
| `enqueue` | Send a job to the fleet: one run per item on the intake queue of a profile, as the caller, under one batch name (section 4.9) |
| `batch` | How a job goes: the jobs that wait in the queue because no Task took them yet, every run of a batch by state, the members that hold them, the runs that wait, the runs that failed, the runs that the view forgot, and the usage of the runs, with a mark when the sum is partial (section 4.9) |
| `run_events` | One page of the events of a run as chat items. `from` reads forward, `before` reads back, and `compact` cuts each tool output. |
| `run_result` | What a run produced and what it needs next. The text is the result as a person reads it. For a run of another member, the front builds the result from the run and the events of the owner (section 4.5). |
| `run_file` | A file that a run changed. With `raw` true, it returns the bytes up to 4 MiB (the kernel architecture, section 7.9). |
| `agent_builder` | Start the conversation that creates an agent, from `input` or `fromRun` |
| `capabilities` | The tools of the runtime, each with a plain phrase, built-in tools first. This is the MCP form of `GET /v1/tools`. |
| `dashboard` | The MCP App, and the home of the caller as text |
| `usage` | Deprecated. The result does not change, and the description points to the Prometheus metrics (the kernel architecture, section 10). |
| `connections` | The services that act for the caller, connected or not, each with its connect link (section 11) |
| `disconnect` | Remove the grant of the caller for one service |

`run_events` refuses `from` and `before` together. Its result has `prev`, the value of `before` for the page in front. If the page starts at the first event, the result has no `prev`. For a run of another member, the server turns `before` into `from` and `limit` on the events route of that member. The sequence numbers of a run have no gaps, so the page is the same. With `compact` true, each tool output of the page keeps its first 200 characters, `size` holds the bytes of the output, and `cut` is true, so a poll of a busy run costs little. The page without `compact` holds the full outputs.

`agent_builder` takes exactly one of `input` and `fromRun`, and it returns the run of the builder at once. The structured result of `run_result` is `{view: "result", result}`, and the structured result of `capabilities` is `{view: "capabilities", tools}`. An exposed tool of `mcp.expose` never takes the name of a work tool or a management tool.

The tool `dashboard` returns an MCP App: an HTML page that the host renders in a sandbox and that calls the same tools through the host. Only the tool `dashboard` names the page in `_meta.ui.resourceUri`. A host therefore shows the page once, at the call that opens it. The other tools return text for the model and data for the page. The view `home` of `dashboard` adds `home`, the shared home below, over every run that the caller can see on every member. The old fields `runs`, `glance`, `agents`, and `whoami` stay for pages that are in the field. Its text is the home, then the agents of the caller without usage, then the members of the fleet. If `mcp.auth.audience` or `a2a.url` gives a public address, the text also names a browser link. The view `usage` still answers, and its first line says that it is deprecated.

### 6.2 The page

The same page is served at `GET /ui` for a browser, with browser sign-in (section 5) or a pasted bearer token. It is one embedded HTML file with no framework, no build step, and no external asset. The page shows each person only what their roles allow. It shows no usage, because operators read the Prometheus metrics (the kernel architecture, section 10).

The page has two places, Work and Agents. A small header holds the feather, the two places, and an account menu. The account menu names the person and opens Connections, and it links to the metrics when `workbench.links.metrics` in `fw.yaml` names a dashboard, such as the Grafana dashboard of `examples/observability` in the kernel repository. If the browser holds a token, the menu also offers Sign out. Connections (`#connections`) lists every service that acts for the person (the kernel architecture, section 7.11), with Connect or Disconnect. The address keeps the screen, such as `#run/<id>`, `#agent/<name>`, or `#agents`. An old link to a screen that is gone, such as `#usage`, opens home without an error. In a wide window, the place stays in the side pane, and the main pane shows the open item or the composer.

Home starts with a composer that asks what the person wants to move forward. It takes files from the + button, a paste, or a drop. A small chip names the agent that starts the work. The chip opens a menu of the agents that the person can start, and the page remembers the last choice. Under the composer, the sentence and the lists of `GET /v1/home` follow: Needs you, In progress, and Recent results, then a row for the runs by others. Each row leads with the title of the work. A quiet line under it names the agent, the status, and the time. A run that waits says what it needs on a third line. Each status has a word and an icon of its own shape, so color is never the only signal.

A run opens with its result above the conversation. The title comes first, then a quiet status line, the decision that waits, and what the run produced. That part is the result of the shared views below: the answer, the changed files, What it did, the limits, and the next actions as buttons. What it did is collapsed and shows only recorded outcomes: Done, Error, or No result. Done says that the call returned, never that a check passed. The conversation follows, then Activity, collapsed, and the message bar stays at the bottom. While a run works, the result shows only the title, the status line, and the decision, and the run opens at its end. A file opens its own screen with a Download action.

The page shows each pending decision once, in the result area. When its buttons scroll out of view, a small pointer above the message bar brings the card back. A permission card names the action and the target, such as "reviewer wants to create app.env". It shows the effect, the exact change or command, and why it asks. The tool name, the rule, and the raw input sit behind Details. The allow button uses the label of the server, such as "Run command", and Deny stays. For a run of a worker, a third button, Always allow, allows the call and adds the tool to the policy of the pool of the agent (section 14.9). The card offers it for an ask of the default and of a rule, but not for an ask by a flag such as `destructive`, which no allow rule loosens. A question card reads "Question from reviewer" with the question and its form, and it offers Answer and Decline.

A connect card says what the run waits for. An example is "Connect Tickets, so the agent can act as you there." It offers Connect and Skip, never Allow. The home shows the same request under Needs you, with a Connect button. Connect opens `#connect/<server>/<run>`. If the browser holds no token, that screen signs the person in first. Then it sends the browser to the authorization server. The callback returns to the run, or to `#connected/<server>`, which says Connected. One connection resumes every run of the person that waits for that service.

The page keeps the work of a person:

- Each place keeps one draft: the home composer, the message bar of each run, and the builder form. Text goes to `sessionStorage`, and so do the files of a place up to about 1 MB. A draft survives a move to another screen, a poll, a reload, and a sign-in. A send or a start clears it.
- A poll changes only the rows and the cards that changed. Focus, the caret, scroll positions, an open Details, and an open menu therefore survive it. A form with the focus or with edited fields keeps its content.
- A title that a model writes after the start replaces the first title in the heading, the bar, and the lists. The run screen reads a finished run every 10 seconds for 2 minutes, so a late title still arrives.
- A run opens at its recent end. Load earlier messages, or a scroll up to it, reads the page in front with `before`. The item that the person reads stays in its place. New items still come forward with `from`.

The keyboard reaches every action:

- ArrowUp and ArrowDown, or j and k, move between the rows of a list or a menu. Enter opens a row and moves the focus to the heading of the new screen.
- Escape closes a menu and returns the focus to its button. In a text field, Escape leaves the field. Elsewhere, Escape goes back.
- `/` puts the focus in the home composer. If the composer is not on the screen, `/` opens home first.
- Cmd or Ctrl+Enter sends, starts, or submits the form.

Every action is a button or a link with a visible focus ring. On a touch screen, each target is at least 44 pixels. Motion is short, and `prefers-reduced-motion` turns it off.

When an answer is 401 with `signin`, the page shows Sign in. Sign in goes to `/ui/signin` with `next` set to the current address, and the person comes back to the same screen. A quiet disclosure, "Use an access token", holds the token form. Without browser sign-in, the token form is the screen. An expired token shows the same screen with "Your sign-in ended", and every draft stays. Sign out forgets the token and asks the server again.

The Agents place lists the agents by fleet member and offers New agent. The editor of an agent leads with the name, "What does this agent handle?", the instructions, and what the agent can do in plain words. Model, effort, time zone, globs, access patterns, and the JSON sit under Advanced, collapsed. The contact card of an agent shows no usage.

The screen of an agent says what the agent can do in plain words, so a person never reads a glob to learn it. `Agent.ProfileGrants` gives each tool of the agent with the verdict that a call with no input gets: the stricter of the agent policy and the rules of the profile. It also gives the rules with a command or a field pattern for that tool, such as a deny of `bash: git push*`. It asks no control plane: under `policy.external`, a tool shows "Your control plane decides" unless the profile denies it. The screen groups the tools under "On its own", "Asks you first", and "Cannot". A pattern rule is a second line, such as `Never runs "git push*"`. "Cannot" lists the built-in tools that the profile leaves out and the tools that the policy denies. The view of one agent carries these words as `abilities`, and `agent_get` gives them as text to the model. The screen also words the budget, for example "30 minutes of work, 40 turns". When the store refuses a change, the view carries `locked`, through the optional interface `fw.ProfileLocker`. For a profile that `fw.yaml` defines, the screen says so in place of Edit and Delete.

`agent_put` with `dryRun` and `PUT /v1/agents/{name}?dryRun=true` check a profile as a save does and change nothing. They answer with the words of what the agent can do and with `exists`, which says whether a save replaces an agent. `Agent.CheckProfile` runs these checks in the kernel. `agent_put` takes a profile as `agent_get` returns it, `"skills": null` included.

New agent opens the agent builder. It asks one question, `What should this agent handle?`, and Set it up by hand opens the editor. The answer starts the builder on the server with `POST /v1/builder` or `agent_builder` (see "The builder on the server" below). The builder talks with the person and calls `propose_agent`. The call holds a name, a description, and the instructions of the agent. It also holds the tools of the agent, the tools that ask first, the commands that it never runs, and its minutes of work. The tool checks the names against the tools of the process. When a name is wrong, it lists the tools that exist. It saves nothing. The page reads the `profile` of an accepted proposal from its item. It draws the contact card of the new agent from a dry run, with Create agent. A tap saves the profile through `agent_put` with the rights of the person. The model therefore never creates an agent, and the person becomes its owner. A later proposal fades the earlier card. Save as agent in the next actions of a result sends `fromRun` to the same route.

A run that hands work to other agents reads as a group conversation. A call of `spawn_task` is a line with the face of the helper and its task, and a tap opens the run of the helper. When the tool waits, the answer of the helper is a bubble with its face and its name. The status line names the helpers, for example "with reviewer and writer". A child run records its parent and not the call. The page therefore finds the child run of a handoff by its agent and its task. Subtasks lists only the child runs that no handoff shows. The conversation names no tool for a step. It says "Waits for you to allow a command", "Working on a step", or "Took 3 steps". Activity reads the `lines` of the events page, as over MCP.

A question of `ask_user` is a message of the agent in the conversation, and the answer is a message of the person. Its card reads the schema of the question. When the answer is one property with an enum, or one boolean, the card shows one button per choice and a Decline link. A flat schema of strings, numbers, integers, booleans, and enums becomes a small form. A free answer and a schema that nests keep the text box.

Inside an MCP host, the page follows the theme and the display mode of the host. Inline, it fits its content. The MCP Apps specification has no download method, and a host can block a download in its frame. After a download in a host, the page therefore says that the host can block it and that the file screen still shows the file.

### 6.3 The shared views

The native API, the MCP tools, and the page build their answers from the same functions in `dashboard/` and in the kernel package `surface/view`. A browser, an MCP App, and a host that shows only text therefore say the same thing.

Each run gets one status word. The word comes from the run alone, so a list needs no ledger read.

| Status | The run |
|---|---|
| `needs-you` | Waits for an approval or an answer |
| `working` | Is queued, running, or suspended |
| `ready` | Completed with an answer that a person can review |
| `ended` | Completed with no answer |
| `failed`, `canceled` | Stopped |

No status says that the work passed its checks. A model that ends a run proves nothing about the work.

A view names the work, not the worker. `TitleOf` takes the title of the run (the kernel architecture, section 8.2), else a title from the input, else the agent name. A run of an older fleet member has no title, so the view makes one. A builder run is "New agent", unless its metadata `fw.title` names another title.

Home is `GET /v1/home` and the `home` of the `dashboard` tool. It holds the top-level runs of the caller and of the caller's teams, as `by=team` does (section 8). A child run works for its parent, and its parent shows it. Home reads the runs of the whole fleet that the caller can view. It has three lists:

- Needs you: the runs that wait for a person, newest first.
- In progress: the runs that work, newest first.
- Recent results: the runs that completed, failed, or were canceled in the last 7 days, 20 at most.

`others` counts the top-level runs of other people that the caller can see. `sentence` says in one sentence what the lists hold, and `glance` counts the same runs. Every list is an array, and an empty list is `[]`. A view never changes a run of the kernel or of the fleet view.

The result of a run is `GET /v1/runs/{id}/result` and the MCP tool `run_result`. It reads the run with `a.Get`, which authorizes the caller, and then the whole ledger. It gives the facts in the order that a person reads them:

- Summary: the output of the run, else the last text of the model.
- Files: the net change of the run to each path, in the order of the first change. A later change replaces an earlier one, and a rename moves the entry to the new path. A file that the run created and then edited stays `created`. An attachment of the person is no change of the run.
- Steps: the newest 12 tool calls that ran, each with a plain phrase, such as `Ran "go test ./..."`, `Read docs/a.md`, or `Edited a.go`. A call that did not run, such as a denied call, is no step.
- Pending: the approval or the question that the run waits for, with its label.
- Limits: the budget stops and the loop guard in plain words, such as "Stopped at the limit of 20 turns.". A run that no limit stopped lists each limit that it used for three quarters or more, such as "Used 18 of 20 turns.". `Agent.RunBudget` gives the effective budget for this check.
- Next: the useful actions, the most useful first: `decide` or `answer`, `review`, `reply`, `save-agent`, and `retry`.
- Usage: the usage of this run, beside the limits that it nears.

The label of a decision is the `Action` of the tool (the kernel architecture, section 7.1), else "Allow". A question gets "Answer", because an answer is not a permission to act. If the input says the effect reliably, the decision gives it in one sentence. That is `Runs "<command>".` for `bash`, and `Changes <path>.` or `Removes <path>.` for `files`. Other inputs and questions have no effect sentence.

Save as agent takes only a completed top-level run that is not a builder run. The run can be a run of this process or of another member of the fleet, which the front reads from its owner (section 4.5); a run in a Task of its own that a parent started counts as a child run, as the view knows it. The caller must be the person who started the run and must be able to create agents. A view of the run is not enough, because the builder run keeps the teams of the caller. The terms hold for the next action, for `POST /v1/builder` with `fromRun`, and for `agent_builder` with `fromRun`. A refusal is a `dashboard.SaveRefusal`, which wraps `fw.ErrForbidden`. The native API answers 403, and MCP answers "Forbidden: <reason>.". No owner can grant such a refusal, so its reason names no role. `Home.String` and `Result.String` give the same views as text for a host that shows only text.

### 6.4 The builder on the server

The package `tools/builder` registers the built-in tool `propose_agent`. Its `fw.RegisterDefaults` call adds the tool to the built-in tools of the default configuration.

| Tool | Purpose | Flags |
|---|---|---|
| `propose_agent` | Propose a new agent, which the person creates from a card (section 6.2) | read-only, idempotent, opt-in |

The tool has no `Action` label (the kernel architecture, section 7.1). It changes nothing, so a person seldom allows its calls.

The agent builder runs in Go, so a host that shows only text can use it. `BuilderRequest` starts the builder on the default agent. Its inline profile holds the builder prompt and the tools `propose_agent` and `ask_user`. Its metadata holds `fw.view: builder` and the title "New agent".

`SaveAsAgentInput` makes the first message of Save as agent from the ledger. It holds the first eight messages of the person and the tools that the run called. It also holds the last answer of the agent, cut to 2000 characters. A message that the kernel sends itself, such as "Continue.", is not a message of the person.

`ProposalProfile` turns the input of `propose_agent` into a profile. A tool in `asks_first` becomes an ask rule. A command in `never_runs` becomes the deny rule `bash: <command>`. `minutes` becomes the deadline of the budget. A proposal without a name is an invalid request. The chat item of a `propose_agent` call carries `profile`, the profile of its proposal. A cut input makes no profile.

### 6.5 Text-only hosts

A host that shows only text does the work of the page with tool calls. The text of each answer tells the model how to go on.

To create an agent:

1. Call `agent_builder` with `input`, the work of the new agent. To save a completed run that you started, pass `fromRun` instead.
2. Read what the builder says with `run_result` or `run_events`.
3. Answer the builder with `send` on the run of the builder. If `run_result` shows a pending question, answer it with `approve`.
4. Find the item of the `propose_agent` call in `run_events`. Take its `profile`.
5. Call `agent_put` with that profile and `dryRun` true. Read what the agent can do.
6. Call `agent_put` with the profile. The caller becomes the owner of the agent.

The model never creates an agent in this flow. `propose_agent` saves nothing, and `agent_put` runs with the rights of the caller.

To follow a run to its result:

1. Call `dashboard` with the view `home`, or `runs_list`, to find the run.
2. Call `run_result` with `runId`. Read the summary, the files, the steps, the pending decision, the limits, and the next actions.
3. If a decision waits, call `approve` with `decision`. For a question, put the answer in `content`.
4. To read a changed file, call `run_file`. Set `raw` to true to get the bytes.
5. To continue the work, call `send` with the next message.

## 7. What still lives in the kernel

The split did not change the contract of the package `fw`. The kernel keeps the primitive of a connection: `fw.ConnectRequest`, `fw.RequestConnect`, `fw.PersonTokens`, and `Agent.Connected` (the kernel architecture, section 7.11). The MCP client of the kernel, `tools/mcp`, still acts for a person with the tokens that the package `connections` gives it (section 11).

The workbench still reads three parts of the kernel configuration: the block `mcp` of its server surface, the OAuth setup of each `tools.mcp` entry with `auth.oauth.grant: user`, and `auth.oidc`. Everything else of the workbench lives in the block `workbench` of `fw.yaml`: `teams`, `titles`, `fleet`, `grants`, `access`, and `sources`.

## 8. Teams

A team is a group of people and agents that work together. The dashboard shows the work of a person and of the person's teams first. The runs of other people sit behind one row. A team groups the view and grants no access: the access blocks of the kernel, or its control plane, still decide who can do what. The package `teams` holds the rules, and the kernel knows no team (the kernel architecture, section 13.6).

The admins keep the teams upstream, in the identity provider:

| Source | The teams of a person | The teams of an agent |
|---|---|---|
| The rules of the kernel | The groups of the principal, from the groups claim of OIDC, that match the patterns | The groups that the access block of the profile names as `group:<name>` and that match the patterns |

The patterns come from the block `workbench` of `fw.yaml`, which the package claims with `fw.RegisterExtension`:

```yaml
workbench:
  teams: ["team-*"]   # glob patterns over group names; empty makes every group a team
```

The variable `FW_WORKBENCH_TEAMS`, with commas between the patterns, wins over the file. An empty list, which is the default, makes every group a team. A team name is an opaque string. With `access.external`, the kernel asks a control plane about access, and the teams still come from the groups. A later revision moves `access.external` to the workbench and lets the control plane name the teams again.

A run keeps the teams of the person who started it. The package registers a run annotator with the kernel (the kernel architecture, section 12.4). When the kernel queues a run that a person starts, the annotator writes the teams of the person into the metadata of the run, under the key `workbench.teams`, with commas between the names. A child run that the process starts keeps the teams of its parent. The teams of a run therefore stay as they were at the start, and every fleet member reads them from the run record. `GET /v1/whoami` and the tool `whoami` return the teams of the caller. A fleet record names the teams of each agent. Without a fleet, the page knows the teams of an agent that the person shares, from the access block of the agent.

The runs lists of the workbench take `by`, which keeps a part of the runs that the caller can see:

| `by` | The runs |
|---|---|
| `me` | The runs that the caller started |
| `team` | The runs that the caller started, and the runs that a person on a team of the caller started |
| `others` | Every other run that the caller can see |

The MCP tool `runs_list` takes `by`, and the sentence and the counts of its answer cover the same runs. Home (`GET /v1/home`) holds the runs of `by=team`, and `GET /v1/home/others` holds the top-level runs of `by=others`. The row and the status line of a run of a teammate name that person, for example "by alice". In the conversation, a message of another person shows the name of that person above it. In the Agents place, the agents that the caller owns come first, then the agents of the caller's teams, then the rest. If other people have runs that the caller can see, a last row on home reads "Runs by others" with their number. It opens their runs, grouped by person. A person with no team sees only their own runs on home and the rest behind that row.

## 9. Titles

A run has a title, such as "Fix checkout retries", so a list names the work and not only the worker. The kernel gives a run the title of its request, or else the first sentence of its input, and makes no title call (the kernel architecture, section 8.2). The package `titles` asks a model for a title of a few words the first time that the workbench shows a run that has none: on home, in the result of a run, and in the list of the tool `runs_list`. A run that nobody looks at costs no call.

The call goes to `compaction.model` when it is set, else to the model of the agent of the run. `titles.Clean` turns the answer into one line of at most 64 characters. The package records the title on the run with `fw.Agent.Annotate`, as the metadata `fw.title`, together with the usage, the provider, and the model of the call. The usage and the cost of the run include the call, and the metrics count it. A call that yields no title still records its usage, because the tokens cost. A top-level run of this process gets one call per process, whatever the outcome. A child run, a run of another fleet member, a run whose request set a title, a replay, and a run without text in its input get no call.

The mode comes from the block `workbench` of `fw.yaml`, or from the variable `FW_WORKBENCH_TITLES`, which wins:

```yaml
workbench:
  titles: model   # model, the default, or input: the first sentence of the input, with no call
```

A title that arrives after a run finished changes only the title and the usage of the run. The run stays finished, in the metrics, in the traces, in the ledger file, and across a restart.

## 10. Managing agents

The kernel reads its agent profiles from the configuration and writes none (the kernel architecture, section 13.3). The package `agents` creates, changes, and removes profiles for people. It normalizes a profile, checks it with `fw.Agent.CheckProfile`, authorizes the caller with the kernel's actions, writes the file under `<home>/agents` through the store of the config package, and hands the kernel the new set with `fw.Agent.SetProfiles`, so a run can name the profile at once. A run that is already queued keeps the profile that it was queued with, and only a stricter policy of the changed profile reaches it, also a call that waits for an approval. After a restart, such a run takes the system prompt and the instructions of the profile then, and the kernel records a change of them on the run (the kernel architecture, section 12.5). A new profile needs `agent.create`, a change needs `agent.edit`, and a removal needs `agent.delete`.

A principal that creates a profile without an `access` block becomes its first owner. An individual or a group therefore owns the agents that they make. A replaced profile that names no access block keeps the access block it had, so an edit never drops the owners by accident. A profile that the configuration defines, in `fw.yaml`, in a variable, or through a Go option, cannot change here, because the configuration wins over the file at the next start. The kernel reports such a profile as locked, and the page says so before a person edits it.

The routes `PUT` and `DELETE /v1/agents/{name}`, the MCP tools `agent_put` and `agent_delete`, and the verbs `fw-workbench agents put <file.yaml>` and `fw-workbench agents delete <name>` all go through the package. The package writes to a place, `agents.Place`: the store of the process, or on a front with pools the overlay of the pool that runs the profile (section 14.9). The checks, the access rules, and the roles are the same for both. `PUT /v1/agents/{name}?dryRun=true` and `agent_put` with `dryRun` check a profile as a save does and change nothing. They answer an `agents.Draft`: the view of the profile, what the agent would do, and whether saving it replaces an agent that exists. The agent builder proposes a profile and never saves one; `agent_put` saves it with the rights of the caller (section 6.4).

## 11. Connections

A tools.mcp entry of the kernel with `auth.oauth.grant: user` acts for the person who started the run (the kernel architecture, section 7.11). A grant is the permission that a person gives the workbench at one authorization server. In OAuth terms, it is a refresh token (RFC 6749, section 6), with its scopes and its client. The workbench keeps one grant per person, per authorization server, and per client. With a grant, a run goes on for hours or days after the access token of the person expired. The grant also survives a restart of the process.

The package `connections` holds all of it. It is a service of the kernel (`fw.RegisterService`), so a process that serves has it and a verb that runs one piece of work does not. It is the `fw.PersonTokens` of the agent: the MCP client of the kernel asks it for the access token of a person at a service, and it answers from its store and the authorization server. It serves the Connect flow through the routes of section 2.1 and the tools of section 3.4, and it resumes the paused calls of a person with `fw.Agent.Connected` when a connection ends. It adds the `connections` line to `fw-workbench doctor`.

### 11.1 Configuration

The entries of `tools.mcp` carry the OAuth setup of each service: the client, the issuer, the scopes, and the exchange (the kernel architecture, section 7.11). The store of the grants is the block `grants` under the key `workbench` of `fw.yaml`. Each variable wins over the file.

| Key | Variable | Meaning | Default |
|---|---|---|---|
| `workbench.grants.store` | `FW_WORKBENCH_GRANTS_STORE` | `file`, `memory`, or a registered driver | `file` |
| `workbench.grants.dir` | `FW_WORKBENCH_GRANTS_DIR` | The directory of the file store | `<home>/grants` |
| `workbench.grants.key` | `FW_WORKBENCH_GRANTS_KEY` | The `Secrets` name of the key that encrypts the file store | `FW_GRANT_KEY` |

An entry with `grant: user` needs an HTTP endpoint. `auth.mode` must include `oidc`, because only a person with an OpenID Connect identity can connect. An entry without a client of its own also needs `auth.oidc.clientId`. With the browser sign-in of the workbench on (the workbench architecture, section 5), a person who opens a connect link signs in first. Without it, that person pastes a token first. The `connections` line of `fw-workbench doctor` reports each of these conditions.

### 11.2 Two kinds of service

A service can trust the sign-in issuer: its authorization server is the issuer of `auth.oidc`. Such an entry needs no client of its own. It uses the client of the browser sign-in, and the person connects once for all such entries.

A service can also have an authorization server of its own, such as a hosted product. The entry then names its client with `clientId` and `clientSecret`. If `issuer` is empty, `fw` reads the protected resource metadata of the MCP server (RFC 9728) to find the authorization server. Then it reads the metadata of that server (RFC 8414, or OpenID Connect discovery). The person connects each such service on its own.

Each authorization server registers one more redirect address: `/ui/connect/callback` on the public address of the page (section 2.1).

### 11.3 How a person connects

The kernel pauses the call (the kernel architecture, section 7.11). The workbench keeps the grant, serves the Connect card, the routes under `/v1/connections`, and the callback `/ui/connect/callback` (sections 2.1, 3.4, and 6.2), and resumes the call with `fw.Agent.Connected`. A program that embeds the kernel without the workbench sets its own `fw.PersonTokens`. The flow has these steps:

1. A call needs a grant that the person does not have, or the server refused the grant. The call pauses. The kernel appends `fw.approval.requested` with `connect`: the server, its title, and the reason, `missing` or `expired`. The run moves to `input-required`.
2. Every surface shows the request. The dashboard shows a card with Connect and Skip in the run, and a row under Needs you on the home. An MCP client gets a URL-mode elicitation (the workbench architecture, section 3.4).
3. Connect opens `/ui#connect/<server>/<run>`. If the person is not signed in, the page signs the person in first.
4. The page calls `POST /v1/connections/<server>` and sends the browser to the authorization request in the answer. The request uses the authorization code flow with PKCE, a `state`, the scopes, and the `resource` of the MCP server (RFC 8707).
5. The authorization server sends the browser to `GET /ui/connect/callback`. The workbench exchanges the code and stores the grant. Then it resumes every paused call of the person for every service that is now connected.
6. The browser returns to the page, which says Connected and offers the run.

The answer of the code exchange must hold a refresh token. Without one, the connection fails, and the message asks for offline access. Skip denies the request. The call then fails with a plain message to the model, and the run does not ask again for that service. The kernel refuses a decision that allows a connect request, because a click is not a grant. Only the end of a connection allows it.

### 11.4 Tokens for a server

Each call needs an access token of the person for its server. The MCP client of the kernel asks for it through `fw.PersonTokens` (the kernel architecture, section 7.11). Without `exchange`, the workbench refreshes the grant with the `resource` parameter of the server (RFC 8707). The authorization server then issues a token for that server alone, as the MCP authorization specification requires. With `exchange`, the workbench refreshes the grant once for a token of its own client. Then it exchanges that token for a token with the configured `audience` (RFC 8693). Use `exchange` with an authorization server that ignores `resource`.

Access tokens stay in memory until shortly before they expire. A token that the server refused goes at once, and the next request gets a new one. When the server keeps refusing the tokens of a person, the grant goes, and the person connects again.

### 11.5 Rules that keep a grant alive

Many authorization servers rotate refresh tokens (RFC 9700, section 4.14). Each refresh returns a new refresh token and ends the old one. If an old refresh token comes back, the server cannot tell a thief from the client. It then revokes the grant, and some servers revoke more. The workbench therefore follows four rules with every server:

1. A grant has one holder: the grant store of one process.
2. One refresh of a grant runs at a time, for all the runs of the person.
3. The workbench stores the new refresh token durably before it uses the new access token.
4. If the server refuses a grant, the workbench deletes it, and the next call asks the person to connect again.

A lost grant is safe, because the person connects again. An old copy of a grant is not safe, because its refresh token counts as a reuse.

By default, the workbench asks for the scope `offline_access` (OpenID Connect Core 1.0, section 11). A plain refresh token usually belongs to the browser session of the person. If the person signs out, or if that session reaches its time limit, the refresh token ends. Each refresh from the workbench also keeps that browser session alive. An offline grant has a session of its own, and the person can revoke it at the authorization server. Some servers have no such scope. Google is one: set `offline: false` and give `authParams` the values `access_type: offline` and `prompt: consent`.

### 11.6 The grant store

`connections.Store` keeps the grants. Each write of a grant gets a new revision number. A write can name the revision that it replaces, so a shared store can refuse a stale write.

The default store, `file`, keeps one file per grant under `workbench.grants.dir`. The file name is a hash, so no subject or issuer appears in a directory listing. AES-256-GCM encrypts each file. The key comes from the secret `FW_GRANT_KEY`. Without that secret, the store creates a random key in the file `grant.key`, with mode 0600. A write goes to a temporary file. The store syncs that file, renames it over the old file, and syncs the directory. After the write returns, the grant is therefore whole and durable.

The store `memory` keeps the grants in the process, and a stop of the process forgets them. `connections.RegisterStore` adds a driver. Tests use `connections.NewMemStore()`, or install a `connections.Connector` of their own with `connections.Install`.

### 11.7 Restarts and ax

A call that waits for a connection survives a restart (the kernel architecture, section 7.11). Under ax, the home directory is on the durable volume. A suspend saves that volume and then kills the process (section 11.5). A refresh after the save is lost on resume, so the store holds the old refresh token. A server with rotation then refuses the grant, and the person connects again. A resume from an older snapshot, or a copy of an actor, has the same effect. The person sees one more Connect card. If the workbench has a client of its own, no other grant of the person breaks (see the example below).

### 11.8 Example: Keycloak

Keycloak is a common OpenID Connect issuer, and it shows each rule. Give the workbench a client of its own at the issuer, for example `fw`:

- Make it a confidential client with the standard flow. Token exchange needs a confidential client.
- Register `https://fw.example.com/ui/callback` and `https://fw.example.com/ui/connect/callback` as its redirect URIs.
- Keep the client scope `offline_access` on the client. Keycloak assigns it by default.
- For each MCP server with `exchange`, turn on standard token exchange on the `fw` client. Set `exchange.audience` to the client id of the MCP server.

Keycloak keeps a grant with `offline_access` as an offline session. The offline session survives the sign-out of the person. By default, the setting Offline Session Idle ends it after 30 days without a refresh. The person sees it under Applications in the account console and can revoke it there.

At the issuer, the setting Revoke Refresh Token turns rotation on. It is off by default. The setting Refresh Token Max Reuse says how many times an old refresh token still works. With rotation on, a reuse past that number gets `invalid_grant`. Keycloak also detaches the client session from the user session, which ends every refresh token of that client for that person. A dedicated client therefore limits the damage to the grants of the workbench, and the other applications of the person keep their sessions. The workbench assumes rotation with a reuse count of 0, the strictest case, so it works with every setting.

The MCP guide of Keycloak lists Resource Indicators (RFC 8707) as not supported. Keycloak therefore ignores `resource`, and a refreshed token does not name the MCP server in its audience. Use one of two ways:

- Set `exchange.audience` to the MCP server. Keycloak then issues a token for that server alone.
- Add an audience mapper to the `fw` client that names the MCP server. Every token of the client then works at that server. Each server then gets a token that also works at the other servers.

```yaml
auth:
  mode: oidc
  oidc:
    issuer: https://id.example.com/realms/main
    audience: [fw]
    clientId: fw
    clientSecret: FW_OIDC_CLIENT_SECRET
tools:
  mcp:
    - name: tickets
      title: Tickets
      endpoint: https://tickets.example.com/mcp
      auth:
        oauth:
          grant: user
          exchange:
            audience: tickets-mcp
```

### 11.9 The token broker

In a fleet, every run must act as the person who started it, on whatever member holds it, and a grant has one holder. The front therefore keeps every grant and runs every refresh, and each member gets the tokens of a person from the front. The kernel package `tokens` is the client: a member with `tokens.url` set asks the front, and the kernel architecture, section 7.11, has its side. This package serves the routes, through `native.Extend`, on the authenticated listener of the front.

`POST /v1/tokens` takes the subject of the person, the server, and the id of the run of that person. The caller must be the static token of the fleet or a workload, never a person: a person could ask for the tokens of others. The front looks the run up in its fleet records as the process itself, waits a moment for a run that another member started just now, and refuses when the fleet does not know the run or the person did not start it. Then it answers with the token of the person at the server and the expiry of the token, from the same grants and the same broker that its own tool calls use. A refusal is a JSON body with a `reason`: `forbidden`, `unknown-run`, `unknown-service`, `not-connected`, `expired`, `no-fleet`, `invalid-request`, or `error`. `POST /v1/tokens/reject` drops a token that the service refused, and `POST /v1/tokens/forget` drops the grant of the person at the server.

When a connection ends, `Finish` resumes the paused calls of this process with `fw.Agent.Connected` and publishes `fw.connection.connected` on the bus subject of `tokens.subject`, `fw.connections` by default, with the subject and the connected services. The client of each member turns the notice into `Agent.Connected` there.

`Disconnect`, and `Forget` after a service kept refusing a grant, publish `fw.connection.disconnected` on the same subject. The client of each member then drops every cached token of the person. Each front also keeps the access tokens of people in memory until they expire, so every front listens on the subject too. On either notice from another front, it drops the tokens and the pending grants of the person in its broker. The event names its process in `node`, and a front skips its own notices. Without these notices, a second front went on serving the token of a grant that the first front deleted, until the token expired.

The members of a fleet therefore need the bus, the shared static token, and `tokens.url`; `examples/agentgateway/fleet.yaml` shows all three. A process with `tokens.url` starts no connections of its own, whatever its `tools.mcp` entries say: the front keeps the grants, and the front therefore needs the same entries with `grant: user` in its configuration, so that it knows each service that a worker acts at.

### 11.10 Security

- No refresh token, access token, code, or PKCE verifier enters a ledger event, a log line, or an error message. The ledger holds the connect request and its decision. The decider is the person, and the note is `connected`.
- The file store encrypts every grant. Keep `FW_GRANT_KEY` in a secret store, apart from the volume of the grants. A key in `grant.key` beside the grants protects only a copy of the grant files that leaves the key behind.
- A cookie binds each connection to the browser that started it. The cookie holds the PKCE verifier, so it is HttpOnly and signed, and it lasts 10 minutes. A link from another person therefore cannot connect their account to your runs.
- For a service that trusts the sign-in issuer, the ID token of the connection must name the person of the run. A browser that is signed in at the issuer as another person cannot connect.
- Disconnect deletes the grant. If the authorization server offers revocation (RFC 7009), the workbench also revokes the grant there.
- In a fleet, the grants live on the front, and a member gets an access token of a person only for a run that the person started on it (section 11.9). A member never holds a refresh token, so a suspend or a restore of a Task touches no grant.
- The workbench has no device flow for the command line yet. A person connects in the dashboard or through an MCP client. The verbs that run one piece of work start no service, so they keep no connections.

## 12. Control-plane authorization

A deployment with many agents and many people often keeps its rules in one control plane. With `workbench.access.external`, the front hands every authorization decision to it: who views an agent, who runs it, who reads or operates a run, who changes a profile, and who is an admin. The front sends the facts of each check in the protocol of the kernel (the kernel architecture, section 13.5) and enforces the answer. It makes no decision of its own, so the control plane is the single source of truth, and a security team changes a rule without a change to `fw.yaml` and without a rebuild.

This is a plug of the front, not of a worker. The kernel of a worker decides with the rules of `access` alone: the static token of the fleet, the person who started a run, `access.admins`, and the access block of each profile. The package `access` builds the plug through `fw.WithAuthzFrom` when the binary starts an agent, so every verb of `fw-workbench` has it, and the kernel reports the mode `external` for it. The verbs `whoami`, `agents`, and `doctor` know the control plane through `cli.ExternalAccess`, and they act as the local principal, so the control plane decides for the local user too.

### 12.1 Configuration

| Key | Variable | Meaning | Default |
|---|---|---|---|
| `workbench.access.external.mcp` | `FW_WORKBENCH_ACCESS_EXTERNAL_MCP` | The name of a `tools.mcp` entry. The endpoint, the TLS, and the authentication of that entry apply, and the model never sees its tools. | none |
| `workbench.access.external.tool` | `FW_WORKBENCH_ACCESS_EXTERNAL_TOOL` | The tool on that server | `authorize` |
| `workbench.access.external.bus` | `FW_WORKBENCH_ACCESS_EXTERNAL_BUS` | The bus subject that receives the requests | none |
| `workbench.access.external.timeout` | `FW_WORKBENCH_ACCESS_EXTERNAL_TIMEOUT` | The limit of one request | `5s` |

The block sets exactly one of `mcp` and `bus`, or the process stops at start, and `mcp` must name an entry of `tools.mcp`. With `access.external` set, `access.default`, `access.admins`, and `access.creators` do not apply; the access block of each profile travels in each check as data, and the control plane decides whether it counts. This configuration sends the decisions of the front and the tool-call decisions of the kernel to one MCP server:

```yaml
tools:
  mcp:
    - name: control
      endpoint: https://control.example.com/mcp
      auth: { bearer: CONTROL_TOKEN }
policy:
  external: { mcp: control }
workbench:
  access:
    external: { mcp: control }
```

### 12.2 The checks

Each check names an action of the kernel, `agent.view`, `agent.run`, `agent.edit`, `agent.delete`, `agent.create`, `run.view`, `run.operate`, or `admin`, the agent, its access block, and the run with its state, its principal, and its lineage, the ancestors of a child run with the parent first. A check about an agent or a run of another member of the fleet carries `member`, the name of that member, and its access block and lineage come from the view (section 4.4). A listing of runs or agents sends all of its checks in one request, and so does each view of the dashboard. The control plane decides about a child run by itself, from the lineage in the check; after a refusal the front does not ask again for the ancestors.

The answer holds one decision per check, in order, with `allow`, an optional `reason`, and an optional `ttlMs`. `ttlMs` lets the front reuse a decision for that many milliseconds. Zero, the default, means no reuse, so a revocation takes effect at the next request. The cache key holds every fact of the check, so a change to an access block or to a run asks again.

### 12.3 When the control plane does not answer

If the control plane does not answer in time, the front refuses the request with `fw.ErrAuthzUnavailable`. An answer that it cannot read, or that holds the wrong number of decisions, counts as no answer. The native API answers 503 with `Retry-After: 5`, the MCP server returns an error result that says that the service did not answer, and a listing reports the outage instead of an empty list. A caller therefore tells a refusal, a 403, from a control plane that is down. A call from the process itself carries no principal and never asks, so the front keeps reading its sources during an outage. `fw-workbench doctor` prints one line for the control plane, `workbench.access.external`, and asks an MCP decision point whether it lists the tool.

## 13. Event sources

An event source turns the events of an outside system into runs, with no person in between. Examples of such a system are a ticket system, an alert manager, and a repository host. Without a source, a person or an agent calls `enqueue` for each piece of work. With a source, the front reacts to the events by itself, starts one run for each new event, and starts no second run for the same event, also with two replicas of the front. The package `sources` holds the sources. It is a service of the kernel (`fw.RegisterService`), so a process that serves has it, and a verb that runs one piece of work, such as `fw-workbench run`, does not.

### 13.1 How a source works

A source is an entry of the list `workbench.sources` in `fw.yaml`. There are two kinds:

- A webhook source takes events on `POST /v1/sources/<name>`. The outside system sends one event, a JSON object, or an array of events.
- A poll source reads an HTTP endpoint at an interval, for a system that sends no webhooks. The endpoint answers JSON to a GET, and a dotted path names the array of items in the answer.

For each event, the source renders its templates into a run request of its agent profile: the input, the title, the metadata, and the de-duplication key. The source claims the key (section 13.3). Then it puts the request on the intake queue of the profile, `<workbench.fleet.intake>.<agent>`, through `fleet.Enqueue`, the publish of the tool `enqueue` (section 4.9). After the publish, the front wakes the sleeping Tasks that serve the profile (section 4.12).

Each run request carries this metadata:

| Key | Value |
|---|---|
| `fw.batch` | The name of the source. The tool `batch`, and `runs_list` with `batch`, follow the runs of a source as one job. |
| `fw.title` | The title from the template `title`, when the source sets one |
| `workbench.source` | The name of the source |
| `workbench.sourceKey` | The de-duplication key of the event |
| `workbench.sourceGeneration` | With `redrive`, the generation of the claim of the event (section 13.9) |

The metadata of the configuration comes on top. The idempotency key of the request is `<source>/<key>`, so a worker that gets the same request twice returns the first run. A redrive has the key `/source-redrive/<generation>` instead (section 13.9).

A run of a source acts as `runAs`, which is `source:<name>` of the kind `workload` by default. In the rules mode, the admins and the owners, operators, and viewers of the profile see these runs (the kernel architecture, section 13.2). To let the runs act for a person at a service (section 11), set `runAs` to the subject of that person and `runAsKind` to `user`. That person must have connected the service.

### 13.2 Configuration

Each entry of `workbench.sources` takes these keys. No variable sets the list.

| Key | Meaning | Default |
|---|---|---|
| `name` | The name of the source: 1 to 63 lower-case letters, digits, `-`, or `_`. It names the route, the batch, and the bucket of the keys. | required |
| `agent` | The agent profile, and so the queue `<workbench.fleet.intake>.<agent>` | required |
| `auth.mode` | For a webhook: `bearer`, `hmac`, or `none` (section 13.4) | required for a webhook |
| `auth.secret` | The variable that holds the bearer token or the HMAC key | required for `bearer` and `hmac` |
| `auth.header` | The header that carries the HMAC signature | `X-Signature-256` |
| `poll.url` | The endpoint of a poll source, an absolute http or https URL without a user or a password. This key makes the entry a poll source. | none |
| `poll.every` | The time between two polls, 5 seconds or more | `1m` |
| `poll.timeout` | The limit of one poll | `30s` |
| `poll.secret` | The variable that holds the credential of the poll | none |
| `poll.header` | The header that carries the credential as it is. Without it, the credential goes as `Authorization: Bearer <credential>`. | none |
| `items` | A dotted path to the array of events in the body or in the answer, such as `result.tickets` | the body itself |
| `input` | The template of the input of the run | the event as indented JSON |
| `title` | The template of the title of the run, cut to 200 characters | no title |
| `metadata` | Templates of metadata, by key | none |
| `key` | The template of the de-duplication key, at most 512 bytes | required |
| `dedupFor` | How long a key blocks a second run, one minute or more | `168h` (7 days) |
| `redrive` | Lets an event whose job reached the dead letter run again (section 13.9). Without the key, a dead-lettered event stays a duplicate for `dedupFor`. | off |
| `redrive.maxAttempts` | The runs of one event that the source starts, the first one included, 2 to 10 | `3` |
| `redrive.cooldown` | The time from the dead letter to the next run, one minute or more and shorter than `dedupFor` | `5m` |
| `runAs` | The principal of the runs | `source:<name>` |
| `runAsKind` | The kind of that principal, `workload` or `user` | `workload` |

The sources need `workbench.fleet.intake` (section 4.6), and the bus `nats` for the keys (section 13.3). A configuration that cannot work stops the start. These are the cases:

- A webhook source has no `auth.mode`. A webhook without a credential must say `none`.
- The variable of `auth.secret` or `poll.secret` holds no secret.
- A template does not parse, or `key` is empty.
- A metadata key is one that the source sets, or one that the kernel reserves.
- `workbench.fleet.intake` is empty.
- The bus is neither `nats` nor `mem`.
- A source has `redrive`, and the bus is not `nats`, or `redrive.maxAttempts` or `redrive.cooldown` is out of its range.

A template is a Go `text/template` over one event. A field that the event does not have is an error, so `{{.id}}` refuses an event without `id`, and a key never comes out empty by accident. A template reads an optional field with `index`, such as `{{index . "assignee"}}`. Besides the built-in functions, a template can call `json` (a value as compact JSON, such as `{{json .}}` for the whole event), `lower`, `upper`, `trim`, and `default` (such as `{{index . "priority" | default "normal"}}`). A number keeps every digit of the body. A key must not be empty, and it must not hold a field whose value is null. The loader of `fw.yaml` expands `${NAME}` in every value first (the kernel architecture, section 12.1), so a template that needs the text `${` writes `$${`. A variable of a template, such as `$i`, needs no escape.

Choose a key that changes when the work changes. For a ticket, the id and the time of the last change, such as `{{.id}}/{{.updated}}`, make a new run for each change and one run for each change that arrives twice.

### 13.3 De-duplication

The keys live in NATS JetStream, so every replica of the front shares them, and a restart keeps them. Each source has one key-value bucket, `<stream>_SOURCE_<name>`, where `<stream>` is `bus.options.stream`, `FW` by default. The TTL of the bucket is `dedupFor`, so the server forgets a key when its window ends. At the first claim of a source after a start, the front makes the bucket when it is missing, and it sets the TTL of a bucket that exists to the value of the configuration. The front opens its own connection for the keys, with `bus.url` and the options `credentials` and `token` of the bus. With NATS accounts, allow the front `$JS.API.>` and `$KV.<stream>_SOURCE_*.>`.

The name of a key in the bucket is the SHA-256 of the de-duplication key, because a key can hold any character. The value holds the state of the claim, the key, the name of the replica, and the time. With `redrive`, it also holds the generation, the attempt, and the time of the failure (section 13.9).

A source handles each event in four steps:

1. It claims the key. A claim is a create-if-absent of the key with the state `pending`. The server decides, so of two replicas that claim one key at the same time, exactly one gets the claim.
2. It publishes the run request.
3. It marks the claim `done`. The key then blocks a second run for `dedupFor`.
4. If the publish fails, it releases the claim instead, so the next delivery of the event makes the run.

When a claim exists, the source does this:

- A `done` claim makes the event a duplicate. The event makes no run. With `redrive`, a claim whose job reached the dead letter can run again (section 13.9).
- A `pending` claim younger than two minutes means that another replica handles the event now. The event is busy: a webhook answers 503, and a poll meets the item again at its next poll.
- A `pending` claim older than two minutes means that its replica stopped in the middle. The next delivery takes the claim over, with a compare of the revision, so only one replica takes it.

When NATS does not answer, every claim fails at once. The source publishes nothing without a claim. A webhook answers 503 with `Retry-After: 5`, and a poll counts a failed poll and waits longer before the next one (section 13.5).

So the same key starts at most one run in its window, with one exception. A replica can publish and then lose NATS before it marks the claim done. The claim then stays pending, and after two minutes a later delivery publishes the request again, with the same idempotency key. A worker that holds the first run returns it, and another worker starts a second run. A second run therefore needs a failure of NATS in the short time between a publish and the end of its claim. The opposite failure loses no event: when a publish fails and the release fails too, the pending claim goes to the next delivery after two minutes.

With the bus `mem`, the process has no replica, because that bus reaches no other process. The keys then live in the memory of the process, a restart forgets them, and the log says so at start.

### 13.4 The webhook route

`POST /v1/sources/{name}` sits outside the authentication of the native API, because an outside system holds no token of a person. Each source checks its own credential:

- `bearer`: the request carries `Authorization: Bearer <token>`, and the token equals the secret of `auth.secret`. The source checks it before it reads the body.
- `hmac`: the header of `auth.header` holds the HMAC-SHA256 of the raw body, keyed with the secret of `auth.secret`, in hex or in base64, with an optional `sha256=` in front.
- `none`: the source takes every request. The log warns about such a source at start.

Every compare takes constant time. The body holds one event, an array of events, or, with `items`, a document that holds the array. One request takes at most 500 events and 1 MB. The source renders every event first. If one event does not fit the templates, no event of the request makes a run, and the detail of the 422 names that event.

| Status | When |
|---|---|
| 202 | At least one event made a run request. The body lists the requests. |
| 200 | Every event was a duplicate, or the body holds no event. The body has a `note`. |
| 400 | The body is not JSON, an event is not an object, the path of `items` finds nothing, or the body holds more than 500 events. |
| 401 | The credential is missing or wrong. A bearer source adds `WWW-Authenticate`. |
| 404 | No webhook source has that name. |
| 413 | The body is larger than 1 MB. |
| 422 | An event does not fit the templates. The detail names the event. |
| 503 | The store of the keys or the queue did not answer, or another replica handles an event now. The answer has `Retry-After: 5`. |

After a 503, the sender sends the same request again. Every event of it that already made a run counts as a duplicate then, so a retry is always safe. The errors are problem documents, as on the run routes. A 202 or a 200 has this body:

```json
{
  "source": "tickets",
  "agent": "triage",
  "queue": "fw.jobs.triage",
  "batch": "tickets",
  "accepted": 1,
  "duplicate": 1,
  "runs": [
    {"event": 2, "key": "T-1042/2026-10-19T10:05:00Z",
     "idempotencyKey": "tickets/T-1042/2026-10-19T10:05:00Z", "title": "Triage T-1042"}
  ],
  "duplicates": [{"event": 1, "key": "T-1042/2026-10-19T10:00:00Z"}]
}
```

`event` is the position of the event in the body, from 1. The worker names the run when it takes the request, so the answer holds no run id. To find the run of an event, list the runs of the batch with `runs_list` and read `workbench.sourceKey`.

### 13.5 Poll sources

Every replica of the front polls each poll source, and the claims keep the runs single. The first poll comes after a random pause of up to 5 seconds, and each pause gets a random tenth on top, so the replicas do not poll at the same moment.

A poll is a GET with `Accept: application/json` and the credential of `poll.secret`. An answer that is not 2xx, that is larger than 8 MB, or that is not JSON is a failed poll. After a failed poll, the source waits twice as long as before, up to 30 minutes, or up to `poll.every` when that is longer. A poll that works resets the pause. A poll follows a redirect only to the same scheme and host, so the credential never reaches another host. An item that does not fit the templates counts as refused, and the other items of the answer go on.

The endpoint must list the same item with the same key while the work is the same. An item that leaves the list and comes back after `dedupFor` makes a new run.

### 13.6 Counters

Each source counts what it did since the process started. `sources.Stats(a)` returns one `sources.Stat` per source, in the order of the configuration, or nil when the agent runs no source:

| Field | Meaning |
|---|---|
| `Source`, `Kind`, `Agent` | The name, `webhook` or `poll`, and the profile |
| `Accepted` | The events that made a run request |
| `Duplicate` | The events whose key made a run within the window |
| `Busy` | The events that another replica handled at that moment |
| `Refused` | The events that made no run for any other reason: a wrong credential, a body or a template that failed, a store or a queue that did not answer. A request that the source refuses before it reads its events counts as one event. |
| `LastError`, `LastErrorAt` | The last reason of a refusal or of a failed poll, in words that hold no secret, and its time |
| `LastAcceptedAt` | The time of the last run request |
| `LastPollAt` | The time of the last poll that read the endpoint |

Each replica counts its own events, so a sum over the replicas gives the counts of the fleet. A collector of metrics reads `Stats` to export them. `fw-workbench doctor` has a line that names each source, its kind, its agent, and the store of the keys. It fails when a variable of a secret is empty or when the bus keeps no keys.

### 13.7 Security

- The route takes no credential of the native API, so the credential of the source is the only check. Prefer `hmac`, which also proves that nobody changed the body. Limit who reaches `/v1/sources/` with the gateway, above all for a source with `auth.mode: none`, which starts runs for anyone who reaches it.
- A signature holds no time. A replayed request within the window is a duplicate, and after the window it makes a run again. A key with the time of the change keeps such a replay a duplicate.
- The content of an event becomes the input of a run, so an outside system can write instructions to the agent. Give the profile of a source only the tools that its work needs, and make the tools that change things ask first (the kernel architecture, section 7.5).
- No token, no HMAC key, and no credential of a poll enters a log line, an answer, or a counter. An error of a poll names the host of the endpoint and never its whole address, because a query can hold a key.
- The runs act as `runAs`. A source with `runAsKind: user` acts for that person at the services of section 11. Only an operator who can change `fw.yaml` sets it.

### 13.8 Example

This configuration has a webhook source for a ticket system and a poll source for an alert system without webhooks. The ticket system posts a body such as `{"id": "T-1042", "updated": "2026-10-19T10:05:00Z", "summary": "The printer is on fire", "requester": {"name": "Ada"}}` for each new or changed ticket, signed with a shared key. The directory `examples/sources` holds the same file.

```yaml
bus: { driver: nats, url: "nats://nats.agents.svc:4222" }
workbench:
  fleet:
    egress: fw.events
    intake: fw.jobs
  sources:
    - name: tickets
      agent: triage
      auth: { mode: hmac, secret: TICKETS_WEBHOOK_KEY, header: X-Signature-256 }
      input: |
        A ticket changed in the ticket system. Triage it.
        Ticket: {{.id}}
        Summary: {{.summary}}
        Requester: {{.requester.name}}
        Assignee: {{index . "assignee" | default "nobody"}}
      title: "Triage {{.id}}"
      metadata: { ticket: "{{.id}}" }
      key: "{{.id}}/{{.updated}}"
      redrive: { maxAttempts: 3, cooldown: 10m }
    - name: alerts
      agent: oncall
      poll:
        url: https://alerts.example.com/api/v1/alerts?state=firing
        every: 2m
        secret: ALERTS_API_TOKEN
      items: data.alerts
      input: "Look into the alert {{.name}} on {{.target}}. It fires since {{.startsAt}}."
      title: "Alert {{.name}} on {{.target}}"
      key: "{{.fingerprint}}/{{.startsAt}}"
      dedupFor: 24h
```

### 13.9 Redrive after a dead letter

A claim is done once its run request is on the queue, not once its run ends well. When the run of the job fails after its last attempt, the worker sends the job to the dead letter of the queue, `<queue>.dead` (the kernel architecture, section 9.3), and the key of the event still blocks a second run for `dedupFor`. Each later delivery of the event, such as a webhook that a sweeper sends again, is a duplicate. Without `redrive`, only a person brings such an event back: the person reads the record of the dead letter and starts the run again with `enqueue`.

With `redrive`, the dead letter gives the event another run. The source works as follows:

1. Each claim holds a generation, a random token, and an attempt, from 1. The run request carries the generation in `workbench.sourceGeneration`. A retry of the kernel intake (`bus.intakeRetries`) copies the metadata of the request, so every run of one job carries the same generation.
2. Every replica of the front reads `<queue>.dead` through one durable JetStream consumer of the source, `sources_redrive_<hash>`, which the replicas share. Each record therefore reaches one replica, and a restart goes on where the consumer stopped. The consumer is not the consumer of a worker intake, so it takes no job from a worker.
3. A record of a failed or rejected job marks the claim `failed` when it matches the claim: the source, the key, and the generation in the metadata of the request, the agent of the source, and `runAs` and `runAsKind`. The front acknowledges the record after it wrote the claim. A record of another source, of a run that a person queued, or of an older generation changes nothing. A second record of the same failure leaves the time of the failure as it was, so it never moves the cooldown. A record that does not read is skipped with a log line that names the source only. When NATS does not answer, the record comes back after a second.
4. After `redrive.cooldown`, the next delivery of the event takes the failed claim again, with a compare of the revision, so only one replica takes it. The claim gets a new generation and the next attempt, and the run request gets the idempotency key `/source-redrive/<generation>`, because a worker returns the failed run for the old key. The metadata `workbench.source` and `workbench.sourceKey` stay. The webhook checks its credential first, as for any delivery.
5. A delivery before the cooldown ends, and every delivery after the last attempt, is a duplicate.

The source never sends a job by itself: an event runs again only when the outside system delivers it again, or when the next poll finds it. A failure after a done claim, a dead letter that arrives before the claim is done, and a publish that fails all keep the generation and the attempt. A failed publish releases the claim, and the next delivery runs the same generation. A stale pending claim goes to the next delivery with its generation too. A key that ran well stays a duplicate.

The bound of the attempts lasts as long as the key. Each write of a claim starts the window of the key again, and the cooldown is shorter than `dedupFor`, so a key never expires during its cooldown. When no delivery comes for `dedupFor` after the last write, the server forgets the key, and the next delivery starts again at the first attempt.

`redrive` needs the bus `nats`. A claim from before `redrive`, or a record of the dead letter from before it, holds no generation and matches no record, so that event still needs a person and `enqueue`. A pending claim from before `redrive` goes to the next delivery after two minutes, as in section 13.3. When a source turns `redrive` off, its consumer stays on the NATS server, and an operator can remove it with `nats consumer rm`. The redrive does not read the state of the failed run. A person who continues a failed run of a source, with a follow-up or a new budget, can therefore meet a second run of the same event, as with `bus.intakeRetries` (the kernel architecture, section 9.3).

## 14. Pools

A pool keeps a set of ax Tasks that run one agent profile. The block `workbench.pools` of `fw.yaml` lists the pools. Each pool names the image, the fw configuration of the workers, their variables, their egress, and the number of Tasks. At every interval, the front makes the Tasks of each pool match the configuration. A person no longer renders and applies the manifests of the workers by hand. The package `pools` reads the configuration, renders the Workspace and the Task of each worker, and runs the reconciler. The fleet runs it after each read of the Task list (section 4.1).

### 14.1 Configuration

Each entry of `workbench.pools` has these keys. A key that a pool does not know is an error.

| Key | Meaning | Default |
|---|---|---|
| `name` | The name of the pool. Its Tasks are `<name>-<index>`, and their Workspaces are `<name>-<index>-ws`. | required |
| `atespace` | The atespace of the Tasks. It must be one of `workbench.fleet.atespaces`. | the only atespace of the fleet |
| `replicas` | The number of Tasks. With `0`, the front deletes every Task of the pool once the Task is idle. A Task that failed after it ran waits for a person (section 14.4). | required |
| `image` | The worker image by digest, `<image>@sha256:<64 hex digits>`. Substrate pulls an actor image only by digest. | required |
| `agent` | The profile of `fw.agents` whose queue the workers take. | the only profile of `fw.agents` |
| `intake` | The intake queue of the workers. | `<workbench.fleet.intake>.<agent>`, else `fw.jobs.<agent>` |
| `revision` | Free text. A new value replaces every Task, for example to start every worker again with a new Workspace. | none |
| `generation` | A number of 0 or more that orders the configurations of the pool (section 14.5). A new value replaces every Task. With more than one replica of the front, raise it with every change of the pool. | `0`, no order |
| `env` | The variables of the workers. Each entry has `name` and `value`, or `name` and `fromEnv`, the name of a variable of the front. | none |
| `egress` | `spec.egress` of each Task: `rules`, each with one of `http`, `https`, and `tls`, which hold `hostnames` and `ports`. ax creates the egress policy of the actor from it before the Task first runs. | required |
| `resources` | `requests` and `limits` of each Task, each with `cpu` and `memory`. | none |
| `files` | Extra files of the Workspace, each with a `path` relative to `/workspace` and its `content`. | none |
| `fw` | The `fw.yaml` of each worker: the agents and their profiles, the policy, the model, the providers, and the tools. | required |

The front writes `fw` to `.fw/fw.yaml` in the Workspace of each Task. The Workspace is bound at `/workspace`, where the kernel reads its configuration under ax. The front adds these keys:

- `bus.intake` from `intake`, `bus.egress` from `workbench.fleet.egress`, and `bus.egressTypes` with the event types that the view reads (section 4.1). A pool cannot set them in `fw`.
- The defaults that `fw` can change: `bus.driver` and `bus.url` of the front, `auth.mode: token`, and `ax.server` and `ax.router` of the front. A worker in a Substrate sandbox reaches NATS over WebSocket, so a pool usually sets its own `bus.url`. The front never copies a bus URL that holds a credential.
- On NATS, the default `bus.options.concurrency: 1`. A Task runs one run at a time. The NATS intake takes four messages at a time by default, so one worker can hold four jobs while the other workers idle.

The front adds these variables to each Task:

- `FW_POOL`, the name of the pool.
- `FW_POOL_SPEC`, the hash of the configuration of the pool.
- `FW_POOL_GENERATION`, the generation of the pool, when it is above 0, and beside it `FW_POOL_BASE`, the hash of the configuration of the pool without the overlay of the front (section 14.9).
- `FW_BASH_HIDE`, which keeps `AX_TASK_YAML`, the token variable, and each variable with `fromEnv` out of the bash tool of the worker. `AX_TASK_YAML` holds every variable of the Task.
- The static token of the front, under `auth.tokenEnv` of the worker, which is `FW_API_TOKEN` by default (section 14.6).

The policy of the workers is `fw.policy`, the policy of the Task, in the open. A profile can only tighten it. The front adds the profiles and the allow rules of the overlay of the pool to `fw` (section 14.9).

The front expands `${NAME}` in its own `fw.yaml` before it reads the pools, in `workbench.pools` too. The worker expands its own file again. For a variable that the worker expands, write `$${NAME}`. Never write a secret as `${NAME}`. The front then puts the value into the configuration, and the dry run prints it. Use `fromEnv` for a secret.

The front checks every pool at its start, and a problem stops the start. The error names each key and what to set, for example:

- `workbench.pools[0] (triage): image "ghcr.io/example/fw:latest" has no digest: Substrate pulls an actor image only by digest, so write it as <image>@sha256:<64 hex digits>`
- `workbench.pools[0] (triage): set fw.model, the provider/model selector of the workers, such as openai/<model>: a pool has no default model`
- `workbench.pools[0] (triage): replicas -1 is below 0; set 0 or more`

The checks also cover these points:

- `fw` decodes as a configuration of the kernel, and the checks of the kernel pass on it.
- The Task and the Workspace pass the checks of the ax API, such as the names and the egress hostnames.
- `env` sets no variable `FW_*` and no variable `AX_*`, and not the token variable of the worker. A variable `FW_*` changes the configuration of the worker after these checks, so a key of `fw` takes its place.
- Each egress rule sets exactly one of `http`, `https`, and `tls`.
- `fw.auth.mode` includes `token`, and `fw.bus.driver` is the bus of the front.
- `fw.bus.url` holds no credential as text. Write `$${NAME}` for it, and set `NAME` in `env` with `fromEnv`.
- The static token sees every run of a worker under the access rules of `fw` (section 14.4). Else the error says to add `kind:token` to `fw.access.admins`.
- The front has `workbench.fleet.atespaces`, `workbench.fleet.egress`, a bus other than `mem`, `ax.server`, and `ax.router`.
- The front has every variable that a `fromEnv` names.

`fw-workbench doctor` reports these problems on its fleet line. The front also logs a warning for each address of a worker that no egress rule allows. It looks at the bus, the providers, the MCP servers, and the token broker.

The directory `examples/pools` holds a front with one pool. The pool has three workers of the profile `triage` on the queue `fw.jobs.triage`. They use a model behind a proxy and reach the bus, the model proxy, and an MCP server. One tool writes to the ticket system, and it asks a person first. The pool, in short:

```yaml
workbench:
  fleet: {atespaces: [default], egress: fw.events, intake: fw.jobs}
  pools:
    - name: triage
      replicas: 3
      image: ghcr.io/bdubs00/featherweight-kernel@sha256:<64 hex digits>
      env:
        - {name: OPENAI_API_KEY, value: the-proxy-holds-the-key}
        - {name: TICKETS_TOKEN, fromEnv: TICKETS_TOKEN}
      egress:
        rules:
          - http: {hostnames: [nats.agents.svc.cluster.local], ports: [8080]}
          - http: {hostnames: [model-proxy.models.svc.cluster.local], ports: [8080]}
          - https: {hostnames: [tickets-mcp.example.com]}
      fw:
        model: openai/example-model
        bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
        providers:
          openai: {baseUrl: "http://model-proxy.models.svc.cluster.local:8080/v1", apiKeyEnv: OPENAI_API_KEY}
        tools:
          builtin: [files, clock]
          mcp:
            - {name: tickets, endpoint: "https://tickets-mcp.example.com/mcp", auth: {bearer: TICKETS_TOKEN}}
        policy: {default: allow, ask: [tickets.update_ticket]}
        agents:
          triage:
            description: Reads new tickets and sorts each one into a queue.
            tools: [files, tickets.search_tickets, tickets.get_ticket, tickets.update_ticket]
```

### 14.2 The dry run

`fw-workbench pools render [-config file] [file]` prints the Workspaces and the Tasks that the front applies. The output is a stream of YAML documents in the order of the apply. A comment before each pool names its queue, its hash, the variables that the front copies, and its warnings. The command reads the overlay of each pool from the store of the bus and merges it as a pass does (section 14.9), and the comment names each entry. When the store does not answer within five seconds, the command says so and prints the pool without its overlay. Each value that the front copies from its environment is a placeholder, such as `<secret: FW_API_TOKEN of the front>`. So the output holds no secret, and the command needs none of the variables. The hash covers the name of each variable of the front and never its value. When the command reads the configuration with the variables of the front, the hash in the output is the hash that the front writes. A person reviews a change with the output before the front acts.

The output is for review only. Do not apply it: a placeholder is not a value. A Task that holds a placeholder runs an older value of a variable of the front (section 14.4), so the front replaces it.

### 14.3 Ownership

The ax API keeps no labels and no annotations on a Task. It keeps the name and the specification. So the front marks a Task of a pool with its name and its specification. A Task belongs to the pool `P` on two conditions:

- Its name is `P-<index>`, with a decimal index without a leading zero.
- Its variable `FW_POOL` is `P`.

The front never touches another Task. It needs both marks:

- A Task named `triage-1` without `FW_POOL=triage` belongs to no pool, such as a Task that a person made. The pool leaves it alone, says so in its state, and takes the next free name.
- A child Task of a worker comes from `spawn_task` with the ax driver. It inherits `FW_POOL` from its parent, but its name is `<parent>-<run id>`. So it belongs to no pool either.

A Workspace has no variables, so the front marks it in its file: the first line of the rendered `fw.yaml` names the pool. The front writes and deletes only a Workspace with that line, or a Workspace that does not exist. A Workspace of that name without the line keeps its content, and the pool takes the next free name.

A pool with a generation above 0 has one more Workspace, its marker `<name>-generation`. Its file `.fw/generation` names the pool in its first line and holds the newest generation of the pool that a front runs (section 14.5). The front writes the marker only when its own generation is higher, and never a Workspace of that name without the line. No Task binds the marker.

A pool that leaves the configuration, or that moves to another atespace, leaves its Tasks as they are. The front cannot tell a removed pool from a pool of another front. To remove a pool, set `replicas: 0`, wait until its Tasks are gone, and then remove the entry. Delete a Task that failed after it ran by hand, and the marker of the pool too (`ax delete workspace <name>-generation`).

### 14.4 A pass

At every `workbench.fleet.interval`, the front reads the Task list and runs the suspend controller. Then it runs one pass for each pool over the Tasks of its atespace. A current Task of the pool carries the hash of the pool in `FW_POOL_SPEC`. It also holds the values that the front holds now for its variables. So a new value of a variable of the front, such as a new static token, makes every Task older too. The front compares the values in constant time and never logs them. A Task of a lower generation than the pool is older too.

First, a pass compares generations (section 14.5). When the marker or a Task of the pool carries a higher generation than the pool of this front, or a Task of the same generation carries another hash, the pass changes nothing, and the state of the pool says why. Else a pool with a generation above 0 records it in the marker, if the marker holds a lower one. Then a pass has these steps:

1. It sends the delete again for a Task of the pool in the phase `Terminating`. Substrate can refuse a delete while an actor resumes, and nothing else tries again. When the view holds a run of such a Task, the front asks the Task, and a Task that holds the run waits.
2. It puts aside a Task that failed after it ran. Such a Task can hold runs that nobody can read, so the pool never deletes it, and the Task does not count toward `replicas`. The state of the pool asks a person to delete it. It also names a run of the Task that the view still holds, because the actor can still work.
3. It deletes the Tasks above `replicas`, the idle ones first, and the Workspace of each such Task. The order is in the list after these steps.
4. It resumes a current Task that never ran, and a Task that is `Pending`. ax creates every Task suspended, and a resume can fail for a moment. ax sets the condition `WorkspaceReady` at the first resume of a Task, so a Task without it never ran through ax.
5. It creates the missing Tasks at the lowest free names. For each Task, it writes the Workspace, creates the Task, and resumes it.
6. It replaces the Tasks that are not current. It deletes such a Task, and the next pass creates it again with the current configuration. A Task that is not up serves nobody, so the pool replaces it at once. Up means running, or asleep after a first run. Of the Tasks that are up, the pool replaces one in each pass. It waits until every Task of the pool is up. So the pool keeps its workers while it rolls.

A failed Task of the current configuration that never ran holds no run. The pool deletes it `FailedRetry`, five minutes, after the failure, and the next pass creates it again. ax sets the phase `Failed` only for an error that does not go away by itself, so the pool tries again seldom. In between, the state of the pool gives the reason that ax gives.

For a delete above the replica count, the front prefers these Tasks, in this order:

- A Task that never ran.
- A Task of which the view knows no active run.
- A Task of an older configuration.
- A running Task, because its status read wakes nobody.
- The Task with the highest index.

An active run is a run that is not finished, a run that waits for input included. The front deletes a Task only without an active run:

- A Task that never ran, and of which the view knows no active run, holds none.
- Any other Task must answer that it holds none, in `runs.active` of `GET /v1/status` through the router. The front sends the static token that the Task holds, so a Task that holds an older token answers too.
- The Task also answers for each run that the view holds, with `GET /v1/runs/{id}`. A run that the Task does not have, or that ended, is a run that the view holds after a lost event. A run that the Task hides from the token keeps the Task.
- After these answers, the front asks the view once more before the delete. A run that the view learned in between keeps the Task.
- A running Task can have the condition `WorkspaceReady` at `False`, because its kernel never answered at a resume. When it does not answer now either, and the view knows no run of it, it counts as a Task that never ran. So a pool replaces the Tasks of a broken image after a fix.

The ledger of the Task decides. Its status counts a run that the view missed. An example is a run that waits for an approval in a Task that slept since before the front started. When the view still holds a run after a lost event, the Task shows that the run ended, and the front logs the difference. A read of a sleeping Task wakes it, and the front logs that too. The front never deletes a Task that does not answer, or that neither runs nor sleeps. The state of the pool says why, and a person decides. The status counts only the runs that the caller can view. So the static token must see every run of a worker (section 14.1).

The state of the pool says why a Task waits, for example `triage-1 runs an older configuration and holds the active run 01K..., so the pool replaces it once it is idle`. The log of the front has a line for each new reason, and a line for each Task that the front creates, resumes, or deletes.

### 14.5 No leader

No replica of the front leads. Every replica runs every pass, and every action is safe to repeat. So two replicas give the result of one:

- A name comes from the pool and the index, so two creates of one Task make one Task. Before a create, the front reads the Task, and it writes no Workspace for a Task that exists. ax refuses a second create, and the front takes the refusal as the work of the other replica.
- A resume, a write of a Workspace with the same content, and a delete of a Task that is gone change nothing.
- Before a delete, the front reads the Task again. It deletes only the Task that the pass judged: the same `FW_POOL`, the same `FW_POOL_SPEC` and `FW_POOL_GENERATION`, the same create time, and for a delete that it sends again, still the phase `Terminating`. So a replica with an older list never deletes a Task that the other replica made since. The front deletes a Workspace only while no Task of its name exists.
- Two replicas can choose different idle Tasks above the replica count. Then the pool has one Task too few for one interval, and the next pass creates it.

A leader needs a lease, and the front has none: the ax API offers no lock, and the bus of the fleet can be NATS or Redis. A rolling update of the front can run two configurations at once, for example when a deploy changes the configuration and one replica restarts before the other. A hash says that two configurations differ, not which one is newer. So without an order, each replica takes the Tasks of the other for an older configuration, and the two replace each other's Tasks at every interval for as long as both run. The ticket soak of the lab saw this (finding 6 of `e2e/fleet/tickets/FINDINGS.md` in featherweight-lab).

The generation gives the order. A deploy raises `generation` with every change of a pool, and the front stamps it on each Task in `FW_POOL_GENERATION`:

- A Task of a lower generation runs an older configuration, and the pool replaces it as section 14.4 says.
- A replica that sees a Task of a higher generation is superseded. It changes nothing in the pool: no create, no resume, no delete, and no write of a Workspace. It keeps the highest generation that it saw until it stops. Its state says `superseded: generation 7 of another front runs this pool, and this front holds generation 6, so it changes nothing in the pool`.
- A Task of the same generation with another configuration is a conflict. Two configurations claim one generation, and no replica can tell which one is newer. Neither replica changes the pool until a deploy gives one of them a higher generation. A rollback is a new, higher generation too. A generation that goes down, to 0 too, leaves every replica superseded by the marker and the Tasks of the higher one, until a person deletes them.
- A newer replica deletes a Task before it creates the next one, and an older replica would fill that gap with a Task of its own. So a replica of a generation above 0 first records its generation in the marker of the pool (section 14.3), before it changes a Task, and every replica reads the marker at each pass and again before it creates a Task. An older replica therefore stops before the first gap opens, and the pool converges on the newer configuration. A pool of a generation above 0 that cannot read or write its marker changes nothing.
- The overlay of the front (section 14.9) changes the hash of the pool and not its generation. Every replica reads the overlay from one store, so the generation does not order it: a Task whose `FW_POOL_BASE` matches the pool, with another `FW_POOL_SPEC`, runs an older overlay, and the pool replaces it. A conflict compares `FW_POOL_BASE`, or `FW_POOL_SPEC` for a Task without it.
- With generation 0 on both replicas, the hash alone decides, as before pools had a generation, and two configurations still fight.

The front logs each new state of the order once.

### 14.6 The token

The workers need the static token of the fleet. The front reads a worker with it, and a worker accepts it. The front copies the token from its own environment into each Task. It reads the token under `auth.tokenEnv` of the front and writes it under `auth.tokenEnv` of the worker. Both are `FW_API_TOKEN` by default. The configuration file never holds the token, and the front refuses a pool that sets it in `env`. A variable with `fromEnv` takes the same path.

The front reads these variables once at its start and keeps them in memory. It writes them into the Tasks through the ax API and nowhere else. No log line, no file, no state of a pool, and no output of the dry run holds one. An error of the ax API can echo such a value. Then the front replaces the value before it logs or shows the error. It replaces each value of eight characters or more, also in the quoted forms of Go and JSON. So the static token must have eight characters or more, or the front does not start.

To rotate the token, change the Secret of the front and restart the front. Every Task then holds an older value, and the pool replaces the Tasks one at a time, each once it is idle. The front reads each old Task with the token that the Task holds.

### 14.7 Visibility

`GET /v1/fleet` carries `pools`, and the MCP tool `fleet` lists them after the members. Each pool has these fields:

- `name`, `atespace`, `agent`, `intake`, `hash`, and `generation`, when it is above 0.
- `wanted`, the replica count.
- `actual`, the Tasks of the pool that exist, are not being deleted, and did not fail after a run.
- `ready`, the Tasks of those that run the current configuration and are up.
- `tasks`, each Task with its phase and a note.
- `gaps`, one reason for each difference.

The Agents place of the page shows one line for each pool. A caller who can view the agent of a pool sees the pool. The front decides as for the agent of a member (section 4.4). It uses the access block of its own profile of that name, else the access block in `fw`.

### 14.8 Known limits

- At this version, ax keeps the variables of a Task as plain text in its store, because it has no references to Secrets. Whoever can read the Tasks on the ax API reads the static token and every value of `fromEnv`. Limit who can read the ax API.
- Every worker shares the token of the front until each member has a token of its own. A worker can reach the front and every other worker with it.
- A worker can take a job from the queue in the moment between the last question to the view and the delete. The intake acknowledges a job once its run is queued in the ledger of the worker, so the bus does not deliver the job again, and the run goes with the Task. The window is the time between the last question and the delete. Until the kernel can stop the intake of a worker before a delete, scale a pool down when its queue is empty.
- The bash tool of a worker gets each variable of `env` with a `value`. A variable with `fromEnv` stays out of it. To give a command in bash such a secret, name it in `fw.tools.bash.env` under another name, as `$${NAME}`.
- A Workspace of a Task that the pool deleted to replace it stays until the pool creates the Task again. When the delete of a Workspace above the replica count fails, the Workspace stays. Such a Workspace is small and holds no secret.
- The generation orders the configuration of a pool, not the values of the variables of the front. Two replicas that hold two values of a variable of the front, such as a static token during a rotation, still replace each other's Tasks until both hold the same value.
- The generation stops a lasting fight, not every race. The ax API has no conditional delete and no conditional write of a Workspace, so it cannot refuse a call of an older replica. A delete that an older replica sent before it learned of the newer generation can still land on a Task of the newer one, and a pass that started before the marker changed can create one older Task. The newer replica replaces such a Task at its next pass. Strict protection needs a fence in the ax API.
- A replica of an older version of fw-workbench knows no generation. Upgrade every replica of the front before the configuration sets one.
- A pass runs in the loop of the Task list, one call after another. A slow delete waits for Substrate, and it can hold the next read of the Task list back for up to two minutes.
- A change of the overlay reaches a worker when the pool replaces its Task, so a worker that holds a run keeps the old profiles and rules until the run ends (section 14.9).
- The page offers no choice of a pool. A new agent of the builder needs `pool` through MCP, or `?pool=` on `PUT /v1/agents/{name}`.

### 14.9 Profiles and rules of a pool

The front owns the profiles that the workers of its pools run. A worker reads its profiles from the fw.yaml that the pool renders. Without the front, `agent_put` writes a profile into one Task: it reaches one worker only, and the next replacement of the Task removes it. So the front keeps an overlay for each pool. The overlay holds profiles, which are added to `fw.agents` or replace the profile of the same name, and allow rules, which are added to `fw.policy.allow`.

The store. On a NATS bus, the overlay of a pool lives in the key-value bucket `<stream>_POOL_<pool>`, where `<stream>` is `bus.options.stream`, `FW` by default, so every replica of the front reads one truth. Each profile is the entry `profile.<name>`, and each rule is the entry `allow.<rule>`, with the name in base64url. A write of an entry replaces it, so two replicas never race on a list. The bucket keeps the last ten values of each entry. The first write makes the bucket. The front opens its own connection with `bus.url` and the options `credentials` and `token` of the bus. With NATS accounts, allow the front `$JS.API.>` and `$KV.<stream>_POOL_*.>`. On the bus `mem`, the overlay lives in memory. On another bus, the pools run their configuration alone, and a write to a profile of a pool is refused.

The merge. At each pass, the front reads the overlay of the pool and merges it into `fw` before it renders the Workspace. The hash of the pool covers the merged fw.yaml, so a change of the overlay makes every Task of the pool older, and the pool replaces each Task once it is idle (section 14.4). The front needs no restart. When the store does not answer, the pool waits and says why, because a pass without the overlay takes its profiles and its rules away from every Task. An ask wins over an allow in one policy (the kernel architecture, section 7.5), so an allow rule also removes the entries of `fw.policy.ask` that name the same tool. An ask by a glob, a flag, or a pattern stays. Each text of an overlay profile is escaped, so a profile never reads a variable of the worker. A prompt or an instruction of one line gets a line break at its end, because the worker reads such a text as the path of a file when that file exists.

Profiles. On a front with pools, `agent_put`, `agent_delete`, and `PUT` and `DELETE /v1/agents/{name}`, which the builder uses to save, write to the overlay of the pool that runs the profile: the pool whose `agent` is the profile, else the pool whose profiles hold the name. The optional argument `pool`, or `?pool=` on the routes, names the pool when several pools run the profile. It also adds a new profile to a pool, for `spawn_task` or for a run on a member. A pool takes the jobs of its agent from its own queue, so a new agent needs an entry in `workbench.pools`, and a new name without `pool` is an error that names the pools. A profile that only the front holds stays in the store of the front. The checks of section 10 apply: the owner role to change or remove a profile, the creator role to add one, and the same access rules. The front decides with its own access configuration and the access block of the profile in the pool, as it decides for a member (section 4.4). The dry run checks the merged configuration of the pool as the start of the front does, the models of the workers included. `agent_delete` removes an entry of the overlay. A profile of the configuration of the pool stays.

`agents_list` and `agent_get` show the profiles that the workers of each pool run. Each has `pool`, `overlay` (`added`, `replaced`, or empty), `by` and `at` for an entry of the overlay, and `workerPolicy`, the policy of the workers with the rules of the overlay. A profile of the front carries `front`. The tools of the workers live in their Tasks, so a profile of a pool carries no abilities. The text of `agent_get` words the policy of the workers instead.

Rules. Always allow on the approval card and `approve` with `rule` turn a pending approval of a worker into an allow rule of its pool. The rule is the name of the tool as the policy names it, such as `github.create_pull_request`, and for `bash` the whole tool, without a pattern. The pool is the pool whose Task runs the run. The caller needs the owner role on the profile of the run in that pool. The entry holds the agent, the run, the subject of the caller, the time, and the note, so a person sees why the rule exists, and the dry run names it. Before it writes anything, the front refuses a rule in these cases: the ask comes from a flag, such as `destructive`; the reason is no rule, such as a rerun after a resume; the policy of the profile asks too, which a rule of the pool cannot loosen; or the workers ask a control plane (`fw.policy.external`). The rule applies to every profile that the workers of the pool run. `POST /v1/runs/{id}/approvals/{approval}/rule` writes the rule for the page and decides nothing; the page allows the approval next. It answers 409 when no pool runs the run.
