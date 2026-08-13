# Codex `app-server` — JSON-RPC wire reference

Authoritative reference for the JSON-RPC 2.0 notifications Codex
emits over stdio. Consulted by `internal/provider/codex/`
parser code.

Multi-agent shapes in this document were re-verified on 2026-07-09 against
the exact `rust-v0.144.0` tag (`767822446c...`) and a live
`codex-cli 0.144.0` MultiAgentV2 rollout. The local Codex checkout may be on
an older tag; use `git show rust-v0.144.0:<path>` rather than assuming its
worktree revision describes the installed binary.

## Sources

**Shape-of-truth, in priority order:**

1. **codex-source** at `/home/rmurphy/repos/codex` — the
   upstream Codex CLI (`codex-rs/`). Typed wire definitions live in
   `codex-rs/app-server-protocol/` (Rust source +
   generated TypeScript under
   `codex-rs/app-server-protocol/schema/typescript/`).
2. **CodexMonitor** at `/Users/randy/repos/CodexMonitor` — a Tauri
   client for codex app-server, authoritative for how to render the
   events we receive. See `src/features/threads/hooks/useAppServerEvents.ts`
   and `src/utils/threadItems.*.ts`.
3. **forge's CodexAdapter** — provides cross-checks, but be aware
   that forge's `run_in_background` / `runInBackground` handling in
   `eventHelpers.ts:676-681` is **dead code** (those fields don't
   exist on the Codex wire). Don't copy that path.

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

- **Synchronous** — blocks the agent until `item/completed`.
- **Parallel** — multiple tools dispatched in one agent response run
  concurrently (for tools registered with
  `supports_parallel_tool_calls = true`, notably `shell`). The agent
  still waits for all of them to return before continuing.
- **Agent-spawning** — `spawn_agent` creates a child thread that runs
  on its own `thread_id`; the parent's `spawn_agent` tool_call
  completes immediately with `status: completed` while the child
  executes independently. This is the closest Codex analog to
  "backgrounded," but the lifecycle model is fundamentally different
  (see §Collab agent lifecycle below).

**But Codex does have background terminals** — just not via a flag on
items. `exec_command` (`source: "unifiedExecStartup"`) yields to the
model after `yield_time_ms` (default 10s) with whatever output
accumulated, and the PTY keeps running in `UnifiedExecProcessManager`.
The item stays `status: inProgress` until `spawn_exit_watcher` fires
`ExecCommandEnd` — potentially across multiple turns, up to
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
`item_id` — the status flips in place, no sibling row is emitted.
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
(`useAppServerEvents.ts:467-495`) — it dispatches on `method` but
always calls the same upsert handler. Adopting this pattern would
collapse half our Codex handling.

---

## Notification taxonomy

Every server → client envelope comes in two flavours. **Notifications**
(`{"jsonrpc":"2.0","method":"<method>","params":{...}}`) carry no
`id` and expect no response. **Server requests** carry a JSON-RPC `id`
and require a response — approvals and MCP elicitation arrive this way,
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
| `thread/started` | Session-level. First notification on a new thread; emits `EventSessionInit`. |
| `thread/status/changed` | Session-level. Thread status transitions; emits `EventSessionStatus`. |
| `thread/compacted` | Thread housekeeping. Compaction boundary event. |
| `thread/name/updated` | Thread housekeeping. Thread name/title changed. |
| `thread/tokenUsage/updated` | Thread housekeeping. Rolling token-usage snapshot. |
| `account/rateLimits/updated` | Account-wide quota snapshot. Surfaced as `EventRateLimits` / `provider:usage action:"rate_limits"`. |
| `model/rerouted` | Model reroute notice (Codex fell back to a different model). |
| `configWarning` | Session-level notice surfaced to the user. |
| `deprecationNotice` | Session-level deprecation notice. |
| `serverRequest/resolved` | Fires when a previously-sent server request (approval / elicitation) has been resolved by the client. |
| `thread/settings/updated` | Codex's authoritative config echo. Reconciled into the session's observed snapshot; emits nothing. |
| `model/safetyBuffering/updated` | Response held while OpenAI reviews the turn. Emits a notification row on the show edge only. |
| `mcpServer/startupStatus/updated` | Per-server MCP startup delta. Side channel to the App's status cache, not a transcript event. |

**Opting out.** `initialize` accepts
`capabilities.optOutNotificationMethods: string[]`; Codex drops those
methods for that connection before serializing them
(`codex-rs/app-server/src/transport.rs`
`should_skip_notification_for_connection`). Matching is exact-string, so
an unrecognized entry is inert. Agent Overflow sends the complement of
what it consumes — see `internal/provider/codex/notification_catalog.go`
and the catalogue there, which is the pinned 0.142.5 method list plus the
three 0.146.0 additions (`rawResponse/completed`,
`thread/environment/connected`, `thread/environment/disconnected`).

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

That aggregate IS the turn-accounting source, though — Codex has no
per-turn usage signal (`turn/completed` carries no token fields) and no
USD cost anywhere on the wire, so per-turn usage is the delta of
`total` between turn boundaries. Verified in codex-rs source: `total`
accumulates via `TokenUsageInfo::append_last_usage` `add_assign` and
never resets — compaction's `recompute_token_usage` rewrites only
`last`, and resume seeds `total` from the rollout's last TokenCount.
The one exception is `fill_to_context_window` (the
ContextWindowExceeded sentinel), which pegs `total.totalTokens` to the
window and zeroes the components — deltas across that event are
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
predates the field — check the installed binary before concluding a
field does not exist.

Live-verified 2026-07-03 against `codex-cli 0.142.5` (three turns across
a fresh thread + a `thread/resume`, spike per spike-policy; raw capture
not checked in per the rule below):

- The final `thread/tokenUsage/updated` of a turn arrives BEFORE
  `turn/completed` (3/3 turns) — the accounting snapshot at
  turn-complete is complete, no rollover needed in practice.
- `turn/completed.turn` carries exactly `{completedAt, durationMs,
  error, id, items, itemsView, startedAt, status}` — no usage fields.
- `total` grew 12044 → 24106 → 36186 across turns and the resumed
  process's first reading matched the prior process's final total
  exactly (cumulative persists across resume, as the source promised).
- After `thread/resume`, a seed `thread/tokenUsage/updated` carrying
  the historical cumulative arrives BEFORE any turn (between
  `thread/status/changed` and `thread/goal/cleared`) — so the
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
`user:<turnIndex>` row when an AO-initiated send round-trips —
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
structurally different — **a spawn creates a child thread**, not a
backgrounded process inside the parent tool call. Agent Overflow
projects this into the shared background UI when that child is still
non-terminal after the parent turn closes.

### Versioned wire shapes

Codex currently has two collaboration transports. Agent Overflow accepts both
and normalizes them before triage:

| Operation | MultiAgentV1 typed item | MultiAgentV2 typed item |
|---|---|---|
| spawn | `collabAgentToolCall`, `tool:"spawnAgent"`, start + complete | `subAgentActivity`, `kind:"started"` — the completed leg is the signal |
| send/follow-up | `collabAgentToolCall`, `tool:"sendInput"` | `subAgentActivity`, `kind:"interacted"` — the completed leg is the signal |
| interrupt/close | `collabAgentToolCall`, `tool:"closeAgent"` | `subAgentActivity`, `kind:"interrupted"` — the completed leg is the signal |
| wait | `collabAgentToolCall`, `tool:"wait"`, receivers/statuses | `collabAgentToolCall`, `tool:"wait"`, empty receiver/status maps |
| list | model-facing raw function call/output only | model-facing raw function call/output only |

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
The agent work on the child thread continues independently —
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
visible tool row. `thread/read` is the primary label source; raw response
items are only an additional typed signal when present, not a prerequisite.

### MultiAgentV2 spawn normalization

Core's `emit_sub_agent_activity`
(`codex-rs/core/src/tools/handlers/multi_agents_v2.rs`) fires BOTH
`item/started` and `item/completed` for every `subAgentActivity` item — since
codex 0.146 for all three kinds. (Read at tag `rust-v0.146.0` via `git show`;
the local reference checkout's working tree may sit on an older tag — 0.142.5
has no `emit_sub_agent_activity` and emits only `ItemCompleted` from
`event_mapping.rs`, which is also why dropping the started leg is a no-op
below 0.146.) Only the completed leg carries meaning here:
Agent Overflow drops the started leg outright and expands a canonical
`kind:"started"` completion into the normalized spawn start +
completion pair used by the existing projector. Routing the started leg as a
tool row instead would mint a raw `subAgentActivity` tool_call — transient for
`started` / `interacted` (the completion upserts the same item id) but
permanent for `interrupted`, whose completion is a status event that never
settles the row. The normalized completion
contains the authoritative receiver thread and a running `agentsStates`
entry, because successful emission occurs only after core has spawned the
child. This is a typed authorization signal, not an ordering heuristic.

MultiAgentV2 marks `spawn_agent.message`, `send_message.message`, and
`followup_task.message` as encrypted tool parameters. Raw response items carry
opaque model-service ciphertext in those fields; clients cannot decrypt it and
must never normalize it as a plaintext prompt. Safe raw fields such as target,
explicit role, model, and effort may enrich the row. The canonical activity,
active session profile, cached `thread/started`, and `thread/read` metadata
provide path, effective model/effort, and display label. Effective profiles are
tracked per owned provider thread so a nested spawn inherits its immediate
parent agent's model/effort rather than the AO root's. On resume, raw events
are unavailable, so those typed sources remain authoritative.
`thread/resume` history is scanned for both V1 spawn items and V2 started
activities to rebuild ownership without replaying duplicate transcript rows.
One bounded, session-cancellable worker follows descendants with read-only
`thread/read {includeTurns:true}` calls. It resumes only children whose
reported status is `active`, using `excludeTurns:true` solely to restore the
live subscription. A fresh `Session.Resume` starts a new traversal generation;
transient reads/resumes retry, conflicting/self-referential ownership is
rejected, and traversal stops after 256 descendants with a visible warning.

Known child `turn/started` is normalized to a launch-keyed running status. This
reactivates a previously completed child's spawn card when `followup_task`
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

MultiAgentV2 persists that delivery in the parent rollout as an
`inter_agent_communication` record:

```json
{
  "type": "inter_agent_communication",
  "payload": {
    "author": "/root/reviewer",
    "recipient": "/root",
    "content": "Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/reviewer\nPayload:\n<child result>",
    "internal_chat_message_metadata_passthrough": {"turn_id": "child-turn"},
    "trigger_turn": false
  }
}
```

That record—not child `turn/completed` and not `wait_agent` returning—is the
MultiAgentV2 transcript-completion boundary. Agent Overflow emits one flat
completion row per delivered child turn. Fresh sessions see the model-input
projection as a raw `agent_message` response item; resumed sessions see the
durable record above. Older rollouts that persisted the projected response item
remain supported.

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

The parent's `spawn_agent` item carries `agentsStates` — a map of
`thread_id → CollabAgentStatus`. Updated on the item envelope as
state changes. Surfaced by `enrichItemMeta` in `protocol.go` (the
`collabAgentToolCall` branch copies it into `extras.input.agentsStates`
alongside `tool` / `prompt` / `receiverThreadIds`) so the frontend can
render a live child-status badge without subscribing to every child
thread's session-status events. See
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
`seenTurnStarts` — safe for reconnect replay.

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
`Session.Probe` but not surfaced as turn-state signals (correct —
see [`turn-lifecycle.md`](../architecture/turn-lifecycle.md) on why
we don't infer turn activity from session status).

### `approvalsReviewer` (who answers an approval request)

Thread-level state deciding whether an escalation — sandbox escape,
blocked network, MCP approval, ARC escalation — is routed to the client
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
| `ThreadStartParams` / `ThreadResumeParams` / `ThreadForkParams` | `approvalsReviewer` | `Option` — omitted means "config default" |
| `ThreadStartResponse` / `ThreadResumeResponse` / `ThreadForkResponse` | `approvalsReviewer` | **non-`Option`** — always present |
| `TurnStartParams` | `approvalsReviewer` | `Option` — a per-turn override, same slot as `approvalPolicy` / `sandboxPolicy` |

**The silent-drop hazard.** `ThreadStartParams` has no
`#[serde(deny_unknown_fields)]`, so a codex predating the field accepts
the request, discards it, and starts a `user`-reviewer thread with a
success response. There is no capability handshake to gate on:
`initialize` carries no version or capability list for this, and
`thread/started` does not carry the reviewer. The versions:

- pre-0.115 — field unknown, **silently dropped**.
- 0.115–0.123 — field known, value rejected: `-32600`, unknown variant.
- 0.143+ (AO's floor, `internal/provider/codex_version.go`) — accepted.

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
thread **ignores every override in the request** —
`collect_resume_override_mismatches` collects the divergences and
`tracing::warn!`s them server-side, then rejoins the live config
unchanged (`thread_processor.rs`). A cold resume applies them normally.
So a bare `thread/resume {threadId}` against a loaded thread cannot
disturb the reviewer, while the same call against an evicted thread
would reset every unspecified axis to the config default. AO's mid-life
reconcile resumes (`session_probe.go` `Resume`,
`collab_rehydrate.go` `attachActiveChildWithRetry`) target loaded
threads and deliberately send no overrides — sending one that diverged
is what arms the shutdown-and-cold-resume branch. The handshake resume
in `NewSession` names the reviewer because it is the one that can be
cold, and every `turn/start` re-asserts it regardless.

`turn/start` applying it per turn is what makes runtime-mode transitions
live on Codex: `build_thread_settings_overrides` (`turn_processor.rs`)
treats `approvalsReviewer` like the other axes, so no tier change needs
a process restart.

### `thread/settings/updated`

`#[experimental]` — requires `capabilities.experimentalApi`. Fires
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
`activePermissionProfile` are nullable — null means "no override in
force", never a literal value. `sandboxPolicy.type` is camelCase
(`readOnly | workspaceWrite | dangerFullAccess`), the inverse of AO's
hyphenated vocabulary.

This is Codex's view of what the thread IS running, which is not the same
thing as what the client asked for: Codex can change model, effort or
tier on its own (reroute, guardian downgrade, config reload, another
client on the same thread). Agent Overflow keeps the two apart —
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
internal protocol). It means the stored OAuth grant is no longer usable —
the remedy is a sign-in, not a retry, so it must not be flattened into a
generic failure.

---

## Background terminals

`#[experimental]` — all three require `capabilities.experimentalApi`.
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
- `osPid`, `cpuPercent`, `rssKb` are nullable — absent is not zero.
- `terminated: false` means no running process matched (already exited,
  or belongs to another thread). It is a state answer, not an error.
- `list` paginates: pass a non-null `nextCursor` back as `cursor`.

---

## Skills

Skills are Codex's user-invokable prompt units — a directory holding a
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
global_shared_read("config")` — **global**, no thread, no turn, no
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
  cwd means a different directory depending on which process answered — a
  live session's workspace versus an ephemeral fetcher's WorkDir.
- **An empty `cwds` defaults to the answering process's own cwd.** That
  default is a property of the process, not of the request, so AO never
  relies on it (`buildSkillsListParams` rejects an empty list).
- `scope` is snake_case (`user | repo | system | admin`) while everything
  else on this wire is camelCase — `SkillScope` carries
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
name, so a consumer cannot narrow the drop — the only correct response is
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
missing either — it could be shown but not invoked.

---

## Code review — `review/start`

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

`ReviewTarget` is an internally-tagged union — `#[serde(tag = "type",
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

A review runs as a **non-steerable turn**: `turn/start` or `turn/steer`
against it fails with `codexErrorInfo:
{"type":"activeTurnNotSteerable","turnKind":"review"}`
(`v2/shared.rs`).

### Current state in agent-overflow

`Session.StartReview` (`session_review.go`) sends the RPC through a closed
`ReviewTarget` union whose only constructors are the four validated
variants; the zero value refuses to marshal. `ReviewStarted.Detached` is
derived from the returned id versus the session's own thread, so a server
that answers differently than asked is observed rather than assumed away.

`enteredReviewMode` / `exitedReviewMode` already reach the transcript as
`review_status` notification rows (`protocol_item.go`, rendered by
`NotificationRow.svelte`). A DETACHED review's own notifications arrive on
a thread this session does not own, so they hit the fail-closed
child-thread quarantine and are dropped — safe but inert until the
returned `reviewThreadId` is registered with the routing tables.

---

## Manual compaction — `thread/compact/start`

```json
{"method": "thread/compact/start", "params": {"threadId": "..."}}
→ {}
```

`ThreadCompactStart` with `serialization: thread_id(params.thread_id)`;
not `#[experimental]`. Since codex 0.96.0. Params and response are typed at
`codex-rs/app-server-protocol/src/protocol/v2/thread.rs`
(`ThreadCompactStartParams` / `ThreadCompactStartResponse`).

The response body is empty — **the boundary is not on it**. It surfaces as
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
`compact_remote.rs`, `compact_remote_v2.rs` — auto-compact included)
emit both halves. AO consumes `item/started` as `EventCompactionStatus`
Active (the `provider:compacting` window open) and `item/completed` as
the boundary. ⚠ A **failed** compaction sends an error event and never
completes its item — triage's turn-completion clear is the only close
on that path.

⚠ **`thread/compacted` is deprecated.** It is still in the notification
catalogue and still fires on older builds, so both paths must produce the
same downstream input. Agent Overflow emits `EventCompactBoundary` from
each (`protocol_item.go` for the item, `protocol_thread.go` for the
notification), which triage routes through one compaction-divider case.

Compaction is also a non-steerable turn (`turnKind: "compact"`), so callers
gate it on the thread being idle rather than racing a live turn.

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
`AGENT_OVERFLOW_DEBUG=provider` — raw JSON-RPC frames land in
`<dbDir>/logs/provider-events-YYYY-MM-DD.ndjson`.

---

## Contradictions and ambiguities

1. `"wait"` vs `"waitAgent"` — wire value is `"wait"`; some old
   docs and tests say `"waitAgent"`. Canonical: the Rust
   `CollabAgentTool::Wait` variant serialises to `"wait"` (camelCase
   but single word).
2. `activeFlags` enum — observed `runningBackground` and
   `waitingForUser` in test fixtures; full set not documented in
   codex-source's TypeScript schema. Treat as open.
3. `CollabAgentStatus` values — v2 schema lists seven; v1 wait tool
   reports them on `agentsStates`, v2 wait does not. Be defensive.

---

## When this doc is wrong

Capture fresh JSON-RPC (`AGENT_OVERFLOW_DEBUG=provider`), compare,
update before coding. For upstream ambiguities, check codex-source
at the pinned rev; if still unclear, CodexMonitor's handling is the
next-best authority.
