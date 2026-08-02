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

## Background terminals

**Per-row stop is available since codex 0.140.0.** Resolved 2026-06-10
upstream, verified on codex-cli 0.146.0
(2026-08-02).** This section previously recorded per-process termination
as an upstream gap. It is not one any more, and the two claims that
justified it were both falsified: `process_id` IS on the wire, and a
per-process kill RPC exists.

The client-callable surface, all `#[experimental]` (needs
`capabilities.experimentalApi`, which every AO handshake sets):

| Method | Params | Response |
|---|---|---|
| `thread/backgroundTerminals/list` | `{threadId, cursor?, limit?}` | `{data: ThreadBackgroundTerminal[], nextCursor: string\|null}` |
| `thread/backgroundTerminals/terminate` | `{threadId, processId}` | `{terminated: bool}` |
| `thread/backgroundTerminals/clean` | `{threadId}` | `{}` (thread-wide) |

`ThreadBackgroundTerminal` = `{itemId, processId, command, cwd, osPid?,
cpuPercent?, rssKb?}`. `itemId` is the join key back to the transcript
row; the three host-OS metrics are nullable and must not be rendered as
zero when absent. `terminated: false` means the RPC matched no running
process (already exited, or not this thread's) — a state answer, not a
failure.

Types: `codex-rs/app-server-protocol/src/protocol/v2/thread.rs`
(`ThreadBackgroundTerminals*`); dispatch table
`codex-rs/app-server-protocol/src/protocol/common.rs`. Agent Overflow
wraps all three in `internal/provider/codex/session_background.go`.
Version floor: `terminate`/`list` shipped in 0.140.0, below AO's
provider-wide minimum of 0.143.0, so no runtime capability probe is
needed (guarded by
`provider.TestMinimumCodexCLIVersionCoversBackgroundTerminalTerminate`).

Still true, and still not a workaround worth taking:

- `command/exec/terminate { process_id }` applies only to
  client-initiated `command/exec` PTYs, not model-initiated
  `exec_command` items. Use the background-terminal RPCs instead.
- `close_agent` and `write_stdin` remain **model tools**, not
  client-callable. Killing a spawned collab-agent child thread from the
  client still has no path. (Claude's `KillShell` is similarly a model
  tool but ALSO reachable via the client-sent `stop_task`
  control_request — see
  [`claude-wire.md §stop_task`](claude-wire.md#stop_task).)

## Known upstream constraints

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
