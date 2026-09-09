# Harness engines

This tree contains reusable engines behind `--harness` and `--soak`. The
application-facing composition belongs to `internal/harnessrpc`; command-line
driving belongs to `cmd/ao-harness`. See
[agent-harness.md](../../docs/architecture/agent-harness.md) for the boot,
scenario, replay, and RPC architecture.

## Package boundaries

- `scenario` parses, validates, substitutes, and embeds mock-provider scenarios.
- `control` is the authenticated loopback channel between a harness instance and
  its mock-provider children. Keep its token in the child environment; never
  publish it process-wide.
- `instanceinfo` discovers instances by canonical data root. Registry rows are
  token-free discovery records; the authenticated token stays inside the owned
  data root.
- `governor` reserves host-wide memory capacity and observes pressure. It does
  not signal applications. Its subdirectory guide defines the accounting rules.
- `containment` installs the per-instance platform memory policy and fails closed
  on unsupported platforms.
- `darwinbundle` gives isolated macOS runs a distinct bundle identity and cleans
  only the generated bundle and its bundle-scoped WebKit state.
- `Replayer` emits one recorded stream at a time with its original timing.
  Starting a replay while another is active is an error.

Keep these packages independent of `*App` and application store details. The
`internal/harnessrpc.Host` adapter owns production wiring. Changes to control or
scenario wire shapes must be checked against `cmd/ao-mockprovider` and `e2e`.

## Scenario contracts

- Validate scenarios when loading or installing them, before spawning a mock.
- Claude scenarios do not emit `system/init`; the adapter emits init and user
  echo for each turn. Scenario content must use valid assistant framing.
- Every embedded scenario must pass both provider parsers. When a scenario
  claims an effect beyond parsing, add a test through that downstream path.
- Mock reports are the deterministic assertion surface for received user input,
  interrupts, gates, and fixture failures. Provider terminal frames remain on
  provider stdout.

Run `go test ./internal/harness/...` for changes in this tree. Harness boot and
browser integration are covered by `make e2e`.
