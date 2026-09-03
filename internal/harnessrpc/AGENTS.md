# internal/harnessrpc/

Application-level RPC receiver for the isolated agent harness and soak rig.
The reusable engines remain in `internal/harness`; this package owns their
stateful composition with the production app through `Host`.

## Layout

- `harness.go` / `host.go` — receiver state, stable wire DTOs, `Config`, and
  the consumer-owned production capability seam.
- `mock.go` — scenario rules, mock-provider control server, and live commands.
- `replay.go` — recording bundles, snapshot restore, and replay controls.
- `seed.go` / `workflows.go` — declarative fixture creation and reset ordering.
- `soak.go` — free-function soak autopilot over the same receiver.
- `paths.go` — canonical path comparison shared with isolated boot setup.
- `push.go` — `HarnessPushSent`, the ledger of what the push fan-out would
  have sent. The recorder itself lives in `internal/app`
  (`app_push_harness.go`): the harness boot installs it in the
  `push.Sender` seam ONLY where no credential is configured, so everything
  above that seam stays production and a spec can assert §9's redaction
  rule on the real payload. `HarnessReset` clears the ledger through
  `Host.ForgetPushSent`; device rows and their registrations survive,
  because they are access state rather than test state.

## Wire and lifecycle invariants

- `Harness` has exactly 38 exported `Harness*` methods. Root registers the
  receiver with `Package: "main"`, `TypeName: "Harness"`, and `LocalOnly:
  true`; do not add exported lifecycle helpers to the receiver.
- Keep method names and JSON tags stable. `transport_registration_test.go`
  pins every `main.Harness.*` FNV ID and receiver-local policy.
- The control listener starts before `App.Start` in both harness and soak
  modes. `StartControl` returns the provider-only environment; root installs
  it on App before startup. Never publish the token process-wide.
- Store and replay-manager access is dynamic through `Host`: the receiver is
  constructed before `App.Start` initializes either one.
- Native-window access is dynamic through `Config.Window`: windowed boots fill
  the controller only when the Wails shell is constructed; headless boots
  return a named `--window` refusal.
- Every dynamic event and replay frame crosses `Host.Emit`, whose `internal/app` adapter
  calls `App.emit`; never write directly to the transport bus here.
- After restoring a replay snapshot, publish the returned store identity
  immediately through `Host.PublishStoreIdentity` before replay starts.
- Reset order is load-bearing: stop harness emitters, pause/cancel/sync workflow
  startup, stop sessions, settle turns, clear mocks, delete workflow records,
  delete projects, invalidate import projection, drop the push ledger,
  remove harness-owned files, then clear workflow pause via the deferred
  resume closure.

## Safety boundary

- The package never resolves or spawns provider binaries. Root constructs the
  App through `newIsolatedProviderApp`, which pins provider binaries,
  credentials, keychain, and background fetch before this receiver exists.
- Tests use a fake `Host`, isolated stores, and the in-process control/replay
  engines. Real App workflow integration remains in `internal/app` under the repository's
  provider-spawn isolation fixtures.
