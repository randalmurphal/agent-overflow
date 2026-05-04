# Codex `app-server` — JSON-RPC wire reference

Authoritative reference for the JSON-RPC 2.0 notifications Codex
emits over stdio. Consulted by `internal/provider/codex/`
parser code.

## Sources

**Shape-of-truth, in priority order:**

1. **codex-source** at `/Users/randy/repos/codex-source` — the
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
`source: "unifiedExecStartup"` is the wire-typed signal that an item
may become a background terminal. Per
[invariant 25](../architecture/invariants.md#25-codex-backgrounding-uses-wire-typed-signals-never-heuristics),
setting `is_background=true` on such items when they yield is the
sanctioned path — heuristic classifiers (event-ordering, etc.) are
forbidden because that's what produced ghost rows in the former
`BackgroundClassifier` (previously at
`internal/provider/codex/background.go`, retired).

On the wire, Codex items close via `item/completed` using the same
`item_id` — the status flips in place, no sibling row is emitted.
Clients that want tray-pair semantics (Claude-style launch + sibling
completion) synthesize the sibling row at the `item/completed`
boundary themselves. See
[`codex.md §Known upstream constraints`](codex.md#known-upstream-constraints)
for the per-row stop gap.

### 2. Items carry their own status on the wire

Each `item/*` notification includes a `status` field on the item
object directly (`inProgress | completed | failed | ...`). Unlike
Claude's "tool_use then tool_result" split, Codex items are
one-shot upserts — `item/started` creates the row, `item/completed`
updates it. Both handlers can share code via idempotent upsert.

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
[`codex-rs/app-server-protocol/schema/typescript/ServerNotification.ts`](file:///Users/randy/repos/codex-source/codex-rs/app-server-protocol/schema/typescript/ServerNotification.ts).

| `method` | Destination / purpose |
|---|---|
| `turn/started` | Turn lifecycle. `EventTurnStart`. |
| `turn/completed` | Turn lifecycle. `EventTurnComplete`. |
| `turn/aborted` | Turn lifecycle. `EventTurnComplete` with `Meta.aborted = true`. |
| `turn/diff/updated` | Per-turn unified-diff snapshot. |
| `turn/plan/updated` | Per-turn plan updates (markdown). |
| `item/started` | Tool/item lifecycle. `classifyItemNotification` → `EventToolStart` (or drop). |
| `item/completed` | Tool/item lifecycle. `classifyItemCompleted` → `EventToolComplete` (or drop). |
| `thread/started` | Session-level. First notification on a new thread; emits `EventSessionInit`. |
| `thread/status/changed` | Session-level. Thread status transitions; emits `EventSessionStatus`. |
| `thread/compacted` | Thread housekeeping. Compaction boundary event. |
| `thread/name/updated` | Thread housekeeping. Thread name/title changed. |
| `thread/tokenUsage/updated` | Thread housekeeping. Rolling token-usage snapshot. |
| `account/rateLimits/updated` | Rate-limit state updates. Surfaced as `EventSessionStatus` with `kind: rate_limited_*`. |
| `model/rerouted` | Model reroute notice (Codex fell back to a different model). |
| `configWarning` | Session-level notice surfaced to the user. |
| `deprecationNotice` | Session-level deprecation notice. |
| `serverRequest/resolved` | Fires when a previously-sent server request (approval / elicitation) has been resolved by the client. |

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

`last.totalTokens` is what occupies the visible context window. The
rolling `total.totalTokens` value is aggregate processed/spend-style
accounting across messages and must not be shown as context used in the
meter. Keep that aggregate out of the context-meter payload; if future
diagnostics need it, carry it on an explicitly diagnostic or turn
accounting path.

### Server requests (approvals, tool-user-input, elicitation)

Approvals arrive as **server requests** (with a JSON-RPC `id`), not as
notifications. The client is expected to respond with a matching
`id`. Authoritative list from
[`codex-rs/app-server-protocol/schema/typescript/ServerRequest.ts`](file:///Users/randy/repos/codex-source/codex-rs/app-server-protocol/schema/typescript/ServerRequest.ts):

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

## Collab agent lifecycle (`spawn_agent`, `wait`, `close_agent`, etc.)

The closest Codex analog to Claude's backgrounded tools, but
structurally different — **a spawn creates a child thread**, not a
backgrounded process inside the parent tool call. Agent Overflow
projects this into the shared background UI when that child is still
non-terminal after the parent turn closes.

### The `CollabAgentTool` enum

Defined at `codex-rs/app-server-protocol/src/protocol/v2.rs:4977`:

```
CollabAgentTool = "spawnAgent" | "sendInput" | "resumeAgent" | "wait" | "closeAgent"
```

⚠ **The wire value for "wait" is `"wait"`, NOT `"waitAgent"`.**
`protocol.go` normalizes that to `wait_agent` and routes it as a distinct
itemType; keep accepting the older `"waitAgent"` spelling only as a
defensive alias.

### Spawn flow (parent thread)

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

With `thread/start.experimentalRawEvents=true`, some app-server builds also
emit `rawResponseItem/completed` for the model-facing function call and its
tool output. These raw items can carry the same label metadata:

```json
{"type":"function_call","name":"spawn_agent","call_id":"call_spawn","arguments":"{\"agent_type\":\"explorer\",\"message\":\"Inspect parser\"}"}
{"type":"function_call_output","call_id":"call_spawn","output":"{\"agent_id\":\"child-thread\",\"nickname\":\"Boyle\"}"}
```

Agent Overflow treats the typed `item/*` lifecycle as authoritative for the
visible tool row. `thread/read` is the primary label source; raw response
items are only an additional typed signal when present, not a prerequisite.

### Parent learning the child finished

Two paths:

**(a) Explicit `wait` tool**: the parent agent can call `wait` with
a list of child `thread_id`s to block on. Emits
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
The typed `item/completed` usually also carries `agentsStates`, and
that typed state remains the source used to synthesize the indented
spawn-agent completion row.

**(b) Implicit via `<subagent_notification>`**: When a detached
child finishes and the parent has NO `wait` outstanding, Codex core
enqueues an `InterAgentCommunication` with
`trigger_turn: false`. On the parent's NEXT user turn, the injected
user message includes:

```
<subagent_notification>
{"agent_path":"<child_thread_reference>","status":"completed"}
</subagent_notification>
```

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
Child-thread `turn/started` and `turn/completed` on the child are
suppressed as session events at the parent level
(`session.go:623-625`) to avoid spurious turn-lifecycle updates on
the parent, but child `item/*` events are relayed via `ParentToolUseID`.

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
 "params": {"threadId": "...", "turnId": "...", "startedAt": 1776577311}}
```

Emits `EventTurnStart`. `session.go` dedupes on `turnId` via
`seenTurnStarts` — safe for reconnect replay.

---

## `turn/completed`

```json
{"method": "turn/completed",
 "params": {
   "threadId": "...",
   "turnId": "...",
   "status": {"type": "completed"},
   "usage": {...},
   "lastAssistantMessageId": "...",
   "completedAt": 1776577321
 }}
```

### `status.type` values
`completed | interrupted | failed | inProgress` (per
`v2/TurnStatus.ts`).

### ⚠ `lastAssistantMessageId` is on this envelope

Unlike Claude's `result`, Codex's `turn/completed` DOES carry the
last assistant message id directly. Use it for
`turns.assistant_message_id` on the `turns` row.

### Emission

`classifyTurnCompleted` at `protocol.go:99-130` emits
`EventTurnComplete` with `Meta.turn_status` mirroring the upstream
status.

---

## `turn/aborted`

Fires on user interrupt. `classifyTurnAborted` at
`protocol.go:68-77` synthesizes an `EventTurnComplete` with
`Meta.aborted: true` and `Meta.turn_status: "interrupted"`.

---

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

---

## `<subagent_notification>` tag inside user messages

When Codex core detects a detached child thread that finished without
a matching `wait` outstanding on the parent, it injects a notification
fragment into the parent's NEXT user-message item. The fragment lands
as part of the `item/completed` (`type: userMessage`) text content,
wrapped in `<subagent_notification>` tags.

### Authoritative wire shape

Produced by `format_subagent_notification_message` at
[`codex-rs/core/src/session_prefix.rs:8-18`](file:///Users/randy/repos/codex-source/codex-rs/core/src/session_prefix.rs):

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

Extraction is wired at the parser (`session.go` pulls
`<subagent_notification>` fragments out of user-message item text) and
emits `EventSubagentNotification`. The provider maps named `agent_path`
values back to the parent `spawn_agent` item when it has seen the child
`thread/started`; triage falls back to receiver-thread matching for
legacy unnamed flows. Either path writes the same `tool_completion`
sibling row used by explicit `wait_agent` completion.

---

## Captured samples

2026-05-03 spike against `codex-cli 0.128.0` confirmed
`experimentalRawEvents` behavior for terminal waits and collab agents.
Important ordering observed:

- Empty-stdin terminal wait start arrives as raw
  `function_call name=write_stdin` before the typed
  `item/commandExecution/terminalInteraction`.
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
