# internal/harness/governor

Host-wide memory bookkeeping for harness runs. It reserves capacity and
observes crossings. It never starts, stops, or signals an application.

`internal/harness/containment` installs the kernel boundary where the OS
provides one (cgroup v2, Job Object, or `RLIMIT_DATA`). macOS has no usable
memory boundary, so callers enforce this package's application-responsibility
ceiling and host-floor events through their existing identity-checked
termination path. The Darwin sample includes launchd-parented WebKit/Chrome
helpers assigned to owned responsible processes as well as descendants.
The governor itself remains accounting and observation only: do not add a
kill path here.

## Invariants

- **State is host-global and lock-protected.** Reservations live under
  the user cache dir, never a worktree, and every read-modify-write holds
  an OS lock (`flock`, `LockFileEx`). Harnesses start from several
  worktrees, and a Go mutex cannot coordinate separate processes.
  `Options.Dir` must be absolute. A platform with no tested advisory-lock
  primitive returns `ErrUnsupported` rather than allowing an unaccounted
  overcommit.
- **The available-memory sample is taken while the lock is held.** Move
  it outside and two worktrees pass the same capacity check.
  `MemoryReader.AvailableMemory` must return the OS's own available
  figure (`MemAvailable` on Linux), not total minus process RSS.
- **Owner identity is PID plus birth marker** (process start time), never
  PID alone. `Reserve` refuses a supplied birth id that disagrees with
  the live process.
- **Pruning is by verified death, not by TTL.** A probe error preserves
  the lease (an unknown owner is not a dead one) — but know what counts
  as an answer: on Darwin a missing pid answers the `kern.proc.pid`
  sysctl with ZERO bytes, surfacing as `EIO` from `SysctlKinfoProc`,
  and that (like `ESRCH`) is "dead", not a probe error. Reading it as an
  error kept every crashed instance's lease until TTL and blocked
  `ao-harness up` for a day (fixed 2026-09-04, regression-tested). A live owner whose
  birth marker still matches is kept past its TTL. A dead owner, or a
  reused PID, is dropped even before TTL expiry, which is what lets a
  crashed detached `up` return its capacity.
- **`Monitor` re-checks the birth marker after every separate OS query.**
  RSS and liveness are two syscalls, and a PID that exits and is reused
  between them must not produce an event for the old lease.
- **Host pressure is evaluated before the process-tree query**, and it
  re-arms the ceiling edge. A child RSS sample that fails must not mask
  the signal protecting every worktree on the host.
- **Events are edge-triggered per reason**, one per crossing episode, not
  one per sample. Treat them as a level and one episode acts repeatedly.
- **A tree MEMBER that exits mid-sample contributes zero bytes, never an
  error.** Process trees are racy by nature: WebView2 and WebKit recycle
  helper processes constantly, so a pid vanishing between the tree
  snapshot and its memory query is routine operation. Only the OWNER's
  death or identity change ends the monitor (and the monitor's own
  owner rechecks catch that). All three platform samplers follow this;
  Windows treating the gap as an error let the reservation layer read
  helper churn as a safety failure and tear down a healthy instance
  (incident 2026-08-30).

## Defaults and callers

`DefaultCeilingBytes` is 2 GiB (sized when the app could still spawn a managed
Chrome whose measured macOS startup peak was 1.69 GB aggregate RSS; that Chrome
is gone, and the harness now boots a fake browser engine that spawns nothing),
`DefaultAvailableFloorBytes` is 2 GiB.
The floor absorbs an allocation burst between the watchdog's 100ms
samples; a sub-gigabyte floor can let the host OOM before the next exact
measurement. Both are `Options` fields so a machine-specific profile
tightens them without editing this package. Callers: `cmd/ao-harness`,
`cmd/ao-harness-e2e`, `cmd/agent-overflow-windows`, `internal/compare`.
Platform matrix:
[docs/specs/testing-harness.md](../../../docs/specs/testing-harness.md).
