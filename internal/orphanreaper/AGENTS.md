# Orphan process cleanup

This package prevents the process groups the app starts from surviving an
abnormal app exit on macOS and Linux. What it watches is listed in
[process lifetime](../../docs/architecture/process-lifetime.md).

The live sidecar receives `watch <pgid>` and `release <pgid>` over a control
pipe. EOF means its parent is gone, so it terminates every still-watched group.
The startup sweep covers a sidecar that also died: its durable registry records
the group's leader (its pid is the pgid), the leader's `Identity` and the
recording parent, then reaps only a matching process whose current parent is no
longer that parent. Under a subreaper the orphan's parent is not init. Each
record ends in one of three outcomes: dropped (the group ended, or the pid
names another process), reaped (stopped through `procutil.StopGroup` and
dropped once confirmed gone), or kept for the next launch (the leader could not
be read, the stop failed, or the recorder still parents it). Every record left
or kept is logged with the reason.

`observe` is the one reader of a leader, for the capture and the sweep: one
kernel read of its start and parent (`instanceinfo.ReadProcess`) and, on Linux,
the boot id. Never derive identity from the wall clock: gopsutil's Linux start
time adds a boot time that, in a container, it recomputes from the clock on
every read. `ProcInfo.LegacyStartMillis` exists for earlier versions alone: it
compares a record they wrote (`create_unix` only), and `CaptureLeader` writes
it so an earlier version reading a new record still tells a reused pid apart.
This version never compares it for a record with an identity. Only a `Capture`
can be added; the registry rejects and logs an entry whose pid and pgid differ,
whose leader is out of range, or that names no identity.

Do not replace identity and parent checks with PID-only or process-name
matching. Process groups are established by the provider launcher, the
terminal's pty and `procutil.StartGroup`. Registry
writes are atomic. Read errors are surfaced by explicit reads and sweeps; an
add logs and replaces an unreadable registry so it cannot block a new session.

The sidecar is a POSIX sh script (`reaper.go`) that dash, bash and busybox
must all run; it validates every control line itself. The app starts no
sidecar on Windows; `Client` methods are nil-safe there and in tests.
