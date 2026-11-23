# Harness memory governor

This package reserves host-wide capacity and reports memory-pressure crossings.
It never starts, stops, or signals an application. Per-instance kernel policy
lives in `internal/harness/containment`; callers own identity-checked shutdown.

## Invariants

- Reservation state is host-global cache state. Every read-modify-write holds
  the platform file lock so separate worktrees cannot overcommit the host.
  `Options.Dir` must be absolute; unsupported locking fails with
  `ErrUnsupported`.
- Read available memory while holding the reservation lock. Use the operating
  system's available-memory figure, such as Linux `MemAvailable`.
- Identify an owner by PID and process birth marker. Reject a supplied marker
  that does not match the live process.
- Prune only verified-dead or identity-mismatched owners. Probe errors preserve
  leases. A missing Darwin PID may surface as zero-byte `kern.proc.pid` data and
  `EIO`; treat that as dead.
- Recheck the owner identity after separate OS queries. PID reuse between RSS
  and liveness samples must not produce an event for the old lease.
- Check host pressure before sampling the process tree. A failed tree sample
  must not hide a host-floor crossing.
- Crossings are edge-triggered per reason and re-arm after recovery.
- A child disappearing during a tree sample contributes zero bytes. Only loss
  or replacement of the owner ends monitoring.

Windows tree accounting uses one `SystemProcessInformation` snapshot, including
creation times, to reject stale parent relationships without opening every
candidate process. Keep the snapshot buffer reusable. Darwin accounting must
include helpers assigned to the owned application responsibility, including
launchd-parented WebKit or Chromium helpers.

Defaults are a 2 GiB ceiling and 2 GiB host-available floor. See
[testing-harness.md](../../../docs/specs/testing-harness.md) for the platform
matrix and caller policy. Run `go test ./internal/harness/governor` and preserve
the platform-specific tests when changing sampling or identity rules.
