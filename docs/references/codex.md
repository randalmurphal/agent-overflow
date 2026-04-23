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

## Background terminals (unified_exec)

Codex has no `run_in_background` flag, but it does background work via
`exec_command`. The shape to remember:

- `CommandExecution.source == "unifiedExecStartup"` is the wire-typed
  signal for a command that may yield back to the model while its PTY
  keeps running.
- `yield_time_ms` (default 10s, `core/src/tools/handlers/unified_exec.rs`)
  governs how long the tool blocks before returning partial output.
- After yield, the PTY is tracked by `UnifiedExecProcessManager`;
  `spawn_exit_watcher` (`core/src/unified_exec/async_watcher.rs`) fires
  `ExecCommandEnd` when the process actually exits — potentially in a
  later turn, up to `background_terminal_max_timeout` (1h default).
- On the wire: one `item/*` pair. `item/started` (status `inProgress`),
  optional streaming output deltas, eventual `item/completed` (status
  flips in place, same `item_id`). No "yielded" event and no
  `is_background` flag. Agent Overflow synthesizes the sibling
  `tool_completion` row for background-tray/history parity.
- `exec_command` supports parallel tool calls (`supports_parallel_tool_calls = true`).
  Each parallel call has its own `call_id` and random-i32 `process_id`.
- `TerminalInteractionNotification` with empty `stdin` = the model
  polled a running background terminal via `write_stdin` without
  input (the "Waited for background terminal" cell in Codex TUI).

## Known upstream constraints

**Per-row termination of model-initiated background terminals is
unavailable via the app-server protocol.** What exists:

- `thread/backgroundTerminals/clean { thread_id }` — thread-wide only.
- `command/exec/terminate { process_id }` — works only for
  client-initiated `command/exec` PTYs (client supplies the process_id).
  Not applicable to model-initiated `exec_command` items.
- `close_agent`, `write_stdin` — **model tools**, not client-callable.
  (Claude's `KillShell` is similarly a model tool but ALSO reachable via
  the client-sent `stop_task` control_request on the stdio NDJSON
  channel — see [`claude-wire.md §stop_task`](claude-wire.md#stop_task).
  Codex has no equivalent client RPC.)
- `process_group_id` (the real OS PID) is stored internally on
  `SpawnedPty` (`codex-rs/utils/pty/src/pty.rs`) but never serialized
  onto the wire; clients can't kill by OS PID.

**Consequence for agent-overflow**: background-tray per-row stop
buttons for Codex terminals are blocked on an upstream protocol
change. The tray ships with thread-wide "Stop all" only until Codex
exposes a `thread/backgroundTerminals/killOne { thread_id, process_id }`
RPC or equivalent. Issue request drafted; contributions are invitation-only per
[Codex's contributing guide](https://github.com/openai/codex/blob/main/docs/contributing.md).
