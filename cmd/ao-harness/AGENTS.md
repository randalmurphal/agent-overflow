# ao-harness

This CLI starts, discovers, drives, measures, and stops isolated harness and
soak instances. Command syntax is generated from the descriptor tree; use
`ao-harness help`, `ao-harness <group> -h`, and
[ao-harness.md](../../docs/references/ao-harness.md). Regenerate that reference
with `go generate ./cmd/ao-harness` after changing commands.

Architecture and workflows are documented in
[agent-harness.md](../../docs/architecture/agent-harness.md) and
[soak-rig.md](../../docs/architecture/soak-rig.md).

## Safety and ownership

- Keep this binary independent of App and transport-server code. Capabilities
  go through harness RPCs. The only direct process read is the native health
  sample for the owned backend responsibility.
- Never target a developer's real app data or provider homes. Preserve the
  canonical-path and symlink refusals for `up` and `db`. `clone --from` and
  `up --keep-home` remain explicit operator-selected exceptions.
- Every launch reserves host capacity, installs the platform containment
  policy, and arms the detached watchdog before reporting success. New launch
  paths and workloads inherit all three layers.
- Resolve an instance from explicit ID, unique prefix, or data root before
  consulting live/default candidates. Ambiguity is an invocation error and
  must list candidates; never guess for reads or lifecycle operations.
- Treat registry rows as token-free discovery records about data roots. Read
  authentication from the selected root's instance file.
- Before signalling, deleting, or pruning, verify namespace, PID, process birth
  marker, executable identity, and the data root's current claim. A force flag
  may relax only an explicitly defined missing-claim case, never contradictory
  ownership evidence.
- Detached browser and backend launches own process groups. Teardown kills the
  verified group, not only the parent process. Keep live detached browser
  profiles until the browser group is stopped.

## Command contracts

- `db` remains read-only at both layers: a read-only SQLite connection and a
  single-statement allowlist. Do not add a write path or depend on PRAGMA
  `query_only` alone.
- `postmortem` reads stopped-run evidence without attaching to a live instance.
- Event `await` defaults to future events; history is opt-in. Register waits
  before the RPC that may synchronously produce the event.
- Resolve thread selectors before the command's main RPC. An invalid selector
  is an error, never an empty result.
- UI, performance, monitor, and bench commands require a registered page. CDP
  profiling and tracing require Chromium and must state that limitation.
- `attach` proves that its newly spawned page registered and answered the
  bridge. Snapshot existing page IDs before launch and exclude them from the
  result. Timeout or browser exit is failure and tears down the browser group.
- Instruments verify an active containment/watchdog boundary before running and
  avoid perturbing the event stream or reveal queue they measure.
- Exit `0` means success, `2` means invalid or ambiguous invocation, and `1`
  means operational refusal or failure. Health and baseline commands use `3`
  when the command completed but the measured result is bad or missing.

`go test ./cmd/ao-harness` covers parsing, refusals, ownership decisions, and
client behavior without booting the backend. `make e2e` owns real isolated boot
and frontend-bridge coverage. Test clone and scrub behavior with synthetic data
only.
