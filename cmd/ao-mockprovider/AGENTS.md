# Mock provider

This binary impersonates Claude Code and Codex for isolated harness tests. It
implements provider wire behavior, not application behavior. See
[agent-harness.md](../../docs/architecture/agent-harness.md) and
`internal/harness/scenario`.

## Invocation routing

Route exact provider invocation shapes before the general session sniff:

- Claude and Codex one-shot structured text generation;
- Claude sign-in and account probe;
- Codex `app-server`;
- provider version queries; and
- Claude streaming sessions.

One-shot modes do not register with scenario control or enter turn lifecycle.
Codex writes structured output to `--output-last-message`; Claude emits the
last structured result line. Validate schemas with the provider-specific
`internal/providerschema` rules and generate answers from the requested schema.
Never relax schema checks to accommodate a broken caller.

Account sign-in may write credentials only inside an explicitly supplied
isolated `CLAUDE_CONFIG_DIR` or `CODEX_HOME`. Missing isolation means no write.
Keep Claude flow replacement and burn behavior, Codex's exact
`chatgptDeviceCode` discriminator, and login completion through the authenticated
control channel aligned with verified provider behavior.

## Session contracts

- Acquire scenarios from the authenticated control assignment, then an explicit
  scenario file, then the built-in provider fallback.
- The engine owns common step execution, interrupt state, gate buffering, and
  progress reports. Adapters own provider framing and terminal responses.
- Scope completion and buffered advances to a turn. An advance must never cross
  into the next turn; interruption discards remaining steps and advances.
- Claude emits `system/init` and replay user echo once per received user turn.
  Scenarios own assistant message framing.
- Codex reports a parseable app-server version. Queue mutation methods must return explicit errors: Agent Overflow owns
  mid-turn dispatch through `turn/steer`.
- Persist Codex thread history mode under `AO_HARNESS_TRANSCRIPT_HOME` so a new
  mock process resumes with the correct revert behavior.
- Echo Codex steer input and `clientUserMessageId`; pending-send reconciliation
  depends on that ID.
- Unknown JSON-RPC methods return `-32601`. Implement real minimal responses for
  methods used during ordinary startup; optional unsupported methods stay
  unsupported so fallback paths remain exercised.
- Session launch evidence may retain MCP server names, never URLs, headers,
  tokens, or credentials.

Provider wire shapes must come from current provider fixtures, upstream source,
or an isolated spike. Do not infer them from application code. Preserve
provider-specific spelling and error envelopes.

Binary tests build and drive the real mock process over pipes. Put protocol
behavior in those tests and validate Claude output with the application's real
parser. Run `go test ./cmd/ao-mockprovider`; changes to shared scenario/control
contracts also require `go test ./internal/harness/...` and `make e2e`.
