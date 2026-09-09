# internal/aocli

Command routing and presentation for the `agent-overflow` binary. Provider,
persistence, and application lifecycle behavior belongs to their owning
packages. Commands accept arguments and writers directly so tests do not spawn
the binary.

## Entry points

- `Commands()` is the single dispatch table for CLI verbs. `serve` and
  `supervise` boot the application and remain root entry points rather than CLI
  rows.
- `workflow` commands are offline. They discover scopes and delegate definition
  behavior to `internal/workflow/def` and starter content to
  `internal/workflow/starters`.
- `run`, `memory`, `notes`, and `schedule` make one authenticated RPC to a
  running app. `service` manages the host through `internal/serviceinstall` and
  must not contact an app.
- Keep usage, flags, exit codes, and JSON output synchronized with
  [ao-cli.md](../../docs/references/ao-cli.md).

## Session and RPC contract

`session.go` owns `AO_ENDPOINT`, `AO_TOKEN`, `AO_THREAD_ID`, `AO_PROJECT`,
`AO_RUN_ID`, and `AO_PHASE_ID`. The application injects the complete applicable
set into provider sessions. One of `AO_ENDPOINT` or `AO_TOKEN` without the other
is an invalid environment. A 401 means the scoped session ended; do not retry or
mint credentials in the CLI.

Execution commands use `transport.ClientFrame` and `transport.ServerFrame` over
`POST {AO_ENDPOINT}/rpc`. Use method names because the CLI has no generated
numeric binding IDs. Human renderers decode only the fields they display;
`--json` emits the application's result unchanged.

Keep timeout ordering intact: the 30 second client RPC timeout must exceed the
25 second workflow-watch hold and the engine's 20 second runner-start reply
budget. Otherwise a committed mutation can appear to have timed out and invite
an unsafe retry.

## Workflow commands

- `workflow new` namespaces every sibling created by a starter. Only Markdown
  prompt references are rewritten inside YAML.
- Path and ID validation both resolve a workflow scope so `call:` edges and
  project bindings are checked identically. `AO_PROJECT` supplies the default
  project only for offline list and validation. `workflow new` requires an
  explicit destination.
- `run watch` uses cursor-based long polling. Preserve gap reporting and the
  distinct exit codes documented in `ao-cli.md`.
- `--refresh-def` rereads a frozen definition only when the next action enters a
  phase fresh. `run amend` validates changed inputs through
  `def.ValidateInput`. `run guide` queues quoted guidance for the next fresh
  phase entry and never interrupts the current turn.
- Acting verbs render the authoritative post-action state returned by the app.
  Do not reconstruct workflow state or repair advice in this package.

## Output and parsing

Bound every value derived from a provider, runner, or operator. Render such
values with `internal/untrustedtext`; keep narrative content verbatim when it is
the requested result. Resolve narrative paths through application-owned
builders rather than assembling storage paths here.

Reject unknown flags, duplicate single-value flags, blank required values, and
ambiguous positional forms before making an RPC. Keep parsing local to each
command and test the accepted and refused forms.

Grant and confirmation commands must display enough identity and scope for the
operator to understand the decision. Never print credentials, proofs, raw
private payloads, or peer-supplied error prose.

Tests use injected HTTP clients, clocks, runners, writers, and temporary config
roots. They must not start the application, execute host service commands, call
real providers, or use a developer profile.
