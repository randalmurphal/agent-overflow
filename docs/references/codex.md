# Codex References

When touching `internal/provider/codex/` or any Codex-specific behavior,
use these repos as the source of truth — not guesses derived from our
own code.

## Reference Repos

- **Codex source** — https://github.com/openai/codex
  - Local mirror: `/Users/randy/repos/codex-source`.
  - Authoritative behavior of the `codex app-server` process, JSON-RPC
    method shapes, notification payloads, sandbox/approval policies.
  - Read this when the question is "what does Codex actually send?"

- **CodexMonitor** — https://github.com/Dimillian/CodexMonitor
  - Tauri-based, feature-complete client of the Codex app-server.
  - Strong reference implementation for protocol handling, UX flows,
    and operational safeguards around the Codex process.
  - Read this when the question is "how should a client integrate
    with Codex?"

## Docs

- Codex App Server: https://developers.openai.com/codex/sdk/#app-server

## Workflow

1. Check `codex-source` for the canonical wire format and method
   definitions before writing a parser or marshaler.
2. Cross-reference CodexMonitor for proven client patterns (process
   lifecycle, reconnect, interruption, rollback, fork).
3. If the two references disagree, the Codex source wins for
   wire-level concerns; CodexMonitor wins for client-side UX and
   recovery patterns.
4. If both sources are silent or ambiguous, run a spike test — see
   [spike-policy.md](spike-policy.md).
