# Codex References

When touching `internal/provider/codex/` or any Codex-specific behavior,
use these repos as the source of truth, not guesses derived from our
own code.

## Reference Repos

- **Codex source**: https://github.com/openai/codex
  - Local mirror: `/home/rmurphy/repos/codex`.
  - Authoritative behavior of the `codex app-server` process, JSON-RPC
    method shapes, notification payloads, sandbox/approval policies.
  - Read this when the question is "what does Codex actually send?"

- **CodexMonitor**: https://github.com/Dimillian/CodexMonitor
  - Tauri-based, feature-complete client of the Codex app-server.
  - Strong reference implementation for protocol handling, UX flows,
    and operational safeguards around the Codex process.
  - Read this when the question is "how should a client integrate
    with Codex?"

## Docs

- Codex App Server: https://developers.openai.com/codex/sdk/#app-server
- [codex-instructions-tools.md](codex-instructions-tools.md):
  source-verified facts on `baseInstructions` replacement, the per-model
  catalog instruction templates, and the per-thread config keys that
  remove tool schemas from the request.

## Workflow

1. Check `codex-source` for the canonical wire format and method
   definitions before writing a parser or marshaler.
2. Cross-reference CodexMonitor for proven client patterns (process
   lifecycle, reconnect, interruption, rollback, fork).
3. If the two references disagree, the Codex source wins for
   wire-level concerns; CodexMonitor wins for client-side UX and
   recovery patterns.
4. If both sources are silent or ambiguous, run a spike test. See
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
  `ExecCommandEnd` when the process actually exits, potentially in a
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
children for notification subscription. The V2 activity and raw spawn request
do not report the effective child profile. Agent Overflow reads model and
reasoning effort from the child's metadata-only
`thread/resume {excludeTurns:true}` response, without replaying turns. See
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
process (already exited, or not this thread's). That is a state answer,
not a failure.

`processId` is a STRING on the wire that upstream parses as an `i32`
(`thread_background_terminals_terminate_inner`), and it is the same
value the `commandExecution` item carries as `processId`, and both come
from `UnifiedExecProcessManager`'s process-store key
(`ToolEmitter::unified_exec(..., Some(request.process_id.to_string()))`
in `codex-rs/core/src/unified_exec/process_manager.rs`, versus
`entry.process_id.to_string()` in the same file's `list_processes`).
That identity is what lets the tray's per-row Stop join a transcript row
to a running PTY without a `list` call.

Types: `codex-rs/app-server-protocol/src/protocol/v2/thread.rs`
(`ThreadBackgroundTerminals*`); dispatch table
`codex-rs/app-server-protocol/src/protocol/common.rs`. Agent Overflow
wraps all three in `internal/provider/codex/session_background.go`.
`terminate` is bound as `App.TerminateCodexBackgroundTerminal` and backs
the tray's per-row Stop button; `clean` backs Stop-all.
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
  control_request. See
  [`claude-wire.md §stop_task`](claude-wire.md#stop_task).)

## Known upstream constraints

**History truncation is turn-granular only, on every cut upstream
offers.** Source-verified at rust-v0.144.5 / rust-v0.145.0-alpha.23
(2026-07-17) and re-verified at rust-v0.150.1 (2026-08-29).
`ThreadForkParams` has grown a second anchor since the first check:
`last_turn_id` (`truncate_rollout_after_turn_id`, inclusive) and
`before_turn_id` (`truncate_rollout_before_turn_id`, exclusive, added in
0.146.0 behind `#[experimental("thread/fork.beforeTurnId")]`, and the two
cannot be combined). 0.148.0 added a third,
`thread/revert { threadId, beforeTurnId }`
(`#[experimental("thread/revert")]`), which replaces the thread's durable
history in place and emits `thread/reverted`; AO prefers it for
edit-and-resend wherever the thread supports it, and asks for the
paginated history it requires on `thread/start` at the same 0.148 floor
(see `internal/provider/codex/AGENTS.md` §"History truncation").
**All three cut on TURN boundaries**, so the consequence below is
unchanged by any of them. codex-rs core already has a
message-granular fork cut: `ForkSnapshot::TruncateBeforeNthUserMessage`
slices the rollout strictly before the nth user message, mid-turn
steers included, and it is what the Codex TUI's Esc-Esc backtrack uses
via `thread/rollback` (whose `num_turns` counts user-message
boundaries, not wire turns). But that RPC replies "thread/rollback is
deprecated and will be removed soon" and mutates the source thread in
place instead of forking. **Consequence for agent-overflow**: Codex
revert-to-message drops the whole anchor turn (Claude's session-file
slice is message-granular); parity lands when `thread/fork` exposes a
message-granular anchor. See the granularity note on
`codex.Session.ForkAt`.

## Rollout files on disk

The on-disk format Codex writes for its own sessions, which
`internal/provider/codex/rollout/` reads for session import. A rollout is
append-only JSONL of `{"timestamp", "type", "payload"}` envelopes, indexed by
`<codexHome>/state_5.sqlite`. `codex-rs/rollout/src/policy.rs` is the
authority on what is persisted.

### History modes and the record sets

Codex 0.147 introduced `session_meta.history_mode` (`legacy` or `paginated`).
An absent field means legacy, since upstream's enum defaults to it and the
field is newer than most files on disk. The mode decides which RECORD SET
holds the conversation, and the two barely overlap
(`should_persist_event_msg`):

| | legacy | paginated |
|---|---|---|
| `event_msg/user_message`, `agent_message`, `agent_reasoning` | written | **not written** |
| `event_msg/patch_apply_end`, `mcp_tool_call_end`, `web_search_end`, `sub_agent_activity`, `entered_review_mode`, `exited_review_mode`, `context_compacted`, `image_generation_end` | written | **not written** |
| `event_msg/item_completed` | Plan and `clock.sleep` only | **every turn item** |
| `response_item/*` | written | written |
| `turn_context`, `compacted`, `task_started`/`task_complete`/`turn_aborted`, `token_count` | written | written |
| `world_state`, `security_risk_score` | written | written, and recognised-and-dropped by AO |
| `realtime_item` | not written | voice transcript segments plus session and promotion bookkeeping (0.150) |

A reader that only knows the legacy set therefore imports a paginated thread
with no tool detail at all: no commands, no diffs, no MCP results, no
sub-agent activity. On one real 12k-line 0.147 rollout that was 1,852 tool
rows and 479 diffs missing.

### Bookkeeping records with no transcript projection

Two envelope types are content-free bookkeeping, confirmed against their
rust-v0.149.0 shape:

- `world_state` (`WorldStateItem { full, state }`,
  `codex-rs/protocol/src/protocol.rs`) is the engine's resume baseline for
  model-visible context diffing. It has no transcript projection, and Codex
  writes one per turn on every modern thread.
- `security_risk_score` (`SecurityRiskScore { scores, sampled_at }`,
  `codex-rs/protocol/src/security_risk.rs`) carries upstream's own
  prohibition in its doc comment: "Scores must not enter model-visible
  conversation context or user-visible thread item projections."

Codex 0.150's `realtime_item` is mixed: `transcript_segment` records are
conversation content, while session boundaries and `bem_item_promoted`
records duplicate content the ordinary promoted item already owns.

### TurnItem shape facts

- **The variant tags are PascalCase.** `TurnItem` carries
  `#[serde(tag = "type")]` with NO `rename_all`
  (`codex-rs/protocol/src/items.rs`). The camelCase `ThreadItem` in
  `app-server-protocol/src/protocol/v2/item.rs` is a DIFFERENT type, the
  app-server's public mirror of the same data, and is not what a rollout
  holds. The same variant has been spelled three ways across the two surfaces
  and pre-0.147 files, so a reader should match case-insensitively.
- **Extension items are flattened.** `TurnItem::Extension(ExtensionItem)`
  carries both `"type":"Extension"` and ExtensionItem's own `kind`
  (`web.search`, `clock.sleep`, `image_gen.generation`), so a dispatcher reads
  `type` first and `kind` second.
- **`CommandExecutionItem.id` and `FileChangeItem.id` ARE the `call_id`.**
  Upstream's own `as_legacy_end_event` copies the id straight into
  `ExecCommandEndEvent.call_id`, including the synthetic `exec-<uuid>` ids a
  command run from inside an `exec` script gets.
- `agentMessage.delivery` (0.149, `"async"` for a message a background agent
  delivered mid-turn) and `phase` exist on the item and have no counterpart
  in the mirror below.

### Which record wins when both exist

`response_item` lines are persisted in BOTH modes, so a paginated file carries
a `response_item` twin for every message and reasoning item, written on the
very NEXT line (2224 of 2224 on the reference file). What that means for a
reader:

- **The `response_item` mirror is the only complete source of user text,
  assistant text and reasoning.** No `UserMessage` items are written at all in
  a native paginated file. A MIGRATED file's items carry fresh ids that no
  twin shares, so id-based dedup is impossible there, and the migration writes
  one `Reasoning` item per chunk each restating the whole accumulated text, so
  emitting those would triple a three-chunk thought.
- **Items are the only source of everything else.** Tool calls, diffs, MCP
  results, web searches, sub-agent activity and review markers have no mirror.
- A tool call therefore appears twice in a paginated file, once as an item and
  once as a `response_item`, and a reader must emit one row for the pair.

### `history_base`

`session_meta.history_base` (upstream `HistoryPosition`) marks a rollout whose
history BEGINS INSIDE ANOTHER FILE: everything before `end_ordinal_exclusive`
and `end_byte_offset` lives in the rollout named by `thread_id`. That
`thread_id` is a ROLLOUT id, not a thread id, and a reverted thread's prefix
file carries a different one. Chains nest, so any follower needs a cycle or
depth guard.

## The external agent import ledger

Codex can migrate a Claude Code or Cursor session into a Codex thread
(`codex-rs/external-agent-migration/`). When it does it appends a record to
`<codexHome>/external_agent_session_imports.json` (`SESSION_IMPORT_LEDGER_FILE`
in `sessions/ledger.rs`, present since 0.147) naming the file it read and the
thread it produced.

That file is the ONLY place the provenance survives. The resulting rollout is
an ordinary Codex rollout whose `session_meta.originator` says `codex_cli`,
with nothing in it to say the conversation started somewhere else.

The shape, from `ImportedExternalAgentSessionRecord` (serde derives with no
`rename_all`, so the JSON keys are the Rust field names verbatim):

```json
{"records": [{
  "source_path": "/home/u/.claude/projects/-repo/<uuid>.jsonl",
  "content_sha256": "…", "imported_thread_id": "<codex thread uuid>",
  "imported_at": 1786133870, "source_modified_at": 1786133860000000000,
  "connector_names": ["linear"], "title": "Fix the parser"}],
 "detected_connector_records": [{"source_path": "…", "connector_names": ["…"]}]}
```

- **`imported_at` is unix SECONDS and `source_modified_at` is unix NANOS.**
  The units genuinely differ upstream (`now_unix_seconds` versus
  `duration.as_nanos()`).
- **No record names its source agent.** Upstream keeps Claude and Cursor apart
  by TYPE (`SessionRecordFormat::Cla` or `Cur`, chosen by whichever detector
  produced the candidate) and never persists the choice, so a reader must
  derive the agent from the source path's shape.
- **`detected_connector_records` describes sources Codex NOTICED**, not ones
  it imported, and carries no thread id to key on.
