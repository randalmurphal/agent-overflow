# harness/

Engines behind the `--harness` agent test harness boot mode. The full
guide — boot contract, RPC surface, workflows — is
[docs/architecture/agent-harness.md](../../docs/architecture/agent-harness.md);
this file covers the package boundaries.

## What lives where

- `gitfixture.go` — `CreateRepo(path, RepoSpec)`: throwaway git
  repositories for seeded projects (files, commits, dirty state).
- `replayer.go` — `Replayer`: re-emits a recorded NDJSON event stream
  (the `internal/observability/replay` format) onto the live event bus
  with original inter-event timing; pause/resume/single-step. One
  replay at a time — starting over an active run fails loudly.
- `control/` — the loopback HTTP control channel between the harness
  and `ao-mockprovider` processes: `Server` (registration resolve,
  long-poll command delivery, progress reports), `Client` +
  `FromEnv` (the mock side), and the `AO_HARNESS_CONTROL` /
  `AO_HARNESS_CONTROL_TOKEN` env contract.
- `scenario/` — the mock scenario document: `Parse`/`Validate`, step
  types, `${VAR}` substitution, and the `//go:embed`-shipped library
  (`library/*.json`) with `LoadLibrary` / `Library` / `DefaultName`.

## Responsibility boundary

- These packages are engines, not policy: no `*App` access, no store
  schema knowledge beyond what their inputs carry. The `Harness` RPC
  receiver (`app_harness*.go` at the repo root) owns wiring them to
  the live app (event bus, store snapshot/restore, session lifecycle).
- `control` and `scenario` are shared with `cmd/ao-mockprovider` — the
  mock binary is the other consumer. Changing a wire shape or scenario
  field means checking both sides plus `e2e/`.

## Invariants

- Claude scenarios must not emit `system/init`, and assistant
  text/thinking envelopes need a prior `message_start` registering the
  same message id — the mock's adapter owns per-turn init + user echo,
  scenarios own content framing. `scenario/library_test.go` enforces
  this for every shipped scenario; keep the check green when adding
  library entries.
- Every shipped scenario line must survive the real provider parsers
  (`scenario/library_parsers_test.go`).
- Scenario validation happens at load/set time, never inside a spawned
  mock — a bad script must fail the RPC that installed it.
