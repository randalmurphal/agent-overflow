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

## How a session runs

`scenario_source.go` acquires the script, in priority order: control
channel assignment (env `AO_HARNESS_CONTROL*`, via
`internal/harness/control.FromEnv`), `AO_MOCK_SCENARIO_FILE`, or a
builtin per-protocol fallback (announced on stderr). `engine.go`
executes turns step by step — emit/fixture/writeFile/approval/
waitSignal/stall/exit per `internal/harness/scenario` — posting
progress reports and long-polling live commands when a control channel
exists. Without one, the binary still works standalone.

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

Scenario lines own assistant content framing (`message_start` before
text/thinking, `message_stop` after). When touching either side,
verify against a real fixture or a spike (`docs/references/claude.md`,
`docs/references/spike-policy.md`) — do not guess wire behaviour.

## Testing

`binary_test.go` / `codex_bin_test.go` / `control_e2e_test.go` build
the real binary once (TestMain) and drive it over pipes; Claude-mode
stdout is validated line-by-line against the app's actual parser
(`validate_test.go`). Anything protocol-shaped belongs in those tests,
not unit tests of internals.
