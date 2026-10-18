# fw behind agentgateway

This example puts a fleet of fw agents behind one public URL. People add
`https://fleet.example.com/mcp` to their MCP host, log in, and see their own
agents and runs. The same URL serves the dashboard at `/ui` and the API at `/v1`.

The file `fleet.yaml` holds these parts:

- NATS with JetStream, which carries the events of the workers to the front and the jobs of the front to the workers.
- `fw-front`, two fw replicas with no agents. They serve the whole fleet from the events of the workers and forward each action to the Task that owns the run. The Secret `fw-front` holds the key that the two replicas share.
- `reviewer`, one worker with one agent, as a Deployment. The front sees its runs through the events; to act on a worker and to suspend it when it idles, run the workers as ax Tasks and name the atespace in `FW_WORKBENCH_FLEET_ATESPACES`.
- agentgateway routes: `/mcp` through an MCP backend with a login policy, and `/ui` and `/v1` as plain HTTP.

## Install

1. Install agentgateway v1.6.0. The Gateway API CRDs must be installed first.

   ```sh
   helm upgrade -i agentgateway-crds oci://cr.agentgateway.dev/charts/agentgateway-crds \
     --create-namespace --namespace agentgateway-system --version v1.6.0
   helm upgrade -i agentgateway oci://cr.agentgateway.dev/charts/agentgateway \
     --namespace agentgateway-system --version v1.6.0 --wait
   ```

2. If the proxy pod cannot pull its image from `cr.agentgateway.dev`, add `--set proxy.image.registry=ghcr.io/agentgateway` to the second command. The same image is on GHCR.
3. Replace the placeholders that the top of `fleet.yaml` names.
4. Create the namespace, the Secret `fw-front` with a random key that the two replicas share, and the Secret `fw-fleet-token` with the static token that the members present to the front:

   ```
   kubectl create namespace fw-fleet
   kubectl -n fw-fleet create secret generic fw-front --from-literal=FW_MCP_STATE_KEY="$(openssl rand -hex 32)"
   kubectl -n fw-fleet create secret generic fw-fleet-token --from-literal=FW_API_TOKEN="$(openssl rand -hex 32)"
   ```

5. Apply the file: `kubectl apply -f fleet.yaml`.
6. Point the DNS name `fleet.example.com` at the address of the Gateway `fleet`.

## Connect

- In Claude Code, run `claude mcp add --transport http fleet https://fleet.example.com/mcp`. Claude Code does the login itself.
- In claude.ai, ChatGPT, or VS Code, add the same URL as a custom connector or an MCP server. These hosts show the dashboard in the chat.
- In a browser, open `https://fleet.example.com/ui`. Without browser sign-in, paste an access token.

To turn on browser sign-in, do these steps:

1. Register a client for the dashboard at the issuer, with the redirect URI `https://fleet.example.com/ui/callback`.
2. Make sure that the access token of that client carries the audience `https://fleet.example.com/mcp`. fw keeps only a token that its authentication accepts.
3. Add `FW_AUTH_OIDC_CLIENT_ID` to the env of `fw-front`.
4. For a confidential client, set `FW_AUTH_OIDC_CLIENT_SECRET` to a variable name, such as `FW_OIDC_CLIENT_SECRET`. Put the client secret in that variable from a Kubernetes Secret.
5. Apply the file again. The page then sends the person to the issuer through `/ui/signin`.

## Rules that keep the fleet working

- Give the fw MCP backend exactly one target. With two or more, agentgateway changes the tool names and the dashboard link.
- Keep `sessionRouting: Stateful`, which is the default. In the stateless mode, the gateway hides the capabilities of the MCP host from fw.
- Set the same audience in four places: the gateway policy, `resourceMetadata.resource`, `FW_AUTH_OIDC_AUDIENCE`, and `FW_MCP_AUTH_AUDIENCE`. Then one token works on every fw process, and the front can forward it.
- Keep `backend.auth.passthrough`. Without it, the gateway removes the token, and fw cannot tell who the person is.
- If you add `traffic.authorization` rules for MCP, allow the resource `ui://fw/dashboard`. Otherwise the gateway hides the dashboard.
- Give every replica of `fw-front` the same `FW_MCP_STATE_KEY`. A signed approval retry and the callback of a browser sign-in can then reach either replica.

## Metrics

The pods of `fw-front` carry the label `fw.dev/scrape: "true"`, so the job `fw-pods` of [examples/observability](../observability/README.md) scrapes them. A member pushes its own metrics, its ledger, and its traces to the collector of `examples/observability` when `OTEL_EXPORTER_OTLP_ENDPOINT` names one, so Prometheus never scrapes a Task. The gateway does not route `/metrics`, so the route stays inside the cluster.

A member that runs as a plain Deployment, such as `reviewer`, is always on too. To collect its model and tool metrics, add the same label to its pod template. Never add the label to an ax Task, because a scrape wakes a Task that sleeps.

## Members that run as ax Tasks

An ax Task has no Kubernetes Service. Under ax, fw-workbench sets the name of the member to `<atespace>/<task>` and its URL to `ax:<atespace>/<task>` by itself. The front reaches such a member through the atenet router, which wakes the Task first. A Task can send traffic out of its sandbox only over HTTP, so it connects to NATS over WebSocket: set `FW_BUS_URL` to `ws://nats.fw-fleet.svc.cluster.local:8080`.

## What was tested

On 2026-10-14, a k3s cluster ran this layout with agentgateway v1.6.0, and one public URL served all of the following:

- An MCP session of the 2025-11-25 revision stayed on one front replica, and new sessions reached both replicas.
- A request of the 2026-07-28 revision worked without a session.
- The tool list kept `ui://fw/dashboard`. The resource came back with the MIME type `text/html;profile=mcp-app`.
- A run on a member asked for an approval. The approval went through the gateway, and the run finished.
- A request without a token got the 401 challenge, and its protected-resource document resolved.
- Two people saw only their own runs and agents.
- The ledger named the person who approved a run as the decider.

That test used an OIDC issuer inside the cluster over HTTP, with `FW_AUTH_OIDC_INSECURE=true`. Use an https issuer in production.
