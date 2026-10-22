# featherweight-workbench

This repository holds the workbench for people on top of the kernel, featherweight-kernel. It is one binary, `fw-workbench`, that holds the kernel `fw` and adds the parts that people use:

- the page at `/ui`, with the home, the result of a run, the conversation, and the agent builder
- browser sign-in with OpenID Connect, the `oidc` authentication mode
- the MCP server at `/mcp` and over standard input and output, for desktop clients and MCP App hosts
- the fleet, one view of many `fw` workers from the ax API and the bus egress stream, with jobs, a token broker, a suspend controller, and pools that keep a number of Tasks of one profile
- connections, with which an MCP server of the kernel acts for the person who started a run
- the `propose_agent` tool and the titles that a model writes for runs
- bossmode, the operator front of the fleet: the page of featherweight-bossmode at `/bossmode`, its settings, and one live stream of events, off by default

`fw-workbench` takes every verb of `fw`, with the same flags and the same configuration. The product thesis is in [docs/PRODUCT_DIRECTION.md](docs/PRODUCT_DIRECTION.md), and the design is in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). The kernel design is in the architecture of the kernel.

## Build

The workbench builds against the kernel checkout next to it, through the `replace` directive in `go.mod`. Clone both repositories into one directory:

```
git clone https://github.com/bdubs00/featherweight-kernel
git clone https://github.com/bdubs00/featherweight-workbench
cd featherweight-workbench
make build    # writes bin/fw-workbench
make test     # runs every test with the race detector
```

Install Go 1.27 or newer and `make`. To use a different toolchain, set `GO`, for example `make GO=/opt/go/bin/go build`. If the kernel is in another place, set `KERNEL` for the images and change the `replace` directive.

After the kernel has a release, depend on the release instead:

1. Run `go get github.com/bdubs00/featherweight-kernel@v0.1.0` in this directory.
2. Remove the `replace` directive from `go.mod`.
3. Remove the build context `kernel` from the Makefile and the two Dockerfiles.

Container images: `make image` builds `fw-workbench:static` on distroless, and `make image-toolbox` builds `fw-workbench:toolbox` on Debian with git. Both use `docker buildx` with the kernel as a second build context, and with the page of bossmode as a third one when `PAGE` holds a checkout of it (below). Both contain the symlinks `ax-task-runner` and `fw`, so ax starts the binary and `ax ssh <task> -- fw events <run>` works.

### The page of bossmode

The page of bossmode lives in its own repository, featherweight-bossmode, and builds with Node 22 and npm. To embed it in the binary, clone it next to the workbench and run `make page` before the build. The target runs `npm ci && npm run build` in the checkout that `PAGE` names, `../featherweight-bossmode` by default, and copies its `dist` directory into `bossmode/static`. git ignores that copy, and `make page-clean` removes it. Without a copy, the binary embeds a placeholder page that names `workbench.bossmode.dir`, so `go build` works either way.

```
git clone https://github.com/bdubs00/featherweight-bossmode
make page     # builds ../featherweight-bossmode into bossmode/static
make build
```

For an image with the page, pass the checkout as the build context `page` and the argument `PAGE=build`. For an image with the placeholder, pass neither. `make image` chooses by itself.

```
docker buildx build --build-context kernel=../featherweight-kernel \
  --build-context page=../featherweight-bossmode --build-arg PAGE=build \
  -t ghcr.io/bdubs00/featherweight-workbench:static .
docker buildx build --build-context kernel=../featherweight-kernel \
  -t ghcr.io/bdubs00/featherweight-workbench:static .
```

Add `-f Dockerfile.toolbox` and the tag `:toolbox` for the toolbox image. Section 1.2 of the architecture has the details.

## Start here

```
export ANTHROPIC_API_KEY=... FW_API_TOKEN=secret
./bin/fw-workbench serve
```

Open `http://localhost:8080/ui` and paste the token. The listener serves the routes of the kernel and these routes:

| Path | What |
|---|---|
| `/ui` | the page |
| `/ui/signin`, `/ui/callback` | the browser sign-in, with `auth.oidc.clientId` |
| `/mcp` | MCP 2026-07-28 over Streamable HTTP |
| `GET /v1/home` | the runs that need you, the runs in progress, and the recent results |
| `GET /v1/runs/{id}/result` | what a run produced and what it needs next |
| `GET /v1/runs/{id}/transcript` | the conversation of a run as chat items |
| `POST /v1/builder` | a run of the agent builder |
| `GET /v1/fleet` | the members and the pools of the fleet |
| `/v1/pools`, `GET /v1/templates` | the pools of the file and the runtime pools, which a person creates, changes, and deletes, and the worker templates that a runtime pool renders from |
| `/v1/connections` | the connections of the caller to services |
| `/v1/pages` | the wiki of the fleet: pages of Markdown with a history |
| `/v1/faces` | the faces of the agents: an SVG, else an identicon, with a display name and a tagline |
| `/v1/bridges` | the bridges of bossmode: one job split into seats, with one channel for the agents and the operator |
| `GET /v1/usage` | the usage of the runs, deprecated |
| `/bossmode`, `GET /v1/bossmode`, `GET /v1/bossmode/stream` | the page of bossmode, its settings, and its stream of events, with `workbench.bossmode.enabled` |
| `/v1/bossmode/skills`, `/v1/bossmode/marketplaces`, `POST /v1/bossmode/runs/{id}/skill` | the skills of the front, the slash command of a person, and the import of a skill by an admin, with `workbench.bossmode.enabled` |

For a desktop client, run the MCP server over standard input and output:

```
./bin/fw-workbench mcp
```

Its `dashboard` tool renders an MCP App with the same page. A host that shows only text does the same work with `run_result`, `agent_builder`, `capabilities`, and the other management tools.

## Browser sign-in

With `auth.oidc.clientId`, a person signs in to the page through the issuer, with the authorization code flow and PKCE. Pasting a token stays as the path for a developer. Add `oidc` to `auth.mode`, for example `oidc,token`, so that the token of the sign-in authenticates the calls of the page.

## Connections

An MCP server entry of the kernel with `auth.oauth.grant: user` acts for the person who started the run. The first call that needs the service pauses the run. The page shows a Connect card, and an MCP client gets a link through URL-mode elicitation. The person signs in at the authorization server once, and every paused run of that person goes on. An entry without a `clientId` uses the client of the browser sign-in, so one connection covers every such entry.

The workbench keeps each grant, a refresh token, in an encrypted store under `<home>/grants`. The block `grants` under `workbench` in `fw.yaml`, or `FW_WORKBENCH_GRANTS_STORE`, `FW_WORKBENCH_GRANTS_DIR`, and `FW_WORKBENCH_GRANTS_KEY`, choose the store. Set `FW_GRANT_KEY` to hold the key in a secret store. One refresh of a grant runs at a time, and the workbench stores the new refresh token before it uses the new access token, so a server that rotates refresh tokens gets each one once. Section 11 of the architecture has the rules and a Keycloak example. In a fleet, the front keeps every grant and gives each member the tokens of a person for the runs of that person, so no refresh token enters a Task.

## Titles

Each run gets a title that a model writes, such as "Fix checkout retries", the first time that the workbench shows it. The kernel gives a run the first sentence of its input as a title and makes no call. Set `titles: input` in the block `workbench` of `fw.yaml`, or `FW_WORKBENCH_TITLES=input`, to save that call.

## The fleet

A worker is a bare `fw` in an ax Task, and the front is `fw-workbench` outside the sandboxes. The front reads the Tasks from the ax API (`workbench.fleet.atespaces`) and the runs from the egress stream of the workers (`workbench.fleet.egress`), and it serves one view of the whole fleet. A request about a run of a worker goes to its Task through the atenet router with the credential of the caller, a job goes to the intake queue of a profile with `enqueue`, and a Task that idles is suspended until the next request wakes it. [examples/agentgateway](examples/agentgateway/README.md) runs a fleet behind agentgateway on one public URL.

A pool (`workbench.pools`) keeps a number of Tasks that run one profile. The front creates the Tasks with their Workspaces. It replaces a Task of an older configuration once the Task is idle. It deletes the Tasks above the count, but never a Task with an active run. The front owns the profiles of its pools: `agent_put`, `agent_delete`, and the builder write to the overlay of the pool in a NATS key-value bucket, `agents_list` and `agent_get` show what the workers run, and Always allow on the approval card adds an allow rule to the policy of the pool. `fw-workbench pools render` prints the Tasks that the front applies, with a placeholder for each secret and the overlay merged. A worker template (`workbench.templates`) says once what a worker is, without its agent. A person then creates a runtime pool from a template while the front runs, with `POST /v1/pools` or the MCP tool `pool_put`, or in one step with a new agent: `agent_put` with `workers`, or with a default template, makes a pool of two workers named after the agent. The runtime pools live in `<stream>_POOLS` on a NATS bus, and every pass runs them as it runs the pools of the file. [examples/pools](examples/pools/fw.yaml) keeps three workers of one profile and offers one template, and section 14 of the architecture has the rules.

## The wiki and the faces

The front keeps a wiki for the fleet: pages of Markdown such as `notes/plan.md`, with optional front matter, a hash of each text, and the last 64 versions of each page. A write can name the hash that it read, so a stale write fails instead of losing a change. Agents read and write the pages with the MCP tools `pages_list`, `pages_read`, `pages_write`, `pages_search`, and `pages_history`, and people with the routes under `/v1/pages`. After each change, the front writes `_index.md` with the tree. `fw-workbench pages export <dir>` and `fw-workbench pages import <dir>` copy the wiki to files and back.

Each agent has a face: an SVG, a display name, and a tagline. An agent without an SVG has an identicon from the hash of its name. The front keeps only an SVG that passes its sanitizer, and it refuses any other SVG with the reason. The manager sets a face with the MCP tool `faces_put`.

On a NATS bus, both live in key-value buckets of the stream of the fleet, `<stream>_PAGES` and `<stream>_FACES`, so every replica of the front reads the same pages and faces. On the bus `mem`, they live in the memory of the process. Sections 15 and 16 of the architecture have the rules.

## Bossmode

Bossmode is the operator front of the fleet, a chat client in which an operator runs a team of agents. With `workbench.bossmode.enabled`, the front serves the page at `/bossmode`, its settings at `GET /v1/bossmode`, and one stream of server-sent events at `GET /v1/bossmode/stream`. The stream carries the runs, the members, and the Tasks of the fleet, the entries and the seats of the bridges, the pages of the wiki, and the faces, each to the people who can see it. `workbench.bossmode.theme` gives the page the accent, the name, and the logo of a deployment, and `workbench.bossmode.chat` names the profile of the quick chat, a conversation of one person with a general assistant. `fw-workbench doctor` has one line for bossmode: the page, the theme, the manager, the quick chat, and the stores of the wiki, the faces, and the bridges. Section 17 of the architecture has the rules.

## Bridges

A bridge is one large job split into seats, with one agent on each seat and one channel where the agents and the operator talk. The operator opens a bridge with `POST /v1/bridges`, and the front puts one run per seat on the queue of the pool of its agent, or lets the manager seat the members as child runs. A message in the channel reaches the runs of the members as a follow-up that names its sender, under caps that stop a ping-pong between two agents. The channel mirrors the life of each run: the start, an approval with its card, the answer of a turn, a failure, and the sleep of a Task. A thumb on an answer is a quality receipt, and a heart writes the result into `agents/<agent>/learned.md` of the wiki, which the next runs of the agent read. The members read and post with the MCP tools `bridge_read` and `bridge_post`, and the manager opens and changes bridges with `bridge_open`, `bridge_list`, `bridge_add_member`, `bridge_remove_member`, and `bridge_close`. The records and the channels live in `<stream>_BRIDGES` and `<stream>_CHANNELS` on a NATS bus, and in memory on the bus `mem`. Section 18 of the architecture has the rules.

## Skills

The front keeps a catalog of its skills for the page: each skill with its description, its origin, its arguments, and whether a person can hand it to a run. The view of an agent names the skills that its profile allows, resolved as the kernel matches them, at `GET /v1/agents/{name}/skills` and in `agent_get`. A person hands a skill to a running conversation with a slash command: in a bridge with `skill` on `POST /v1/bridges/{id}/messages`, and elsewhere with `POST /v1/bossmode/runs/{id}/skill` or the MCP tool `skill_send`. The front fills the placeholders of the skill with the arguments and sends the message pinned, so it survives every compaction. An admin imports a skill from a URL, a marketplace of `workbench.bossmode.marketplaces`, a pasted text, or an upload, after a preview, into `$FW_HOME/skills`, where the agents load it on their own. Section 20 of the architecture has the rules.

## The manager

The manager of bossmode is one more agent that helps an operator run the fleet. A pool runs it on a Task of its own, and its tool server is the MCP server of the front. It calls that server with the static token of the fleet, and each call names the person of its run. The front then reads and acts as that person. A check for the person also needs the token, so the token never reaches more through a person. On a run of a bridge, the manager approves, changes a budget, or cancels only what the operator of the bridge delegated. [examples/bossmode](examples/bossmode/README.md) holds the pool and the profile, and section 19 of the architecture has the rules.

## How the workbench builds on the kernel

The workbench changes no kernel code. It uses these hooks of the kernel:

1. `native.Extend` mounts the routes of `surface/web` on the native surface of the kernel, behind the same authentication.
2. `fw.RegisterDefaults` turns on the `mcp` surface and adds the `propose_agent` tool.
3. Package `cli` gives the verbs of `fw`, and `cmd/fw-workbench` adds the MCP server to the verb `mcp`, the writes to the verb `agents`, and the verbs `pools` and `pages`.
4. `fw.RegisterExtension` claims the block `workbench` of `fw.yaml`, and `fw.RegisterRunAnnotator` records the teams of a run.
5. `fw.Agent.Annotate` records a title that a model wrote, and `fw.Agent.SetProfiles` makes a saved agent profile live.

## Layout

| Package | What |
|---|---|
| `cmd/fw-workbench` | the binary and its drivers |
| `dashboard` | the page and the home, result, builder, and transcript views |
| `surface/web` | the routes for people on the native surface |
| `surface/mcp` | the MCP server |
| `fleet` | the fleet driver |
| `pools` | the pools of Tasks that the fleet keeps, the worker templates, and the store of the runtime pools |
| `workers` | the routes and the MCP tools of the pools and the templates |
| `pages` | the wiki of the fleet |
| `faces` | the faces of the agents, the sanitizer, and the identicon |
| `bridges` | the bridges of bossmode: the records, the channels, the fan-out, and the system lines |
| `bossmode` | the page of bossmode, its settings, and its stream with the feeds of the bridges, the wiki, and the faces, and the routes and the tools of the skills |
| `skills` | the skills of the front: the catalog, the matching of a profile, the slash command, and the import |
| `internal/bucket` | the key-value bucket under the stores of the front |
| `internal/behalf` | the run behind an MCP call, and the guard of a person of the static token |
| `manager` | the manager of bossmode: the on-behalf-of, the attribution, and the delegation checks |
| `tools/builder` | the `propose_agent` tool |
| `examples/agentgateway` | a fleet behind agentgateway |
| `examples/pools` | a front that keeps a pool of three workers |
| `examples/bossmode` | a front with the pool of the manager and the profiles of the manager and of the quick chat |

## License

MIT. See [LICENSE](LICENSE).
