# cmd/ao-mockprovider

One binary that impersonates both provider CLIs for the agent test
harness (see
[docs/architecture/agent-harness.md](../../docs/architecture/agent-harness.md)).
The harness points `claudeBinaryPath` and `codexBinaryPath` at it; it
sniffs argv to pick a mode:

- `--input-format stream-json ...` → Claude NDJSON session
  (`claude.go`); `--max-turns 0` → the account probe (`probe.go`).
- `app-server` → Codex JSON-RPC 2.0 (`codex.go`).
- `-p --output-format json` → Claude one-shot text generation
  (`textgen.go`).
- `exec --ephemeral` → Codex one-shot text generation (`textgen.go`).
  Checked BEFORE the protocol sniff, which reads every argv without
  `app-server` as Claude. `codex exec` carries no such marker.
- `--version` → a string satisfying both providers' version gates
  (`version.go`).

## One-shot text generation

The third invocation shape, beside a session and the probe: a prompt on
stdin, one structured answer, exit. No scenario, no control-channel
registration, no turn lifecycle. `internal/textgen`'s `RunClaude` /
`RunCodex` build both argvs; the answer travels differently per provider
and `textgen.go` matches each side exactly: Claude prints a `result`
line carrying `structured_output` (the LAST non-empty stdout line is what
`DecodeClaudeStructuredLastLine` reads), Codex writes the bare JSON to the
`--output-last-message` FILE and stdout is ignored.

The answer is generated FROM the schema rather than hardcoded, so it
satisfies whichever decoder the caller runs (thread title, commit message,
workflow digest) instead of only the ones that existed when it was written.
Every declared property is emitted; `cannedText` gives the known field
names plausible values so a wrong-field bug reads as itself in the UI.

Without this mode the Claude branch fell through to the NDJSON session
adapter, which answers a bare prompt with nothing ("claude returned empty
output" three layers away), and the Codex branch was sniffed as Claude
entirely.

The Claude path validates its schema with `providerschema.ValidateClaude`,
not the two-provider union: `internal/commitmsg` and `internal/threadtitle`
each keep a Claude-only schema that the union legitimately rejects. See
`internal/providerschema/AGENTS.md`.

## Session launch receipts

The `session_config` report retains the sorted MCP server NAMES observed in
Claude's `--mcp-config` or Codex's `thread/start config.mcp_servers`, alongside
the existing permission/sandbox fields. It never retains URLs, headers, or
credentials. This lets harness tests prove app-owned MCP wiring reached the
provider boundary without leaking capability tokens into events or evidence
logs.

## The account probe reports models, and they disagree on purpose

The `initialize` control_response carries `account`, `commands` and
`models` (`claude.go`). The `models` array is the ONLY input to
`internal/claudemodels`' merge, so without it a harness run exercises the
un-enriched catalog and the merge policy never runs at all.

One row deliberately DISAGREES with the shipped catalog: `claude-haiku-4-5`
claims `supportsFastMode: true`, which `provider.ClaudeModels` does not.
That single divergence is what makes the merge observable. It produces one
`DriftCapability` line and adds `ModelCapabilityFastMode`. A payload that
agreed everywhere would be indistinguishable from no payload at all. The
other row is the `default` POINTER (`resolvedModel: "claude-opus-5[1m]"`),
which exercises alias/marker normalization and matches the catalog on every
capability, so it contributes no drift.

## Structured-output schemas are validated, not accepted

A structured-output schema is checked against `internal/providerschema`
and an invalid one exits non-zero, matching what the real CLIs do:
Claude validates `--json-schema` at spawn, the Codex app-server validates
`outputSchema` on each `turn/start`.

This is deliberate strictness. A mock that accepts any schema lets the
workflow suite pass green while every real provider run dies at spawn,
which is precisely how five schema defects survived a fully green harness
(no `$schema` draft handling, a leaked `multiline` keyword, open nested
objects, partial `required` lists). If a scenario now fails here, fix the
generator. Do not relax the check.

## How a session runs

`scenario_source.go` acquires the script, in priority order: control
channel assignment (env `AO_HARNESS_CONTROL*`, via
`internal/harness/control.FromEnv`), `AO_MOCK_SCENARIO_FILE`, or a
builtin per-protocol fallback (announced on stderr). `engine.go`
executes turns step by step (emit/fixture/writeFile/approval/
waitSignal/stall/exit per `internal/harness/scenario`), posting
progress reports and long-polling live commands when a control channel
exists. Without one, the binary still works standalone.

Interrupt handling belongs in `engine.go`, once for both adapters. It marks the
active turn aborted, releases `waitSignal`/indefinite `stall`, skips remaining
steps at the next boundary, and reports `turn_interrupted`. The adapters only
own their genuine terminal frames and write the interrupt ack/response first.

### A turn is the unit, not the process

Two engine facts follow from that, and both used to be per-process:

- **`scenario_done` fires once per TURN.** Under the default
  `afterTurns: repeatLast`, turns 2..N re-run the last scripted turn and
  finish exactly as turn 1 did. A once-per-process latch meant every
  turn after the first reported nothing, so the ordinary
  send/await/assert/send-again shape hung on the second await. The
  dedupe is still per turn (one turn can reach the report by more than
  one path) and its entry dies with the turn.
- **A buffered `advance` cannot cross a turn boundary.** `advance`
  arriving before its gate opens is parked, stamped with the turn that
  was live when it landed, and only satisfies a gate of that same turn.
  `finishTurn` and an interrupt both discard whatever is left, reporting
  each discard. An advance that outlived its turn is a command a test
  issued and nothing consumed, and letting it survive is how the next
  turn appears to skip its first gate for no reason. Within one turn an
  UNNAMED advance still releases whichever gate opens next; that is the
  documented wildcard. The buffer is capped, and an advance beyond the
  cap is dropped with a report rather than parked.

Both are visible on the control channel: `advance_released {gate}` and
`advance_buffered {gate, openGate}` join the report vocabulary, and the
control server projects them into `MockInfo.openGate` /
`MockInfo.pendingAdvances` so "which gate is this mock sitting on"
has an answer that does not require reading stderr. `fixture_error`
carries what `fixture` / `writeFile` failures and undelivered command
batches used to say only in the mock's log.

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
`docs/references/spike-policy.md`). Do not guess wire behaviour.

Claude interrupted turns end with the verified 2.1.170
`result{subtype:error_during_execution,is_error:true,
terminal_reason:aborted_streaming}` shape. Codex interrupted turns end with
`turn/completed{turn.status:interrupted}` per the upstream v2 protocol.

### `control_request` acks are subtype-aware and strict

The mock used to answer every `control_request` with a success carrying
`{}`. That is worse than useless: `mcp_status` rendered an empty server
list, `mcp_authenticate` FAILED every time (the app rejects a success
response with no payload), and (the real cost) an outbound wire-KEY
bug was invisible, because a mock that acks anything acks a misspelled
request too. The CLI destructures the fields it wants off `request` and
never validates the object, so `server_name` where it reads `serverName`
reads as `undefined` with the round trip, the error path and the status
projection all working correctly around it. That shipped, for months.

So `writeClaudeControlAck` validates each subtype's REQUIRED keys and
answers an error `control_response` naming the key it wanted, and
answers the successful ones with a minimally real payload. The key
spellings come from `internal/provider/claude`'s
`TestControlRequestWireKeys` (read off the binary), not from what looks
consistent, because the CLI mixes camelCase and snake_case per handler
with no rule (`mcp_toggle.serverName` beside `stop_task.task_id`). A
subtype the mock has never heard of still gets the permissive `{}`, and
logs; forward compatibility beats strictness for an assertion nobody has
written yet.

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
  `list` and `delete` DO answer (over an empty queue, since nothing here can
  fill one) because AO still calls them for the rollback purge and the
  legacy-row sunset. `thread/queue/update` and `.../reorder` are refused too,
  for the same reason they always were: a mock more permissive than the app
  would let a harness run pass against a wrapper nothing verifies.
- **A thread remembers its history mode.** `thread/start` echoes the
  `historyMode` it was asked for (upstream's default, `legacy`, when the
  params say nothing) and RECORDS it under `AO_HARNESS_TRANSCRIPT_HOME`;
  `thread/resume` can only report what is already there. That durability
  is the point: every rollback cuts through a throwaway resume session, a
  second mock process that never saw the start, and a mode held in memory
  would read as legacy there, sending a genuinely paginated thread down
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
  steer arrives, and load-bearing: AO registers a steer's pending send BY
  that client id, so an echo without it leaves the message rendering as
  injected provider context. `turn/start` binds the same id as `${CLIENT_ID}`
  for the scenario's own echo line.

An anchor neither cut recognises is believed on a RESUMED thread and
refused on a STARTED one. The mock keeps no rollout, so on a resumed
thread not knowing a turn is ignorance rather than evidence, and every
real rollback lands there. On a thread this process started it ran the
whole history, so an unknown anchor is nonsense and stays an error.

### An unimplemented method answers -32601, never an empty success

The default branch used to return `{"result":{}}` for anything the mock
did not recognise. No real app-server does that, and it defeated the
app's own fallback machinery: `codex.IsMethodUnsupported` keys on
`-32601`, so under an empty-success default it could never fire and
every optional surface reported success against a server that had done
nothing.

The default is now the JSON-RPC MethodNotFound error, which means the
methods the app calls as a matter of course need real answers or the
DEFAULT harness experience breaks rather than only the optional
surfaces. `handleReadRequest` covers those (`account/read`,
`account/usage/read`, `thread/read`, `thread/turns/list`,
`thread/settings/update`, `skills/list`, `config/read`,
`mcpServerStatus/list`, `thread/backgroundTerminals/list`), each with
the minimum the app's own decoder needs and nothing invented beyond it
(a terminating cursor, a `thread.status.type`, an account with a plan).
Genuinely optional or newer surfaces (`thread/compact/start`,
`review/start`, `config/batchWrite`, `config/mcpServer/reload`,
`mcpServer/oauth/login`, background-terminal `terminate`/`clean`) stay
-32601 on purpose: that is the honest answer, and it is what exercises
the fallback. A scenario's own `responses` template still outranks both.

## Scenario knobs the adapters own

`startupDelayMs` holds the FIRST provider frame (Claude's `system/init`,
Codex's `initialize` response) and is paid once per process, because a
per-frame sleep would turn a 5s spawn-delay scenario into a
5s-per-turn one. It is the only way to drive the app's cold-start window
from a scenario. `providerVersion` overrides what the mock claims to be
in both places the app parses a version from, which is how a spec pins a
DOWNGRADE and drives the closed side of a version gate; without it every
mock is 99.0.0 and every gate is open. `emit.coalesce` writes the step's
lines in one stdout write, for reproducing a reader that mishandles
several NDJSON lines arriving in a single read, invisible when each
line gets its own syscall, so it is mutually exclusive with the pacing
knobs that mean the opposite.

## Testing

`binary_test.go` / `codex_bin_test.go` / `codex_queue_bin_test.go` /
`codex_revert_bin_test.go` / `control_e2e_test.go` build the real binary
once (TestMain) and drive it over pipes; Claude-mode
stdout is validated line-by-line against the app's actual parser
(`validate_test.go`). Anything protocol-shaped belongs in those tests,
not unit tests of internals.
