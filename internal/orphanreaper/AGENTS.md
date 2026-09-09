# macOS orphan process cleanup

This package prevents provider process groups from surviving an abnormal app
exit on macOS.

The live sidecar receives `watch <pgid>` and `release <pgid>` over a control
pipe. EOF means its parent is gone, so it terminates every still-watched group.
The startup sweep covers a sidecar that also died: its durable registry records
PID, process group, and process creation time, then reaps only a matching
process whose current parent is init.

Do not replace creation-time and parent checks with PID-only or process-name
matching. Process groups are established by the provider launcher. Registry
writes are atomic. Read errors are surfaced by explicit reads and sweeps; an
add logs and replaces an unreadable registry so it cannot block a new session.

Callers use the sidecar only on macOS; `Client` methods are nil-safe for other
platform paths that use native parent-death or Job Object ownership. Keep the
internal `__reap` protocol out of user-facing command handling.
