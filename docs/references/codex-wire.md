# Codex `app-server`: JSON-RPC wire reference

Authoritative reference for the JSON-RPC 2.0 notifications Codex
emits over stdio. Consulted by `internal/provider/codex/`
parser code.

Multi-agent shapes in this document were re-verified on 2026-07-09 against
the exact `rust-v0.144.0` tag (`767822446c...`) and a live
`codex-cli 0.144.0` MultiAgentV2 rollout. The local Codex checkout may be on
an older tag; use `git show rust-v0.144.0:<path>` rather than assuming its
worktree revision describes the installed binary.

Child-profile resolution was re-verified on 2026-08-29 against
`rust-v0.150.1`. The public V2 `SubAgentActivity` has no model or effort.
`thread/resume` returns both from the effective child `ThreadConfigSnapshot`.

## Sources

**Shape-of-truth, in priority order:**

1. **codex-source** at `/home/rmurphy/repos/codex`, the
   upstream Codex CLI (`codex-rs/`). Typed wire definitions live in
   `codex-rs/app-server-protocol/` (Rust source +
   generated TypeScript under
   `codex-rs/app-server-protocol/schema/typescript/`).
2. **CodexMonitor** (https://github.com/Dimillian/CodexMonitor), a Tauri
   client for codex app-server, authoritative for how to render the
   events we receive. See `src/features/threads/hooks/useAppServerEvents.ts`
   and `src/utils/threadItems.*.ts`.

**Capturing fresh samples**: run `make dev PROVIDER_DEBUG=1` (or
`make dev-wsl PROVIDER_DEBUG=1` on the WSL launcher path), or set
`AGENT_OVERFLOW_DEBUG=provider` directly before launching the app. Raw
stdio lines (pre-parse, JSON-RPC framing included) land in
`<dbDir>/logs/provider-events-YYYY-MM-DD.ndjson` with RFC3339Nano
timestamps.

## The two critical differences from Claude

### 1. Codex has no `run_in_background` flag

Unlike Claude's `Bash` + `run_in_background: true` pattern, **Codex
has no per-tool backgrounding flag**. Every tool is either:

- **Synchronous**: blocks the agent until `item/completed`.
- **Parallel**: multiple tools dispatched in one agent response run
  concurrently (for tools registered with
  `supports_parallel_tool_calls = true`, notably `shell`). The agent
  still waits for all of them to return before continuing.
- **Agent-spawning**: `spawn_agent` creates a child thread that runs
  on its own `thread_id`; the parent's `spawn_agent` tool_call
  completes immediately with `status: completed` while the child
  executes independently. This is the closest Codex analog to
  "backgrounded," but the lifecycle model is fundamentally different
  (see §Collab agent lifecycle below).

**But Codex does have background terminals**, just not via a flag on
items. `exec_command` (`source: "unifiedExecStartup"`) yields to the
model after `yield_time_ms` (default 10s) with whatever output
accumulated, and the PTY keeps running in `UnifiedExecProcessManager`.
The item stays `status: inProgress` until `spawn_exit_watcher` fires
`ExecCommandEnd`, potentially across multiple turns, up to
`background_terminal_max_timeout` (1h default). See
`codex-rs/core/src/tools/handlers/unified_exec.rs`,
`codex-rs/core/src/unified_exec/async_watcher.rs`.

**Implication for agent-overflow**:
`source: "unifiedExecStartup"` is the wire-typed signal that an item may
become a background terminal. Typed `item/completed` clears the transient live
tracker and is the history source for the command row, using the same item id,
only while a Codex wire round is active. Typed
`TerminalInteractionNotification` is the history source for the separate
waited/interacted marker rows. Raw `exec_command` function-call output is
model-facing text; it can enrich live metadata but must not gate or fabricate
chat history. Per
[invariant 25](../architecture/invariants.md#25-codex-backgrounding-uses-wire-typed-signals-never-heuristics),
heuristic classifiers (event-ordering, etc.) are forbidden because that's
what produced ghost rows in the former `BackgroundClassifier` (previously at
`internal/provider/codex/background.go`, retired).

On the wire, Codex items close via `item/completed` using the same
`item_id`. The status flips in place, no sibling row is emitted.
Agent Overflow follows that shape for Codex command executions when they are
persisted: no `tool_completion` sibling is synthesized for unified exec command completion.
See
[`codex.md §Background terminals`](codex.md#background-terminals)
for the per-row stop RPCs.

### 2. Items carry their own status on the wire

Each `item/*` notification includes a `status` field on the item
object directly (`inProgress | completed | failed | ...`). Unlike
Claude's "tool_use then tool_result" split, most Codex items are
one-shot upserts: `item/started` creates the row, `item/completed`
updates it. `unifiedExecStartup` command executions are the important
exception in Agent Overflow: starts are transient tray state, and completion
history is gated on an active Codex wire round.

This is the pattern CodexMonitor uses
(`useAppServerEvents.ts:467-495`). It dispatches on `method` but
always calls the same upsert handler. Adopting this pattern would
collapse half our Codex handling.

---

## Notification taxonomy

Every server → client envelope comes in two flavours. **Notifications**
(`{"jsonrpc":"2.0","method":"<method>","params":{...}}`) carry no
`id` and expect no response. **Server requests** carry a JSON-RPC `id`
and require a response. Approvals and MCP elicitation arrive this way,
not as notifications. Dispatched in
`internal/provider/codex/session.go` via the top-level read loop;
`handleServerRequest` handles the request flavour, `handleNotification`
handles the notification flavour.

### Notifications

Authoritative method list from
[`codex-rs/app-server-protocol/schema/typescript/ServerNotification.ts`](/home/rmurphy/repos/codex/codex-rs/app-server-protocol/schema/typescript/ServerNotification.ts).

| `method` | Destination / purpose |
|---|---|
| `turn/started` | Turn lifecycle. `EventTurnStart`. |
| `turn/completed` | Turn lifecycle. `EventTurnComplete`. |
| `turn/diff/updated` | Per-turn unified-diff snapshot. |
| `turn/plan/updated` | Per-turn plan updates (markdown). |
| `item/started` | Tool/item lifecycle. `classifyItemNotification` → `EventToolStart` (or drop). |
| `item/completed` | Tool/item lifecycle. `classifyItemCompleted` → `EventToolComplete` (or drop). |
| `item/agentMessage/delta` | Streaming assistant text. |
| `item/reasoning/textDelta`, `.../summaryTextDelta`, `.../summaryPartAdded` | Streaming reasoning. |
| `item/plan/delta` | Buffered by `appendPlanDelta`; surfaces on the completed plan item, never on the delta. Consumed INLINE, not by a classifier. |
| `item/commandExecution/outputDelta` | Streaming command output. |
| `item/commandExecution/terminalInteraction` | The wire-typed background-terminal signal (waited / interacted marker rows). See §Background terminals and invariant 25. |
| `item/fileChange/outputDelta`, `item/fileChange/patchUpdated` | Streaming patch progress. |
| `rawResponseItem/completed` | Raw response items: `spawn_agent` / `wait_agent` / `write_stdin` enrichment and the live mailbox carrier. Only available on a fresh `thread/start` with `experimentalRawEvents`. See §`<subagent_notification>`. |
| `item/mcpToolCall/progress`, `item/autoApprovalReview/started`, `item/autoApprovalReview/completed` | Recognised and dropped (consumed, so never opted out). |
| `autoApprovalReview/strictReviewRequired` | 0.149. Reachable only in the `auto` runtime mode: strict review replaced the cheap in-line assessment, so tool calls slow down. One warning row; the payload carries no reason. |
| `hook/started`, `hook/completed` | Hook lifecycle; one notification row each. |
| `thread/started` | Session-level. First notification on a new thread; emits `EventSessionInit`. |
| `thread/status/changed` | Session-level. Thread status transitions; emits `EventSessionStatus`. |
| `thread/archived`, `thread/unarchived`, `thread/closed` | Recognised, no event. |
| `thread/reverted` | The echo `thread/revert` waits on. Releases the RPC's bounded wait; an UNSOLICITED one is logged and never acted on (it carries a thread id and no boundary). See §History truncation in the package guide. |
| `thread/queue/changed` | 0.148. The thread's provider-side queue changed. `{threadId}` and nothing else: no depth, no item id, no text. Below 0.148 the classifier's own notice is the answer; on a queue-native session the session layer replaces it with a `thread/queue/list` diffed against AO's own client ids. See §Externally queued turns in the package guide. |
| `thread/compacted` | Thread housekeeping. Compaction boundary event (deprecated upstream in favour of the `contextCompaction` item; both feed `EventCompactBoundary`). |
| `thread/name/updated` | Thread housekeeping. Thread name/title changed. |
| `thread/tokenUsage/updated` | Thread housekeeping. Rolling CUMULATIVE token-usage snapshot; per-turn deltas are derived (`usage_accounting.go`). On a SPAWNED CHILD thread it is the one thread-wide notification not suppressed. It is re-scoped onto that spawn's live background projection as `EventSubagentProgress` and never meters the parent. See below. |
| `thread/settings/updated` | Codex's authoritative config echo. Reconciled into the session's observed snapshot; emits nothing. |
| `skills/changed` | Side channel. An EMPTY struct upstream (no cwd, no scope, no name), so the whole `internal/codexskills` cache is dropped rather than narrowed. |
| `account/rateLimits/updated` | Account-wide quota snapshot. Surfaced as `EventRateLimits` / `provider:usage action:"rate_limits"`. |
| `account/updated`, `account/login/completed` | Recognised, no event. A SESSION never signs in, so neither is acted on there; the sign-in connection is a separate process that opts out of everything except `account/login/completed` and settles on it. See §Account sign-in. |
| `model/rerouted` | Model reroute notice (Codex fell back to a different model). |
| `model/verification` | Model-verification warning row. |
| `model/safetyBuffering/updated` | Response held while OpenAI reviews the turn. Emits a notification row on the show edge only. |
| `mcpServer/startupStatus/updated` | Per-server MCP startup delta. Side channel to the App's status cache, not a transcript event. |
| `mcpServer/oauthLogin/completed` | Side channel: an MCP server finished its OAuth login. |
| `serverRequest/resolved` | A previously-sent server request (approval / elicitation) was resolved. `EventApprovalResolved`. |
| `error` | User-facing error state, not a log entry. |
| `warning`, `guardianWarning`, `configWarning`, `deprecationNotice` | Session-level notices surfaced to the user. |

Everything catalogued and NOT in this table is opted out at `initialize`
(see below). The split is derived, so this table is documentation of a
decision made in code, never its source.

**Opting out.** `initialize` accepts
`capabilities.optOutNotificationMethods: string[]`; Codex drops those
methods for that connection before serializing them
(`codex-rs/app-server/src/transport.rs`
`should_skip_notification_for_connection`). Matching is exact-string, so
an unrecognized entry is inert. Agent Overflow sends the complement of
what it consumes. See `internal/provider/codex/notification_catalog.go`,
whose catalogue is the `server_notification_definitions!` block at
`codex-rs/app-server-protocol/src/protocol/common.rs` @ **rust-v0.149.0**
(77 entries). A method upstream adds and the catalogue has not listed is
never opted out: it arrives, no classifier claims it, and the per-session
drift log names it once.

⚠ **Wire-name gotchas.**

- It's `thread/started` (NOT `thread/created`).
- It's `thread/status/changed` (NOT `thread/status_changed`).
- It's `account/rateLimits/updated` (NOT `rate_limit/warning`).
- There is no `item/updated` method. Any code that dispatches for
  `item/updated` is dispatching on a phantom method; the two item
  notifications on the wire are `item/started` and `item/completed`.

Detailed param shapes live in
`codex-rs/app-server-protocol/schema/typescript/v2/`. Read that when
adding handlers; the TypeScript schema is the canonical shape
reference.

### `thread/tokenUsage/updated`

This is the Codex context-meter signal. Treat `tokenUsage.last` as the
current context-window occupancy and `tokenUsage.modelContextWindow` as
the max window:

```json
{
  "tokenUsage": {
    "last": {"inputTokens": 100, "outputTokens": 20, "cachedInputTokens": 6, "totalTokens": 126},
    "total": {"inputTokens": 9000, "outputTokens": 2000, "cachedInputTokens": 839, "totalTokens": 11839},
    "modelContextWindow": 258400
  }
}
```

`total.totalTokens` is the CUMULATIVE spend for that thread; `last` is
the current context occupancy. The two are not interchangeable: the
context meter reads `last`, and anything reporting "how much has this
cost" reads `total`.

**On a spawned child thread.** Every other thread-wide notification from
a collab child is suppressed so a subagent cannot overwrite the parent's
meter, title, or compact state (ADR-002). This one is not: Agent
Overflow intercepts it and emits a `EventSubagentProgress` scoped to the
spawn's `parentToolUseID`, carrying the child's provider thread id as
`taskId` and, as the progress total, `total.inputTokens -
total.cachedInputTokens + total.cacheWriteInputTokens +
total.outputTokens`, the child's cumulative spend with every token
counted once, monotonic because each term is a provider cumulative. Not
`total.totalTokens`: that re-counts the cached prompt on every round,
which on a long child is an order of magnitude above the agent's own
spend and grows with round count rather than with work. It never reaches
the parent's usage accounting and is never emitted as `EventTokenUsage`.
This is the only channel through which a child's usage is visible on the
parent thread. AO's suppression and carve-out rules are in
[`internal/provider/codex/AGENTS.md` §Child threads](../../internal/provider/codex/AGENTS.md).

### `account/rateLimits/updated` and `account/rateLimits/read`

Re-verified against the installed `codex-cli 0.144.0`: the canonical quota
shape is unchanged. `rateLimits.primary` is the 300-minute window and
`rateLimits.secondary` is the 10,080-minute window; each carries
`usedPercent`, `windowDurationMins`, and Unix-seconds `resetsAt`.
`account/rateLimits/read` additionally returns `rateLimitsByLimitId`; Agent
Overflow selects only its canonical `codex` bucket and ignores model-specific
buckets that share the same window durations.

The backend retains the last normalized snapshot per provider, merging by
`windowDurationMins` because Claude reports its 5h and 7d windows separately.
The cache rejects older reset boundaries and same-window lower readings just
like the frontend store, so a delayed session event cannot regress hydration.
Frontends call `GetRateLimitsSnapshots` after installing the live
`provider:usage` listener and again when that channel reports a transport gap.
This is necessary because a startup account probe can complete before the
first websocket connection has a prior channel sequence to replay, and a
long-lived client can reconnect after the event ring has rolled over.

`last.totalTokens` is what occupies the visible context window. The
rolling `total.totalTokens` value is aggregate processed/spend-style
accounting across messages and must not be shown as context used in the
meter. Keep that aggregate out of the context-meter payload.

That aggregate IS the turn-accounting source, though. Codex has no
per-turn usage signal (`turn/completed` carries no token fields) and no
USD cost anywhere on the wire, so per-turn usage is the delta of
`total` between turn boundaries. Verified in codex-rs source: `total`
accumulates via `TokenUsageInfo::append_last_usage` `add_assign` and
never resets: compaction's `recompute_token_usage` rewrites only
`last`, and resume seeds `total` from the rollout's last TokenCount.
The one exception is `fill_to_context_window` (the
ContextWindowExceeded sentinel), which pegs `total.totalTokens` to the
window and zeroes the components, so deltas across that event are
garbage. Also note wire `inputTokens` INCLUDES `cachedInputTokens`
(`TokenUsage::non_cached_input` subtracts). All of this is owned by
`internal/provider/codex/usage_accounting.go`.

`TokenUsageBreakdown` grew a sixth component after 0.142.5:
`cacheWriteInputTokens`, verified 2026-08-02 in the installed
`codex-cli 0.146.0` (its embedded `TokenUsageBreakdown.ts` binding lists
`totalTokens`, `inputTokens`, `cachedInputTokens`,
`cacheWriteInputTokens`, `outputTokens`, `reasoningOutputTokens`) and in
`codex-rs/protocol/src/protocol.rs` at tag `rust-v0.146.0`, where
`TokenUsage::add_assign` accumulates it like every other component and
`non_cached_input` does NOT subtract it. It is a billed class of its own
and maps onto the shared `TokenUsage.CacheCreationInputTokens`. The
local `/home/rmurphy/repos/codex` checkout is pinned at 0.142.5 and
predates the field. Check the installed binary before concluding a
field does not exist.

Live-verified 2026-07-03 against `codex-cli 0.142.5` (three turns across
a fresh thread + a `thread/resume`, spike per spike-policy; raw capture
not checked in per the rule below):

- The final `thread/tokenUsage/updated` of a turn arrives BEFORE
  `turn/completed` (3/3 turns), so the accounting snapshot at
  turn-complete is complete, no rollover needed in practice.
- `turn/completed.turn` carries exactly `{completedAt, durationMs,
  error, id, items, itemsView, startedAt, status}`, with no usage fields.
- `total` grew 12044 → 24106 → 36186 across turns and the resumed
  process's first reading matched the prior process's final total
  exactly (cumulative persists across resume, as the source promised).
- After `thread/resume`, a seed `thread/tokenUsage/updated` carrying
  the historical cumulative arrives BEFORE any turn (between
  `thread/status/changed` and `thread/goal/cleared`), so the
  accounting's pre-turn baseline path is the live path and the
  skip-first-resumed-turn fallback is a backstop only.
- No cost / USD / dollar field appears in any notification.
- `inputTokens` includes `cachedInputTokens` on the live wire
  (in=12039, cached=9600, out=5, total=12044).

### Server requests (approvals, tool-user-input, elicitation)

Approvals arrive as **server requests** (with a JSON-RPC `id`), not as
notifications. The client is expected to respond with a matching
`id`. Authoritative list from
[`codex-rs/app-server-protocol/schema/typescript/ServerRequest.ts`](/home/rmurphy/repos/codex/codex-rs/app-server-protocol/schema/typescript/ServerRequest.ts):

| `method` | Purpose |
|---|---|
| `item/commandExecution/requestApproval` | Approve/deny a shell command execution. |
| `item/fileChange/requestApproval` | Approve/deny a write/apply_patch. |
| `item/permissions/requestApproval` | Approve/deny a permission grant. |
| `item/tool/requestUserInput` | Tool is requesting structured user input. |
| `item/tool/call` | Dynamic tool-call request. |
| `mcpServer/elicitation/request` | MCP server-side elicitation. |
| `account/chatgptAuthTokens/refresh` | ChatGPT auth token refresh request. |
| `applyPatchApproval` | Legacy apply-patch approval. |
| `execCommandApproval` | Legacy exec-command approval. |

Dispatch lives in `handleServerRequest` in
`internal/provider/codex/session.go`. Once the client responds, Codex
fires a `serverRequest/resolved` notification so the original request
can be garbage-collected on both sides.

---

## `item/started` and `item/completed`

Every `ThreadItem` subtype has its own shape
(`v2.rs:4443+` in codex-source). The envelope wrapper:

```json
{"method": "item/started",
 "params": {
   "threadId": "<thread_id>",
   "turnId": "<turn_id>",
   "item": { "type": "<itemType>", "id": "<call_id>", "status": "inProgress", ... }
 }}
```

### Item types handled by `classifyCodexItemType` (protocol.go:648)

| Wire `type` | Item kind | Notes |
|---|---|---|
| `userMessage` | `user_text` | User-submitted message. |
| `agentMessage` | `assistant_text` | Assistant text response. |
| `assistantMessage` | `assistant_text` | Alias. |
| `reasoning` | `thinking` | Model reasoning block. |
| `commandExecution` | `tool_call` | `shell` / `local_shell` bash. |
| `mcpToolCall` | `tool_call` | MCP-provided tool. |
| `webSearch` | `tool_call` | Built-in web search. |
| `fileChange` | `tool_call` | `write` / `apply_patch`. |
| `plan` | `plan` | Plan updates (markdown). |
| `todoList` | `todo_list` | Todo list updates. |
| `collabAgentToolCall` | `collab_agent` | Subagent spawn/wait/control. |
| `error` | `error` | Runtime error row. |

### Dropped `item/*` events (intentional)

`protocol.go:227-232` drops these explicitly:
- `item/commandExecution/terminalInteraction`
- `item/mcpToolCall/progress`
- `item/autoApprovalReview/started` and `/completed`
- `item/reasoning/summaryPartAdded`

They're progress/transient signals that would bloat the item
timeline. Card-level updates come through `item/started` +
`item/completed` upserts.

### `userMessage` is promoted, not dropped

`userMessage` lives in the `nonToolItemTypes` deny-list because we
must NOT settle a tool_call row for it, but it is no longer silently
dropped on `item/completed`. The classifier promotes the wire echo
to `EventUserText` carrying `meta.provider_item_id = item.id`, so
triage's pending-send correlator can stamp the AO-owned
`user:<turnIndex>` row when an AO-initiated send round-trips,
or when a future cascade injection (the Codex equivalent of
Claude's `task_notification` echo, e.g. an MCP-injected user
input) lands. The `item/started userMessage` half is still
dropped: there is no UI signal for the in-flight envelope.
Mirrors the Claude side's `isReplay:true` promotion in
`internal/provider/claude/parse_user.go`.

### `status` values (per ItemType)

Each ItemType has its own status enum:
- `CommandExecutionStatus`: `inProgress | completed | failed`
- `McpToolCallStatus`: `inProgress | completed | failed`
- `CollabAgentToolCallStatus`: `inProgress | completed | failed`
- `CollabAgentStatus` (child-agent state): `pendingInit | running | interrupted | completed | errored | shutdown | notFound`

Enum values are `camelCase` on the wire (`#[serde(rename_all =
"camelCase")]` in Rust).

---

## Collab agent lifecycle (MultiAgentV1 and MultiAgentV2)

The closest Codex analog to Claude's backgrounded tools, but
structurally different: **a spawn creates a child thread**, not a
backgrounded process inside the parent tool call. Agent Overflow
projects this into the shared background UI when that child is still
non-terminal after the parent turn closes.

### Versioned wire shapes

Codex currently has two collaboration transports. Agent Overflow accepts both
and normalizes them before triage:

| Operation | MultiAgentV1 typed item | MultiAgentV2 typed item |
|---|---|---|
| spawn | `collabAgentToolCall`, `tool:"spawnAgent"`, start + complete | `subAgentActivity`, `kind:"started"`, where the completed leg is the signal |
| send/follow-up | `collabAgentToolCall`, `tool:"sendInput"` | `subAgentActivity`, `kind:"interacted"`, where the completed leg is the signal |
| interrupt/close | `collabAgentToolCall`, `tool:"closeAgent"` | `subAgentActivity`, `kind:"interrupted"`, where the completed leg is the signal |
| wait | `collabAgentToolCall`, `tool:"wait"`, receivers/statuses | `collabAgentToolCall`, `tool:"wait"`, empty receiver/status maps |
| list | model-facing raw function call only (no item) | model-facing raw function call only (no item) |

### Client-side child stop

Verified against `rust-v0.150.1`. App-server has no client `close_agent` or
`interrupt_agent` RPC. Those names are model collaboration tools. A client
stops live child work with the existing `turn/interrupt` request:

```json
{"threadId":"<child provider thread id>","turnId":"<active child turn id>"}
```

An empty `turnId` is the typed startup-interrupt form. App-server submits the
same core interrupt but responds immediately because no `TurnAborted` event
exists to await before startup finishes. Agent Overflow therefore records the
active child turn id from child-scoped `turn/started`, resolves the UI launch
id through typed ownership, and emits an interrupted child status after a
successful request. It never accepts a provider thread id from the UI.

Closing the app-server aborts its active tasks, including child tasks. The
child thread identity and persisted history remain resumable. A client restart
must clear the old live-work projection without deleting spawn ownership or
fabricating a child completion result.

⚠ **V2's two messaging verbs are indistinguishable on the typed wire.**
`send_message` (QueueOnly: queues into the child's mailbox, starts no turn) and
`followup_task` (TriggerTurn: starts a new child turn) share one handler path
(`core/src/tools/handlers/multi_agents_v2/message_tool.rs`) and both end in a
single `kind:"interacted"` item with no verb field. The ONLY signal is the raw
function-call `name` on `rawResponseItem/completed`, which is live-only: a
resumed session never sees it. Agent Overflow persists it on the standalone
`send_input` activity row as `input.activityTool`, and labels the operation
neutrally when it is absent. It must never be inferred from whether a child
turn followed. That is
exactly the ordering heuristic
[invariant 25](../architecture/invariants.md#25-codex-backgrounding-uses-wire-typed-signals-never-heuristics)
forbids.

Namespacing (`features.multi_agent_v2.tool_namespace`, default
`"collaboration"`) does not change the name: the raw function call carries the
bare name plus a separate `namespace` field
(`{"name":"send_message","namespace":"collaboration"}`).

Every V2 activity item arrives as a started/completed pair on the wire (see
[§MultiAgentV2 spawn normalization](#multiagentv2-spawn-normalization)); the
started leg is dropped, so the completed leg is the only one that produces
events.

The V2 item is:

```json
{
  "id": "call_spawn",
  "type": "subAgentActivity",
  "kind": "started",
  "agentThreadId": "<child-thread-id>",
  "agentPath": "/root/reviewer"
}
```

`kind` is `started | interacted | interrupted`. The `id` is the function
call id. For `started`, it is also the stable parent spawn-card id; for the
other kinds it is the control call's own id. `agentThreadId` is the routing
identity and `agentPath` is the canonical task path. V2 spawn output normally
returns `{"task_name":"/root/reviewer"}` and may intentionally hide nickname
metadata; the activity item, not the raw output, is the ownership source.

V2 core creates and starts the child before emitting `subAgentActivity`, so
the child's `thread/started`, `turn/started`, or transcript deltas can arrive
first. The session adapter must quarantine every unmapped non-root provider
thread and replay it only after this typed ownership edge arrives. Falling
through to the AO root is forbidden: child turn starts reset root stream
counters, and child turn completions falsely close the root round. The
quarantine has per-thread, total-count, byte, and thread-id bounds. If typed
ownership does not arrive within ten seconds, queued approvals are rejected,
the child events are dropped, and one visible session warning is emitted.
The quarantined `thread/started` display metadata is cached separately so the
event remains unroutable while its nickname can still enrich the spawn row on
first paint once ownership arrives.

This rule is recursive. A `subAgentActivity kind:"started"` received on a
known child thread creates a nested edge from that child's spawn item to the
grandchild provider thread. Grandchild output is scoped to the nested spawn
card; if its edge is late, it remains quarantined and never reaches the root.

### The MultiAgentV1 `CollabAgentTool` enum

Defined at `codex-rs/app-server-protocol/src/protocol/v2.rs:4977`:

```
CollabAgentTool = "spawnAgent" | "sendInput" | "resumeAgent" | "wait" | "closeAgent"
```

⚠ **The wire value for "wait" is `"wait"`, NOT `"waitAgent"`.**
`protocol.go` normalizes that to `wait_agent` and routes it as a distinct
itemType; keep accepting the older `"waitAgent"` spelling only as a
defensive alias.

### MultiAgentV1 spawn flow (parent thread)

1. Agent emits a `FunctionCall` for `spawn_agent`.
2. `item/started` fires with `type: "collabAgentToolCall"`,
   `tool: "spawnAgent"`, `status: "inProgress"`.
3. **The child thread is created** (new `thread_id`).
4. `item/completed` fires **immediately** with `status: "completed"`,
   carrying `receiverThreadIds: ["<child_thread_id>"]` and
   `agentsStates: {"<child_thread_id>": "running"}`.

**The parent's `spawn_agent` tool_call is CLOSED at this point.**
The agent work on the child thread continues independently,
emitting its own `turn/started`, `item/*`, `turn/completed`
notifications on a separate `thread_id`.

The child thread metadata, not the parent `CollabAgentToolCall`, is the
authoritative source for display labels. Codex core's spawn-end event
knows `new_agent_nickname` and `new_agent_role`, but app-server's
`ThreadItem::CollabAgentToolCall` does not currently expose those fields.
After the typed spawn completion gives Agent Overflow the
`receiverThreadIds`, the provider adapter reads the child with
`thread/read` and merges `thread.agentNickname` / `thread.agentRole` back
onto the parent spawn row as a metadata-only update. Wait rows then reuse
that receiver-thread label map.

With `thread/start.experimentalRawEvents=true`, app-server builds also
emit `rawResponseItem/completed` for the model-facing function call and its
tool output. These raw items can carry the same label metadata:

```json
{"type":"function_call","name":"spawn_agent","call_id":"call_spawn","arguments":"{\"agent_type\":\"explorer\",\"message\":\"Inspect parser\"}"}
{"type":"function_call_output","call_id":"call_spawn","output":"{\"agent_id\":\"child-thread\",\"nickname\":\"Boyle\"}"}
```

Agent Overflow treats the typed `item/*` lifecycle as authoritative for the
visible tool row. `thread/read` is the primary label source. A metadata-only
child `thread/resume` is the profile source. Raw response items are only an
additional typed signal when present, not a prerequisite.

### MultiAgentV2 spawn normalization

Core's `emit_sub_agent_activity`
(`codex-rs/core/src/tools/handlers/multi_agents_v2.rs`) fires BOTH
`item/started` and `item/completed` for every `subAgentActivity` item, since
codex 0.146 for all three kinds. (Read at tag `rust-v0.146.0` via `git show`;
the local reference checkout's working tree may sit on an older tag, and 0.142.5
has no `emit_sub_agent_activity` and emits only `ItemCompleted` from
`event_mapping.rs`, which is also why dropping the started leg is a no-op
below 0.146.) Only the completed leg carries meaning here:
Agent Overflow drops the started leg outright and expands a canonical
`kind:"started"` completion into the normalized spawn start +
completion pair used by the existing projector. Routing the started leg as a
tool row instead would mint a raw `subAgentActivity` tool_call, transient for
`started` / `interacted` (the completion upserts the same item id) but
permanent for `interrupted`, whose completion is a status event that never
settles the row. The normalized completion
contains the authoritative receiver thread and a running `agentsStates`
entry, because successful emission occurs only after core has spawned the
child. This is a typed authorization signal, not an ordering heuristic.

MultiAgentV2 marks `spawn_agent.message`, `send_message.message`, and
`followup_task.message` as encrypted tool parameters. Raw response items carry
opaque model-service ciphertext in those fields; clients cannot decrypt it and
must never normalize it as a plaintext prompt. Safe raw fields such as target
and explicit role may enrich the row. Raw model and effort are requested
values, not the effective child profile, so they must not populate the model
badge. On codex 0.149.0 a V2
`spawn_agent` in practice carries only `{task_name, fork_turns, message}` and
its output only `{task_name}`, with no nickname and no agent_type, so the
model-chosen `task_name` is the whole plaintext statement of what the child
was asked to do, and a display label derived from it is the same string
twice. The canonical activity,
cached `thread/started`, and `thread/read` metadata provide path and display
label. They do not provide the effective model or effort. Agent Overflow sends
`thread/resume {threadId, excludeTurns:true}` with no overrides after typed
ownership arrives. The response's top-level `model` and `reasoningEffort`
describe the child after Codex has applied explicit requests, defaults, and
role configuration. Each nested child is queried independently. No child
inherits a displayed profile from its parent. On resume, raw events are
unavailable, so the same response repairs active child profiles without
replaying turns.
`thread/resume` history is scanned for both V1 spawn items and V2 started
activities to rebuild ownership without replaying duplicate transcript rows.
A sequential, session-cancellable worker inspects each unresolved child with
`thread/read {includeTurns:false}` and one latest-turn query when needed. It
resumes only children whose reported status is `active`, using
`excludeTurns:true` to restore the live subscription and recover the effective
profile. The queue bounds concurrency without an arbitrary child-count limit.
A fresh `Session.Resume` starts a new traversal generation. Transient
reads/resumes retry, and conflicting or self-referential ownership is rejected.

Known child `turn/started` is normalized to a launch-keyed running status. This
reactivates a previously completed child's background projection when `followup_task`
starts another turn; its later `turn/completed` marks that launch inactive
again. Neither lifecycle event is allowed to mutate the root turn.

### Parent learning the child finished

Two paths:

**(a) Explicit `wait` tool**: in V1 the parent agent calls `wait` with a list
of child `thread_id`s. V2 `wait_agent` instead waits for any mailbox/input
queue activity and accepts only `timeout_ms`. Both emit
`CollabWaitingBegin` → `CollabWaitingEnd` (which surface as
`item/started` + `item/completed` for `tool: "wait"`,
`receiverThreadIds` on the item, and in V1 `agentsStates` populated
with per-agent terminal status on the end event).

The raw `wait_agent` function call is the most stable source for the
target list, especially when the wait times out and the typed
`item/completed` envelope comes back with empty `receiverThreadIds`:

```json
{"type":"function_call","name":"wait_agent","call_id":"call_wait","arguments":"{\"targets\":[\"child-thread\"],\"timeout_ms\":10000}"}
{"type":"function_call_output","call_id":"call_wait","output":"{\"status\":{},\"timed_out\":true}"}
```

When the wait observes completion, the raw output has
`"timed_out":false` and `status` carries the terminal child result.
The typed `item/completed` usually also carries `agentsStates`, and that typed
state remains the V1 source used to synthesize its completion row. V2 completes
with empty `receiverThreadIds` and `agentsStates`; child `turn/completed`
updates only the launch's live status. Its result does not enter parent history
until the mailbox is drained into parent context.

MultiAgentV2 persists that delivery in the parent rollout. **On codex 0.147 it
is TWO consecutive records, not one.** A bare `inter_agent_communication`
record carrying `author` / `recipient` / `content` is a legacy/replay shape and
is still accepted, but it is not what a current rollout contains:

```json
{"type": "inter_agent_communication_metadata", "payload": {"trigger_turn": false}}
{"type": "response_item",
 "payload": {
   "type": "agent_message",
   "id": "amsg_01a020e3-...",
   "author": "/root/reviewer",
   "recipient": "/root",
   "content": [{"type": "input_text",
                "text": "Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/reviewer\nPayload:\n<child result>"}],
   "internal_chat_message_metadata_passthrough": {"turn_id": "<RECEIVING PARENT TURN ID>"}}}
```

The metadata record carries only `trigger_turn`; the envelope itself is the
`response_item`, and the two are matched by adjacency, not by an id.

#### `Message Type:` is the classifier, and it has three values

The envelope's own first line is the only wire-typed signal for what a delivery
means. Three types are observed:

| Type | Direction | Meaning |
|---|---|---|
| `FINAL_ANSWER` | child → parent | The child's terminal answer. **The transcript-completion boundary.** |
| `MESSAGE` | child → parent | A mid-run progress note (`send_message`, QueueOnly). The child is still running. |
| `NEW_TASK` | parent → child | Task assignment. Appears in the CHILD's rollout with the child path as `recipient`. |

A child → parent envelope always has `recipient: "/root"` and
`Task name: /root`, which is what keeps `NEW_TASK` out of the parent's
completion path. Treating any delivery as terminal without reading this header
marks a still-running child as finished.

#### ⚠ `internal_chat_message_metadata_passthrough.turn_id` is the RECEIVING PARENT turn

It is **not** the child turn, and it is **not** a delivery identity. Every
delivery drained into one parent turn carries the same value. Corpus proof: a
parent rollout with two distinct `FINAL_ANSWER`s from one child, 3.5 minutes
apart, both stamped `01a020d1-a06b-7b71-9791-749c71f19cd7`; and another whose
ten `MESSAGE` deliveries from four different children all share
`01a02202-9b32-76b3-872f-4bd409b794d3`. Keying a completion row on it collapses
every same-turn delivery onto one row and silently loses all but the last
(the bug fixed by `interAgentContentDeliveryID` in `subagent_notifications.go`).
Delivery identity is content: agent path, message type, payload text, and a
digest of the non-text content blocks.

#### Encrypted envelopes carry two content blocks

`send_message.message` and `followup_task.message` are encrypted tool
parameters, so an envelope's `content` is commonly
`[{"type": "input_text", ...}, {"type": "encrypted_content", ...}]`: the
plaintext half is the header and stops at `"Payload:\n"`, and the body never
leaves the ciphertext. A parser that requires exactly one text block sees none
of these. Two `MESSAGE` deliveries from the same sender therefore have byte-
identical plaintext, which is why the ciphertext block has to be folded into the
delivery digest for them to stay distinct.

That envelope, not child `turn/completed` and not `wait_agent` returning, is the
MultiAgentV2 transcript-completion boundary, and only for `FINAL_ANSWER`. Agent
Overflow emits one flat completion row per DELIVERY (a child that answers twice
in one parent turn produces two rows); a `MESSAGE` delivery produces no
completion row, but does produce its own chronological `send_input` progress
row. Fresh
sessions see the model-input projection as a raw `agent_message` response item;
resumed sessions see the durable record above. Older rollouts that persisted the
projected response item remain supported.

**(b) Legacy implicit via `<subagent_notification>`**: When a detached
child finishes and the parent has NO `wait` outstanding, Codex core
enqueues a mailbox notification for the parent. The parent sees it when
the mailbox item is accepted into pending input for a later parent turn.
Current Codex renders this as contextual user state; with
`thread/start.experimentalRawEvents=true`, app-server exposes that
boundary as a `rawResponseItem/completed` message item with `role:
"user"` and a single `input_text` block:

```json
{
  "type": "message",
  "role": "user",
  "content": [{
    "type": "input_text",
    "text": "<subagent_notification>{...}</subagent_notification>"
  }]
}
```

`thread/resume` does not currently expose an `experimentalRawEvents`
opt-in. After Agent Overflow restarts onto an existing Codex thread, the
same parent-observation boundary is therefore read from the active
rollout JSONL instead: an appended `response_item` record whose
`payload` is the message item above. The observer starts at EOF during
session construction, so old notifications in history are not replayed.

Some traces and rollout tooling can also expose the same parent-mailbox
delivery as a serialized `InterAgentCommunication` in an
assistant/commentary raw message:

```json
{
  "type": "message",
  "role": "assistant",
  "phase": "commentary",
  "content": [{
    "type": "output_text",
    "text": "{\"author\":\"/root/researcher\",\"recipient\":\"/root\",\"other_recipients\":[],\"content\":\"<subagent_notification>{...}</subagent_notification>\",\"trigger_turn\":false}"
  }]
}
```

The wrapper's `content` field includes:

```
<subagent_notification>
{"agent_path":"<child_thread_reference>","status":"completed"}
</subagent_notification>
```

Older/replay paths may also expose the same fragment inside a
`userMessage` `item/completed` carrier. Agent Overflow handles these
observed carriers, but the raw mailbox / rollout response-item carrier
is the preferred signal because it corresponds to the parent accepting
the context, not merely the child reaching terminal state.

Test coverage:
`codex-rs/core/tests/suite/subagent_notifications.rs:274-296`.

### Parent turn vs child lifecycle

The parent's `turn/completed` fires **without waiting** for spawned
children. The spawn item's `agentsStates` tells us whether the child
was still running when spawn completed; Agent Overflow stamps the
parent's spawn row `is_background=true` at parent turn completion if
that state is still non-terminal. Child runs independently on its own
thread id, producing its own `turn/completed` stream relayed through
the session's child/parent map.

### Child thread rejoining

When events arrive on a known child `thread_id`, our session maps
them back to the parent's `spawn_agent` card via `childParentByThread`.
Child `turn/started` is suppressed. Child `turn/completed` becomes only
`EventSubagentStatus`; it never becomes a parent turn boundary. Child
thread-wide state (token usage, name, status, model reroutes, plans, user
message echoes) is suppressed so it cannot overwrite the root projection.
Transcript-bearing child events are relayed via `ParentToolUseID`. Fatal child
errors are downgraded to scoped, non-fatal error rows and terminal child
status; they cannot close the root turn.

### `agentsStates` field on spawn/wait cards

The parent's `spawn_agent` item carries `agentsStates`, a map of
`thread_id → CollabAgentStatus`. Updated on the item envelope as
state changes. Surfaced by `enrichItemMeta` in `protocol.go` (the
`collabAgentToolCall` branch copies it into `extras.input.agentsStates`
alongside `tool` / `prompt` / `receiverThreadIds`) for launch correlation and
background projection. The immutable timeline spawn row does not render that
live child state. See
`CodexMonitor/src/utils/threadItems.collab.ts:299-369` for the
reference rendering.

---

## `turn/started`

```json
{"method": "turn/started",
 "params": {
   "threadId": "...",
   "turn": {
     "id": "...",
     "items": [],
     "status": "inProgress",
     "error": null,
     "startedAt": 1777926299,
     "completedAt": null,
     "durationMs": null
   }
 }}
```

Emits `EventTurnStart`. `session.go` dedupes on `turn.id` via
`seenTurnStarts`, which is safe for reconnect replay.

---

## `turn/completed`

```json
{"method": "turn/completed",
 "params": {
   "threadId": "...",
   "turn": {
     "id": "...",
     "items": [],
     "status": "completed",
     "error": null,
     "startedAt": 1777926299,
     "completedAt": 1777926306,
     "durationMs": 6637
   }
 }}
```

### `turn.status` values
`completed | interrupted | failed | inProgress` (per
`v2/TurnStatus.ts`).

### No assistant message id

`turn/completed` does **not** carry `lastAssistantMessageId`. The current
`codex-cli 0.128.0` wire and upstream schema both define the payload as
`{threadId, turn}` only:

- `/home/rmurphy/repos/codex/codex-rs/app-server-protocol/src/protocol/v2.rs`
  `TurnCompletedNotification`
- `/home/rmurphy/repos/codex/codex-rs/app-server-protocol/schema/typescript/v2/TurnCompletedNotification.ts`

The adapter therefore leaves `WireTurnCompleteMeta.AssistantMessageID`
empty for Codex turn completion.

### Emission

`classifyTurnCompleted` emits `EventTurnComplete` with a typed
`provider.WireTurnCompleteMeta`. The adapter normalizes Codex's
`turn.status` before it crosses the provider boundary:
`completed -> stop_reason=end_turn`, `failed -> stop_reason=error`,
and `interrupted -> stop_reason=interrupted, aborted=true`.

---

## `turn/steer` and client message identity

`turn/start` and `turn/steer` both take `clientUserMessageId`
(`Option<String>` on both params structs since 0.136, below AO's 0.143
floor, so no version gate). The `userMessage` ThreadItem the turn produces
echoes it back as `clientId` (`ThreadItem::UserMessage`,
`codex-rs/app-server-protocol/src/protocol/v2/item.rs:236` @ rust-v0.149.0),
which is how a caller matches an echo to the row that produced it without
relying on ordering. Send no key rather than an empty one: upstream mints its
own uuid for a producer that supplies none, so an explicit empty string is a
value no echo can ever match.

`turn/steer` takes no config fields, so an in-flight turn cannot be
reconfigured. It requires a non-empty `expectedTurnId`, which
`turn_processor.rs` checks before the request reaches the session.

**Three refusals share one JSON-RPC code.** All arrive as -32600
`invalid_request`, so the code discriminates nothing and the payload has to
be read (`classifySteerRejection`, `session_turn.go`).

| upstream `SteerInputError` | recognised by | meaning |
|---|---|---|
| `NoActiveTurn` | message `no active turn to steer` | race: the turn ended |
| `ExpectedTurnMismatch` | message ``expected active turn id `X` but found `Y` `` | race: a new turn started |
| `ActiveTurnNotSteerable` | `error.data`'s `codexErrorInfo` is `{"activeTurnNotSteerable":{turnKind}}` | state: a review or compaction turn is running |

The first two are the same race between reading the active-turn registry and
the steer arriving, and the recovery is to open a fresh turn. The mismatch
message names the turn id upstream found, but retrying against it is worse
than a fresh turn: by the time the answer is read that id can have rolled
again, and the message was not written for it.

The third is a different state and must never be folded into the race. A turn
IS running (`review/start` or `thread/compact/start`) and simply cannot take
input, so the message waits for the next turn boundary rather than opening a
second turn that would interleave with the review. It is also the only one of
the three upstream attaches structured data to, which is why a client must
keep `error.data` verbatim: without it, "not steerable" is separable from the
two races only by its English sentence.

## Session / thread state

### `thread/started`

First notification on a session. Carries the thread id and initial
metadata.

### `thread/status/changed`

```json
{"method": "thread/status/changed",
 "params": {
   "threadId": "...",
   "status": {"type": "active"},
   "activeFlags": ["runningBackground", "waitingForUser"]
 }}
```

### `Thread.status.type` values
`notLoaded | idle | active | systemError`.

### `activeFlags` values (observed)
`runningBackground`, `waitingForUser`. Currently read by
`Session.Probe` but not surfaced as turn-state signals (correct, per
see [`turn-lifecycle.md`](../architecture/turn-lifecycle.md) on why
we don't infer turn activity from session status).

### `approvalsReviewer` (who answers an approval request)

Thread-level state deciding whether an escalation (sandbox escape,
blocked network, MCP approval, ARC escalation) is routed to the client
as an approval request or adjudicated by a Codex-side subagent.

```
ApprovalsReviewer = "user" | "auto_review"
```

`"guardian_subagent"` is a serde alias for `auto_review`, kept for
compatibility (`codex-rs/protocol/src/config_types.rs`); Codex only ever
*serializes* `auto_review`, so a client that accepts one spelling on the
way out is enough. The Rust enum is `#[default] User`.

It appears in three places, with different optionality on each:

| Shape | Field | Type |
|---|---|---|
| `ThreadStartParams` / `ThreadResumeParams` / `ThreadForkParams` | `approvalsReviewer` | `Option`, where omitted means "config default" |
| `ThreadStartResponse` / `ThreadResumeResponse` / `ThreadForkResponse` | `approvalsReviewer` | **non-`Option`**, always present |
| `TurnStartParams` | `approvalsReviewer` | `Option`, a per-turn override, same slot as `approvalPolicy` / `sandboxPolicy` |

**The silent-drop hazard.** `ThreadStartParams` has no
`#[serde(deny_unknown_fields)]`, so a codex predating the field accepts
the request, discards it, and starts a `user`-reviewer thread with a
success response. There is no capability handshake to gate on:
`initialize` carries no version or capability list for this, and
`thread/started` does not carry the reviewer. The versions:

- pre-0.115: field unknown, **silently dropped**.
- 0.115–0.123: field known, value rejected: `-32600`, unknown variant.
- 0.143+ (AO's floor, `internal/provider/codex_version.go`): accepted.

So the start/resume **response** is the only probe. AO reads
`approvalsReviewer` back off it (`verifyApprovalsReviewerEcho`,
`session_helpers.go`) and fails the session when the echo differs from
what was asked. An *absent* echo is read as `"user"`: the field is
non-`Option` upstream, so silence can only come from a build that does
not have it, which is exactly the dropped case.

A mismatch is not only a version problem. `allowed_approvals_reviewers`
in the config requirements (`config_requirements.rs`) can forbid
`auto_review` by policy, which is the same observable: a successful
start running a reviewer the client did not ask for.

**Resume is asymmetric, and it matters.** Resuming an already-loaded
thread **ignores every override in the request**.
`collect_resume_override_mismatches` collects the divergences and
`tracing::warn!`s them server-side, then rejoins the live config
unchanged (`thread_processor.rs`). A cold resume applies them normally.
So a bare `thread/resume {threadId}` against a loaded thread cannot
disturb the reviewer, while the same call against an evicted thread
would reset every unspecified axis to the config default. AO's mid-life
reconcile resumes (`session_probe.go` `Resume`,
`collab_rehydrate.go` `attachActiveChildWithRetry`) target loaded
threads and deliberately send no overrides, because sending one that diverged
is what arms the shutdown-and-cold-resume branch. The handshake resume
in `NewSession` names the reviewer because it is the one that can be
cold, and every `turn/start` re-asserts it regardless.

`turn/start` applying it per turn is what makes runtime-mode transitions
live on Codex: `build_thread_settings_overrides` (`turn_processor.rs`)
treats `approvalsReviewer` like the other axes, so no tier change needs
a process restart.

### `thread/settings/updated`

`#[experimental]`, so it requires `capabilities.experimentalApi`. Fires
whenever the thread's live configuration changes, including as a result
of the per-turn overrides on `turn/start`. Captured verbatim from
codex-cli 0.146.0:

```json
{"method": "thread/settings/updated",
 "params": {
   "threadId": "019fc2ff-9050-7971-ac4e-b902cc3b9f00",
   "threadSettings": {
     "cwd": "/tmp/work",
     "approvalPolicy": "on-request",
     "approvalsReviewer": "auto_review",
     "sandboxPolicy": {"type": "workspaceWrite", "writableRoots": [],
                       "networkAccess": false, "excludeTmpdirEnvVar": false,
                       "excludeSlashTmp": false},
     "activePermissionProfile": null,
     "model": "gpt-5.6-sol", "modelProvider": "openai",
     "serviceTier": "priority", "effort": "high", "summary": null,
     "collaborationMode": {"mode": "default", "settings": {...}},
     "multiAgentMode": "explicitRequestOnly", "personality": "pragmatic"
   }}}
```

`effort`, `serviceTier`, `summary`, `personality` and
`activePermissionProfile` are nullable: null means "no override in
force", never a literal value. `sandboxPolicy.type` is camelCase
(`readOnly | workspaceWrite | dangerFullAccess`), the inverse of AO's
hyphenated vocabulary.

This is Codex's view of what the thread IS running, which is not the same
thing as what the client asked for: Codex can change model, effort or
tier on its own (reroute, guardian downgrade, config reload, another
client on the same thread). Agent Overflow keeps the two apart.
`thread_settings.go` records the echo for usage attribution, and the
requested turn config stays owned by `ApplyLiveUpdate` so a stale echo
cannot undo a pending user selection.

### `model/safetyBuffering/updated`

```json
{"method": "model/safetyBuffering/updated",
 "params": {"threadId": "...", "turnId": "...", "model": "gpt-5.6-sol",
            "useCases": ["..."], "reasons": ["..."],
            "showBufferingUi": true, "fasterModel": "gpt-5.6-luna"}}
```

The model's response is being held while OpenAI reviews it. `reasons`
and `useCases` are server-authored free-form strings, not a closed enum
(`codex-rs/core/src/session/turn.rs` passes the response's lists
straight through), so render them verbatim rather than mapping them.
`showBufferingUi: false` is the hold ending. The Codex TUI ignores this
notification entirely; without surfacing it a client is
indistinguishable from a hung app during the hold.

### `mcpServer/startupStatus/updated`

```json
{"method": "mcpServer/startupStatus/updated",
 "params": {"threadId": "...", "name": "codex_apps", "status": "starting",
            "error": null, "failureReason": null}}
```

`status` ∈ `starting | ready | failed | cancelled`. `failureReason` is
the machine-readable half of a failure; upstream's
`McpStartupFailureReason` enum has exactly one variant today,
`"reauthenticationRequired"` (spelled `reauthentication_required` in the
internal protocol). It means the stored OAuth grant is no longer usable.
The remedy is a sign-in, not a retry, so it must not be flattened into a
generic failure.

**`failureReason` is deterministically `null` for a revoked refresh
token.** `mcp_startup_failure_reason`
([`codex-rs/codex-mcp/src/connection_manager/startup.rs`](/home/rmurphy/repos/codex/codex-rs/codex-mcp/src/connection_manager/startup.rs),
read at `rust-v0.147.0`) returns the variant only when the stored token
already reads `AuthorizationRequired`, which is structurally unusable. A refresh
token that is intact on disk but revoked server-side reads `Usable`, so
the attempt fails with `invalid_grant`, `authStatus: "oAuth"` and
`failureReason: null`. Absence of the reason is therefore not drift and
not evidence that a sign-in would not help: a plain `failed` has to be
actionable on its own.

Upstream's own TUI treats these notifications as lossy (a stale update
from a finished round can arrive late and a terminal one can be missed),
so retained state must be last-write-wins and self-correcting, with
`mcpServerStatus/list` as the reconciler.

---

## `mcpServerStatus/list`

```json
{"method": "mcpServerStatus/list",
 "params": {"threadId": "...", "detail": "toolsAndAuthOnly"}}
→ {"data": [{"name": "atlassian", "authStatus": "oAuth",
             "serverInfo": {"name": "atlassian", "version": "1.4.0"},
             "tools": {"fetchTicket": {...}}}],
   "nextCursor": null}
```

`authStatus` ∈ `unsupported | notLoggedIn | bearerToken | oAuth`.

**This is a fresh, settled connection probe, not a read of a loaded
thread's MCP manager.** `list_mcp_server_status`
([`codex-rs/app-server/src/request_processors/mcp_processor.rs`](/home/rmurphy/repos/codex/codex-rs/app-server/src/request_processors/mcp_processor.rs))
builds a new `McpConnectionSet` on every call, `threadId` only selecting
which config applies; `collect_mcp_server_status_snapshot_with_detail`
([`codex-rs/codex-mcp/src/mcp/mod.rs`](/home/rmurphy/repos/codex/codex-rs/codex-mcp/src/mcp/mod.rs))
answers through `list_available_server_infos`
([`connection_manager.rs`](/home/rmurphy/repos/codex/codex-rs/codex-mcp/src/connection_manager.rs)),
which **awaits** every pending client's startup first. By response time
each server's attempt has settled, so "no evidence" means failed, never
"still starting".

- `serverInfo` (`Option<McpServerInfo>`: name / title / version, no
  secrets) is populated whenever `initialize` succeeded, at ALL detail
  levels including `toolsAndAuthOnly`, on every version from
  `rust-v0.143.0` (AO's floor). MCP makes `serverInfo` mandatory in a
  successful `initialize` response, so its presence proves
  initialization and its absence on a settled attempt proves failure.
- **Tool count proves nothing on its own.** Zero tools is a legitimate
  answer for a resources-only server; a non-zero count is only a
  secondary confirmation that initialize completed.
- Two consequences for a client: a live thread's own `startupStatus`
  history is the better answer for *that thread's* runtime, and the list
  is the better answer for membership and for a re-checked attempt.

## `config/mcpServer/reload`

```json
{"method": "config/mcpServer/reload", "params": null} → {}
```

Re-reads the on-disk config and marks loaded threads' MCP runtime dirty;
the reload is applied at the next turn boundary and emits a fresh
`mcpServer/startupStatus/updated` round. Spawns no new app-server: it is
one RPC on the connection already running. It re-reads the WHOLE config,
so unrelated hand-edits to `config.toml` land with it.

Without it, a thread that loaded with a failed MCP server (expired OAuth
grant, say) keeps that failed manager for the rest of its life. A
successful `mcpServer/oauth/login` round-trip alone changes nothing for
the running thread.

---

## Background terminals

`#[experimental]`: all three require `capabilities.experimentalApi`.
Available since codex 0.140.0; verified on 0.146.0.

```json
{"method": "thread/backgroundTerminals/list",
 "params": {"threadId": "...", "cursor": null, "limit": null}}
→ {"data": [{"itemId": "...", "processId": "42", "command": "pnpm dev",
             "cwd": "/repo", "osPid": 98765, "cpuPercent": 12.5,
             "rssKb": 204800}],
   "nextCursor": null}

{"method": "thread/backgroundTerminals/terminate",
 "params": {"threadId": "...", "processId": "42"}}
→ {"terminated": true}

{"method": "thread/backgroundTerminals/clean", "params": {"threadId": "..."}}
→ {}
```

- `processId` is the app-server handle, not the OS pid; it is the same
  value the `commandExecution` item carries and AO stores as
  `meta.process_id`. Omitting it from `terminate` returns
  `-32600 "Invalid request: missing field processId"`.
- `osPid`, `cpuPercent`, `rssKb` are nullable, and absent is not zero.
- `terminated: false` means no running process matched (already exited,
  or belongs to another thread). It is a state answer, not an error.
- `list` paginates: pass a non-null `nextCursor` back as `cursor`.

---

## Skills

Skills are Codex's user-invokable prompt units: a directory holding a
`SKILL.md` (plus an optional `SKILL.json` interface block). **They are the
replacement for custom prompts, which upstream removed in 0.118**; there is
no `customPrompts/list` to fall back to.

Types: [`codex-rs/app-server-protocol/src/protocol/v2/plugin.rs`](/home/rmurphy/repos/codex/codex-rs/app-server-protocol/src/protocol/v2/plugin.rs)
(`SkillsListParams`, `SkillsListResponse`, `SkillsListEntry`,
`SkillMetadata`, `SkillInterface`, `SkillErrorInfo`,
`SkillsChangedNotification`). Method registration:
`codex-rs/app-server-protocol/src/protocol/common.rs`. Shapes below verified
against `rust-v0.146.0-alpha.4`.

### `skills/list`

`SkillsList => "skills/list"` with `serialization:
global_shared_read("config")`. It is **global**, no thread, no turn, no
`#[experimental]` gate. Since codex 0.73.0, far below AO's 0.143 floor, so
no capability probe is needed.

```json
{"method": "skills/list",
 "params": {"cwds": ["/repo"], "forceReload": false}}
→ {"data": [{
     "cwd": "/repo",
     "skills": [{
       "name": "code-review",
       "description": "Reviews a diff",
       "shortDescription": "legacy short",
       "interface": {"displayName": "Code Review",
                     "shortDescription": "Review a diff",
                     "iconSmall": "/repo/.codex/skills/code-review/small.png",
                     "iconLarge": null, "brandColor": "#ff0000",
                     "defaultPrompt": "Review my working tree"},
       "dependencies": {"tools": [{"type": "mcp", "value": "github"}]},
       "path": "/repo/.codex/skills/code-review/SKILL.md",
       "scope": "repo",
       "enabled": true}],
     "errors": [{"path": "/repo/.codex/skills/broken",
                 "message": "missing SKILL.md"}]}]}
```

- **`cwds` is per-directory and the answer is per-directory.** Skills are
  directory-scoped: the `repo` tier comes from the workspace itself, so two
  workspaces genuinely have different answers. The response `cwd` echoes the
  REQUESTED path, not the resolved absolute form, so it joins back to the
  request.
- **Always send absolute paths.** The handler resolves each entry with
  `AbsolutePathBuf::relative_to_current_dir`
  (`app-server/src/request_processors/catalog_processor.rs`), so a relative
  cwd means a different directory depending on which process answered, a
  live session's workspace versus an ephemeral fetcher's WorkDir.
- **An empty `cwds` defaults to the answering process's own cwd.** That
  default is a property of the process, not of the request, so AO never
  relies on it (`buildSkillsListParams` rejects an empty list).
- `scope` is snake_case (`user | repo | system | admin`) while everything
  else on this wire is camelCase, because `SkillScope` carries
  `#[serde(rename_all = "snake_case")]`.
- `interface.shortDescription` wins over the top-level `shortDescription`;
  upstream's own comment marks the latter legacy.
- `enabled: false` is a skill the user turned off in Codex's config. It is
  still returned, so a UI shows it as off rather than omitting it.
- `errors[]` is per-cwd load failure, not per-skill. A directory that could
  not be read reports here instead of shortening the list silently.
- `forceReload: true` bypasses the app-server's own on-disk skill cache.
  Reserve it for a user-initiated refresh.

### `skills/changed`

```json
{"method": "skills/changed", "params": {}}
```

`SkillsChangedNotification` is an **empty struct**. Upstream documents it
as "treat this as an invalidation signal and re-run `skills/list` with the
client's current parameters". It carries no cwd, no scope and no skill
name, so a consumer cannot narrow the drop. The only correct response is
to invalidate everything it has cached.

### Invoking a skill

Two forms, both server-side; neither takes arguments (there is no
`arguments` field anywhere in the skill surface).

1. **Text token.** A `$skill-name` token inside ordinary turn text is
   scanned server-side and expanded.
2. **Structured input.** `turn/start`'s `input` array accepts a
   `UserInput::Skill` variant
   (`codex-rs/app-server-protocol/src/protocol/v2/turn.rs`):

```json
{"type": "skill", "name": "code-review",
 "path": "/repo/.codex/skills/code-review/SKILL.md"}
```

Both `name` and `path` are required, which is why AO drops a listed skill
missing either, since it could be shown but not invoked.

---

## Code review: `review/start`

Types: [`codex-rs/app-server-protocol/src/protocol/v2/review.rs`](/home/rmurphy/repos/codex/codex-rs/app-server-protocol/src/protocol/v2/review.rs).
`ReviewStart => "review/start"` with `serialization:
thread_id(params.thread_id)`; **not** `#[experimental]`. Since codex
0.59.0; `detached` delivery since 0.64.0.

```json
{"method": "review/start",
 "params": {"threadId": "...",
            "target": {"type": "baseBranch", "branch": "main"},
            "delivery": "detached"}}
→ {"turn": {"id": "turn-1", "items": [], "status": "inProgress"},
   "reviewThreadId": "review-thread-9"}
```

`ReviewTarget` is an internally-tagged union: `#[serde(tag = "type",
rename_all = "camelCase")]` on the enum (so the tag is the camelCased
variant name) plus `#[serde(rename_all = "camelCase")]` on each struct
variant:

```json
{"type": "uncommittedChanges"}
{"type": "baseBranch",  "branch": "main"}
{"type": "commit",      "sha": "abc123", "title": "fix: thing"}
{"type": "commit",      "sha": "abc123", "title": null}
{"type": "custom",      "instructions": "look for races"}
```

`commit.title` is `Option<String>` with **no** `skip_serializing_if`, so
the key is always present and `null` when there is no label.

`delivery` ∈ `"inline"` (default when omitted) | `"detached"`
(`ReviewDelivery`, camelCase via `v2_enum_from_core!`).

⚠ **Route on the returned `reviewThreadId`, never on the requested
delivery and never on your own thread id.** Upstream's TUI does exactly
this (`codex-rs/tui/src/app/thread_routing.rs`). For an inline review the
returned id is the original thread; for a detached one it is a freshly
created thread. Assuming inline means the original thread happens to be
true today and is not what the protocol promises.

The transcript is bracketed by two thread items
(`codex-rs/app-server-protocol/src/protocol/v2/item.rs`):

```json
{"type": "enteredReviewMode", "id": "...", "review": "..."}
{"type": "exitedReviewMode",  "id": "...", "review": "..."}
```

A current inline review exposes two distinct turn ids. The id returned by
`review/start` also appears on `enteredReviewMode`, review item activity,
`exitedReviewMode`, and the outer `turn/completed`. A separate private
`turn/started` id identifies the reviewer execution. `turn/interrupt` accepts
only that private id. Showing both would create a false second user turn, but
discarding the private id makes Stop ineffective.

The review's final answer also has two forms. The reviewer first emits raw
structured JSON. After `exitedReviewMode`, Codex emits the formatted Markdown
answer that it injects into the parent context. The formatted answer is the
user-visible result.

A review runs as a **non-steerable turn**: `turn/start` or `turn/steer`
against it fails with `codexErrorInfo:
{"type":"activeTurnNotSteerable","turnKind":"review"}`
(`v2/shared.rs`).

### Current state in agent-overflow

`/review` follows the normal composer send transaction and calls
`Session.StartReviewForTurn`. The outer id owns one visible turn and one
`codex_review` agent launch. Review tools, thinking, and intermediate prose
are parented under that launch. The private id stays in `activeTurnID` for
Stop. The raw final JSON is discarded. The formatted answer becomes a
top-level sourced `command_result`, so it reads as "Code review result"
without claiming the parent agent authored it.

`config/read` resolves `review_model`; the parent model is the fallback. A
DETACHED review is still refused because its returned thread is not registered
with this session's routing tables.

On disk, the root rollout contains the review boundary and formatted result,
while a separate `source:{"subagent":"review"}` rollout contains the reviewer
tools and reasoning. Session import joins that child by parent thread id and
private control turn id. An incomplete root review is held behind the import
cursor until its terminal boundary arrives, so refresh never persists a launch
that a later independent batch cannot settle.

---

## Manual compaction: `thread/compact/start`

```json
{"method": "thread/compact/start", "params": {"threadId": "..."}}
→ {}
```

`ThreadCompactStart` with `serialization: thread_id(params.thread_id)`;
not `#[experimental]`. Since codex 0.96.0. Params and response are typed at
`codex-rs/app-server-protocol/src/protocol/v2/thread.rs`
(`ThreadCompactStartParams` / `ThreadCompactStartResponse`).

The response body is empty, and **the boundary is not on it**. It surfaces as
the `contextCompaction` thread item:

```json
{"method": "item/started",
 "params": {"threadId": "...", "turnId": "...",
            "item": {"type": "contextCompaction", "id": "..."}}}
{"method": "item/completed",
 "params": {"threadId": "...", "turnId": "...",
            "item": {"type": "contextCompaction", "id": "..."}}}
```

All three compaction paths in codex core (`compact.rs`,
`compact_remote.rs`, `compact_remote_v2.rs`, auto-compact included)
emit both halves. AO consumes `item/started` as `EventCompactionStatus`
Active (the `provider:compacting` window open) and `item/completed` as
the boundary. ⚠ A **failed** compaction sends an error event and never
completes its item, so triage's turn-completion clear is the only close
on that path.

⚠ **`thread/compacted` is deprecated.** It is still in the notification
catalogue and still fires on older builds, so both paths must produce the
same downstream input. Agent Overflow emits `EventCompactBoundary` from
each (`protocol_item.go` for the item, `protocol_thread.go` for the
notification), which triage routes through one compaction-divider case.

Compaction is also a non-steerable turn (`turnKind: "compact"`), so callers
gate it on the thread being idle rather than racing a live turn.

---

## History truncation: `thread/revert` and `historyMode`

Three turn-granular cuts exist upstream; AO uses two, and the choice is
per THREAD, decided at creation.

```json
{"method": "thread/start",
 "params": {"cwd": "...", "historyMode": "paginated", ...}}
{"method": "thread/revert",
 "params": {"threadId": "...", "beforeTurnId": "turn-7"}}
→ {"thread": {"id": "<same id>", "turns": []},
   "turnsBackwardsCursor": null, "itemsBackwardsCursor": null}
{"method": "thread/reverted", "params": {"threadId": "..."}}
```

`ThreadRevertParams` / `ThreadRevertResponse` at
`codex-rs/app-server-protocol/src/protocol/v2/thread.rs` @ rust-v0.150.1;
handler `thread_revert_response` in
`codex-rs/app-server/src/request_processors/thread_processor.rs`.
`#[experimental("thread/revert")]`, so it rides the
`capabilities.experimentalApi` every AO handshake already sets. Since
0.148.

Five facts that decide how a client must call it:

- **`beforeTurnId` is EXCLUSIVE**: the first turn DROPPED. `thread/fork`'s
  `lastTurnId` is the last turn KEPT. Same boundary, opposite sides, so
  the two anchors must be resolved separately and never interchanged.
- **Paginated threads only.** Upstream refuses a legacy-history thread
  before shutdown or history mutation, and a thread's history contract
  is fixed at creation (`ThreadResumeParams` has no history-mode field).
  Upstream's default is legacy, so a client that never sends
  `historyMode` gets threads that can never be reverted, which is why
  AO asks for `"paginated"` on `thread/start` from 0.148 up. The floor is
  the REVERT floor, not the field's own (paginated shipped in 0.147): a
  paginated thread on a server with no `thread/revert` carries the
  differences with none of the benefit. An app-server whose thread store
  has no SQLite state database refuses the field itself ("paginated
  threads require thread/turns/list and thread/items/list support"),
  raised while destructuring the params, so the client retries once
  without it.
- **`thread.turns` on the response is ALWAYS empty.** Upstream points
  clients at `thread/turns/list` to re-hydrate. The thread-identity echo
  is therefore the only validation available, and the load-bearing one,
  since a caller keeps its session pointed at that thread.
- **It is NOT refused mid-turn.** The handler subscribes to shutdown events,
  submits a shutdown, waits for both the runtime and listener to drain, reverts,
  then reloads the runtime with
  `has_live_in_progress_turn = false`. AO uses that whole operation for an Esc
  un-send. It does not send a separate `turn/interrupt` or replace the
  app-server connection around the cut.
- **Nothing is destroyed on disk.** `revert_thread` writes a NEW immutable
  rollout referencing the retained prefix and moves only the SQLite
  rollout pointer (`codex-rs/thread-store/src/local/revert_thread.rs`),
  so the pre-revert rollout survives exactly like a fork's source does.

Failure taxonomy matters here, because a client that falls back to
`thread/fork` must know whether durable history changed. Every refusal AO
maps to a fallback is raised before the replacement rollout is written
and long before the pointer CAS: the paginated gate, and the anchor
resolution in `history_base_at_boundary` ("turn not found: …", "does not
have persisted rollout positions", "does not have a persisted start
boundary", "fork boundary exceeds inherited source history"). All arrive
as invalid_request (-32600), because upstream folds them onto one code in
`thread_store_mutation_error`, and they are told apart by message. Errors
from a later stage (the shutdown timeouts, the CAS conflict) leave a
thread no fork should be built on.

`thread/reverted` carries `{threadId}` only: it is an echo for the client
that asked, not a description of what was cut.

### What `historyMode: paginated` changes

Checked against rust-v0.150.1 for every consumer of thread shape:

- `thread/fork` still works on a paginated source (`prepare_fork` with
  `ForkBoundary::ThroughTurn`) and still returns turns, so the fork cut stays
  valid as a fallback. Its one paginated-specific refusal needs
  `ephemeral: true`.
- Two methods are refused outright on a paginated thread: `thread/rollback`
  ("paginated threads do not support thread/rollback") and DETACHED review
  ("paginated threads do not support detached review",
  `turn_processor.rs:1308`). Inline review is unaffected.
- Rollout persistence: `RolloutItem::ResponseItem` and
  `RolloutItem::InterAgentCommunication` are written in BOTH modes
  (`codex-rs/rollout/src/policy.rs`). What paginated drops is the legacy
  `EventMsg` mirror (`user_message`, `agent_message`, `sub_agent_activity`,
  and the rest), replaced by `item_completed`. A reader that only knows the
  legacy set imports a paginated thread with no tool detail at all.
- Resume feeds the model `load_latest_model_context` rather than the full
  stored history. For a thread that never compacted those are the same items;
  after a compaction it is the compacted window, which is what the model had.
- Live turn and item notifications do not depend on the mode.
- Downgrade hazard: a paginated rollout is unreadable by a codex older than
  0.143 (`reject_unknown_thread_history_mode`), which is also AO's provider
  floor, so every supported app-server understands the mode.

---

## The provider-owned queue: `thread/queue/*`

Since 0.148, all `#[experimental]`.

```json
{"method": "thread/queue/add",
 "params": {"threadId": "...", "input": [{"type": "text", "text": "..."}],
            "clientUserMessageId": "user:3:flush:1"}}
→ {"queuedSubmission": {"id": "...", "input": [...],
                        "clientUserMessageId": "user:3:flush:1"}}
{"method": "thread/queue/list",
 "params": {"threadId": "...", "cursor": null, "limit": null}}
→ {"data": [QueuedSubmission], "nextCursor": null}
{"method": "thread/queue/delete",
 "params": {"threadId": "...", "queuedSubmissionId": "..."}}
→ {"deleted": true}
```

`update` (`{threadId, queuedSubmissionId, input}`) and `reorder`
(`{threadId, queuedSubmissionIds}`) exist on the same file. `start`
(`{threadId, queuedSubmissionId?}` → `{turn}`) exists too and **must not
be called**: `QueuedItemService` is a `ThreadLifecycleContributor` whose
`on_thread_idle` → `dispatch_if_idle` → `start_turn_if_idle` path already
drains the queue, and `enqueue` itself calls `wake_if_loaded`, so an
idle thread dispatches INSIDE the `add` request and a client `start` on
top of that races the drain.

`list` returns ONE PAGE. Upstream's own README states the contract:
"pass optional `cursor` and `limit` values to request a page, and continue
with the returned `nextCursor` until it is `null`"
(`codex-rs/app-server/README.md:808`, rust-v0.149.0), so `nextCursor` is
the only thing that says the walk finished. A client that stops early for
any other reason (its own page cap, a server repeating a cursor) is
holding a PREFIX, and a prefix presented as the whole queue is
indistinguishable from a short queue: it reads as "nothing else is
queued". AO returns `ErrThreadQueueListIncomplete` with the rows it did
read rather than let a purge or an ownership walk conclude anything from
a truncated answer.

**Every field on these three shapes is required.** `QueuedSubmission` is
`{id: String, input: Vec<UserInput>, client_user_message_id: String}` and
`ThreadQueueDeleteResponse` is `{deleted: bool}`, all non-`Option`, none
with a serde default (`codex-rs/app-server-protocol/src/protocol/v2/thread.rs`
lines 869 and 940, rust-v0.149.0), so upstream's own deserializer refuses a
body missing any of them. A client that decodes them leniently converts wire
drift into a plausible-looking answer: an unreadable `QueuedSubmission`
becomes an empty one, which is indistinguishable from an ABSENT row, and a
missing `deleted` becomes `false`, which reads as "already dispatched". Both
are the direction that loses a message rather than reporting a fault, so AO
decodes `deleted` as a pointer and returns
`ErrThreadQueueListMalformed` for an element it cannot read (including a
server-assigned `id` that came back empty).

`clientUserMessageId` is required and upstream mints a uuid when a
producer omits it, so an empty value gives up correlation silently rather
than failing. It comes back on the echoed `userMessage` as `clientId`,
which is what lets a `thread/queue/list` say which entries belong to this
client.

### Externally queued turns

The queue is also how a turn can start on a thread a client owns without that
client ever sending `turn/start`. It is a normal condition, not a protocol
violation, and there is no way to opt out of it.

- Every app-server backed by a LOCAL thread store installs the queued-item
  extension unconditionally (`codex-rs/app-server/src/extensions.rs`). No
  initialize capability disables it.
- `QueuedItemService::watch_external_messages`
  (`codex-rs/ext/queue/src/service.rs`) polls SQLite's cheap `data_version` on
  `state_5.sqlite` every 10 seconds, asks the durable revision index which
  LOADED threads changed, emits `thread/queue/changed` for each, and spawns a
  dispatch task calling `start_turn_if_idle`, retrying every 10s while the
  thread stays busy.
- The producer is `codex queue --thread <uuid> --message <text>`
  (`codex-rs/cli/src/queue_cmd.rs`). It writes one SQLite row and exits: no
  running app-server needed, and it never takes the thread writer lock, so it
  works while another client holds the thread.

What a client sees, in order: `thread/queue/changed`, then up to ~10s later a
`turn/started` it did not ask for, followed by a full `item/*` stream
including an `item/completed` `userMessage` it never sent.
`thread/queue/changed` carries `{threadId}` and nothing else, so depth and
authorship can only come from a `thread/queue/list`. AO's adoption and
attribution rules are in `internal/provider/codex/AGENTS.md` §"Turns AO did
not start".

---

## Thread-scoped `account/usage/read`

Since 0.148 the params carry an optional `threadId`
(`GetAccountTokenUsageParams`, `codex-rs/app-server-protocol/src/protocol/v2/account.rs`):

```json
{"method": "account/usage/read", "params": {"threadId": "..."}}
→ {"summary": {…all null…}, "dailyUsageBuckets": null,
   "threadUsage": {"threadId": "...", "estimatedUsageCreditsMicros": 4200000,
                   "estimatedUsageUsdMicros": 137500, "groups": [...]}}
```

Four things a caller has to know:

- **The params were `Option<()>` through 0.147.** A `{threadId}` request
  to an older app-server is a hard `invalid_params`, not a graceful
  degradation, so the call has to be version-gated off the handshake.
- **The response is thread-scoped or account-scoped, never both.** A
  thread-scoped answer's `summary` is all-`None` and its
  `dailyUsageBuckets` is `null`, so it must never feed an account-usage
  cache.
- **The estimate is CUMULATIVE for the thread**, not a per-turn delta,
  and it is the ChatGPT backend's own billing estimate rather than a
  settled invoice.
- **Absence is a state.** `threadUsage: null` is what upstream returns
  when the billing route is unavailable for the thread (403/404 mapped to
  null in `account_processor.rs`), and a present object with no
  `estimatedUsageUsdMicros` means the thread was priced in credits only.
  Both mean "keep your own price", not "the call failed".

---

## `<subagent_notification>` parent-mailbox carrier

When Codex core detects a detached child thread that finished without
a matching `wait` outstanding on the parent, it enqueues an
`InterAgentCommunication` / contextual-user notification for the parent.
On mailbox delivery, raw events can expose a `rawResponseItem/completed`
message item. Current Codex app-server builds that context as `role:
"user"` with a direct `<subagent_notification>` fragment in the single
`input_text` block. Some traces and rollout tooling expose a serialized
`InterAgentCommunication` wrapper in an assistant/commentary raw message;
that wrapper's `content` field carries the same notification fragment.
On resumed threads, app-server has no raw-event opt-in; Agent Overflow
tails the active rollout file from EOF and parses appended
`response_item` records with the same `role:"user"` / `input_text`
payload as the parent-sees-context signal.

Compatibility path: older/replay builds can still surface the same
fragment inside an `item/completed` (`type: userMessage`) text content
carrier. That path is parsed only when the carrier is standalone and
resolves to a known spawned child, so user prose cannot forge a
completion row.

### Authoritative wire shape

Produced by `format_subagent_notification_message` at
[`codex-rs/core/src/session_prefix.rs:8-18`](/home/rmurphy/repos/codex/codex-rs/core/src/session_prefix.rs):

```
<subagent_notification>
{"agent_path":"<child_agent_reference>","status":"<AgentStatus>"}
</subagent_notification>
```

⚠ **The wire field is `agent_path`, NOT `agent_id`.** Named Codex agents
report a path such as `/root/researcher`; unnamed or older flows may use
the child thread id as the reference. The `status` value is serialized
from Codex core's `AgentStatus`, so terminal variants may be objects such
as `{"completed":"final message"}` or `{"errored":"boom"}`, not just
plain strings.

### Current state in agent-overflow

Extraction is wired at the Codex session parser: it pulls
`<subagent_notification>` fragments out of raw mailbox message carriers
(direct raw user context or serialized raw inter-agent wrapper), appended
rollout `response_item` records on resumed sessions, and the legacy
standalone user-message carrier, then emits
`EventSubagentNotification`. The provider also maps child
`turn/completed` lifecycle notifications to `EventSubagentStatus`, which
is used as live-state evidence only; it does not write parent transcript
completion rows. The provider maps named `agent_path` values back to the
parent `spawn_agent` item when it has seen the child `thread/started`;
triage falls back to receiver-thread matching for legacy unnamed flows.
Parent transcript completions are written from explicit `wait_agent`
completion or `EventSubagentNotification`.

---

## Account sign-in

`account/login/start` in two variants of one RPC, on an app-server spawned for
nothing else. Both complete on the same notification. Client:
`internal/provider/codex/login.go`.

The connection is an ordinary `app-server` — same argv as a session — with
`CODEX_HOME` pointed at an isolated login home and
`cli_auth_credentials_store="file"` pinned, so the credential lands as a file
the account layer can move rather than in a keyring AO cannot. `initialize`
opts out of every notification except `account/login/completed`;
`account/updated` (which follows a success with `{authMode, planType}`) is
deliberately among the opt-outs, because the adoption epilogue probes the login
home for identity and that answer is authoritative.

**Only one login runs per process.** Starting a second supersedes the first,
which is why a client that offers both variants cancels before switching.

### `account/login/start {type:"chatgpt"}`

```json
{"jsonrpc":"2.0","id":2,"method":"account/login/start",
 "params":{"type":"chatgpt","codexStreamlinedLogin":false}}
```

```json
{"id":2,"result":{"type":"chatgpt","loginId":"…","authUrl":"https://auth.openai.com/…"}}
```

`authUrl` completes on a loopback listener THIS app-server bound, so it is
finishable only by a browser on this machine. Showing it to another device
produces a page that can never come back.

### `account/login/start {type:"chatgptDeviceCode"}`

```json
{"jsonrpc":"2.0","id":2,"method":"account/login/start",
 "params":{"type":"chatgptDeviceCode"}}
```

```json
{"id":2,"result":{"type":"chatgptDeviceCode","loginId":"…",
                  "verificationUrl":"https://…","userCode":"XXXX-XXXX"}}
```

`verificationUrl` is CONSTANT and carries no code, so it is only useful shown
beside `userCode`. This is the variant for a person who is not at the machine.

**The discriminant's casing is exact.** Upstream matches the string literally
and answers a wrong spelling by listing every variant it knows, which reads as
a protocol failure rather than as the typo it is.

**There is no expiry field on the wire.** A countdown has to come from the
client, and the value used is upstream's own device-code `max_wait`
(15 minutes) — a user watching a timer that outlives the code is worse than no
timer.

### `account/login/completed`

```json
{"method":"account/login/completed",
 "params":{"loginId":"…","success":true}}
```

Failure carries `success:false` and an `error` string.

**`loginId` is not bookkeeping.** A superseded or cancelled login emits a
FAILED completion for its OWN id, so a client that ignored the correlation
would report the previous login's failure as the current one's.

### `account/login/cancel`

```json
{"jsonrpc":"2.0","id":3,"method":"account/login/cancel","params":{"loginId":"…"}}
```

Answers `{"status":"canceled"}` or `{"status":"notFound"}`. `notFound` is a
real outcome, not an error: after our own cancel, or after the login already
settled, there is nothing left to cancel and that is what was wanted.
Cancelling makes upstream emit the FAILED completion described above — the
frame is real, it is just not news.

Closing the process is what actually guarantees a cancelled sign-in stops: the
device-code poll and the browser flow's listener both live in that child, and
the login home goes with it.

### Responses omit `jsonrpc`

Same as everywhere else on this app-server (§The two critical differences from
Claude), and it bites harder here because a login client is a small decoder
written on its own: nothing on this path may REQUIRE the field. Decoding
leniently is the contract, not a tolerance.

Error responses carry provider-controlled text on a path holding OAuth state,
so the numeric code is what the client surfaces. It is enough to tell a
wrong-shape request from a refused one.

---

## Wire surface Agent Overflow declines

Everything below exists at rust-v0.150.1 and is deliberately not consumed.
Listed so a future sync can tell "we have not looked at this" from "we looked
and declined". None of these methods is in `codexNotificationCatalog`'s
consumed set, and the notification methods here are opted out at initialize.

- `thread/queue/add` / `start` / `update` / `reorder` (0.148). AO does not
  write to the provider's queue; a mid-turn message goes to `turn/steer`. Only
  `list` / `delete` are adopted, and only to clear a foreign producer's rows.
  `start` is the one that would be actively dangerous, since dispatch is
  already automatic (`QueuedItemService::on_thread_idle`) and a client `start`
  races that drain.
- `project/create` / `delete` / `import` / `list` / `move` / `read` / `update`
  plus `project/changed` and `thread/project/updated` (0.149). AO owns its own
  project rows keyed on the git root (core principle 7), and adopting
  upstream's project identity would mean two authorities for one concept.
- `Thread.projectId` (0.149). It rides on every `thread/start`,
  `thread/resume`, `thread/fork` and `thread/read` response AO decodes. AO's
  structs are narrow and none uses `deny_unknown_fields`, so it is dropped
  silently (`TestThreadProjectIDIsIgnoredWithoutError`).
- `server/diagnostics` (0.149): a health surface with no consumer.
- `account/bedrock/discover` / `setup` (0.149): AO offers no Bedrock login.
- `McpServerStatus.pluginId` (0.149): plugin provenance has no UI.
- `mcpServer/event/stream/notification` (0.150): AO does not start the
  event-stream surface.
- `thread/realtime/item/started`, `thread/realtime/item/transcript/delta`,
  `thread/realtime/item/completed` (0.150): AO starts no realtime session.
  Historical `realtime_item` transcript segments are still imported.
- `thread/rollback`: deprecated upstream, mutates in place, and its `num_turns`
  counts user-MESSAGE boundaries rather than wire turns.
- `close_agent` / `write_stdin`: model tools, not client-callable.

---

## Captured samples

2026-05-03 spike against `codex-cli 0.128.0` confirmed
`experimentalRawEvents` behavior for terminal waits and collab agents.
Important ordering observed:

- Empty-stdin terminal wait start arrives as raw
  `function_call name=write_stdin` before the typed
  `item/commandExecution/terminalInteraction`. Agent Overflow treats this
  raw item as event-log detail only; UI projection follows the typed
  terminal-interaction notification.
- The `write_stdin` raw `function_call_output` distinguishes timeout
  from completion via the tool-output text:
  `Process running with session ID ...` vs
  `Process exited with code ...`.
- `wait_agent` typed `item/completed` reports a completed tool call for
  both timeout and completion. The raw output's `timed_out` boolean is
  the timeout/completion discriminator; `agentsStates` is the completed
  child-status carrier when a child actually completed.
- A long foreground `exec_command` is not a terminal wait. It emits
  `rawResponseItem/completed` for the function call, then typed
  `item/started` / `item/completed` commandExecution events. Do not
  render that path as a wait carrier.
- For explicit `wait_agent`, the typed `wait_agent` `item/completed`
  already carries the terminal child state needed to attach the
  subagent completion under the wait row. The later
  `subagent_notification` is a secondary notification path, not the
  source of truth for the wait-attached completion.
- For detached `spawn_agent` completions with no explicit wait, the
  parent-observation signal is currently the raw `message` item with
  `role:"user"` and a direct `input_text` notification fragment. Some
  traces can expose the same delivery as a serialized
  `InterAgentCommunication` assistant message; in both cases the
  notification is parsed only after that mailbox context is accepted
  into parent input.
- 2026-05-04 spike against `codex-cli 0.128.0` confirmed
  `turn/completed` shape as `{threadId, turn}` with no
  `lastAssistantMessageId`, and confirmed parent `turn/completed`
  fires before spawned child-thread completion. In the captured
  spawn-agent run, the parent completed about 8.9s before the child.

Raw `experimentalRawEvents` can echo developer/user prompt material.
Do not check in raw Codex spike captures unredacted; summarize the
ordering and field shapes instead.

To capture fresh samples, run a session in agent-overflow with
`AGENT_OVERFLOW_DEBUG=provider`. Raw JSON-RPC frames land in
`<dbDir>/logs/provider-events-YYYY-MM-DD.ndjson`.

---

## Contradictions and ambiguities

1. `"wait"` vs `"waitAgent"`: wire value is `"wait"`; some old
   docs and tests say `"waitAgent"`. Canonical: the Rust
   `CollabAgentTool::Wait` variant serialises to `"wait"` (camelCase
   but single word).
2. `activeFlags` enum: observed `runningBackground` and
   `waitingForUser` in test fixtures; full set not documented in
   codex-source's TypeScript schema. Treat as open.
3. `CollabAgentStatus` values: v2 schema lists seven; v1 wait tool
   reports them on `agentsStates`, v2 wait does not. Be defensive.

---

## When this doc is wrong

Capture fresh JSON-RPC (`AGENT_OVERFLOW_DEBUG=provider`), compare,
update before coding. For upstream ambiguities, check codex-source
at the pinned rev; if still unclear, CodexMonitor's handling is the
next-best authority.
