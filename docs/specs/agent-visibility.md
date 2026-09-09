# Agent visibility design

Status: PARTIALLY IMPLEMENTED. Updated for direct command forks and live
transcript mirroring on 2026-08-24. Unchecked success criteria remain open.

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

## Immutable agent history

Completed chat history items are immutable, including their rendered contents.
A Codex spawn row records the spawn event and provides an open-pane button.
After that event is recorded, later activity must never change its fields,
metadata, status, timestamps, progress, preview or transcript contents. It is
not the reusable agent's runtime record.

The background tray represents the current execution. When that execution
finishes, its background entry disappears and a new completion item is added
at the completion's timeline position. Subsequent executions have distinct
completion items. Messages and signals appear where they happen; they do not
reactivate, enrich or attach themselves to earlier completed items. Each
completion card retains only its execution's history and settled values.

The separate agent pane contains the agent's continuous history across
executions, including correctly scoped nested-agent activity. Live status
belongs to the pane and background tray, independently of historical events.

The Codex spawn contract may change only if Codex fundamentally changes its
subagent model and the user explicitly authorizes the corresponding change.

## Key decisions

- Card = today's inline subagent card for every kind. Awaited vs background
  changes placement and tray membership, not a card pill. Its expanded
  digest is capped, virtualized, and faded at the top.
- Collapsed row: kind chip (`agent` / `skill`), name, state indicator,
  elapsed, tool count, current activity line; tokens when the row has
  room, hidden under a container-width breakpoint (Q1).
- The initial prompt is a plain user-side message row nested under the
  launch (ruling 2026-08-23), not a bespoke shape: `user_text` with
  `meta.wire_only`, so it renders as a user bubble with no edit / fork /
  resend actions and stays out of every reader-authored read (nav rail,
  title regeneration). Claude creates it from the Agent/Task launch input
  before child output as `user:subagent-prompt:<launchID>`. The inline echo
  or sidechain transcript later stamps the transcript uuid onto that row in
  place. Codex V2 records observed incoming NEW_TASK and MESSAGE deliveries as
  sender-attributed user rows in the recipient scope. Encrypted bodies show an
  explicit placeholder; readable text remains available. Message delivery does
  not imply that either agent is currently running. The canonical task path
  remains the fallback launch label.
- A Codex child's final answer is a NORMAL message, not a special block
  (ruling 2026-08-23). Its transcript streams to the parent parented to
  the launch, so the answer already renders in the card body and the
  pane as its own assistant row. The FINAL_ANSWER on the completion
  sibling is a 240-char preview and stays the COLLAPSED one-liner only.
  It was briefly rendered in the body too, which showed the same text
  twice, unformatted and cut mid-word.
- Expanded body is an allowlist (ruling 2026-08-23): the initial prompt
  (first `user_text`), tool call rows, a provider refusal's reason
  (`permission_denied` notification), error rows, and the final text.
  Thinking, intermediate prose, later prompts, progress chatter,
  compaction, retries, and child launches live in the pane. The digest is a
  capped virtualized inner timeline with the normal bottom-follow spring and
  reader escape. It never recursively embeds child agents in the main thread.
- Pane = companion kind `agent` with a scope (launch item id), rendered by the
  thread renderer filtered to direct `parent_id == scope` rows. A direct child
  launch appears as a normal agent row without its descendants.
  Opening a child from inside swaps the scope and pushes a breadcrumb
  (`main › code-review › Angle B`); no stacking (Q4, Q4b).
- Pane keeps the composer shell, non-interactive, with Stop (= kill
  where the wire can) as its only live control; background button and
  status/elapsed sit in the pane header beside the breadcrumb (Q20).
  While the agent runs, the shell's top row is the working chip: the
  agent's own spinner sprite / LED chase, verb and elapsed timer, keyed
  on the launch (ruling 2026-08-23, reversing the earlier "no run timer,
  no spinner" call); idle, the row stays as a height twin.
- Pane lifetime mirrors the review pane: persisted and restored, closed
  when the source thread changes, closes itself when the scoped row is
  gone on restore (Q5).
- Background section lists every node that is backgrounded or descends
  from one, indented by depth; a row click opens the pane and scrolls the
  timeline to the card. The row's explicit open button opens the pane
  only — the timeline jump is explicit navigation and releases
  bottom-follow, which a reader pinned to a streaming tail did not ask
  for (2026-08-31). Forks appear without a kill button (Q8).
- Background action: icon button on a running inline agent or Bash row
  (Claude only: `background_tasks` control_request by `tool_use_id`);
  no keyboard shortcut (Q9). Claude stops forwarding the node through the
  ordinary sidechain stream, but AO's always-on session mirror continues its
  pane live. Sessions started before mirror support fall back to terminal
  transcript recovery.
- Kill only where the wire can: Claude nodes with a task id
  (`stop_task`) and owned Codex child turns (`turn/interrupt`); never forks.
  A reusable Codex agent reads current execution metadata for its spinner,
  elapsed timer, waiting label, and Stop action, independently of older answers.
- A direct Claude slash command appears as a running Command row on its
  `command_lifecycle` started frame. If its mirror has ownerless
  `agent_metadata` and an `isSidechain:true` row carrying `attributionSkill`,
  the same row changes to Skill and owns the mirrored transcript. An ordinary
  Agent mirror carries its launch `toolUseId` in `agent_metadata`; AO leaves its
  duplicate mirror ignored while normal child stdout populates the Agent row.
  A mirror takes over only after backgrounding stops stdout, or below an
  already mirrored parent. Inherited inline-skill attribution does not change
  that ownership. Main-agent rows can carry the same attribution with
  `isSidechain:false` and leave the Command row alone. This uses wire evidence,
  not a maintained list of commands that may fork.
- A forked command's outer synthetic answer renders once after that activity
  as top-level Markdown labelled `<skill> · skill result`. It remains a
  system `command_result`, not parent-agent prose, and remains visible when
  the activity collapses. When that sourced result exists, the matching
  mirrored final assistant row remains in the agent pane but is excluded from
  the Skill card digest. Without a synthetic result, the digest keeps it.
- Live progress is in-memory UI state fed by Claude `task_progress`
  (tool count, tokens, elapsed, activity line) and, for Codex, the
  child thread's `thread/tokenUsage/updated` (unsuppressed into a scoped
  progress event) plus row counts. Codex final numbers are captured on
  each execution completion; the spawn row never receives them. Claude
  launch settlement retains its provider-specific progress handling.
- Attribution rule: anything a subagent causes carries its scope.
  `permission_denied` fixed (ce580f3f); `can_use_tool` approvals must
  resolve `agent_id` → launch tool_use (parser task map, triage row
  lookup as fallback) so the approval row nests under the card. The
  approval itself shows ONLY in the composer's approval UI, with no pill
  on the card, awaited or background (user ruling 2026-08-23 reverses
  Q10b).
- The bell is the timeline `notification` row and nothing else (no
  toast, no OS notification), and it fires for top-level nodes only;
  nested completions render without a bell (Q11). A parked async
  agent (claude-wire.md §E6b) rings it at every stop, and the frontend
  hides all of them together once the completed sibling lands.
- A DETACHED launch (async ack, `run_in_background`, a Codex spawn, a
  SendMessage resume carrier, or backgrounded mid-flight:
  `launchRunsDetached`) keeps the launch row it had before this feature,
  plus ONE approved addition, the open-in-pane door (ruling 2026-08-23):
  a Claude background launch is the compact agent row (robot icon,
  label, model, description, the `backgrounded` indicator, launch time;
  no ticker, no text pill, see c58f9b55), a Codex `spawn_agent` launch is
  the collab `launched` row. Neither changes after the spawn and neither
  is ever a card. Each execution's card (status, duration, tool count,
  tokens, the expandable transcript, open-in-pane) renders AT its
  completion sibling (`SubagentGroupNode.anchor`): top-level, inside the
  parent card for a nested node, or under the `wait_agent` group that
  claimed a Codex completion (`WaitGroupNode.children` are nodes), after
  everything the main thread wrote while the agent ran. A Codex card
  summarizes the completed execution; the answer is a normal message
  in its body. Later answer deliveries remain separate timeline events. While
  the agent runs there is no card: the pane and the tray are its live
  surfaces, and the tray row shows tokens plus only the latest direct tool
  call as its activity line. The bell is hidden on the strength of the completion
  rendering (`utils/notificationFilter.ts`), which is why the card sits
  at the sibling rather than folding it onto a card at the launch (the
  fold-and-drop version left the transcript with no trace of the agent
  finishing, regression 2026-08-22; tripwire
  `utils/backgroundCompletionVisibility.test.ts`). Awaited launches are
  unchanged: one card at the launch, completing in place.
- Row actions (open-in-pane, background, stop) render before the
  status / duration / timestamp columns on every row so the timestamp
  column stays aligned (`ToolHeaderMeta`'s `actions` slot; chat
  guide [rows and transcript identity](../../frontend/src/lib/components/chat/AGENTS.md#rows-and-transcript-identity)).
- The agent pane keys its whole scoped window as ONE turn
  (`ThreadPane.timelineTurns`, overridden by the scope facade): active
  while the scoped launch runs, settled on the launch's own completion
  with the agent's duration. A subagent's rows span however many
  provider turns it outlives, so keying the response divider/pill on the
  main thread's turn stamped "Response 1m 58s" on a still-running agent
  the moment the main turn settled (regression 2026-08-22).
- Forked skills are detected structurally: the first attributed sidechain row
  marks the fork; the completion's
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

- [x] A forked `code-review` renders as one `skill` card; none of its
      tool calls appear as the main agent's.
- [ ] A depth-2 background agent renders as a running card under its
      parent's card with live tool count and activity line, and in the
      background section indented under its parent.
- [x] Opening any card shows that node's full transcript in the agent
      pane; opening a child from inside swaps scope with a working
      breadcrumb; reload restores the pane to the same scope.
- [ ] Every agent card and pane opens with what the agent was asked to
      do, and an agent that is awaited inline streams its thinking,
      prose and final answer as it produces them, not only its tool
      calls.
- [ ] A subagent's `permission_denied` and `can_use_tool` rows nest
      under its card; the card shows the approval pill while the prompt
      is pending.
- [x] Background button on a running inline agent returns the main turn, the
      card flips to background, and the pane continues from session-mirror
      rows without waiting for task notification.
- [ ] Codex `spawn_agent` children render with the same card (at the
      completion point) and pane, counting the child thread's own
      cumulative spend (fresh input + cache writes + all output), which
      never goes backwards.
- [ ] A Codex child's answer appears exactly once, as its own message.
- [ ] Top-level completions notify; nested completions do not.

## Migration/removal

| Old | New | Action |
|-----|-----|--------|
| Unbounded expanded subagent digest | Capped virtualized digest with top fade and bottom follow | REPLACED |
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
