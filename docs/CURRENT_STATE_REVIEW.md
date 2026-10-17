> Moved on 2026-10-17 from the root of featherweight-agent. The text below is unchanged. Its file paths name that repository at the time of writing. The code it describes now lives in featherweight-workbench and, for the kernel, in featherweight-kernel.

# Current-state review

Date: 2026-10-16  
Initial revision: `d3c8b24`, plus the existing working-tree changes  
Final revision checked: `05a8039` — “Make model titles the default”  
Basis: source review, Go verification, rendered-browser checks, and focused runtime reproductions

## Bottom line

**This is a substantial improvement, not a cosmetic response to the earlier feedback.** The project now presents work rather than fleet machinery, gives results a clear home, and moves important workflow semantics out of JavaScript into shared Go projections. Removing Usage without removing accounting was the right decision. The MCP work closes most of the practical gaps identified in the previous review.

My assessment is **a credible self-hosted agent workbench in alpha**, not yet a dependable enterprise execution platform or a differentiated “bicycle for the mind.”

The next investment is reliability, not more features. I found reproducible problems in starting work, applying proposed restrictions, retrying work, following changes from another client, and the lifecycle of model-generated titles. These are fixable within the current architecture. None calls for a frontend framework or another orchestration layer.

I would use this with a small group of technical users, trusted workloads, narrow permissions, and recoverable tasks. I would not yet present profiles sharing one process as isolation boundaries for mutually untrusted users.

## Verification and scope

I changed no implementation files. This report is the only repository file I created. I left `PRODUCT_DIRECTION.md`, `REVIEW.md`, the untracked `e2e/`, and the existing modifications untouched.

Another change landed while this review was in progress: `05a8039` committed the model-title default, added `fw.WithTitles`, updated documentation, and adapted many test fixtures. I inspected that diff and reran verification against it. The table below reports the final state, not the earlier broad failure.

Toolchain: `go1.27.2 linux/amd64`.

| Check | Result |
|---|---|
| `go build ./...` at `05a8039` | Passed |
| `go vet ./...` at `05a8039` | Passed |
| `go test -short -timeout=120s ./...` at `05a8039` | Passed on this run |
| `go test -timeout=120s ./...` at `05a8039` | Passed on this run |
| Race-enabled checks for kernel, dashboard, native, MCP, metrics, and telemetry | Kernel failed in `TestBusIntakeAndEgress`; the other selected packages passed |
| `go test -race -count=10 -run '^TestBusIntakeAndEgress$' ./fw` | Failed all ten iterations |
| Headless Chrome desktop and mobile checks | Confirmed improvements and the browser defects below |
| Title lifecycle/accounting probes, rerun at `05a8039` | Confirmed the reported defects |

The full suite here uses the default build tags; the build-tagged live E2E suite was not run. A failure under `-race` is not automatically a detected data race: this failure is a test assertion caused by concurrent consumption of scripted model responses.

Before `05a8039`, the local title-default change caused widespread failures and a timeout. A Go overlay substituting only `fw/config.go` from `d3c8b24` restored passing full and focused race suites without editing the working tree. The newer commit fixes most of that test migration, but misses the direct bus-test constructor described in finding 4. Do not read the earlier failure logs as the current state of every subsystem.

Browser checks used the actual, unchanged `surface/dashboard/index.html`, served with a deterministic local mock API. They exercised browser rendering, validation, keyboard input, navigation, and outbound request payloads. They were not live-model or real-MCP-host acceptance tests. Desktop was 1280×900; mobile was 390×844.

I also exercised the actual file ledger and model-title path with small Go probes. No real model provider was called. No live identity provider, AX cluster, Grafana deployment, or MCP host was tested. This is not a security audit.

## What is genuinely better

### The product hierarchy now makes sense

- Home starts with an intention and a compact agent choice, including attachments.
- Work has titles rather than being identified mainly by the agent name.
- Needs you, In progress, and Recent results make a useful default hierarchy.
- Completed work opens at its result, with changed files and next actions above the conversation.
- Usage, the opening animation, and the prominent status distribution are gone. Fleet details live in the Operator view.
- Agent editing puts the technical configuration under Advanced.

The rendered desktop and mobile views look restrained and coherent. The sampled mobile view had no horizontal overflow. The home text draft survived navigation to Agents and back.

### The implementation follows the right architectural direction

`surface/dashboard/work.go` and `surface/dashboard/result.go` now own shared home, result, builder, and capability semantics. That is more valuable than another set of UI controls: a text-only MCP client can use the same concepts as the browser.

The result view avoids claiming that completion proves correctness. “What it did” is a record of calls, not a fabricated test verdict. The raw-download bounds, changed-file warnings, backward transcript pages, and authorization tests show attention to real failure cases.

The frontend remains one embedded page with no external assets or build pipeline. Keep that deployment simplicity. Browser behavior now needs stronger tests, but it does not need a rewrite.

### Observability is a much better boundary

The supported Prometheus configuration, alerts, and Grafana dashboard are useful deliverables. Separating unknown prices from zero prices, limiting model-label cardinality, and reading sleeping AX members through fleet records are especially good decisions.

Keeping the old usage contracts with explicit deprecation is better than breaking clients to remove a navigation item. The documentation also correctly separates operational trends from durable accounting, subject to the title-accounting defect below.

## Confirmed defects, in priority order

P1 means fix before a wider pilot or before enabling the affected behavior by default. P2 means a meaningful workflow or correctness defect. These findings include both new regressions and remaining problems in existing paths.

### 1. P1 — A proposal can display new restrictions while launching the old, less restricted agent

**Location:** `surface/dashboard/index.html:1801–1821`, `fillDraft`.

The saved-profile comparison checks only system prompt, description, and tools. It ignores policy, budget, and other effective settings. When those three fields match, the proposal gets “Start run” and “Open agent” instead of “Replace.” The card still displays the proposed abilities and limits.

**Reproduced:** the saved agent allowed shell commands and had a one-hour deadline. A proposal with the same prompt/description/tools denied `git push*` and reduced the deadline to one minute. The card displayed “Never runs git push*” and “1 minute of work,” but offered only “Start run” and “Open agent.” The stored restrictions had not changed.

This is not merely a stale label. The interface can tell the person that a restriction exists when the action launches a profile without it.

**Fix direction:** compare a normalized complete effective profile, or have the server return whether the exact proposal is already stored. Require an explicit replacement whenever policy, budget, or another effective field differs. Add a browser regression test that changes only a policy or a deadline.

### 2. P1 — “Try again” does not retry the original execution contract

**Location:** `surface/dashboard/index.html:2031`.

The retry action starts a new run with only `agent`, `input`, and `member`. It omits initial attachments, run instructions, inline profile, budget, and other request context.

**Reproduced:** the outgoing retry POST contained only the agent and original text. Source inspection confirms that no path reconstructs the other fields.

A failed run with an inline read-only tool subset or tighter budget can therefore be retried under the broader named-profile defaults. An attachment-dependent task also loses its supplied files.

**Fix direction:** make retry a server-side operation that reconstructs and validates the intended request, preserves its restrictions, and explicitly handles attachments and changed profile versions. Alternatively, open a prefilled review form and call the action “Start a new run.” Do not silently present a materially different request as the same attempt.

### 3. P1 — Late model titles leave completed ledger files open

**Locations:** `fw/title.go:68–77`; `fw/ledger_file.go:104–166`.

The title call can append `fw.run.titled` after `fw.run.finished`. The terminal append closes the run file. The later title append opens it again, but that event is not terminal, so the file stays open until another closing operation or a flush/shutdown.

**Reproduced with the real file ledger:**

```text
Open ledger files after 12 terminal events: 0
Open ledger files after 12 late titles: 12
```

A long-lived process accumulates open files for short completed runs when titles arrive late. Model titles are now enabled by default, which makes this especially relevant.

**Fix direction:** give post-completion metadata appends an explicit close/durability lifecycle. Test that many completed runs with late titles do not retain one descriptor per run. Do not classify a title as a generic terminal event merely to close a file; that would affect other consumers.

### 4. P1 — The default-title test migration is still incomplete

**Locations:** `fw/config.go:483`; `fw/busio_test.go:34–35`; `fw/title.go:41–77`.

The new default adds a concurrent model call. Commit `05a8039` correctly introduces `fw.WithTitles(fw.TitlesInput)` for scripted fixtures and updates many callers, but `TestBusIntakeAndEgress` constructs its agent directly with the default configuration and a one-response script. It bypasses the updated common helper.

When the title consumes that response first, the actual work fails with `model: script: no response left`. The normal full suite passed once, but the focused race-enabled reproduction failed all ten iterations:

```sh
go test -race -count=10 -run '^TestBusIntakeAndEgress$' ./fw
```

This is a fixture/default interaction, not proof that production bus delivery is broken. It still means the baseline is not reliably green, including the race-enabled workflow advertised by `make test`.

**Fix direction:** finish auditing every direct scripted-model constructor and pass `WithTitles(TitlesInput)` before construction. Test model titles separately with a fake that distinguishes request kinds. Keep the documented extra-call behavior, and resolve the production title lifecycle/accounting findings before relying on the new default.

### 5. P1 — Repeated Enter starts duplicate work

**Locations:** `surface/dashboard/index.html:2183–2190`, `2250–2258`; native and MCP start adapters.

Buttons are disabled during submission, but the keyboard handler still calls `requestSubmit()` while the first request is outstanding. There is no form-level in-flight guard. The page also does not supply the native idempotency key or MCP `requestId` for these starts.

**Reproduced in Chrome:** pressing Enter twice before a delayed response produced two identical `POST /v1/runs` requests, without an idempotency key.

For an agent, duplicate submission means duplicate work and potentially duplicate external effects, not just duplicate chat bubbles.

**Fix direction:** guard the form itself against concurrent submission and assign one stable request identity to a logical start, reused on transport retry. Cover keyboard submission, slow responses, and lost responses on both adapters. Follow-up submission needs an in-flight guard too.

### 6. P2 — A required boolean question cannot be answered false in a multi-field form

**Location:** `surface/dashboard/index.html:1165–1166`.

A required JSON Schema boolean becomes an HTML checkbox with `required`. JSON Schema requires the property to exist; HTML requires that checkbox to be checked. Those are different meanings.

**Reproduced:** a question required `publish: boolean` and `reason: string`. With the reason filled and Publish off, Chrome reported `valueMissing: true`. Clicking Answer sent no request. A valid and important answer, `publish: false`, was unavailable.

**Fix direction:** represent required booleans as an explicit Yes/No choice, or always serialize checked/unchecked while validating property presence independently of HTML checkbox `required`. Keep false distinct from absent for optional values too.

### 7. P2 — An open completed run misses later work from another client

**Location:** `surface/dashboard/index.html:1296–1312`.

Once `ctx.idle` is true, the page stops normal transcript updates. The temporary late-title poll reads a run but updates only its title. After two minutes even that read stops.

**Reproduced:** after opening a completed run and letting it become idle, the mock server changed it to `input-required`, representing a follow-up from another client. Home updated to Needs you. The open detail remained Ready to review with no decision card, even after the title poll read the changed run.

Cross-client continuity matters in a product with browser, MCP, API, and team workflows.

**Fix direction:** if an idle refresh sees a state or sequence change, resume normal application of transcript/result updates. Continue a cheap invalidation mechanism for an open completed run, or use a subscription. Never treat title-only refreshes as sufficient synchronization.

### 8. P2 — A late file response can overwrite the screen the person navigated to

**Location:** `surface/dashboard/index.html:1769–1784`, `screens.file.open`.

Unlike the run and agent screens, the file screen does not verify that its navigation entry is still current after awaiting the API response.

**Reproduced:** open a changed file, then immediately navigate home while the file response is delayed. When that response arrives, it replaces home with the file screen. The URL is home, the file path is visible, and the composer is gone.

**Fix direction:** apply a consistent navigation generation check or cancellation token to every asynchronous screen load. Also prevent an old polling callback from scheduling itself again after `stop()` while its request is in flight. The latter is visible in `every()` and merits a dedicated regression test.

### 9. P2 — Title handling still has accounting and restart inconsistencies

**Locations:** `fw/title.go:98–105`; `fw/resume.go:56`.

Two additional cases remain despite the welcome telemetry fix:

- A successful title response with no usable title returns `errNoTitle`. Its reported usage is discarded rather than recorded. A fake provider returning 100 input and 20 output tokens for a blank title, plus 1/1 for the work response, left recorded usage at **1/1 instead of 101/21**.
- Restart classification looks at the last event type rather than the projected run state. A completed run ending in a late title receives a spurious `fw.run.resumed`. The probe confirmed that event. The terminal-state projection and turn cursor still prevent a new model call; this is not a claim of duplicated execution. It is misleading lifecycle history and unnecessary resume work.

**Fix direction:** record usage independently of whether title text is usable, and decide whether a run needs recovery from its lifecycle state rather than the final event type alone. Test the entire sequence: finish, late metadata, close, restart, observe.

## MCP parity: much improved, still qualified

The previous parity review is now outdated in several important ways.

| Area | Current assessment |
|---|---|
| Shared home scope | Implemented through the shared Home projection |
| Result semantics | Implemented through native result route and MCP `run_result` |
| Guided builder | Now server-side; text-only hosts no longer reconstruct its prompt |
| Runtime capabilities | MCP `capabilities` closes the catalog gap |
| Raw artifacts | MCP raw reads and file resources exist, bounded to 4 MiB |
| Downloads in the page | Present; actual MCP-host download behavior remains host-dependent |
| Approval state after a single-process restart | Key now persists in the home directory |
| Replicas | Still require a stable shared `FW_MCP_STATE_KEY` |
| Fleet actions | Supported tools forward to the owner; run resources remain local |
| Tasks extension | Still not implemented |

Two smaller inconsistencies deserve attention:

1. `surface/mcp/work.go:50–56` and `:174–183` tell a text host to answer builder questions with `send`. A pending `ask_user` question needs `approve` with its answer content. `Send` does not clear that pending decision (`fw/kernel.go:649–687`; `fw/turn.go:272`). The architecture describes the distinction correctly; the tool instructions need to do the same.
2. Native `POST /v1/builder` accepts files and an idempotency key. MCP `agent_builder` exposes only `input` and `fromRun`. That is narrower than full builder parity, even though the current visual builder does not offer an attachment picker either.

The appropriate claim is **strong shared-workflow parity**, not interchangeable transport capabilities or proven host compatibility.

## What remains from the larger product critique

### A result area is not yet verified evidence

The result view is useful, and its language is appropriately cautious. But it still shows current workspace files, not immutable deliverables. Shell changes are not recorded as `fw.file.changed`. A successful tool response is not necessarily a successful check: the bash tool returns a normal result even for a nonzero exit code, with that code in the text.

Keep “What it did” rather than “Verified.” Before adding confident verification or Apply/Compare controls, introduce one narrowly scoped, structured result/check representation with provenance. Do not build a general artifact platform first.

### Save as agent is a useful shortcut, not the complete learning loop

The builder is real progress toward reuse. It does not yet establish that corrections become durable, reviewed improvements.

For example, `surface/dashboard/result.go:648–738` keeps the first eight user messages and a shortened last answer when assembling Save as agent. A correction later in a long conversation can be omitted. It also lists tools requested by the model, not a validated capability recipe or a set of proven checks.

The next product experiment is one recurring task with explicit accepted rules and a regression check. Measure whether the third execution needs less supervision than the first. More profile-generation features are not a substitute for that evidence.

### Enterprise isolation and aggregate controls remain unfinished

These are earlier architectural concerns, not regressions caused by the workbench revision:

- **Shared execution context:** `tools/builtin/bash.go` still maintains one persistent shell. Ordinary OS access through bash is not constrained by the `files` tool's path policy. Processes sharing the sandbox can reach supervisor state subject to OS permissions.
- **Defaults versus ceilings:** inline tools can still exceed a named profile's list. The new notice makes the behavior visible; it does not turn profile defaults into an administrative ceiling.
- **Delegation budgets:** `fw/turn.go:606–649` caps a child's limits but checks each run's own consumption. This is not an aggregate spend budget for the delegation tree.
- **Admission control:** children still bypass the top-level `maxRuns` slots (`fw/kernel.go:889`). Depth and per-parent child counts bound the tree but do not provide an aggregate active-work limit.
- **Recovery:** durable tool intent, downstream idempotency, reconciliation, and explicit unknown outcomes remain important. AX snapshot rollback can lose evidence of an external action that already happened. The architecture now documents this honestly.
- **Debug access:** AX guest services remain a separate unauthenticated execution path when debug is enabled. Keep them off in hardened deployments.

For an enterprise story, name the trust boundary precisely: trusted supervision and policy outside untrusted execution, plus aggregate resource controls. gVisor protects the host from a workload; it does not by itself separate the supervisor from tools inside that same workload.

## UI and engineering recommendations

1. **Keep the design. Fix its promises.** The visual direction is better. Correct proposal identity, retry semantics, submission control, and cross-client synchronization before adding more screens.
2. **Add browser behavior tests to CI.** `surface/dashboard/page_test.go` primarily checks strings and source shape. Those tests confirm that features are present, not that they work. The confirmed defects above passed those tests.
3. **Use one async navigation/submission discipline.** A small generation counter, guarded submit helper, and consistent request identity would remove several classes of failure without a framework.
4. **Converge the two start experiences.** Home has attachment and draft support, but the older Start run screen reached from an agent card still has neither. Starting from an agent must not lose those capabilities.
5. **Keep relevant limits beside active work.** The API carries run usage, but the page omits it, and `outcomeHTML` hides the result body while work is active or awaiting input. Removing fleet-wide Usage did not require hiding useful per-task limit context.
6. **Separate design history from current documentation.** `docs/ARCHITECTURE.md` still says near the beginning that it describes code that does not exist and ends with “Nothing in this document is compiled or tested yet,” while its delivery section records completed implementation. Remove that contradiction. It weakens an otherwise unusually detailed document.

## Recommended next sequence

### First: a reliability pass

- Resolve the title default and its ledger/accounting lifecycle.
- Fix the misleading saved-proposal comparison and retry contract.
- Prevent duplicate submissions.
- Fix boolean answers, cross-client refresh, and stale navigation responses.
- Put these reproductions into browser/runtime regression tests.

### Then: a bounded pilot

Exercise one workflow through native HTTP, browser, text-only MCP, and one real MCP App host:

**Start with an attachment → answer a question → approve an action → disconnect/reconnect → inspect/download the result → correct it → save and reuse the configuration.**

Include a restart while waiting for approval and a follow-up sent from a different surface. Make failures explicit rather than merely checking that the happy path returns 200.

### Only then: prove one differentiated capability

Choose either a safe rehearsal with an inspectable change and a real check, or a correction-to-rule loop for one repeated task. My preference remains the combination, implemented narrowly rather than as a new general orchestration system.

**Final assessment:** the workbench direction is now visible in real code and a better interface. The remaining gap is less about presentation and more about whether the system reliably does exactly what its controls say. Close that gap before expanding the product surface.

## Local reproduction artifacts

These temporary files support this review but are not project changes or permanent test coverage:

- `/tmp/fw-review-final-tests.log` and `/tmp/fw-review-final-full-tests.log`: passing short and full runs at `05a8039`.
- `/tmp/fw-review-final-race.log` and `/tmp/fw-review-final-bus-race.log`: remaining bus-test failures at `05a8039`.
- `/tmp/fw-review-tests.log`: historical failures before `05a8039`.
- `/tmp/fw-review-overlay.json`: historical overlay substituting `fw/config.go` from `d3c8b24`.
- `/tmp/fw-review-overlay-tests.log`, `/tmp/fw-review-overlay-full-tests.log`, and `/tmp/fw-review-race.log`: passing historical overlay checks.
- `/tmp/fw-review-browser.mjs`: Chrome/CDP harness serving the unchanged page with a mock API.
- `/tmp/fw-review-browser-results.json`: captured browser results and request payloads.
- `/tmp/fw-review-{home,result,question,mobile}.png`: rendered screenshots.
- `/tmp/fw-review-title-probes.go`: real-ledger and fake-model title reproductions.
- `/tmp/fw-review-final-title-probes.log`: those reproductions rerun at `05a8039`.

The temporary browser harness reproduces defects; it is not an acceptance suite that passes when the product is correct. These artifacts can disappear when temporary storage is cleaned. The descriptions above contain the essential reproduction conditions.
