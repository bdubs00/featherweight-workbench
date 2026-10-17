# Featherweight Workbench architecture

## 1. Summary

Featherweight Workbench is the workbench for people on top of the Featherweight Kernel. Its binary, `fw-workbench`, is the kernel binary `fw` plus the parts that people use. These parts are the page at `/ui`, the browser sign-in, the routes for people, the MCP server, the fleet, and the agent builder. The module is `github.com/bdubs00/featherweight-workbench`.

The kernel is a separate repository, featherweight-kernel. Its document `docs/ARCHITECTURE.md` describes the agent runtime for one google/ax Task: the ledger, the turn loop, the models, the tools, the native API for runs, and ax. This document calls it the kernel architecture. This document describes only what the workbench adds. In both documents, `fw` names the agent. Here, `fw-workbench` names the binary.

### 1.1 How the workbench builds on the kernel

The workbench changes no kernel code. It plugs in through the extension points of the kernel:

- `native.Extend` adds routes to the native surface of the kernel, on the same listener and behind the same authentication. The package `surface/web` registers one extension in `init`. Its routes get a `*native.Kit`, so they answer errors and forward the runs of other fleet members as the run routes do.
- `fw.RegisterDefaults` changes the default configuration in `init`. The package `surface/mcp` adds `mcp` to `surfaces`, and the package `tools/builder` adds `propose_agent` to the built-in tools. A file or a variable still overrides each default.
- The package `cli` of the kernel holds the command line. `cmd/fw-workbench` calls `cli.Run`, so `fw-workbench` takes every verb of `fw`. It sets `cli.ServeMCPStdio`, so the verb `mcp` serves the MCP server over stdio.
- `fw.RegisterExtension` claims a top-level block of `fw.yaml` for the workbench. The package `teams` claims the block `workbench`, and `fw.Config.Extension` decodes it (section 8).
- `fw.RegisterRunAnnotator` adds metadata to every run that the kernel queues. The package `teams` records the teams of the person who starts a run this way (section 8).
- `fw.Agent.SetProfiles` replaces the profiles of the kernel at runtime, and `cli.AgentsPut` and `cli.AgentsDelete` give the verb `agents` its writes. The package `agents` uses them (section 10).
- `fw.RegisterService` runs work beside the agent for as long as it lives, `Kit.SetRelay` of the native surface answers a request about a run of another process, `metrics.RegisterCollector` adds series to the metrics surface, and `cli.RegisterDoctorCheck` adds a line to `fw doctor`. The package `fleet` uses all four (section 4).
- `fw.Agent.SetPersonTokens` gives the tool sources of the kernel the access tokens of people, and `fw.Agent.Connected` resumes the calls that wait for a connection. The package `connections` uses both (section 11).

Each driver of the workbench registers itself in `init` through `fw.Register*`, as the kernel drivers do. `authn/oidc` registers the mode `oidc`, `fleet` registers the fleet driver, and `surface/mcp` registers the surface `mcp`. A blank import in `cmd/fw-workbench/drivers.go` links each one, beside every driver of the kernel binary.

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
fleet/                  the fleet: the member record, the view, and forwarding (section 4)
authn/oidc/             the oidc authentication driver (the kernel architecture, section 13.1)
tools/builder/          the tool propose_agent (section 6.4)
examples/agentgateway/  a fleet behind agentgateway on one public URL
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

The start is the start of `POST /v1/runs`, so the principal, the origin `http`, and `Idempotency-Key` apply. The answer is 201 with the run object and a `Location` header. The builder runs as the default agent with an inline profile, so the caller needs the operator role on the default agent. A `fromRun` of another fleet member sends the whole request to that member. The builder then runs there, and the run object carries `member`.

### 2.4 Deprecated usage

`GET /v1/usage` still answers with the same body, but it is deprecated. The answer carries `Deprecation: true` and a `Link` header with `rel="deprecation"` that points to section 10 of the kernel architecture, at the anchor `#observability`. The MCP tool `usage` is deprecated too. So are the Go view `AgentView.Usage` of the kernel package `surface/view` and the views `Report` and `Usages` of `dashboard/`. The dashboard shows no usage. Operators read the Prometheus metrics of `/metrics` instead.

## 3. MCP server: work intake

The MCP server makes `fw` a tool for any MCP client, for example Claude Code, an IDE, or another agent. It is a work intake equal to the native API, A2A, and the bus. The transport is Streamable HTTP at `/mcp`, with POST only, as the 2026-07-28 revision requires. The command `fw-workbench mcp` serves the same server over stdio for desktop clients. The server is dual-era. A request that carries modern `_meta` is served without a session, and an `initialize` request selects the legacy 2025-11-25 behavior with `Mcp-Session-Id`.

### 3.1 What the server offers

- `server/discover` returns `supportedVersions`, `capabilities`, and protocol instructions with no profile names or prompt text. The capabilities include `tools` with `listChanged`, `resources`, and `prompts`. The Tasks extension waits for SDK support.
- Tools: `run`, `run_status`, `approve`, `cancel`, `send`, the management tools of section 6.1, and every tool listed under `mcp.expose`.
- Resources: `fw://runs/{id}` (the run object), `fw://runs/{id}/ledger` (JSONL), the template `fw://runs/{id}/files/{+path}` (a file that the run changed, see the kernel architecture, section 7.9), and `fw://skills/{name}`. The three run resources read the runs of this process only. For a run of another fleet member, use the tools `run_status`, `run_events`, `run_result`, and `run_file`, which reach that member (section 4.5).
- Prompts: one per agent profile that the caller can view. Lists apply access rules before pagination, and reads check access again. Both use the current profiles, so a create, edit, deletion, or access change takes effect on the next request. Prompt lists are private with a zero cache lifetime. The server does not advertise prompt list-change notifications.

The `run` tool takes `input`, `title`, `agent`, `instructions`, `profile`, `wait` (default `true`), `budget`, and `requestId`. `title` names the work, as on the native API (the kernel architecture, section 8.2). `instructions` and `profile` have the shape of the native API fields. An MCP budget accepts `deadline` as a duration string, such as `30m`, or as positive integer nanoseconds, the form that profile reads return. This applies to `agent_put` and to both budget fields of `run`. `requestId` is the idempotency key. A retried `run` with the same `requestId` returns the same run instead of starting a second one. `metadata` labels the run, as it does on the native API, and the run object returns it. `run_status` is marked `readOnlyHint`. `run` and `send` are marked `destructiveHint`, because they start work that acts on the world.

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

A fleet is a set of `fw` processes that share one bus and one fleet subject. Each process in the fleet is a member. A member publishes a record of itself and of its runs on the bus. Every member reads the records of the others. Any member therefore serves one view of the whole fleet through its native API, its MCP server, and its dashboard. No member leads the others, and no election takes place.

A fleet is not a set of replicas. Replicas of one agent share one ledger and serve the same runs (section 3.6). Each member of a fleet keeps its own ledger. A member knows the runs of another member from the record only, and it acts on such a run through the member that owns it.

### 4.1 The member record

Each member has a name and a URL. Under ax, the name is `<atespace>/<task>` and the URL is `ax:<atespace>/<task>`. Elsewhere, the name is `<host>:<port>`, and the URL is empty until `workbench.fleet.url` sets it. A member without a URL is read-only to the other members. The name must be unique in the fleet.

The record of a member holds these facts:

- The name, the URL, and the interval.
- The authorization mode: `rules`, `external` for `access.external`, or `custom` for `fw.WithAuthz`. In `rules` mode, the record also holds `access.default` and `access.admins`.
- Each agent profile with its description and its `access` block.
- Every run that is not finished, and the 50 newest finished runs, as run objects.

A process that configures no agent profile offers no agent to the fleet. A front that only serves the view is such a process, so a person never sees it as a member.

The run objects in a record are short. The input, the output, and the error of a run are cut to 2 KB. If the input of a pending tool call is larger than 4 KB, the record leaves it out. A record is at most 512 KB, and the oldest finished runs are left out first. When a person opens a run of another member, the view reads the full run from its owner.

A member publishes its record to `<subject>.members.<token>`. The token is the name with each character other than a letter, a digit, `-`, or `_` changed to `_`. One second after an event that changes a run object, the member publishes again. Such events are a new run, an approval request, a decision, the end of a turn, and the end of a run. Every interval, the member builds a fresh record and compares it with the last one. If the two differ, for example after a profile change, it publishes the record. Otherwise it publishes a heartbeat to `<subject>.alive.<token>`. When the process stops on `SIGTERM`, it publishes a last record with `stopped: true`.

### 4.2 Retained subjects

The bus keeps the last record of each member. A member that starts later therefore sees the whole fleet at once. Under ax, this matters, because a Task that sleeps publishes nothing, but its last record still shows its runs. A bus driver that keeps events in this way implements `fw.Retainer` (the kernel architecture, section 9.1). The `nats` driver keeps them in the key-value bucket `FW_RETAINED`. The `redis` driver keeps them in the hash `fw:retained`, and the `mem` driver keeps them in memory. On a bus without `Retainer`, the fleet uses plain publish and subscribe. A member then publishes its full record at every interval instead of a heartbeat.

### 4.3 The view

A member starts its view at the first request that needs the fleet. The view watches both subjects and reads the retained records. A member that never serves such a request spends no work on the view. A scrape of `/metrics` is such a request, because the fleet gauges read the view (the kernel architecture, section 10.1). The first scrape therefore starts the view, and it can wait up to 2 s for the retained records. The view gives each member one of three states. A member that published within three of its intervals is `live`. A member that did not is `quiet`, and a member that sent a last record is `stopped`. Under ax, a quiet member is usually a Task that sleeps. The view ignores a record that is older than `workbench.fleet.expire`.

### 4.4 Who sees what

The view decides as the owner decides, from the facts in the record:

- For a member in `rules` mode, the view applies the access rules (the kernel architecture, section 13.2). It applies them to the published `access.default`, `access.admins`, and agent access blocks. Whoever started a run sees that run. Whoever sees a run sees its children.
- For a member in `external` or `custom` mode, the view asks the `Authz` of its own process. The target names the member, and a request to `access.external` carries `member` in each check (the kernel architecture, section 13.5). The default rules refuse a target in another member. A view without a decision point therefore shows no run of such a member. The view has no local fallback.

A record can be one interval old. When an owner removes a viewer, that viewer can see the short run objects in other views for up to one interval more. Every action goes to the owner, and the owner decides with its current rules.

### 4.5 Acting on a run of another member

Run ids are unique across the fleet. A request can name a run that the process does not have but the view knows. Then the process forwards the request to the owner. This applies to the native routes under `/v1/runs/{id}`. It also applies to the MCP tools `run_status`, `run_events`, `run_result`, `run_file`, `approve`, `cancel`, and `send`. `POST /v1/builder` and the MCP tool `agent_builder` with a `fromRun` of another member start the builder on that member. `POST /v1/runs?member=<name>` and the `member` argument of the MCP tool `run` start a run on another member. Through MCP, such a run does not wait, and the client follows it with `run_status`.

The forwarded request carries the bearer credential of the caller. The owner authenticates the caller again and authorizes the action in its kernel. The view therefore never acts under an identity of its own, and the owner records the real decider in its ledger. A credential goes to two kinds of address only:

- An ax address, through the router at `ax.router` with the header `ate-target-actor`. The router wakes a Task that sleeps, so an approval reaches a sleeping member.
- A URL that matches a glob in `workbench.fleet.forward`. In such a glob, `*` matches any text without a slash. The glob matches the scheme, the host, and the path of the URL. A URL with user information, a query, or a fragment never receives a credential.

A record therefore cannot send a credential to any other address. The relay of the native surface, which the package registers with `Kit.SetRelay`, does the forwarding for the run routes of the kernel, and the MCP server does it for its work tools. For the owner to accept the credential, both members trust the same issuer and the same `auth.oidc.audience`, or share the static token. A caller with an X.509 client certificate, or over stdio, carries no bearer credential. Such a caller sees the fleet but acts only on the runs of the member that it reached. If the owner refuses the credential, the view answers 403, and if the owner does not answer, the view answers 502. A forwarded request carries the header `Fw-Fleet-Via` with the name of the view. A member never forwards such a request again, so a stale record cannot cause a loop.

### 4.6 Configuration

The fleet reads the block `fleet` under the key `workbench` of `fw.yaml`, which the workbench claims (section 8). Each variable wins over the file.

| Key | Variable | Meaning | Default |
|---|---|---|---|
| `workbench.fleet.subject` | `FW_WORKBENCH_FLEET_SUBJECT` | The subject prefix of the fleet. Empty turns the fleet off. | empty |
| `workbench.fleet.name` | `FW_WORKBENCH_FLEET_NAME` | The name of this member | `<atespace>/<task>` under ax, else `<host>:<port>` |
| `workbench.fleet.url` | `FW_WORKBENCH_FLEET_URL` | The address where other members reach this one | `ax:<atespace>/<task>` under ax, else empty |
| `workbench.fleet.forward` | `FW_WORKBENCH_FLEET_FORWARD` | Globs over the URLs that can receive the credential of a caller, with commas between them | empty |
| `workbench.fleet.interval` | `FW_WORKBENCH_FLEET_INTERVAL` | The time between two heartbeats | `30s` |
| `workbench.fleet.expire` | `FW_WORKBENCH_FLEET_EXPIRE` | The age after which the view ignores a record | `168h` |

Under ax, three variables on each Task make a fleet: `FW_BUS_DRIVER=nats`, `FW_BUS_URL`, and `FW_WORKBENCH_FLEET_SUBJECT=fw.fleet`. Outside ax, each member also sets the URL, and each member that serves a view sets the forward globs:

```yaml
bus: { driver: nats, url: "nats://nats.agents.svc:4222" }
workbench:
  fleet:
    subject: fw.fleet
    url: "http://reviewer.agents.svc:8080"
    forward: ["http://*.agents.svc:8080"]
```

The fleet is a service of the kernel (`fw.RegisterService`), and the commands that do not serve, for example `fw-workbench run`, `fw-workbench mcp`, and `fw-workbench agents`, start no service, so they never join a fleet. `fw-workbench doctor` has a line about the fleet.

The directory `examples/agentgateway` puts a fleet behind agentgateway on one public URL. Its README lists the rules that keep the dashboard and the login working through the gateway.

### 4.7 Surfaces

- The native API serves `GET /v1/fleet`. It lists the members that the caller can see. Each member has its state, the agents that the caller can see, and its counts of active runs and of runs that wait for input. `GET /v1/fleet/runs` lists the runs of the whole fleet in the shape of `GET /v1/runs` of the kernel, which covers this process only. Its parameter `scope` is `fleet`, the default, or `local`, `member` narrows the list to one member, and `parent` lists the children of a run of any member. `agent`, `state`, `principal`, and `limit` narrow it as on `GET /v1/runs`. Each run of another member carries the field `member`. `POST /v1/runs?member=<name>` starts a run on that member.
- The MCP server adds the tool `fleet`, and `runs_list` takes the arguments `scope` and `member`: the scope `fleet`, the default, covers every member, and `local` covers this process only.
- The home of the dashboard lists the work of the whole fleet that the caller can see. The Agents place groups the agents by member, and the Operator view lists the members (section 6.2). Approve, deny, cancel, and a follow-up work in the same way on every run.

The A2A server and the MCP resources `fw://runs/{id}`, `fw://runs/{id}/ledger`, and `fw://runs/{id}/files/{+path}` serve the runs of their own process only.

### 4.8 Security

A record carries the inputs, the outputs, and the tool inputs of runs, cut to size. The `Redact` middleware removes secrets before an event enters the ledger, so no secret reaches a record. Limit publish and subscribe on the fleet subject to `fw` workloads, for example with NATS accounts or Redis ACLs. A process that can publish on the fleet subject can show false runs in a view. It cannot send a credential to an address outside `fleet.forward`, and it cannot act on a real run, because the owner decides.

### 4.9 What the fleet does not do

The fleet shares no ledger, elects no leader, and schedules no work. Bus intake (the kernel architecture, section 9.3) spreads work across the processes that consume one queue. A2A peers (the kernel architecture, section 7.4) let one agent ask another.

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
| `agents_list`, `agent_get` | The agents that the caller can see, and one agent with the words of what it can do |
| `agent_put`, `agent_delete` | Create, change, or remove an agent. `dryRun` checks a profile and changes nothing. |
| `runs_list` | The runs that the caller can see, with `scope`, `member`, and `by` (section 8) |
| `run_events` | One page of the events of a run as chat items. `from` reads forward, and `before` reads back. |
| `run_result` | What a run produced and what it needs next. The text is the result as a person reads it. |
| `run_file` | A file that a run changed. With `raw` true, it returns the bytes up to 4 MiB (the kernel architecture, section 7.9). |
| `agent_builder` | Start the conversation that creates an agent, from `input` or `fromRun` |
| `capabilities` | The tools of the runtime, each with a plain phrase, built-in tools first. This is the MCP form of `GET /v1/tools`. |
| `dashboard` | The MCP App, and the home of the caller as text |
| `usage` | Deprecated. The result does not change, and the description points to the Prometheus metrics (the kernel architecture, section 10). |
| `connections` | The services that act for the caller, connected or not, each with its connect link (section 11) |
| `disconnect` | Remove the grant of the caller for one service |

`run_events` refuses `from` and `before` together. Its result has `prev`, the value of `before` for the page in front. If the page starts at the first event, the result has no `prev`. For a run of another member, the server turns `before` into `from` and `limit` on the events route of that member. The sequence numbers of a run have no gaps, so the page is the same.

`agent_builder` takes exactly one of `input` and `fromRun`, and it returns the run of the builder at once. The structured result of `run_result` is `{view: "result", result}`, and the structured result of `capabilities` is `{view: "capabilities", tools}`. An exposed tool of `mcp.expose` never takes the name of a work tool or a management tool.

The tool `dashboard` returns an MCP App: an HTML page that the host renders in a sandbox and that calls the same tools through the host. Only the tool `dashboard` names the page in `_meta.ui.resourceUri`. A host therefore shows the page once, at the call that opens it. The other tools return text for the model and data for the page. The view `home` of `dashboard` adds `home`, the shared home below, over every run that the caller can see on every member. The old fields `runs`, `glance`, `agents`, and `whoami` stay for pages that are in the field. Its text is the home, then the agents of the caller without usage, then the members of the fleet. If `mcp.auth.audience` or `a2a.url` gives a public address, the text also names a browser link. The view `usage` still answers, and its first line says that it is deprecated.

### 6.2 The page

The same page is served at `GET /ui` for a browser, with browser sign-in (section 5) or a pasted bearer token. It is one embedded HTML file with no framework, no build step, and no external asset. The page shows each person only what their roles allow. It shows no usage, because operators read the Prometheus metrics (the kernel architecture, section 10).

The page has two places, Work and Agents. A small header holds the feather, the two places, and an account menu. The account menu names the person and opens the Operator view and Connections. If the browser holds a token, the menu also offers Sign out. Connections (`#connections`) lists every service that acts for the person (the kernel architecture, section 7.11), with Connect or Disconnect. The Operator view lists the fleet members and says where the metrics are. The address keeps the screen, such as `#run/<id>`, `#agent/<name>`, or `#agents`. An old link to a screen that is gone, such as `#usage`, opens home without an error. In a wide window, the place stays in the side pane, and the main pane shows the open item or the composer.

Home starts with a composer that asks what the person wants to move forward. It takes files from the + button, a paste, or a drop. A small chip names the agent that starts the work. The chip opens a menu of the agents that the person can start, and the page remembers the last choice. Under the composer, the sentence and the lists of `GET /v1/home` follow: Needs you, In progress, and Recent results, then a row for the runs by others. Each row leads with the title of the work. A quiet line under it names the agent, the status, and the time. A run that waits says what it needs on a third line. Each status has a word and an icon of its own shape, so color is never the only signal.

A run opens with its result above the conversation. The title comes first, then a quiet status line, the decision that waits, and what the run produced. That part is the result of the shared views below: the answer, the changed files, What it did, the limits, and the next actions as buttons. What it did is collapsed and shows only recorded outcomes: Done, Error, or No result. Done says that the call returned, never that a check passed. The conversation follows, then Activity, collapsed, and the message bar stays at the bottom. While a run works, the result shows only the title, the status line, and the decision, and the run opens at its end. A file opens its own screen with a Download action.

The page shows each pending decision once, in the result area. When its buttons scroll out of view, a small pointer above the message bar brings the card back. A permission card names the action and the target, such as "reviewer wants to create app.env". It shows the effect, the exact change or command, and why it asks. The tool name, the rule, and the raw input sit behind Details. The allow button uses the label of the server, such as "Run command", and Deny stays. A question card reads "Question from reviewer" with the question and its form, and it offers Answer and Decline.

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

Save as agent takes only a completed top-level run of this process that is not a builder run. The caller must be the person who started the run and must be able to create agents. A view of the run is not enough, because the builder run keeps the teams of the caller. The terms hold for the next action, for `POST /v1/builder` with `fromRun`, and for `agent_builder` with `fromRun`. A refusal is a `dashboard.SaveRefusal`, which wraps `fw.ErrForbidden`. The native API answers 403, and MCP answers "Forbidden: <reason>.". No owner can grant such a refusal, so its reason names no role. `Home.String` and `Result.String` give the same views as text for a host that shows only text.

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

The workbench still reads three parts of the kernel configuration: the block `mcp` of its server surface, the OAuth setup of each `tools.mcp` entry with `auth.oauth.grant: user`, and `auth.oidc`. Everything else of the workbench lives in the block `workbench` of `fw.yaml`: `teams`, `titles`, `fleet`, and `grants`.

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

The kernel reads its agent profiles from the configuration and writes none (the kernel architecture, section 13.3). The package `agents` creates, changes, and removes profiles for people. It normalizes a profile, checks it with `fw.Agent.CheckProfile`, authorizes the caller with the kernel's actions, writes the file under `<home>/agents` through the store of the config package, and hands the kernel the new set with `fw.Agent.SetProfiles`, so a run can name the profile at once. A new profile needs `agent.create`, a change needs `agent.edit`, and a removal needs `agent.delete`.

A principal that creates a profile without an `access` block becomes its first owner. An individual or a group therefore owns the agents that they make. A replaced profile that names no access block keeps the access block it had, so an edit never drops the owners by accident. A profile that the configuration defines, in `fw.yaml`, in a variable, or through a Go option, cannot change here, because the configuration wins over the file at the next start. The kernel reports such a profile as locked, and the page says so before a person edits it.

The routes `PUT` and `DELETE /v1/agents/{name}`, the MCP tools `agent_put` and `agent_delete`, and the verbs `fw-workbench agents put <file.yaml>` and `fw-workbench agents delete <name>` all go through the package. `PUT /v1/agents/{name}?dryRun=true` and `agent_put` with `dryRun` check a profile as a save does and change nothing. They answer an `agents.Draft`: the view of the profile, what the agent would do, and whether saving it replaces an agent that exists. The agent builder proposes a profile and never saves one; `agent_put` saves it with the rights of the caller (section 6.4).

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

### 11.9 Security

- No refresh token, access token, code, or PKCE verifier enters a ledger event, a log line, or an error message. The ledger holds the connect request and its decision. The decider is the person, and the note is `connected`.
- The file store encrypts every grant. Keep `FW_GRANT_KEY` in a secret store, apart from the volume of the grants. A key in `grant.key` beside the grants protects only a copy of the grant files that leaves the key behind.
- A cookie binds each connection to the browser that started it. The cookie holds the PKCE verifier, so it is HttpOnly and signed, and it lasts 10 minutes. A link from another person therefore cannot connect their account to your runs.
- For a service that trusts the sign-in issuer, the ID token of the connection must name the person of the run. A browser that is signed in at the issuer as another person cannot connect.
- Disconnect deletes the grant. If the authorization server offers revocation (RFC 7009), the workbench also revokes the grant there.
- In a fleet, a grant lives on the member that handled the callback. A run on another member does not see that grant. A grant store that members share is future work.
- The workbench has no device flow for the command line yet. A person connects in the dashboard or through an MCP client. The verbs that run one piece of work start no service, so they keep no connections.
