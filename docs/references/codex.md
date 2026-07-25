# Codex References

When touching `internal/provider/codex/` or any Codex-specific behavior,
use these repos as the source of truth — not guesses derived from our
own code.

## Reference Repos

- **Codex source** — https://github.com/openai/codex
  - Local mirror: `/home/rmurphy/repos/codex`.
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
  signal for a unified exec command. Raw `exec_command` result text can say
  whether the model saw a running process or an immediate exit, but it is not
  the UI history source.
- `yield_time_ms` (default 10s, `core/src/tools/handlers/unified_exec.rs`)
  governs how long the tool blocks before returning partial output.
- After yield, the PTY is tracked by `UnifiedExecProcessManager`;
  `spawn_exit_watcher` (`core/src/unified_exec/async_watcher.rs`) fires
  `ExecCommandEnd` when the process actually exits — potentially in a
  later turn, up to `background_terminal_max_timeout` (1h default).
- On the wire: one `item/*` pair. `item/started` (status `inProgress`),
  optional streaming output deltas, eventual `item/completed` (status
  flips in place, same `item_id`). No "yielded" event and no
  `is_background` flag. Agent Overflow clears transient live state on typed
  `item/completed`; it persists the same item id as the command row only while
  a Codex wire round is still active, matching Codex TUI timing.
- `exec_command` supports parallel tool calls (`supports_parallel_tool_calls = true`).
  Each parallel call has its own `call_id` and random-i32 `process_id`.
- `TerminalInteractionNotification` with empty `stdin` = the model
  polled a running background terminal via `write_stdin` without
  input (the "Waited for background terminal" cell in Codex TUI).

## MultiAgentV2 child routing

Codex 0.144 can select `multi_agent_version:"v2"`. Its canonical spawn
signal is an `item/completed` `subAgentActivity` with `kind:"started"`,
`agentThreadId`, and `agentPath`; it is not a `collabAgentToolCall` spawn.
Core can start the child before this parent-side activity is emitted, so any
unmapped non-root provider thread must be quarantined rather than treated as
the AO root. A started activity emitted on a child creates a nested ownership
edge for a grandchild. Reopen recovery walks persisted descendant histories
with bounded read-only `thread/read` calls and resumes only currently-active
children for notification subscription. See
[`codex-wire.md §Collab agent lifecycle`](codex-wire.md#collab-agent-lifecycle-multiagentv1-and-multiagentv2).

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

**History truncation via `thread/fork` is turn-granular only.**
Source-verified at rust-v0.144.5 and rust-v0.145.0-alpha.23
(2026-07-17): `ThreadForkParams`'s only cut is `last_turn_id`
(`truncate_rollout_after_turn_id`). codex-rs core already has a
message-granular fork cut — `ForkSnapshot::TruncateBeforeNthUserMessage`
slices the rollout strictly before the nth user message, mid-turn
steers included, and it is what the Codex TUI's Esc-Esc backtrack uses
via `thread/rollback` (whose `num_turns` counts user-message
boundaries, not wire turns) — but that RPC replies "thread/rollback is
deprecated and will be removed soon" and mutates the source thread in
place instead of forking. **Consequence for agent-overflow**: Codex
revert-to-message drops the whole anchor turn (Claude's session-file
slice is message-granular); parity lands when `thread/fork` exposes a
message-granular anchor. See the granularity note on
`codex.Session.ForkAt`.
