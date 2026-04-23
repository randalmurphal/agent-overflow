# Claude Wire Fixtures

Checked-in NDJSON captures for Claude background-task and TaskOutput
behavior. These back parser replay tests and the reference docs in
[`docs/references/claude-wire.md`](../../claude-wire.md).

## Files

- `ndjson_bash.log` — backgrounded Bash, foreground Bash, Read
- `ndjson_task.log` — Task subagent plus TaskOutput retrieval
- `ndjson_outlives.log` — backgrounded task outliving its launching turn
- `ndjson_outlives_turn2.log` — follow-up turn on the same session
- `taskoutput_multi.ndjson` — two background Bashes plus one blocking TaskOutput
- `interactive_outlives_taskoutput_monitor.ndjson` — long-lived app-style session
- `blocking_taskoutput.ndjson` — successful blocking TaskOutput while task still runs
- `blocking_taskoutput_failure.ndjson` — failed blocking TaskOutput while task still runs
- `plain_failure_outlives.ndjson` — failed background Bash without TaskOutput
- `*_summary.json` — notes captured during the spike runs

## Refresh

1. Run a fresh capture with `AGENT_OVERFLOW_DEBUG=provider`.
2. Replace the relevant fixture files here.
3. Update `docs/references/claude-wire.md` if the observed behavior changed.
4. Re-run the Claude replay tests.
