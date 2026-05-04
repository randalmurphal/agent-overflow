# Claude Wire Fixtures

Checked-in NDJSON captures for Claude background-task and TaskOutput
behavior. These back parser replay tests and the reference docs in
[`docs/references/claude-wire.md`](../../claude-wire.md).

## Files

- `ndjson_bash.log` — backgrounded Bash, foreground Bash, Read
- `ndjson_task.log` — Task subagent plus TaskOutput retrieval
- `ndjson_outlives.log` — backgrounded Bash outliving its launching turn
  (the wire `result` envelope arrives BEFORE the task's `task_updated`)
- `ndjson_outlives_turn2.log` — follow-up turn on the same session
- `taskoutput_multi.ndjson` — two background Bashes plus one blocking TaskOutput
- `interactive_outlives_taskoutput_monitor.ndjson` — long-lived app-style session
- `blocking_taskoutput.ndjson` — successful blocking TaskOutput while task still runs
- `blocking_taskoutput_failure.ndjson` — failed blocking TaskOutput while task still runs
- `plain_failure_outlives.ndjson` — failed background Bash without TaskOutput
- `local_agent_outlives.ndjson` — parent launches a `local_agent` (Task)
  subagent in background, ends its message with stop_reason=end_turn,
  and Claude CLI **withholds** the `result` envelope until the
  subagent completes (~9.84s gap). Counterpart to `ndjson_outlives.log`
  — proves local_agent and local_bash background tasks behave
  differently at the wire level.
- `local_agent_user_input_during_wait.ndjson` — same scenario, but
  the host injects a new user message via stdin during the gap. CLI
  re-rounds within 32ms and processes the injection cleanly with the
  original subagent still running. Backs the safety argument for
  unblocking the composer on parent end_turn.
- `local_agent_plus_bg_bash.ndjson` — bg Bash + bg local_agent
  combined: confirms the result-delay is keyed on local_agent
  specifically, not on any backgrounded task.
- `*_summary.json` — notes captured during the spike runs

## Refresh

1. Run a fresh capture with `AGENT_OVERFLOW_DEBUG=provider`.
2. Replace the relevant fixture files here.
3. Update `docs/references/claude-wire.md` if the observed behavior changed.
4. Re-run the Claude replay tests.
