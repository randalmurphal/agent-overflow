# Agent visibility design

Status: DRAFT, awaiting sign-off (brainstorm 2026-08-22). Nothing implemented.

## Goal

Every subagent a thread spawns is a first-class, inspectable node: visible
while it runs, attributed for everything it causes, openable as its own
read-only thread view, for Claude and Codex alike.

## Approach

Model a thread's agents as a tree keyed on what the store already holds:
a launch row (Agent/Task, forked Skill, SendMessage-resume, Codex
`spawn_agent`) and its descendants by `parent_id`. One card component
renders every node in the timeline and in the background section; one
companion pane kind renders any node as a thread view scoped to its
launch id. Provider adapters answer three questions only: what is a
launch, how do progress and terminal signals arrive, which controls exist
(background / kill). Everything above the adapters is provider-neutral.

## Key decisions

- Card = today's inline subagent card for every kind; awaited vs
  background is a status pill, the capped-body + fade variant goes away
  (Q6).
- Collapsed row: kind chip (`agent` / `skill`), name, status pill,
  elapsed, tool count, current activity line; tokens when the row has
  room, hidden under a container-width breakpoint (Q1).
- Expanded body: the node's tool calls + its final text; thinking and
  intermediate text live in the pane. A spawned child shows as a
  collapsed child card inside the parent's body (Q2, Q3).
- Pane = new companion kind `agent` with a scope (launch item id),
  rendered by the thread renderer filtered to `parent_id == scope`.
  Opening a child from inside swaps the scope and pushes a breadcrumb
  (`main › code-review › Angle B`); no stacking (Q4, Q4b).
- Pane keeps the composer shell, non-interactive, with Stop (= kill
  where the wire can) as its only live control; background button and
  status/elapsed sit in the pane header beside the breadcrumb (Q20).
  The shell's top row is the working chip — the agent's own spinner
  sprite / LED chase, verb and elapsed timer, keyed on the launch — while
  the agent runs (ruling 2026-08-23, reversing the earlier "no run timer,
  no spinner" call); idle, the row stays as a height twin.
- Pane lifetime mirrors the review pane: persisted and restored, closed
  when the source thread changes, closes itself when the scoped row is
  gone on restore (Q5).
- Background section lists every node that is backgrounded or descends
  from one, indented by depth; click opens the pane and scrolls the
  timeline to the card. Forks appear without a kill button (Q8).
- Background action: icon button on a running inline agent or Bash row
  (Claude only: `background_tasks` control_request by `tool_use_id`);
  no keyboard shortcut (Q9). A node backgrounded mid-flight stops
  streaming until it completes (CC behavior); its pane shows a
  "streaming paused until it finishes" marker at the cut.
- Kill only where the wire can: Claude nodes with a task id
  (`stop_task`); never forks (interrupt-only) or Codex children
  (`close_agent` is model-only).
- Live progress is in-memory UI state fed by Claude `task_progress`
  (tool count, tokens, elapsed, activity line) and, for Codex, the
  child thread's `thread/tokenUsage/updated` (unsuppressed into a scoped
  progress event) plus row counts. The final numbers persist onto the
  launch row at terminal so a reloaded thread shows them (Q5 fold-in).
- Attribution rule: anything a subagent causes carries its scope.
  `permission_denied` fixed (ce580f3f); `can_use_tool` approvals must
  resolve `agent_id` → launch tool_use (parser task map, triage row
  lookup as fallback) so the approval row nests under the card and the
  card shows a "needs approval" pill; the normal approval UI still
  presents the prompt (Q10, Q10b).
- Notifications (bell/toast) fire for top-level nodes only; nested
  completions update their card silently (Q11).
- A DETACHED launch (async ack, `run_in_background`, a Codex spawn, a
  SendMessage resume carrier, or backgrounded mid-flight —
  `launchRunsDetached`) has an immutable spawn row and ONE card, at its
  completion point (ruling 2026-08-23): the spawn row is a compact agent
  row (label, model, description, launch time, a static `background`
  marker, the open-in-pane door) that never changes after the spawn; the
  card — status, duration, tool count, tokens, the expandable transcript,
  open-in-pane — renders AT the `complete:<id>` sibling
  (`SubagentGroupNode.anchor`), top-level or inside the parent card for
  a nested node, after everything the main thread wrote while the agent
  ran. While the agent runs there is no card: the pane and the tray are
  its live surfaces. The bell is hidden on the strength of the completion
  rendering (`utils/notificationFilter.ts`), which is why the card sits
  at the sibling rather than folding it onto a card at the launch (the
  fold-and-drop version left the transcript with no trace of the agent
  finishing — regression 2026-08-22; tripwire
  `utils/backgroundCompletionVisibility.test.ts`). Awaited launches are
  unchanged: one card at the launch, completing in place.
- Row actions (open-in-pane, background, stop) render before the
  status / duration / timestamp columns on every row so the timestamp
  column stays aligned (`ToolHeaderMeta`'s `actions` slot; chat
  AGENTS.md "Row Contract").
- The agent pane keys its whole scoped window as ONE turn
  (`ThreadPane.timelineTurns`, overridden by the scope facade): active
  while the scoped launch runs, settled on the launch's own completion
  with the agent's duration. A subagent's rows span however many
  provider turns it outlives, so keying the response divider/pill on the
  main thread's turn stamped "Response 1m 58s" on a still-running agent
  the moment the main turn settled (regression 2026-08-22).
- Forked skills are detected structurally: the first row attributed to
  a `Skill` tool_use marks the fork; the completion's
  `tool_use_result.status:"forked"` + `agentId` closes it. No skill-name
  list (claude-wire.md §E9).

## Non-goals

- Steering a subagent from AO (no wire path; the model-relay exists but
  is not productized).
- Kill/background for Codex children, forks, or anything else the wire
  cannot reach.
- Per-agent composer or any input in the pane.
- Persisting live progress ticks.

## Success criteria

- [ ] A forked `code-review` renders as one `skill` card; none of its
      tool calls appear as the main agent's.
- [ ] A depth-2 background agent renders as a running card under its
      parent's card with live tool count and activity line, and in the
      background section indented under its parent.
- [ ] Opening any card shows that node's full transcript in the agent
      pane; opening a child from inside swaps scope with a working
      breadcrumb; reload restores the pane to the same scope.
- [ ] A subagent's `permission_denied` and `can_use_tool` rows nest
      under its card; the card shows the approval pill while the prompt
      is pending.
- [ ] Background button on a running inline agent returns the main
      turn, the card flips to background, the pane shows the paused
      marker, and the transcript completes on the task notification.
- [ ] Codex `spawn_agent` children render with the same card and pane,
      with token counts from the child thread.
- [ ] Top-level completions notify; nested completions do not.

## Migration/removal

| Old | New | Action |
|-----|-----|--------|
| Inline subagent group with capped body + top fade (`subagentGrouping.ts` group node, `AssistantMessage`/subagent card cap) | Shared agent card, expanded body on demand | MIGRATE (card first, then delete the cap/fade path) |
| Background completion card showing ack text as done | Same agent card with background pill | DELETE the ack-text rendering |
| `parse_system.go` skip of `task_progress`, default-drop of `background_tasks_changed` | Typed progress event + level set | MIGRATE (parse, emit, consume) |
| Codex `thread/tokenUsage/updated` suppressed for children (`collab_agents.go`) | Scoped child progress event | MIGRATE (unsuppress into the progress event, keep it off the parent's own meter) |
| Background tray filter `parent_id = ''` (display query only) | Tree listing by backgrounded ancestry | MIGRATE; reaper/queue gates KEEP their top-level filter |
| Anchor set `toolName in (Agent, Task)` | Provider-neutral launch predicate (Agent/Task, forked Skill, SendMessage-resume, Codex spawn_agent) | MIGRATE |

## Testing strategy

- Parser: fixtures for `task_progress`, `background_tasks_changed`, the
  `background_tasks` control round-trip (`patch:{is_backgrounded:true}`
  stays non-terminal), `can_use_tool` with `agent_id`, and the forked
  Skill sequence (no task_started, attributed rows, `status:"forked"`).
  Captures from the 2026-08-22 spikes become checked-in fixtures.
- Triage: scope inheritance for approvals (agent_id → launch; fallback
  row lookup), final-progress persistence at terminal, tray listing by
  backgrounded ancestry, Codex child progress scoping.
- Frontend: `subagentGrouping` tree tests for every launch kind and
  depth 3; card state tests (pills, breakpoint token hide); pane scope
  swap + breadcrumb + restore-with-missing-row; background-section
  ordering.
- Harness (e2e): one scenario per success criterion above, driven by the
  mock provider replaying the spike captures; the fan-out scenario
  asserts zero top-level rows from any child.
