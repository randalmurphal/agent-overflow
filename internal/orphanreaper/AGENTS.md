# Orphan process cleanup

This package prevents the process groups the app starts from surviving an
abnormal app exit on macOS and Linux. What it watches is listed in
[process lifetime](../../docs/architecture/process-lifetime.md).

The live sidecar receives `watch <pgid>` and `release <pgid>` over a control
pipe. EOF means its parent is gone, so it terminates every still-watched group.
The startup sweep covers a sidecar that also died: its durable registry records
PID, process group, process creation time and the recording parent, then reaps
only a matching process whose current parent is no longer that parent. Under a
subreaper the orphan's parent is not init.

Do not replace creation-time and parent checks with PID-only or process-name
matching. Process groups are established by the provider launcher, the
terminal's pty and `procutil.StartGroup`. Registry
writes are atomic. Read errors are surfaced by explicit reads and sweeps; an
add logs and replaces an unreadable registry so it cannot block a new session.

The sidecar is a POSIX sh script (`reaper.go`) that dash, bash and busybox
must all run; it validates every control line itself. The app starts no
sidecar on Windows; `Client` methods are nil-safe there and in tests.
