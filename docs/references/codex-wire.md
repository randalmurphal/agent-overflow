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

**Capturing fresh samples**: set `AGENT_OVERFLOW_DEBUG=provider` before
launching the app. Raw stdio lines (pre-parse, JSON-RPC framing
included) land in `<dbDir>/logs/provider-events-YYYY-MM-DD.ndjson`.

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

**Implication for agent-overflow**: the `tool_completion` sibling-row
model and the `BackgroundTaskTray` are **Claude-specific**. Codex
tool_calls always close via `item/completed`; there is no sibling row
to append. No Codex code stamps `is_background=true`; the former
`BackgroundClassifier` (previously at
`internal/provider/codex/background.go`) was retired because its
heuristic ("assistant text after tool started = background") didn't
map to anything real on the Codex wire.

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
backgrounded tool.

### The `CollabAgentTool` enum

Defined at `codex-rs/app-server-protocol/src/protocol/v2.rs:4977`:

```
CollabAgentTool = "spawnAgent" | "sendInput" | "resumeAgent" | "wait" | "closeAgent"
```

⚠ **The wire value for "wait" is `"wait"`, NOT `"waitAgent"`.** Our
`protocol.go:640-655` `normalizeCollabToolName` should accept `"wait"`
and route it to a distinct itemType (currently it returns `"wait"`
but the downstream switch doesn't branch on it).

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

### Parent learning the child finished

Two paths:

**(a) Explicit `wait` tool**: the parent agent can call `wait` with
a list of child `thread_id`s to block on. Emits
`CollabWaitingBegin` → `CollabWaitingEnd` (which surface as
`item/started` + `item/completed` for `tool: "wait"`,
`receiverThreadIds` on the item, and in V1 `agentsStates` populated
with per-agent terminal status on the end event).

**(b) Implicit via `<subagent_notification>`**: When a detached
child finishes and the parent has NO `wait` outstanding, Codex core
enqueues an `InterAgentCommunication` with
`trigger_turn: false`. On the parent's NEXT user turn, the injected
user message includes:

```
<subagent_notification>
{"agent_id":"<child_thread_id>","status":"completed"}
</subagent_notification>
```

Test coverage:
`codex-rs/core/tests/suite/subagent_notifications.rs:274-296`.

### Parent turn vs child lifecycle

The parent's `turn/completed` fires **without waiting** for spawned
children. There is NO signal like "parent done, still awaiting child."
Child runs independently on its own thread id, producing its own
`turn/completed` stream relayed through the session's
`childParentByThread` map (`internal/provider/codex/session.go:85-97`,
`710-736`).

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
{"agent_path":"<child_thread_reference>","status":"<AgentStatus>"}
</subagent_notification>
```

⚠ **The wire field is `agent_path`, NOT `agent_id`.** It's a
reference to the child thread (thread-id-style path), not a bare id.
The `status` value is a serialized `AgentStatus` — one of Codex's
`CollabAgentStatus` variants.

### Current state in agent-overflow

Extraction is **wired at the parser** (`session.go` pulls
`<subagent_notification>` fragments out of user-message item text) but
emission to the frontend is **deferred** — the parser produces no
visible UI event yet, because the UX for a detached-subagent
completion notice hasn't landed. The internal event exists as a stub
so the plumbing can be turned on without another parser change.

---

## Captured samples

No captured Codex samples on hand. To capture, run a session in
agent-overflow with `AGENT_OVERFLOW_DEBUG=provider` — raw JSON-RPC
frames land in `<dbDir>/logs/provider-events-YYYY-MM-DD.ndjson`.

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
