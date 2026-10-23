# The manager of bossmode

This example runs the manager of bossmode: one more fw agent that helps an
operator run the fleet. The manager reads the fleet, opens bridges, writes
reports into the wiki, and decides what the operator of a bridge delegates
to it. It is the profile `manager` on a Task of its own pool, `manager`.
Its tool server is the MCP server of the front. One run of the manager is
one conversation with one operator. The design is in section 19 of
`docs/ARCHITECTURE.md`.

The file `fw.yaml` is the configuration of a front with this pool. To add
the manager to a front that you run already, copy three parts into its
`fw.yaml`: the entry `manager` of `workbench.pools`, beside the pools that
the front keeps, `workbench.bossmode.manager: manager`, and
`workbench.bossmode.chat.agent: chat` for the quick chat.

## What the pool holds

- One Task, `manager-0`. The Task has no workspace to contend for, so its
  `fw` sets `maxRuns: 4`, and one Task serves four conversations at the
  same time. More operators need more replicas.
- `spawn.driver: ax` and an egress to the ax API and to the router, so a
  bridge with the placement `spawn` can seat its members in child Tasks of
  the Task of the manager. Such a bridge also needs the profiles of its
  members in the pool. `agent_put` with `pool: manager` adds them. The
  profile `manager` also needs the tools `spawn_task` and `await_task`.
  Before you use that placement, add both to `tools.builtin` and to the
  tools of the profile, and add `spawn_task` to its `policy.ask`. The example leaves them
  out, so a manager seats no child Task until you choose it.
- The variables `OPENAI_API_KEY` and `ANTHROPIC_API_KEY` of the pools of the
  lab. The model proxy holds the real keys.
- An egress to the bus, the model proxy, the front, the ax API, and the
  router, and to nothing else.
- The entry `front` of `tools.mcp`: the MCP server of the front, with the
  static token of the fleet as its bearer and `trusted: true`. A trusted
  entry names the person of the run in the `_meta` of each call, so the
  front acts for that person.
- The memory scope `principal`, the default under ax: the notes of the
  manager belong to one operator.
- No model. The front refuses a pool without one, so nothing runs on a
  model that you did not choose. Set `fw.model` of the pool, or `model` of
  the profile, to a selector of your model proxy.

## The template `agent`

The block `workbench.templates` holds one worker template, `agent`, the
default. The page of bossmode creates an agent with `PUT /v1/agents/{name}`,
and the front then makes a runtime pool of two workers for it from the
template, named after the agent. The operator never sees the image or the
egress: the template holds them. Like the pool of the manager, the template
names no model, so each agent sets one in its profile, or you set
`fw.model` of the template. The settings of the page, `GET /v1/bossmode`,
name the templates. Section 14.10 of the architecture has the rules.

## The profile `manager`

- The tools `front.*`, `memory`, `recall`, `clock`, and `files`.
- The policy asks the operator before `front.approve`, `front.cancel`,
  `front.run_budget`, `front.enqueue`, `front.run`, `front.bridge_open`,
  `front.bridge_remove_member`, `front.agent_put`, `front.agent_delete`,
  `front.faces_put`, and `front.routine_standing`, so a sign-off of a routine
  waits for the operator. Every read runs without a question. A write to the
  wiki with `front.pages_write` runs without a question too, because the
  wiki keeps each older version.
- The budget of one conversation: 400 turns, 8 hours of working time, and
  2000 tool calls.
- A system prompt that says what a manager does and does not do. It reads
  before it acts, and it asks before it spends. It reads the open bridges
  with `front.bridge_list` before it proposes or opens work, and builds on
  a bridge that already does the work. It opens a bridge only with
  `front.bridge_open`, and then it tells the operator to follow the bridge
  on the page, because nothing wakes the manager when a bridge finishes,
  so it never promises a later report. It writes its reports into the wiki, and it names the
  person it acts for. It answers in plain words. It never claims that a run
  passed a check that it did not see.

## The profile `chat`

The pool runs a second profile, `chat`, for the quick chat of the page:
a conversation of one person with a general assistant, beside the
bridges. `workbench.bossmode.chat.agent: chat` names it, and the settings
of the page, `GET /v1/bossmode`, carry it in `chat` with the member that
runs it. The pool names `agent: manager`, because a pool with two
profiles takes the queue of one.

- The tools `files`, `recall`, `memory`, `clock`, and the tools of the
  wiki: `front.pages_list`, `front.pages_read`, `front.pages_write`,
  `front.pages_search`, and `front.pages_history`. The page sends the
  picks of the person as the inline profile of each run, so
  `profile.tools` and `profile.skills` replace these lists for that run.
- No model, as for the manager.
- The memory scope `principal` of the pool: the notes of the quick chat
  belong to one person.
- A short system prompt for a general assistant. It reads before it
  answers, writes a page of the wiki only when the person asks, and says
  so when it does not know.

The page starts a quick chat as it starts a conversation with the manager,
with `{"agent": "chat", "input": "...", "metadata": {"fw.view":
"quickchat"}}`, and reads the answer live from `GET /v1/runs/{id}/live`
through the front. Section 17.7 of the architecture has the rules.

## Install

1. Replace the placeholders that the top of `fw.yaml` names, and set the
   model.
2. The person who talks to the manager signs in at the front. The front
   decides for the person with its rules. It then sends the static token
   of the fleet to the Task, with the header `Fw-On-Behalf-Of` that names
   the person. So the Task takes the static token alone: its `fw.auth` is
   `mode: token`. A run of the token has no person, so the front acts as
   the token for that run.
3. Review what the front applies:

   ```
   fw-workbench pools render -config fw.yaml
   ```

4. Start the front, or restart it with the new pool. It creates the Task
   `manager-0` and keeps it.
5. Check the line `manager` of `fw-workbench doctor`. It names the pool
   that runs the profile, the trusted entry through which the profile
   reaches the front, the spawn driver, and whether the bridges are set. If
   no pool runs the profile, or if the entry of the front is not trusted,
   the line fails.

## Open a conversation

The page of bossmode opens the conversation, as section 7.4 of the
contract of the page says:

1. The first message starts a run of the manager on its Task:
   `POST /v1/runs?member=<atespace>/manager-0` with
   `{"agent": "manager", "input": "..."}`, with the credential of the
   operator. The front decides for the operator and starts the run with
   the static token, and the run belongs to the operator.
   `GET /v1/bossmode` names the member and the newest run of the
   operator.
2. Each later message is a follow-up: `POST /v1/runs/{id}/messages` with
   `{"content": "..."}`.
3. A run that failed or was canceled ends the conversation, and the next
   message starts a new run. While the run is queued or suspended and the
   first answer is missing, the page says "Waking the manager": the Task
   sleeps between messages, and the router wakes it in about five seconds.

Any MCP host or script does the same through the MCP tools of the front:
`run` with `member`, then `send`.

## The lab

The lab deploys the pool in `e2e/fleet/front-config.yaml` of the
repository featherweight-lab, beside `lab-pool` and `tickets`: the entry
of `workbench.pools` from this file, with `${FLEET_IMAGE_WORKER}` as the
image and `${FLEET_MODEL}` as `fw.model`, which `front-config.sh` fills,
and `workbench.bossmode.manager: manager`. The lab front verifies the
tokens of people from `http://issuer.fw-fleet.svc.cluster.local`. The Task
takes the static token alone, as this file says. The members of a bridge come from the pool `crew` of the lab.

## How strong the manager is

The manager calls the front with the static token of the fleet, so it is
as strong as that token on the front. Three things bound it:

- The policy of its profile. The operator approves each call that starts
  work, changes a budget, decides an approval, cancels a run, or changes a
  profile, on the card of the call.
- The delegation flags of each bridge. On a run of a bridge, the manager
  can approve, change a budget, or cancel only what the operator of that
  bridge delegated to it. Outside a bridge, its policy alone binds it.
- The on-behalf-of. Each call names the person of the manager run, and the
  front reads and acts for that person, with the rights that the person and
  the token both have. The manager sees only what its operator sees, and it
  never reaches through a person what the token cannot reach.

Each action of the manager carries its name:

- The note of a decision says `by manager run <id> for <person>`.
- A message names the manager run as its sender.
- A run that the manager starts carries `fw.via: manager:<id>`.
- The log of the front names the manager run for a cancel and for a change
  of a budget.

The owner of a run records the static token as the decider. The tools of
the wiki and of the faces write with the rights of the token. So every
person who can talk to the manager can write a page through it. The same
person can also set a face after the approval on its card. To keep that to
some people, name them in the access block of the profile.
