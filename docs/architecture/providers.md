# Providers

Both supported providers run as subprocesses talking over stdio. Go brokers;
the provider process owns turn state.

## Process Model

- One provider process per **active** thread. Inactive threads hold no
  process; spawning happens on resume.
- Claude Code: `claude --output-format stream-json --input-format stream-json
  --verbose`. NDJSON both directions. Authentication is via the CLI's own
  OAuth flow — we don't see credentials.
- Codex: `codex app-server`. JSON-RPC 2.0 over stdio. Same one-process-per-thread
  shape that forge proved out.

## Session Identity

- `session_ref` on `threads` records the provider-side session file path
  (`~/.claude/...` or `~/.codex/...`). This is what resume feeds back in.
- Fork creates a new thread row; `parent_thread_id` records the lineage.
  Codex has a native `thread/fork` method; Claude forks by replay from a
  chosen turn.

## Approvals (Bidirectional)

```
provider stdout → Go parses → emits approval-request event (with request_id)
                                       ↓
                              frontend renders ApprovalPrompt
                                       ↓
                 user clicks → frontend → Go (RespondToApproval)
                                       ↓
                 Go marshals provider-specific → writes to provider stdin
                                       ↓
                              provider resumes turn
```

Both providers use the same frontend shape (`ApprovalRequest` with `kind`).
Provider-specific adaptation (`CanUseTool` vs Codex sandbox/approval policies)
stays inside the provider package.

Approval `kind` values that currently arrive from providers:

- `user-input` — multi-question form.
- `permission` — grant scope: `turn` or `session`.
- `file-change`, `file-read`, `command` — Codex sandbox approvals.
- `mcp-elicitation` — MCP server dynamic config request.

The frontend must render a branch for each kind it can receive. Missing
branches cause a silent dead-end.

## Rate Limits, Costs, Compaction

- Rate limit events surface through the provider package's normalizer and
  become a single event shape the frontend renders in the status bar.
- Cost and token counts update on turn completion and are persisted per
  thread.
- Claude emits `compact_boundary` markers when it compacts context. These
  are preserved as timeline items so forked threads can re-derive state.

## What Goes Where

- Protocol parsing, framing, and session lifecycle → `internal/provider/{claude,codex}`.
- Event classification and routing → `internal/triage`.
- Persistence and retrieval → `internal/store`.
- Nothing provider-specific escapes `internal/provider/` into triage or store.

For Claude NDJSON specifics see `internal/provider/claude/AGENTS.md`.
For Codex JSON-RPC specifics see `internal/provider/codex/AGENTS.md`.
