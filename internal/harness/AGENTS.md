# harness/

Engines behind the `--harness` agent test harness boot mode. The full
guide (boot contract, RPC surface, workflows) is
[docs/architecture/agent-harness.md](../../docs/architecture/agent-harness.md);
this file covers the package boundaries.

## What lives where

- `gitfixture.go` defines `CreateRepo(path, RepoSpec)`: throwaway git
  repositories for seeded projects (files, commits, dirty state, and
  extra branches left unchecked-out so a worktree can attach one).
- `replayer.go` defines `Replayer`: re-emits a recorded NDJSON event stream
  (the `internal/observability/replay` format) onto the live event bus
  with original inter-event timing; pause/resume/single-step. One
  replay at a time. Starting over an active run fails loudly.
- `control/` is the loopback HTTP control channel between the harness
  and `ao-mockprovider` processes: `Server` (registration resolve,
  long-poll command delivery, progress reports), `Client` +
  `FromEnv` (the mock side), and the `AO_HARNESS_CONTROL` /
  `AO_HARNESS_CONTROL_TOKEN` env contract. `CommandLoginComplete` is the
  one command that is not about a scenario: the Codex device-code
  sign-in finishes on another screen, so nothing written to the mock's
  stdin reaches that moment, and the completion has to be paired with
  the credential adoption then reads out of the isolated login home.
- `instanceinfo/` covers instance discovery for `--harness` / `--soak`
  boots: `ID(dataRoot)` (first 8 hex of the canonical root's SHA-256),
  the `Row` written to
  `<user cache dir>/agent-overflow/harness-instances/<id>.json`, and
  `List` with a signal-0 liveness probe so a reader can tell a live row
  from a killed process's leftovers. Deliberately token-free. The
  token lives in `<dataDir>/harness-instance.json`, inside the data
  root a reader must already be able to open, so a planted row can at
  worst name a path.
- `scenario/` is the mock scenario document: `Parse`/`Validate`, step
	types, `${VAR}` substitution, and the `//go:embed`-shipped library
	(`library/*.json`) with `LoadLibrary` / `Library` / `DefaultName`.
- `governor/`: host-wide memory bookkeeping shared by every harness
  launcher. A cross-process capacity lease under an OS file lock, plus the
  monitor that reports ceiling and host-floor crossings. It never signals an
  application, so it is not the OOM protection. Has its own subarea guide.
- `darwinbundle/`: gives an isolated macOS harness executable its own
  application bundle. WKWebView keys its default data store by bundle id, so
  the ordinary `com.agentoverflow.app` bundle must never host an isolated
  run. Cleanup removes the exact generated bundle and bundle-id-scoped
  user-Library WebKit state after the supervised process exits. A no-op on
  every other platform.
- `containment/`: memory containment policy for harness launches. Linux uses
  cgroup v2 or inherited `RLIMIT_DATA` when cgroup delegation is unavailable,
  Windows uses a Job Object, and macOS uses a native application-responsibility
  ceiling plus host-floor watchdog because its kernel rejects lowering the
  available memory rlimits. Unsupported platforms fail closed.

## Responsibility boundary

- These packages are engines, not policy: no `*App` access, no store
  schema knowledge beyond what their inputs carry. The `internal/harnessrpc`
  receiver owns wiring them to the live app through its explicit `Host`
  adapter (event emission, store snapshot/restore, session lifecycle).
- `control` and `scenario` are shared with `cmd/ao-mockprovider` — the
  mock binary is the other consumer. Changing a wire shape or scenario
  field means checking both sides plus `e2e/`.
- The mock engine reports `turn_interrupted` when a provider interrupt wins an
  active scenario turn. This is the deterministic assertion surface for cancel
  and watchdog tests; adapter terminal frames remain provider stdout traffic.
- Both mock adapters report `user_input` carrying the text they received and
  the provider session that received it (Claude session id or Codex thread id)
  when a turn starts, and when a Codex turn is steered. It is the only surface
  that answers both "what did the app actually send" and "where did it send
  it"; neither question can be inferred from the stored transcript or process
  id.

## Invariants

- Claude scenarios must not emit `system/init`, and assistant
  text/thinking envelopes need a prior `message_start` registering the
  same message id. The mock's adapter owns per-turn init + user echo,
  scenarios own content framing. `scenario/library_test.go` enforces
  this for every shipped scenario; keep the check green when adding
  library entries.
- Every shipped scenario line must survive the real provider parsers
  (`scenario/library_parsers_test.go`).
- A scenario that claims a downstream effect the parsers alone cannot
  show gets a test that drives its own lines through that path:
  `file-edit-diff`'s inline diff payload is pinned by
  `internal/triage/scenario_file_edit_diff_test.go` (parser + Router +
  store), the usage-limit pair by `scenario/library_parsers_test.go`.
- Scenario validation happens at load/set time, never inside a spawned
  mock. A bad script must fail the RPC that installed it.
