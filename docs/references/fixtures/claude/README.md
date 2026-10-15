# Claude Wire Fixtures

Checked-in NDJSON captures for Claude background-task and TaskOutput
behavior. These back parser replay tests and the reference docs in
[`docs/references/claude-wire.md`](../../claude-wire.md).

## Files

- `ndjson_bash.log`: backgrounded Bash, foreground Bash, Read
- `ndjson_task.log`: Task subagent plus TaskOutput retrieval
- `local_agent_async_launch.ndjson`: `local_agent` (Agent tool)
  launched with NO `run_in_background`, run asynchronously anyway: the
  bare "Async agent launched successfully." ack (claude-wire.md §E5),
  then `system/task_updated` + `system/task_notification`. `prompt`
  fields truncated; every other key/value byte-identical to the
  capture.
- `local_agent_async_resume.ndjson`: an E5 async agent resumed via the
  harness's SendMessage tool (claude-wire.md §E6): the CLI rebinds
  `system/task_started` onto SendMessage's own `tool_use_id` carrying
  the ORIGINAL agent's `description`, and the SendMessage ack has no
  async markers at all (`{success, message, resumedAgentId}`). Two
  full rounds back to back, same `task_id` throughout. Long free-text
  fields (prompts, SendMessage `input.message`/`input.content`)
  truncated to placeholders; every other key/value byte-identical to
  the capture.
- `local_agent_owned_shell_wake_20260908.ndjson`: an E5 async agent
  on 2.1.261 that launches a backgrounded Bash (`owned_by_subagent`),
  stops with the shell still running, and is WOKEN by the CLI when the
  shell reports (claude-wire.md §E6b). Twice over: the second shell
  fails with exit code 3. Shows the stop-and-wake pair: at every stop
  the agent's `task_updated{completed}` + `task_notification` (three
  of each here for one launch), then a `system/task_started` with the
  SAME `task_id`, `task_type:"local_agent"`, `is_backgrounded:true`,
  `spawn_depth`, a `prompt` holding the shell's `<task-notification>`
  XML, and NO `tool_use_id`. Backs the parser's
  `parse_task_wake_test.go` replay. `stream_event`s and `system/init`
  dropped; launch prompts and user text truncated.
- `local_agent_owned_shell_stop_wake_20260908.ndjson`: the same shape
  when the main session kills the owned shell (`stop_task`): the shell
  settles `killed` + `task_notification{stopped}`, and the parked agent
  still wakes, with `<status>stopped</status>` and the "was stopped by
  main session" summary in the wake prompt.
- `local_agent_parked_send_message_20260908.ndjson`: a §E6 SendMessage
  onto a PARKED agent. The rebind `task_started` lands on the
  SendMessage `tool_use_id` immediately, that carrier round parks in
  turn (the shell is still live), and the later wake `task_started`
  again carries no `tool_use_id`: the wake binds to whichever call the
  lifecycle last named, here the carrier.
- `local_agent_nested_async_child_no_wake_20260908.ndjson`: an async
  agent that launches a nested async agent and stops. No wake, ever:
  the child is a top-level task of its own (its `task_started` has no
  `owned_by_subagent`), so a parent whose only live child is an agent
  is done at its stop. The park predicate keys on shells and watch
  tasks only because of this capture.
- `local_agent_parked_stop_task_20260908.ndjson`: `stop_task` on the
  parked agent itself. The agent gets `task_updated{killed}` with no
  further notification, `background_tasks_changed` empties, and its
  owned shell is killed with it (`killed` + `task_notification{stopped}`).
  Nothing wakes.
- `local_agent_parked_copy_20260925.ndjson`: a background agent on
  2.1.280 parks on its own backgrounded Bash while the main agent is in
  a foreground tool call, so the CLI hands the park to the model again
  as `<task-notification>` XML on the `isReplay` echo, while the shell
  still runs. The copy's `<result>` and `<usage>` repeat the structured
  envelope's `summary` and `usage`. The wake `task_started` names the
  bound tool_use (claude-wire.md §Wake shape). Backs
  `TestReplay2_1_280WakeAndStopCopy` and triage's
  `TestAgentStopCopyWhileParkedWritesNoSecondCard`.
- `local_agent_copy_before_wake_20260925.ndjson`: the same, with the
  shell reporting before the main agent's tool call returns: the copy
  arrives after the shell's terminal and before the wake, which waits
  for the main agent's turn to end. Backs triage's
  `TestAgentStopCopyBeforeWakeDoesNotEndTheAgent`.

  Both: `system/init` and `rate_limit_event` dropped; host paths
  synthetic; every other line byte-identical to the capture.
- `local_agent_dropped_wake_20261014.ndjson`: a background agent on
  2.1.284 with `--session-mirror`, parked on its own backgrounded Bash
  in an `isolation:"worktree"` worktree that is removed while it is
  parked. The shell's terminal arrives and the agent never wakes: the
  main file's mirror enqueues the wake and removes it with
  `reason:"agent_stopped"` (claude-wire.md §E6b, dropped wake). The last
  line is the bare `success` ack of a later `stop_task` naming the
  agent. Backs `TestReplayDroppedAgentWake`.
- `local_agent_mirror_wake_20261014.ndjson`: the same run with the
  worktree kept. The wake is removed with `reason:"delivered_to_agent"`
  and the agent runs again. Backs
  `TestReplayMirroredDeliveredWakeIsNoDrop`.

  Both: `system/init`, `rate_limit_event` and subagent mirror batches
  dropped; main-file mirror batches keep only their `queue-operation`
  entries; assistant text cut to its first 40 characters; host paths
  synthetic.
- Forked skills on 2.1.284 (claude-wire.md §E9), live haiku probes of a
  throwaway `context: fork` skill driven by the Skill tool unless noted:
  - `forked_skill_task_started_20261014.ndjson`: the fork's
    `task_started` bound to the Skill call, a depth-2 awaited Agent, the
    fork's lone `task_notification`, the forked result. No mirror. Backs
    `TestReplay_ForkedSkillTaskStartedIsALaunch` and triage's
    `TestForkedSkillTaskSettlesTheSkillRow`.
  - `forked_skill_background_children_20261014.ndjson`: the fork starts a
    background Bash and an async Agent and returns; the shell is killed
    at its stop, the Agent outlives it and wakes the main agent. Backs
    `TestForkedSkillBackgroundChildrenOutliveTheFork`.
  - `forked_skill_stop_task_20261014.ndjson` /
    `forked_skill_interrupt_20261014.ndjson`: a running fork stopped by
    `stop_task` and by `interrupt` (the control requests themselves are
    not in the files). Back `TestForkedSkillTaskSettlesTheSkillRow` and
    the mock's awaited-agent stop frames.
  - `forked_skill_background_refused_20261014.ndjson`: `background_tasks`
    on a running fork answers `{backgrounded:false}`.
  - `forked_skill_direct_command_20261014.ndjson`: `/probe-fork` typed
    as the user message: `task_started` with no `tool_use_id`, the fork's
    Agent announced, and its metadata mirrored, before the mirror
    delivers its tool_use, and each agent's last message mirrored after
    its terminal. Backs `TestReplay_ForkedSkillDirectCommandTaskAndChild`,
    `TestReplay_TypedForkWithoutATaskNamesNoTask`,
    `TestReplay_TypedForkChildAgentProjects` and
    `TestReplay_TypedForkChildRowsBeforeItsLaunch`.
  - `forked_skill_direct_command_stop_task_20261014.ndjson` /
    `forked_skill_direct_command_interrupt_20261014.ndjson`: a typed
    `/slow-fork` stopped after its first tool call; the kill frames name
    no tool_use, nothing answers a launch, and the turn's result is
    `success`. Back `TestReplay_TypedForkSettlesByItsTaskTerminal`.
  - `forked_skill_in_agent_20261014.ndjson`: an awaited Agent calls the
    Skill tool; the fork ends with its notification alone and its
    result on the agent's sidechain has no `tool_use_result`. Backs
    `TestForkedSkillInAnAgentSettlesInPlace`.
- `local_agent_backgrounded_midrun_20261014.ndjson`: an awaited Agent
  moved to the background with `background_tasks` mid-run, then the
  session mirror carries its rows; its background shell is killed at
  its stop and nothing wakes it. Backs `TestBackgroundedAgentKeepsItsLabel`.
- `local_agent_foreground_stop_task_20261014.ndjson` /
  `local_agent_foreground_interrupt_20261014.ndjson`: `stop_task` and
  `interrupt` on a running awaited Agent that owns a background shell.
  `stop_task` kills the shell after the ack; `interrupt` kills it, then
  the agent. Back `TestAwaitedAgentStopKillsItsShell` and the mock's
  awaited-agent stop frames.
- `local_agent_resumed_by_typed_fork_20261014.ndjson`: turn 1 runs an
  awaited Agent; turn 2 types `/resume-fork <agentId>`, whose fork
  resumes that agent with a `SendMessage` the mirror delivers after the
  rebind `task_started`; the resumed agent then resumes the finished
  fork from stdout, and the resumed fork's rows reach only the mirror,
  under metadata naming no tool_use. Backs the
  `task_resume_mirror_test.go` replays, including the turn-2 and turn-3
  starts that stand in for a restarted process.

  All of these: `system/init` and `rate_limit_event` dropped, mirror
  `attachment` entries dropped (account and environment context), host
  paths synthetic, string values longer than 320 characters truncated
  with a `…[trimmed for fixture]` marker.
- `send_message_ack_20260904.ndjson`: two `SendMessage` round-trips on
  2.1.257, a refusal (`to: "A"`, no such agent) and a queued send to a
  live agent by id. Proves the `tool_result` block is `is_error:false`
  either way and the verdict is `tool_use_result.success`, with
  `display` (the TUI's line) beside `message` and `pin{id,name,ref}`
  naming the resolved agent (claude-wire.md §"`SendMessage` ack").
  Backs `sendmessage_ack_test.go` and triage's `ApplySendMessageAck`.
- `cron_create_fire_20261010.ndjson`: 2.1.284 (claude-wire.md §E11).
  `CronCreate` of a recurring every-minute job, three idle fires, then
  one turn with `CronList`, a one-shot `CronCreate`, a `CronDelete` of
  an unknown id (`is_error`, string `tool_use_result`), two
  `CronDelete`s and an empty `CronList`. Each fire is a
  `command_lifecycle` `started`/`completed` pair with a CLI-minted uuid;
  its prompt is only in the `transcript_mirror` user row with
  `turnOrigin:"scheduled"` and the same uuid. Backs
  `parse_cron_test.go` and `e2e/tests/claude-cron-wakes.spec.ts`.
- `cron_loop_midturn_20261010.ndjson`: 2.1.284. `/loop 1m` arms a
  recurring job, an 80s foreground Bash turn holds the fire due during
  it, the held fire runs once right after that turn's `result`, then
  `CronList` and `CronDelete`.

  Both: captured under the isolated spike policy with a clean
  environment and sanitized: session ids, home and project paths and
  the user email are synthetic.
  `cron_spike_20261010.summary.json` records the shapes, fire timings and
  the 2.1.284 scheduler constants read from the CLI bundle.
- Queued-message delivery orders on 2.1.284 (claude-wire.md §"Frame order
  per consumption path"), Sonnet, AO's flag set, run 1 of three per case;
  `queued_delivery_orders_20261015.summary.json` holds every run's order
  and timings:
  - `queued_midloop_after_cascade_20261015.ndjson`: a message drained
    mid-loop into a task-notification cascade round: its echo, then
    `started`.
  - `queued_deferred_pair_midloop_20261015.ndjson`: A from idle, B written
    right after `started` A and drained mid-loop into A's round.
  - `queued_boundary_merge_20261015.ndjson` /
    `queued_boundary_merge_after_cascade_20261015.ndjson`: A and B held and
    merged at a boundary, written during a running turn and during a
    cascade round: merged-away echo A, `started` A and B, `system/init`,
    survivor echo B.
  - `queued_idle_pair_split_20261015.ndjson`: A and B in one stdin write at
    an idle session run as two turns.

  All: `system/init` kept (its position is the finding) with its payload
  cut to identity keys; `stream_event`, `system/status` and
  `rate_limit_event` dropped; mirror batches keep only `queue-operation`,
  main-chain `user` and `queued_command` entries; session ids and host
  paths synthetic. In `queued_idle_pair_split_20261015.ndjson` one answer
  is cut to its first word, in its assistant message and its `result`.
- `ndjson_outlives.log`: backgrounded Bash outliving its launching turn
  (the wire `result` envelope arrives BEFORE the task's `task_updated`)
- `ndjson_outlives_turn2.log`: follow-up turn on the same session
- `taskoutput_multi.ndjson`: two background Bashes plus one blocking TaskOutput
- `interactive_outlives_taskoutput_monitor.ndjson`: long-lived app-style session
- `blocking_taskoutput.ndjson`: successful blocking TaskOutput while task still runs
- `blocking_taskoutput_failure.ndjson`: failed blocking TaskOutput while task still runs
- `plain_failure_outlives.ndjson`: failed background Bash without TaskOutput
- `local_agent_outlives.ndjson`: parent launches a `local_agent` (Task)
  subagent in background, ends its message with stop_reason=end_turn,
  and Claude CLI **withholds** the `result` envelope until the
  subagent completes (~9.84s gap). Counterpart to `ndjson_outlives.log`,
  proving local_agent and local_bash background tasks behave
  differently at the wire level.
- `local_agent_user_input_during_wait.ndjson`: same scenario, but
  the host injects a new user message via stdin during the gap. CLI
  re-rounds within 32ms and processes the injection cleanly with the
  original subagent still running. Backs the safety argument for
  unblocking the composer on parent end_turn.
- `local_agent_plus_bg_bash.ndjson`: bg Bash + bg local_agent
  combined: confirms the result-delay is keyed on local_agent
  specifically, not on any backgrounded task.
- `opus47_thinking_redacted.ndjson` /
  `opus47_thinking_summarized.ndjson`: same prompt against
  `claude-opus-4-7`, captured on 2.1.132. The redacted run uses the
  app's current invocation flags; the summarized run adds the hidden
  `--thinking-display summarized` flag and surfaces a Haiku-generated
  summary of the reasoning via `thinking_delta` events. See
  [`opus47_thinking_summary.json`](opus47_thinking_summary.json) for
  the per-fixture stats and `claude-wire.md` §thinking-display for the
  wire-level explanation.
- `session_api_error_offbranch.jsonl`: session **JSONL** (not wire
  NDJSON): sanitized replica of the 2026-06-10 incident topology.
  Deferred `system/api_error` rows written at the next user send with
  a stale `parentUuid` that bypasses the prior turn's tail
  (`a2`/`a3-final`), then a user row chained onto them. Backs
  the active-branch and resume-filter sections of `claude-wire.md`,
  `sessionfork/rechain_test.go`, the
  `sessionleaf_branch.go` tests, and the draft upstream report
  ([`claude-api-error-upstream-report.md`](../../claude-api-error-upstream-report.md)).
  Dropped into `~/.claude/projects/<slug>/<id>.jsonl`, resume-at
  `a3-final` reproduces the pre-init hard failure on 2.1.170.
- `multiturn_cost_cumulative_20260703.ndjson`: three trivial turns in
  one `-p --input-format stream-json` session (haiku). Proves
  `result.total_cost_usd` and `result.modelUsage` are
  SESSION-CUMULATIVE (cost 0.0216 → 0.0253 → 0.0282; modelUsage
  inputTokens 10 → 20 → 30) while flat `result.usage` stays per-turn.
  Authoritative for the snapshot-delta accounting in
  `internal/provider/claude/usage_accounting.go`.
- `subagent_usage_inclusion_20260703.ndjson`: one turn that launches a
  Task (general-purpose agent). Proves flat `result.usage` is
  PARENT-ONLY (in=42, cacheCreate=22168) while `result.modelUsage`
  INCLUDES the sidechain (in=52, cacheCreate=35397 = parent 22168 +
  sidechain 13229) and carries the CLI-computed per-model `costUSD`.
  Authoritative for preferring modelUsage over flat usage in turn
  accounting.
- `context_usage_control_20260803.summary.json`: sanitized
  `control_request{subtype:"get_context_usage"}` round-trip on 2.1.219,
  issued on a session that had never received a user message (it
  consumes no turn and makes no API call). Records the full response
  key set, the `categories[]` rows, and the arithmetic AO's breakdown
  UI leans on: deferred rows are excluded from `totalTokens`, and the
  non-deferred rows sum to exactly `rawMaxTokens`. Also records the
  drift from the 2.1.88 SDK schema (`autocompactSource` and two new
  `messageBreakdown` keys). Backs
  `internal/provider/claude/context_usage.go`.
- `*_summary.json`: notes captured during the spike runs

## Refresh

1. Run a fresh capture with `AGENT_OVERFLOW_DEBUG=provider`.
2. Replace the relevant fixture files here.
3. Update `docs/references/claude-wire.md` if the observed behavior changed.
4. Re-run the Claude replay tests.
