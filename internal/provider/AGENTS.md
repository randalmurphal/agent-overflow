# internal/provider/

Owns every wire-level interaction with a coding-agent subprocess. Triage
and store consume the output; nothing downstream should need to know which
provider produced an event.

## Structure

- `provider/` — shared types and interfaces (event shapes, approval
  kinds, session-registry helpers).
- `provider/claude/` — Claude Code CLI integration. NDJSON over stdio.
- `provider/codex/` — Codex `app-server` integration. JSON-RPC 2.0 over
  stdio.

Each subpackage has its own `AGENTS.md`.

## Contract With the Rest of the App

- Output: a stream of provider-agnostic events on a channel. Triage reads
  this channel.
- Input: approval responses, new-turn messages, interrupt/rollback/fork
  commands — all called as methods on the provider's session object.
- Lifecycle: `Start → stream → Stop`. Reconnect is the caller's job
  (session registry in `app.go`).
- **No ownership of SQLite, no `app.Event.Emit` calls here.** The provider
  package only produces structured values. `app.go` and `triage` decide
  where those values go.

## Approvals

The two providers disagree about everything except that there is an
approval. We normalize early:

- Both produce `ApprovalRequest` values with a `Kind` the frontend can
  branch on (`user-input`, `permission`, `file-change`, `file-read`,
  `command`, `mcp-elicitation`).
- The original provider shape is preserved where we need it to send a
  response back, but the frontend never sees it.
- When the provider introduces a new request type, add a new `Kind`
  here and make sure the frontend has a branch for it before shipping.
  A `Kind` the frontend doesn't render is a silent dead-end.

## When Behavior Is Unclear

Don't infer from this code. Confirm with the references:

- Claude → `@anthropic-ai/claude-agent-sdk` and the `forge` ClaudeAdapter.
- Codex → `codex-source` (wire format) and `CodexMonitor` (client
  patterns). See `docs/references/codex.md`.

If both references are silent, spike-test in isolation
(`docs/references/spike-policy.md`) before changing this code.
