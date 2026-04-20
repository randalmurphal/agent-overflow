# internal/provider/codex/

Wraps `codex app-server`. One process per active thread, JSON-RPC 2.0
over stdio.

## Methods We Call

- `thread/new`, `thread/resume`, `thread/fork`, `thread/rollback`
  (rollback exists in the protocol; frontend wiring is incomplete —
  see the missing-pieces backlog).
- `thread/read` — on-reopen liveness probe. Called by `Session.Probe`
  to fetch the current `thread.status.type` so the app-layer reconciler
  can flip stale running background tool rows (see `session_probe.go`).
- `message/send` — deliver a user turn.
- Sandbox/approval method family — `file/write`, `file/delete`,
  `file/mkdir`, `command/execute`, etc. These arrive as requests we
  must respond to.

## Notifications We Handle

- `turn/started`, `turn/completed`.
- `tool/started`, `tool/completed`.
- Rate-limit, model-reroute, reasoning-delta, thread-rename,
  thread-compact events.
- `error` notifications (these are user-facing state, not log entries).

## Approvals

Sandbox method requests (`file/write`, `command/execute`, ...) are
translated into the shared `ApprovalRequest` shape before leaving this
package. MCP elicitation comes in as its own method and currently maps
to `Kind: "mcp-elicitation"` — confirm the frontend has a branch for
this before relying on it.

## Reference Repos (Read These Before Guessing)

- **Codex source** — https://github.com/openai/codex  
  Local: `/Users/randy/repos/codex-source`.  
  The canonical wire format and method definitions. If our parser
  disagrees with the source, the source wins.
- **CodexMonitor** — https://github.com/Dimillian/CodexMonitor  
  Tauri, feature-complete client. The reference for client-side
  patterns: process lifecycle, reconnect, interruption, rollback, fork.

Workflow:

1. Open `codex-source` for wire format when writing or modifying a
   parser/marshaler.
2. Cross-reference CodexMonitor for proven client patterns.
3. If both sources are silent or disagree on client behavior,
   spike-test against a real `codex app-server` before changing
   this code. See `docs/references/spike-policy.md`.

## Docs

- Codex App Server: https://developers.openai.com/codex/sdk/#app-server
