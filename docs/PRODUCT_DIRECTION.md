> Moved on 2026-10-17 from the root of featherweight-agent. The text below is unchanged. Its file paths name that repository at the time of writing. The code it describes now lives in featherweight-workbench and, for the kernel, in featherweight-kernel.

# Product direction: a bicycle for the mind

Status: brainstorming. These are hypotheses to validate, not committed scope.

## The central idea

The strongest direction is **more agency for the person, not more autonomous agents**.

Many agent products invite the user to become a manager: assign work, monitor progress, answer questions, and inspect results. That can be useful, but it can also feel like hiring an unpredictable employee.

The bicycle metaphor suggests something different:

> A tool that makes your intentions easier to express, your ideas cheaper to test, and your abilities grow with use.

That is a richer ambition than an assistant that does tasks.

## What users want

These are hypotheses to validate with a specific audience, not universal truths.

| What users ask for | The underlying desire |
|---|---|
| “Automate this.” | Stop making me carry this obligation in my head. |
| “Remember my preferences.” | Stop making me explain myself repeatedly. |
| “Help me decide.” | Make the situation understandable, not just summarize documents. |
| “Do it for me.” | Produce a result I can trust without supervising every step. |
| “Let me control it.” | Let me intervene without having to operate the machinery. |
| “Keep my data private.” | Do not make me surrender ownership to gain capability. |

The combination is **leverage without loss of agency**.

Three product ideas could express that.

## 1. Try it before you do it

### Make experimentation almost frictionless

Most assistants move from conversation to execution. A distinctive intermediate step is **rehearsal**:

> Let me see what would happen.

For example:

- “Try these two approaches to fixing the build. Compare the results.”
- “Reorganize this project, but show me the proposed structure before changing my working copy.”
- “Run the analysis with these three assumptions. Tell me which conclusion changes.”
- “Prepare the customer update using the new numbers. Show me what changed from last week.”

The experience is:

**Ask → try in a bounded environment → inspect evidence and differences → apply selected changes.**

This is not a model merely describing a hypothetical outcome. Where possible, it performs the work against a copy and runs checks.

### Why this captures the bicycle idea

It makes a person more willing and able to explore.

The benefit is not just saving thirty minutes. It is making an experiment cheap enough that the user tries something they otherwise would not attempt.

> “What if?” becomes a button that does useful work.

This gives AX and gVisor a user-facing purpose without making users understand either technology. They enable safe working environments behind an ordinary “Try it” interaction.

A sandbox does not make external actions reversible. Sending an email, spending money, and changing a production service stay behind explicit action gates. Call it a rehearsal, not a universal undo system.

**Potential positioning:** A place to try your ideas before committing to them.

## 2. Every correction makes you more capable

### Turn successful work into reusable abilities, not just chat history

A common assistant experience is:

1. Explain the task.
2. Correct the answer.
3. Explain your conventions.
4. Get a useful result.
5. Repeat much of that next week.

The product opportunity is to make that effort accumulate.

Suppose the user says:

> This is good, but exclude internal transfers, use our fiscal weeks, and flag missing data rather than estimating it.

After completing the task, the system asks:

> Use these rules for future cash-flow reports?

Acceptance creates something concrete and inspectable:

- The intended outcome.
- The inputs and connected resources.
- The rules of the user.
- The expected output.
- The checks that establish success.
- The actions that still require permission.

The next request becomes:

> Run my cash-flow report.

### The important part is the learning loop

Not “the agent remembers everything.” Instead:

> A correction becomes an explicit rule. A failure becomes a check. A successful task becomes a reusable ability.

For engineering work, a correction can become a regression test. For documents, it can become a checklist or example. For judgment-heavy tasks, it can become a stated preference, not an invented objective truth.

Changes are proposed, versioned, and reversible. They never silently expand permissions.

This is closer to a personal power tool than a generic assistant. The user gradually builds a repertoire of things they can accomplish easily.

Featherweight already has many ingredients: skills, profiles, recorded runs, artifacts, and a conversational builder. The differentiator is the quality of this refinement cycle, not another configuration format.

**Potential positioning:** Do it once. Make it yours. Get better every time.

## 3. Give it an obligation, not a prompt

### Own the follow-through

Users often do not want another answer. They want a nagging obligation removed.

Compare:

> Summarize these open issues.

With:

> Help me keep this release ready. Tell me when something changes that threatens Friday.

The second requires continuity:

- Remember the desired state.
- Watch relevant signals.
- Notice meaningful changes.
- Prepare useful next actions.
- Interrupt only when the person needs to decide.
- Explain why an interruption matters.

This is not merely scheduled prompting. It is **attention management**.

A useful notification says:

> The release is blocked by a new dependency failure. I reproduced it and prepared a patch. Here are the test results. Do you want to open the PR?

An unhelpful notification says:

> Here is your daily summary of 38 issues.

### Why people would care

It removes the burden of remembering to check.

This is also the hardest concept to get right. Bad monitoring creates noise. Vague responsibilities create unbounded work. Start with one narrow obligation and explicit limits.

**Potential positioning:** Keep the outcome in sight without keeping the task in your head.

## Recommended direction: combine the first two

Build around:

> A personal workbench where you can try things safely and turn what works into lasting capabilities.

That produces a simple loop:

```text
Express an intention
        ↓
Try it with me
        ↓
Inspect the result
        ↓
Correct what matters
        ↓
Save the useful capability
        ↓
Delegate it next time
```

The user can move between three natural modes:

- **With me:** help me explore and understand.
- **For me:** execute this understood task.
- **Keep doing:** repeat a proven responsibility within agreed limits.

Those are not three different products. They are stages of trust.

They avoid forcing users to choose between a passive chatbot and an unsupervised agent.

## The interface matters as much as the runtime

The primary object is not an agent personality or a scrolling transcript. It is **the work itself**:

- A document being improved.
- A patch being tested.
- A decision with alternatives and evidence.
- An analysis with assumptions the user can change.
- A recurring responsibility with a clear current state.

Conversation helps manipulate that work. It is not the only way to see it.

Useful controls include:

- **Try another approach**
- **Change this assumption**
- **Show the evidence**
- **Compare versions**
- **Apply these changes**
- **Use this rule next time**

Those controls give people direct leverage. “Create agent,” “configure tool,” and “view execution graph” mostly expose implementation.

## Product decision: remove the Usage destination

Remove the Usage page from the browser and MCP App navigation. Remove the aggregate usage section from agent cards too. The product is a workbench, not a monitoring console.

Operational reporting belongs in Prometheus/Grafana or an equivalent observability stack:

- Model tokens and cost, with cost available only when prices are configured.
- Model and tool latency, errors, throughput, and availability.
- Run outcomes, pending approvals, and budget or loop failures.
- Infrastructure and fleet health.

Keep task-specific limits, budget warnings, and relevant cost information beside the work that they affect. Do not remove budget enforcement or durable usage records from the kernel. Unknown cost is not zero cost.

The `/metrics` surface and telemetry observer already exist. Ship a supported scrape configuration, a small Grafana dashboard, and useful alerts rather than rebuilding them in fw. Keep the metrics endpoint inside the monitoring perimeter. Avoid user identities, run IDs, prompts, and other sensitive or unbounded values as metric labels.

Prometheus is not the authoritative billing or audit record. Scrapes can miss short-lived processes, and counters reset on restart. Preserve durable records for exact accounting. Under AX, avoid scraping sleeping actors through the wake-on-request router. Use an always-on collector or control-plane path where needed.

Removing a UI destination does not require immediately deleting the native usage API or the MCP `usage` tool. Preserve compatibility or deprecate those contracts explicitly. Remove Usage from the advertised dashboard views and handle old links deliberately.

This section records the removal decision. It does not implement the UI change.

## UI direction: a calm workbench

The existing UI has useful foundations: system typography, light and dark themes, a responsive split layout, collapsed tool steps, file previews, schema-driven questions, and a conversational agent builder. Preserve those strengths. Improve hierarchy and interaction before adding decoration.

### Start with the intention

Put a composer on home: “What would you like to move forward?” Include attachments from the first message. Keep agent selection available as a compact choice, with the selected agent visible, rather than requiring a visit to an agent card before starting work.

On wide screens, use the main pane for this composer or the selected work. Do not open to a large empty pane that only asks the person to select a conversation.

Keep primary navigation small: work, reusable agents, and an account/settings menu. Put fleet topology and infrastructure details in the operator view. Reveal a member or connection when it changes what the person can do, not on every row.

### Name the work, not just the worker

Title a work item “Fix checkout retries” rather than “default” or “reviewer.” Put the agent name, owner, and time beneath the title. Keep avatars small and secondary.

Prioritize Needs you, In progress, and Recent results. Use “Ready to review” only when a reviewable result exists. A model ending a run does not prove that the work passed its checks.

Replace the large status distribution bar with a restrained summary when it helps. The most important visual element is the next useful action, not the ratio of completed to failed runs.

### Give the result a place to live

On desktop, give the selected work a stable result area. Show the current artifact or change, a short summary, evidence, and the next action. Keep the conversation and detailed activity available without making the person scroll through them to find the deliverable.

On mobile and in narrow MCP hosts, use the same hierarchy in one column: result or pending decision first, supporting details next, conversation within reach.

Add result manifests and immutable deliverables before presenting proposed comparison, rehearsal, or verification controls as implemented features. A new layout cannot manufacture evidence that the runtime does not record.

### Make decisions legible

An approval card names the action, target, consequences, and scope of consent. Show the exact change or payload before the decision. Use a specific action label such as “Open pull request” when the tool provides trustworthy structured data. Keep a neutral generic label when the action cannot be described reliably.

Distinguish an answer to a question from permission to act. Keep raw JSON and tool names behind Details. Never hide material consequences merely to make the card shorter.

### Remove friction rather than add spectacle

- Remove the blocking feather opening animation. Keep the feather as a quiet brand mark or a small loading indicator. Render usable content as soon as it arrives.
- Preserve message drafts, attachments, scroll position, keyboard focus, and in-progress approval answers across navigation and refreshes.
- Update changed rows instead of replacing a whole form-bearing view during polling.
- Open long conversations at their recent end and load older history on demand instead of fetching every page before rendering.
- Keep the neutral palette and one primary accent. Use status colors with words or icons, not as the only signal.
- Use larger, comfortable action targets, clear focus indicators, restrained motion, and accessible contrast. Keep monospace for code and paths.
- Put model selectors, globs, identity patterns, and JSON behind an Advanced section. Lead the agent editor with responsibility, boundaries, and examples.
- Offer ordinary sign-in for browser users. Token pasting remains a developer escape hatch.
- In an MCP App, respect the host theme and available display modes. Keep useful text fallbacks when the host cannot render the page.

Elegance means less explanation, fewer interruptions, and a clear next action. It does not require another frontend framework.

## MCP parity: current code review

The browser dashboard and MCP App serve the same `surface/dashboard/index.html`. Its native and MCP adapters expose matching operations for the current shared page. Most improvements to that page therefore benefit both environments.

This is strong workflow parity, not complete API or host-experience parity.

| Area | Current status |
|---|---|
| Start, inspect, follow up, approve, and cancel runs | Present through both native API and MCP tools. |
| Profile discovery, creation, editing, deletion, and dry-run validation | Present through both. |
| Conversation, questions, child runs, file previews, and agent-builder cards | Shared UI and shared server-side view models. |
| Attachments | Native and MCP run/send APIs support them. The shared initial-run form has no attachment picker; the follow-up composer does. Host payload limits still need testing. |
| Fleet run actions | Native routes and MCP management tools forward supported actions. Cross-member operations require a forwardable caller credential. |
| Live updates | The shared page polls on both transports. Native API also has an SSE event stream. MCP offers progress on waiting calls and paged reads, but no Tasks extension in this implementation. |
| Raw file access | Native API offers file listings and raw downloads. MCP `run_file` offers bounded text/image previews, not an equivalent general binary download. The shared page also lacks a download action. |
| Runtime tool discovery | Native `/v1/tools` lists runtime tool specifications. MCP `tools/list` lists the management and explicitly exposed tools, not the same capability catalog. |
| Fleet resources | MCP management tools can reach remote runs. `fw://runs/{id}` and its ledger resource read local runs only. |
| Text-only home | The MCP `dashboard` response summarizes all visible runs. The shared page defaults to the person and their teams. Access checks still apply, but the home experiences differ. |
| Builder without the page | Typed profile operations exist, but the guided builder prompt and “Save as agent” assembly live in JavaScript. A text-only host has to reconstruct the workflow. |
| Approval retry after restart | Signed MCP `requestState` needs a stable `FW_MCP_STATE_KEY`, shared across replicas. Without it, the approval stays in the ledger but the old signed retry cannot be verified. The explicit `approve` tool remains the fallback. |

Close the practical gaps before adding more protocol surface:

1. Share the home scope, builder flow, and result semantics between visual and text-only experiences.
2. Add a discoverable capability catalog and an authorized artifact retrieval path that suit both transports.
3. Document remote resource limitations and configure durable approval signing in the supported deployment.
4. Exercise the same workflows through native HTTP, the MCP App bridge, and text-only MCP: create, preview, start, attach, approve, resume, inspect a result, and reuse the setup.
5. Test real hosts for frame sizing, theme, keyboard use, attachment limits, errors, and disconnect recovery. Shared HTML alone does not establish host compatibility.

Rehearsal, version comparison, accepted-result evidence, and correction-to-rule learning remain proposed product capabilities on both surfaces.

Review basis: source inspection and passing short tests for `surface/dashboard`, `surface/native`, `surface/mcp`, `surface/metrics`, and `telemetry`. This pass did not include a rendered-browser or live MCP-host usability test.

## Where the differentiation becomes defensible

None of these ideas is individually unique. The differentiation comes from delivering them together exceptionally well.

The advantage could become:

1. **Reliable rehearsal:** real execution and checks, not persuasive previews.
2. **User-owned accumulated knowledge:** explicit, portable rules and capabilities.
3. **Low supervision cost:** the system presents evidence and decisions, not a stream of activity.
4. **Continuity across tools:** the same capabilities work from the browser, an IDE, or an MCP host.
5. **Trust earned through repeated results:** the user knows what a capability does because they helped refine it.

Open source and self-hosting reinforce the story:

> The abilities you build belong to you.

They are supporting evidence, not the headline.

## Validate before building much more

Find five people with the same recurring, messy task. Choose a task involving judgment and several tools, not a simple automation that already has an obvious script.

Help each person complete it three times.

Watch for:

- What they explain repeatedly.
- Which mistakes they consistently correct.
- What evidence they need before accepting the result.
- Where they want exploration versus delegation.
- Whether the third attempt requires substantially less supervision.

The key experiment:

> Does the product become more useful after correction, or does each interaction feel like starting over?

If users begin saying “this is how I do that now,” the product is something stronger than a chatbot they occasionally consult.

## North star

> Every successful interaction leaves the user with more capability, not merely another answer.

That is a bicycle for the mind.
