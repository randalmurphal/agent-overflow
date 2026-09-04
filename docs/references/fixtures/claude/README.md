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
- `send_message_ack_20260904.ndjson`: two `SendMessage` round-trips on
  2.1.257, a refusal (`to: "A"`, no such agent) and a queued send to a
  live agent by id. Proves the `tool_result` block is `is_error:false`
  either way and the verdict is `tool_use_result.success`, with
  `display` (the TUI's line) beside `message` and `pin{id,name,ref}`
  naming the resolved agent (claude-wire.md §"`SendMessage` ack").
  Backs `sendmessage_ack_test.go` and triage's `ApplySendMessageAck`.
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
  invariant 28, `sessionfork/rechain_test.go`, the
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
