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
- `background.go` — background tool tracking (Codex's long-running
  tool analogue to Claude's Task tool).
- `mcp.go` — MCP elicitation flow.
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

- `turn/started`, `turn/completed`.
- `tool/started`, `tool/completed`.
- Rate-limit, model-reroute, reasoning-delta, thread-rename,
  thread-compact events.
- `error` notifications (these are user-facing state, not log entries).

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

## Approvals

Sandbox method requests (`file/write`, `command/execute`, ...) are
translated into the shared `ApprovalRequest` shape before leaving this
package. MCP elicitation comes in as its own method and currently maps
to `Kind: "mcp-elicitation"` — confirm the frontend has a branch for
this before relying on it.

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
