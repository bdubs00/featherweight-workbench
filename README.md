# featherweight-workbench

This repository holds the workbench for people on top of the kernel, featherweight-kernel. It is one binary, `fw-workbench`, that holds the kernel `fw` and adds the parts that people use:

- the page at `/ui`, with the home, the result of a run, the conversation, and the agent builder
- browser sign-in with OpenID Connect, the `oidc` authentication mode
- the MCP server at `/mcp` and over standard input and output, for desktop clients and MCP App hosts
- the fleet, one view of many `fw` processes over retained bus records
- connections, with which an MCP server of the kernel acts for the person who started a run
- the `propose_agent` tool and the titles that a model writes for runs

`fw-workbench` takes every verb of `fw`, with the same flags and the same configuration. The design is in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). The kernel design is in the architecture of the kernel.

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

Container images: `make image` builds `fw-workbench:static` on distroless, and `make image-toolbox` builds `fw-workbench:toolbox` on Debian with git. Both use `docker buildx` with the kernel as a second build context. Both contain the symlinks `ax-task-runner` and `fw`, so ax starts the binary and `ax ssh <task> -- fw events <run>` works.

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
| `GET /v1/fleet` | the members of the fleet |
| `/v1/connections` | the connections of the caller to services |
| `GET /v1/usage` | the usage of the runs, deprecated |

For a desktop client, run the MCP server over standard input and output:

```
./bin/fw-workbench mcp
```

Its `dashboard` tool renders an MCP App with the same page. A host that shows only text does the same work with `run_result`, `agent_builder`, `capabilities`, and the other management tools.

## Browser sign-in

With `auth.oidc.clientId`, a person signs in to the page through the issuer, with the authorization code flow and PKCE. Pasting a token stays as the path for a developer. Add `oidc` to `auth.mode`, for example `oidc,token`, so that the token of the sign-in authenticates the calls of the page.

## Connections

An MCP server entry of the kernel with `auth.oauth.grant: user` acts for the person who started the run. The first call that needs the service pauses the run. The page shows a Connect card, and an MCP client gets a link through URL-mode elicitation. The person signs in at the authorization server once, and every paused run of that person goes on. An entry without a `clientId` uses the client of the browser sign-in, so one connection covers every such entry.

## Titles

Each run gets a title that a model writes, such as "Fix checkout retries", the first time that the workbench shows it. The kernel gives a run the first sentence of its input as a title and makes no call. Set `titles: input` in the block `workbench` of `fw.yaml`, or `FW_WORKBENCH_TITLES=input`, to save that call.

## The fleet

Each process with `workbench.fleet.subject` publishes a record of itself and of its runs on the bus. Any process serves one view of the whole fleet. A request about a run of another process goes to that process with the credential of the caller. [examples/agentgateway](examples/agentgateway/README.md) runs a fleet behind agentgateway on one public URL.

## How the workbench builds on the kernel

The workbench changes no kernel code. It uses these hooks of the kernel:

1. `native.Extend` mounts the routes of `surface/web` on the native surface of the kernel, behind the same authentication.
2. `fw.RegisterDefaults` turns on the `mcp` surface and adds the `propose_agent` tool.
3. Package `cli` gives the verbs of `fw`, and `cmd/fw-workbench` adds the MCP server to the verb `mcp` and the writes to the verb `agents`.
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
| `authn/oidc` | the `oidc` authentication mode |
| `tools/builder` | the `propose_agent` tool |
| `examples/agentgateway` | a fleet behind agentgateway |

## License

MIT. See [LICENSE](LICENSE).
