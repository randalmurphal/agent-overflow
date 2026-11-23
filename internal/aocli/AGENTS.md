# internal/aocli

Command routing and presentation for the `agent-overflow` binary. Provider,
persistence, and application lifecycle behavior belongs to their owning
packages. Commands accept arguments and writers directly so tests do not spawn
the binary.

## Entry points

- `topLevelCommands` is the single dispatch table for CLI verbs; `IsCommand`
  answers from it for the app binary and `Commands()` lists it. `serve` and
  `supervise` boot the application and remain root entry points rather than CLI
  rows.
- `service` manages the host through `internal/serviceinstall` and must not
  contact an app. `pair` and `thread` talk to the local app through its
  control file, not a session credential.
- Workflows, runs, campaign memory, and remote commands are not CLI verbs.
  Agents reach them through `ao-workflow-tools` (`internal/workflowtools`) and
  `ao-remote-tools`. Do not add workflow or remote verbs or shims here.
- Keep usage, flags, exit codes, and output synchronized with
  [ao-cli.md](../../docs/references/ao-cli.md).

## Session marker

`EnvInSession` (`AO_IN_SESSION`) is set to `"1"` in every provider session's
environment so `main_entry.go` refuses to boot a second app instance from inside
a session.

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
