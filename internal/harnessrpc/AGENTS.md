# Harness RPC receiver

This package composes reusable harness engines with the production application
through the `Host` interface. It owns the local-only `Harness*` RPC surface,
mock control, replay, reset, seeding, UI/performance queries, and soak autopilot.
See [agent-harness.md](../../docs/architecture/agent-harness.md).

## Wire and lifecycle contracts

- Treat exported method names, parameters, result tags, and registered FNV IDs
  as a stable wire API. Update the registration tests and every string-based
  caller in `e2e` and `cmd/ao-harness` together.
- Do not add exported non-RPC helpers to `Harness`; registration deliberately
  exposes its exported method set as `main.Harness` with `LocalOnly: true`.
- Start the provider control listener before `App.Start`. Return the narrowly
  scoped child environment and never publish its token process-wide.
- Resolve the store, replay manager, and native window dynamically through
  `Host`; this receiver is constructed before application startup finishes.
- Emit dynamic and replay events only through `Host.Emit`, whose App adapter
  uses the shared transport.
- After restoring a replay snapshot, publish its store identity before replay
  can start.

Reset is an ownership operation. Stop harness emitters and workflow startup,
stop sessions and settle turns, clear mock and workflow state, delete seeded
projects, invalidate derived projections, clear harness evidence, then release
the workflow pause. Access records such as device pairing survive unless the
RPC explicitly owns them.

The push ledger records payloads at the `push.Sender` seam only when the
isolated boot has no real push credential. `HarnessReset` clears the ledger but
does not remove device registrations.

This package never resolves or spawns provider binaries. Isolated boot must pin
provider binaries, homes, credentials, keychain behavior, catalogs, and
background fetch before constructing this receiver. Unit tests use a fake
`Host`; App integration uses the repository's provider-spawn isolation.

Run `go test ./internal/harnessrpc` for receiver changes and `make e2e` for wire
or lifecycle changes.
