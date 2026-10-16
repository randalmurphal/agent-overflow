# Codex Wire Fixtures

Checked-in app-server JSON-RPC captures backing
[`docs/references/codex-wire.md`](../../codex-wire.md). Client requests are
interleaved at the position they were written; every other line is what the
app-server wrote.

## Files

Background terminals on codex-cli 0.159.1 (codex-wire.md §"What a kill
writes"), `gpt-6-luna`, one run each:

- `background_terminal_terminate_after_turn_20261015.ndjson`: a PTY
  started in a turn that then completed; `terminate` answers
  `{terminated:true}`, then the command's `item/completed` (`failed`,
  `exitCode:-1`) under the ended turn's id. A repeat and an unknown id
  answer `{terminated:false}`. Backs `cmd/ao-mockprovider/codex_terminals.go`.
- `background_terminal_terminate_mid_turn_20261015.ndjson`: the same kill
  while the turn runs a foreground command; the turn goes on and completes.
- `background_terminals_clean_20261015.ndjson`: `clean` with two PTYs
  running answers `{}`, then one `item/completed` per PTY; a second `clean`
  writes nothing.
- `background_terminals_survive_interrupt_20261015.ndjson`: `turn/interrupt`
  with two PTYs running kills neither; a later `clean` kills both under the
  interrupted turn's id.
- `background_terminal_terminate_refused_20261015.ndjson`: on a fresh
  connection, `terminate` with no `processId` and with `processId:"abc"`;
  each is refused with -32600 and upstream's message.

All: `initialize`, `thread/start` and their responses, `thread/started`,
streaming deltas, token usage, rate limits, MCP startup, remote-control and
account notifications dropped; host paths synthetic.

Thread unload and cold resume on codex-cli 0.162.1 (codex-wire.md §"Thread
unload and cold resume"), `gpt-6-luna`, one run:

- `thread_unload_cold_resume_20261016.ndjson`: a thread started with
  developer instructions, stdio MCP server `alpha` and `view_image`
  disabled; a resume while subscribed that changes all three is ignored;
  `thread/unsubscribe` then an immediate resume cold-starts it (`notLoaded`,
  `beta` starts, an `imageView` item appears, the reply keeps the original
  end token); a second unsubscribe is followed 60 s later by
  `thread/closed`, an empty `thread/loaded/list` and `notLoaded` from a
  third unsubscribe; a resume after the close starts `gamma`.
  `thread/start` and `thread/resume` responses are cut to the thread id and
  status plus `model`, `approvalPolicy`, `approvalsReviewer`, `sandbox`
  and `reasoningEffort`; `turn/completed` items emptied; the stdio server
  is a stub at a synthetic path. Otherwise dropped as above.
