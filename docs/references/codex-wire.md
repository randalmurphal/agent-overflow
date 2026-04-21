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
to append. `internal/provider/codex/background.go`'s
`BackgroundClassifier` should NOT flag any Codex tools as
`is_background: true` — the heuristic it uses ("assistant text after
tool started = background") doesn't map to anything real on the
Codex wire.

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

Every top-level envelope is a JSON-RPC 2.0 notification:
`{"jsonrpc":"2.0","method":"<method>","params":{...}}`. Dispatched
in `internal/provider/codex/protocol.go`.

| `method` | Destination |
|---|---|
| `item/started` | `classifyItemNotification` → `EventToolStart` (or drop) |
| `item/completed` | `classifyItemCompleted` → `EventToolComplete` (or drop) |
| `turn/started` | `EventTurnStart` |
| `turn/completed` | `EventTurnComplete` |
| `turn/aborted` | `EventTurnComplete` with `Meta.aborted = true` |
| `thread/created` | `EventSessionInit` |
| `thread/status_changed` | `EventSessionStatus` |
| `rate_limit/warning` | `EventSessionStatus` with `kind: rate_limited_*` |
| `approval/request`, `approval/resolved` | Approval pipeline |

Detailed fields live in
`codex-rs/app-server-protocol/schema/typescript/v2/`. Read that when
adding handlers; the TypeScript schema is the canonical shape
reference.

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

### Item types handled by `classifyCodexItemType` (protocol.go:628)

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
state changes. **Currently not surfaced** by our `enrichItemMeta`
(`protocol.go:594-617`). CodexMonitor uses it for a live child-status
badge inside the spawn card (see
`CodexMonitor/src/utils/threadItems.collab.ts:299-369`).

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

### `thread/created`

First notification on a session. Carries the thread id and initial
metadata.

### `thread/status_changed`

```json
{"method": "thread/status_changed",
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

When Codex core injects a subagent completion notification, it
surfaces as part of the NEXT `item/completed` (`type: userMessage`)
block's text content, wrapped in `<subagent_notification>` tags. If
we want a distinct "subagent finished" UI event, parse the tag out
of the user-message item text at the triage layer and synthesize an
event. (Not currently done.)

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
