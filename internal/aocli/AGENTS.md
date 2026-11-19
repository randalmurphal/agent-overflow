# internal/aocli

Command routing and presentation for the `agent-overflow` binary. Provider,
persistence, and application lifecycle behavior belongs to their owning
packages. Commands accept arguments and writers directly so tests do not spawn
the binary.

## Entry points

- `Commands()` is the single dispatch table for CLI verbs. `serve` and
  `supervise` boot the application and remain root entry points rather than CLI
  rows.
- `service` manages the host through `internal/serviceinstall` and must not
  contact an app. `pair` and `thread` talk to the local app through its
  control file, not a session credential.
- `remote` makes one authenticated RPC to the running app from inside a
  provider session.
- Workflows, runs and campaign memory are not CLI verbs (D77). Agents reach
  them through `ao-workflow-tools` (`internal/workflowtools`). Do not add
  workflow verbs or shims here.
- Keep usage, flags, exit codes, and output synchronized with
  [ao-cli.md](../../docs/references/ao-cli.md).

## Session and RPC contract

`session.go` owns `AO_ENDPOINT`, `AO_TOKEN`, and `AO_THREAD_ID`. The
application injects them into provider sessions. One of `AO_ENDPOINT` or
`AO_TOKEN` without the other is an invalid environment. A 401 means the scoped
session ended; do not retry or mint credentials in the CLI.

Session commands use `transport.ClientFrame` and `transport.ServerFrame` over
`POST {AO_ENDPOINT}/rpc`. Use method names because the CLI has no generated
numeric binding IDs.

## Output and parsing

Bound every value derived from a provider, runner, or operator. Render such
values with `internal/untrustedtext`.

Reject unknown flags, duplicate single-value flags, blank required values, and
ambiguous positional forms before making an RPC. Keep parsing local to each
command and test the accepted and refused forms.

Grant and confirmation commands must display enough identity and scope for the
operator to understand the decision. Never print credentials, proofs, raw
private payloads, or peer-supplied error prose.

Tests use injected HTTP clients, clocks, runners, writers, and temporary config
roots. They must not start the application, execute host service commands, call
real providers, or use a developer profile.
