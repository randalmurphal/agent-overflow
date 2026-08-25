# cmd/ao-mockprovider

One binary that impersonates both provider CLIs for the agent test
harness (see
[docs/architecture/agent-harness.md](../../docs/architecture/agent-harness.md)).
The harness points `claudeBinaryPath` and `codexBinaryPath` at it; it
sniffs argv to pick a mode:

- `--input-format stream-json ...` → Claude NDJSON session
  (`claude.go`); `--max-turns 0` → the account probe (`probe.go`).
- `app-server` → Codex JSON-RPC 2.0 (`codex.go`).
- `--version` → a string satisfying both providers' version gates
  (`version.go`).

## Structured-output schemas are validated, not accepted

A structured-output schema is checked against `internal/providerschema`
and an invalid one exits non-zero, matching what the real CLIs do —
Claude validates `--json-schema` at spawn, the Codex app-server validates
`outputSchema` on each `turn/start`.

This is deliberate strictness. A mock that accepts any schema lets the
workflow suite pass green while every real provider run dies at spawn,
which is precisely how five schema defects survived a fully green harness
(no `$schema` draft handling, a leaked `multiline` keyword, open nested
objects, partial `required` lists). If a scenario now fails here, fix the
generator — do not relax the check.

## How a session runs

`scenario_source.go` acquires the script, in priority order: control
channel assignment (env `AO_HARNESS_CONTROL*`, via
`internal/harness/control.FromEnv`), `AO_MOCK_SCENARIO_FILE`, or a
builtin per-protocol fallback (announced on stderr). `engine.go`
executes turns step by step — emit/fixture/writeFile/approval/
waitSignal/stall/exit per `internal/harness/scenario` — posting
progress reports and long-polling live commands when a control channel
exists. Without one, the binary still works standalone.

Interrupt handling belongs in `engine.go`, once for both adapters. It marks the
active turn aborted, releases `waitSignal`/indefinite `stall`, skips remaining
steps at the next boundary, and reports `turn_interrupted`. The adapters only
own their genuine terminal frames and write the interrupt ack/response first.

## Claude adapter contract

`claude.go` owns two protocol behaviours the real CLI exhibits, so
scenarios cannot break the app's turn lifecycle:

- **init per user turn**: `system/init` is written after each user
  envelope arrives (the app only opens a turn when init lands with a
  pending send), never at boot and never by scenario lines.
- **user echo**: every user envelope is echoed back with
  `isReplay: true` and a tracked `uuid`/`parentUuid` chain
  (`--replay-user-messages`), which is what resolves the app's
  pending send.

Both adapters also post a `user_input` control report carrying the text
they received and the provider session that received it (Claude: session id
plus the user envelope's text blocks; Codex: thread id plus a `turn/start` or
`turn/steer` `input` vec). It is the only surface that answers both what the
app actually sent and where it sent it.

Scenario lines own assistant content framing (`message_start` before
text/thinking, `message_stop` after). When touching either side,
verify against a real fixture or a spike (`docs/references/claude.md`,
`docs/references/spike-policy.md`) — do not guess wire behaviour.

Claude interrupted turns end with the verified 2.1.170
`result{subtype:error_during_execution,is_error:true,
terminal_reason:aborted_streaming}` shape. Codex interrupted turns end with
`turn/completed{turn.status:interrupted}` per the upstream v2 protocol.

## Codex adapter contract

Three behaviours `codex.go` / `codex_queue.go` / `codex_revert.go` own
that no scenario can express, because each describes what the app-server
does on its own:

- **`initialize` reports a version.** The response carries
  `userAgent: "codex_cli_rs/<mock version> (...)"`, which is where the app
  parses the connected app-server's build from
  (`internal/provider/codex/app_server_version.go`). Every per-method version
  gate fails CLOSED without it, so a mock that answered `{}` would silently
  run harness sessions as an ancient app-server: no `thread/queue`, no
  thread-scoped usage, and the pre-0.149 `untrusted` approval policy instead
  of `on-request`.
- **`thread/queue/add` and `thread/queue/start` are TRIPWIRES.** Upstream
  drains that queue from `on_thread_idle`, on its own clock, so a client that
  ALSO keeps a queue of its own has two dispatchers for one message. Agent
  Overflow sends every mid-turn message with `turn/steer` and must never call
  either method; both answer with a JSON-RPC error, which is what turns a
  regrown caller into a failing harness run instead of a duplicated turn.
  `list` and `delete` DO answer (over an empty queue — nothing here can fill
  one) because AO still calls them for the rollback purge and the
  legacy-row sunset. `thread/queue/update` and `.../reorder` are refused too,
  for the same reason they always were: a mock more permissive than the app
  would let a harness run pass against a wrapper nothing verifies.
- **A thread remembers its history mode.** `thread/start` echoes the
  `historyMode` it was asked for (upstream's default, `legacy`, when the
  params say nothing) and RECORDS it under `AO_HARNESS_TRANSCRIPT_HOME`;
  `thread/resume` can only report what is already there. That durability
  is the point: every rollback cuts through a throwaway resume session, a
  second mock process that never saw the start, and a mode held in memory
  would read as legacy there — sending a genuinely paginated thread down
  the `thread/fork` fallback with no error anywhere. `thread/revert`
  refuses a legacy thread with upstream's own -32600 and its verbatim
  wording, which is what AO's classifier turns into
  `ErrThreadRevertUnsupported`, and answers a paginated one with the
  response plus the `thread/reverted` notification on the same
  connection. Both cuts post a `history_cut` control report before they
  answer, because "which cut did the app choose" has no other observable.

- **A steer echoes its own `userMessage`.** `turn/steer` reports its text on
  the same surface a turn's own input does AND writes the `item/completed`
  the running turn would carry, with the caller's `clientUserMessageId` echoed
  back as `clientId`. Adapter-owned because neither value exists until the
  steer arrives — and load-bearing: AO registers a steer's pending send BY
  that client id, so an echo without it leaves the message rendering as
  injected provider context. `turn/start` binds the same id as `${CLIENT_ID}`
  for the scenario's own echo line.

An anchor neither cut recognises is believed on a RESUMED thread and
refused on a STARTED one. The mock keeps no rollout, so on a resumed
thread not knowing a turn is ignorance rather than evidence — and every
real rollback lands there. On a thread this process started it ran the
whole history, so an unknown anchor is nonsense and stays an error.

## Testing

`binary_test.go` / `codex_bin_test.go` / `codex_queue_bin_test.go` /
`codex_revert_bin_test.go` / `control_e2e_test.go` build the real binary
once (TestMain) and drive it over pipes; Claude-mode
stdout is validated line-by-line against the app's actual parser
(`validate_test.go`). Anything protocol-shaped belongs in those tests,
not unit tests of internals.
