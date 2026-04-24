# internal/provider/codex/

Wraps `codex app-server`. One process per active thread, JSON-RPC 2.0
over stdio.

## Layout

- `session.go` — process lifecycle, the JSON-RPC read loop, and the
  top-level dispatch for requests and notifications.
- `session_helpers.go` — sync/async request helpers, ID generation,
  per-session correlation (pending calls map, ThreadItem cache).
- `session_probe.go` — `thread/read` liveness probe used on reopen to
  reconcile stale running rows. `session_probe_testhelpers.go` hosts
  the fake probe reply harness.
- `session_fork.go` / `session_rollback.go` — the `thread/fork` and
  `thread/rollback` RPC call wrappers.
- `protocol.go` — JSON-RPC frame shapes, turn/tool ThreadItem variants,
  v2 status enum, marshal/unmarshal.
- `protocol_rate_limits.go` — rate-limit event normalizer.
- `approval.go` — sandbox approval-method translation into the shared
  `ApprovalRequest` Kind.
- `mcp.go` — MCP elicitation flow.
- `subagent_notifications.go` — `<subagent_notification>` XML-tag parser
  for detached-child-agent terminal signals injected into the next
  user-message. Pure parsing (regex + JSON decode), no Session state.
- `options.go` — `SessionOptions → Config` hydration, binary probe.

## Methods we call

- `thread/new`, `thread/resume`, `thread/fork`, `thread/rollback`.
- `thread/read` — on-reopen liveness probe. Called by `Session.Probe`
  to fetch the current `thread.status.type` so the app-layer reconciler
  can flip stale running background tool rows.
- `message/send` — deliver a user turn.
- Sandbox/approval method family — `file/write`, `file/delete`,
  `file/mkdir`, `command/execute`, etc. These arrive as requests we
  must respond to.

## Notifications we handle

⚠ **Authoritative wire reference**:
[`docs/references/codex-wire.md`](../../../docs/references/codex-wire.md).
Read that before adding or changing handler logic — it has the
canonical JSON-RPC examples, pinned citations into codex-source
and CodexMonitor, and the rules for how `item/*`, `turn/*`, and the
collab-agent lifecycle interact.

Summary:

- `turn/started`, `turn/completed`, `turn/aborted` — **turn
  lifecycle**. Authoritative turn-complete signal; emit
  `EventTurnComplete` which triage forwards as
  `provider:turn_completed` to the frontend.
- `item/started`, `item/completed` — **tool lifecycle**. Every tool
  produces both (one-shot upsert pattern). Items carry their own
  `status` field (`inProgress | completed | failed | ...`) directly
  on the wire.
- `thread/status/changed` — session-level state.
- Rate-limit, model-reroute, reasoning-delta, thread-rename,
  thread-compact events.
- `error` notifications (user-facing state, not log entries).

## Lifecycles we drive

- **Tool lifecycle** — every `item/*` fires `EventToolStart` /
  `EventToolComplete` for the item's own id. Status comes from the
  wire `item.status`. See
  [`turn-lifecycle.md §Tool lifecycle`](../../../docs/architecture/turn-lifecycle.md#1-tool-lifecycle).
- **Turn lifecycle** — `turn/completed.lastAssistantMessageId` is
  the authoritative final-message marker; use it for the `turns`
  row.
- **Background terminals.** Codex has no `run_in_background` flag, but
  `exec_command` can yield back to the model while its PTY keeps
  running. `CommandExecution.source == "unifiedExecStartup"` is the
  wire-typed authorizing signal (per
  [invariant 25](../../../docs/architecture/invariants.md#25-codex-backgrounding-uses-wire-typed-signals-never-heuristics)).
  This package surfaces `source`, `item_status`, `process_id`,
  `agentsStates`, and `receiverThreadIds` on the event Meta via
  `enrichItemMeta`; the projector in
  `internal/triage/codex_background.go` stamps `is_background=true` on
  the first model-produced yield (text/reasoning delta) or at
  `turn/completed` as the catchall, and synthesizes the sibling
  `tool_completion` row at completion time via `maybeDeferOrPersist`.
  Heuristic event-ordering classifiers are still forbidden as the
  authorization — the yield moment is just the observable trigger for
  an already wire-authorized commitment. Per-row stop for backgrounded
  commands is blocked on an upstream protocol change; see
  [`codex.md §Known upstream constraints`](../../../docs/references/codex.md#known-upstream-constraints).

## Collab agent lifecycle (spawn_agent / wait / close_agent / send_input / resume_agent)

Codex's closest analog to Claude's backgrounded tools, but
structurally different — **a spawn creates a child thread**, not
a backgrounded tool. See
[`codex-wire.md §Collab agent lifecycle`](../../../docs/references/codex-wire.md#collab-agent-lifecycle-spawn_agent-wait-close_agent-etc)
for the wire sequence. Key points:

- `spawn_agent` tool_call completes **immediately** (status:
  completed) when the spawn request is accepted; child runs on a
  separate `thread_id`.
- Parent's `turn/completed` does NOT wait for spawned children.
- Child completion signals the parent via either explicit `wait`
  tool OR a `<subagent_notification>` XML tag in the next user
  message.
- Wire enum for the wait tool is `"wait"` (not `"waitAgent"`).

## Responsibility boundary

- What BELONGS here:
  - JSON-RPC frame marshal / unmarshal and v2 ThreadItem shape handling.
  - Sandbox-method → `ApprovalRequest` translation.
  - Session request/response/notification correlation.
  - `options.go` binary probe and `Config.From(SessionOptions)`.
- What does NOT belong here:
  - Raw JSON-RPC shape inspection outside this package. Other callers
    should only see the normalized `provider.Event` types.
  - SQLite writes, `app.Event.Emit`, or cross-thread orchestration.

## Interactive Requests

Sandbox method requests (`file/write`, `command/execute`, ...) are
translated into the shared `ApprovalRequest` shape before leaving this
package. MCP elicitation comes in as its own method and currently maps
to `Kind: "mcp-elicitation"` — confirm the frontend has a branch for
this before relying on it.

`item/tool/requestUserInput` is a separate structured user-input flow, not
an approval kind. Validate that at least one question is present before
emitting, track it with the user-input resolve kind, and answer through
`RespondToUserInput`.

## Extension points

- To add a new JSON-RPC method/notification: add the frame shape in
  `protocol.go`, dispatch in `session.go`, unit test the round-trip,
  then normalize into a `provider.Event` kind.
- To add a new sandbox approval method: extend `approval.go`'s kind
  table and the shared `provider.ApprovalRequest`. Spike against
  `codex app-server` if the method shape isn't obvious from
  `codex-source`.

## Anti-patterns

- Do NOT inspect raw JSON-RPC shape from outside this adapter. Callers
  see normalized `provider.Event` values only.
- Do NOT invent `ThreadItem.status` values. Respect the v2 status enum
  from upstream — if a new status is needed, update `protocol.go` and
  the reconciler together.
- Do NOT fall back to a generic "unknown notification" path. Each
  notification type has an explicit dispatch or an explicit "ignored"
  log line.

## References

- **Codex source** — https://github.com/openai/codex
  Local: `/Users/randy/repos/codex-source`. Canonical wire format. If
  our parser disagrees with the source, the source wins.
- **CodexMonitor** — https://github.com/Dimillian/CodexMonitor
  Tauri, feature-complete client. Reference for client-side patterns:
  process lifecycle, reconnect, interruption, rollback, fork.
- Codex App Server docs: https://developers.openai.com/codex/sdk/#app-server
- `docs/references/codex.md` — workflow for reading these sources.
- `docs/references/spike-policy.md` — when both are silent.
