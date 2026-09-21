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
  Scenarios own assistant message framing. Each echo's `parentUuid` is the
  transcript leaf: the last main-chain user or assistant `uuid` this process
  wrote, scenario emits included, skipping sidechain rows (`parent_tool_use_id`
  or `isSidechain`) and status/result envelopes. AO verifies
  a user message against that leaf, so a turn that wrote tool rows must not
  chain the next echo past them.
- Codex reports a parseable app-server version. Queue mutation methods must return explicit errors: Agent Overflow owns
  mid-turn dispatch through `turn/steer`.
- Persist Codex thread history mode under `AO_HARNESS_TRANSCRIPT_HOME` so a new
  mock process resumes with the correct revert behavior.
- Hold the Codex writer lock for a thread `thread/fork` minted, under the same
  home, for the life of the process that answered; another process resuming it
  meanwhile gets `-32600` "already has an active writer". Fork ids are unique
  across processes. Ids no fork minted are shared and never contend.
- Refuse workspace mutations (`writeFile`) when the mock's cwd is outside
  `AO_HARNESS_WORKSPACE_ROOT`, and report the refusal as a step failure so the
  scenario keeps running.
- Echo Codex steer input and `clientUserMessageId`; pending-send reconciliation
  depends on that ID.
- Unknown JSON-RPC methods return `-32601`. Implement real minimal responses for
  methods used during ordinary startup; optional unsupported methods stay
  unsupported so fallback paths remain exercised.
- Session launch evidence may retain MCP server names, never URLs, headers,
  tokens, or credentials.
- An `mcpCall` step makes this process a real MCP client against the server
  the app configured for the session. The endpoint, its per-thread token and
  its headers stay inside the process: they reach no report, log line or wire
  frame. A call that fails for any reason is framed as an error tool result
  and reported as `mcp_result`, never as a scenario failure. `mcpList`
  runs the same session's `tools/list` and reports `mcp_tools`, writing
  no wire frame: a CLI reads a tool list at handshake, off the
  transcript.
- A `capture` step binds a `${VAR}` from a regex over already-spellable
  text, for the rest of the process, so a scenario can name a value the
  app minted after the scenario was installed. A pattern that matches
  nothing binds nothing and reports `fixture_error`.

Provider wire shapes must come from current provider fixtures, upstream source,
or an isolated spike. Do not infer them from application code. Preserve
provider-specific spelling and error envelopes.

Binary tests build and drive the real mock process over pipes. Put protocol
behavior in those tests and validate Claude output with the application's real
parser. Run `go test ./cmd/ao-mockprovider`; changes to shared scenario/control
contracts also require `go test ./internal/harness/...` and `make e2e`.
