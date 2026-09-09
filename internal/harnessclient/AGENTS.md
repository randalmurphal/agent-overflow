# Harness client

This package is the Go client and process supervisor for an isolated harness or
soak instance. It is the Go counterpart of `e2e/src/harness.ts` and underpins
`cmd/ao-harness`. See
[agent-harness.md](../../docs/architecture/agent-harness.md).

## Boundary

Keep this package independent of App and transport-server code. It may observe
only the bootstrap line, files written under the selected data root, the
authenticated WebSocket, and processes it started. `internal/transport` is a
test-only dependency used to check the hand-written client frame shapes.

Detached launches use their own process group, write stdout and stderr to owned
files, and poll stdout for the bootstrap line. Do not replace this with
`StdoutPipe`: the launched process must outlive the client and an early exit
must not race the bootstrap reader.

## Event client contracts

- `WaitForEvent` consumes the matching retained event. Scan retained history
  before waiting so fast events are not lost.
- Order concurrent matching waiters by registration, independent of map
  iteration.
- Timeout and dispatch must decide ownership under the same lock. A timeout
  consumes nothing; an event delivered at the deadline remains returnable.
- `Await` registers before the triggering RPC. Every registered await ends in
  `Wait` or `Close`.
- `Listen` callbacks execute on the read loop and must not call back into the
  client. Hand work to another goroutine or channel.
- Keep the event log bounded and shed old entries in chunks. Long-lived history
  belongs in replay rings and evidence files.
- Keep `Count` and the TypeScript client's event semantics aligned.

## Files and processes

`FollowFile` emits complete lines, retains a bounded partial line, and detects
both truncation and replacement. Unix uses device and inode identity;
platforms without a cheap identity fall back to size. Drop any partial line
when the file rotates.

Process termination is successful only after verified exit. Send the graceful
signal, escalate after the grace period, recheck identity before escalation,
and report a process that survives the confirmation window. Never treat a
successful signal call as proof that the process stopped.

Type RPC results only when a mistyped field would be silently dangerous.
`HarnessInfo` qualifies; print-through results should remain `json.RawMessage`.

Run `go test ./internal/harnessclient`. Its fake backend crosses this package's
private frames through the production transport structs without booting an App.
Real boot coverage belongs to `make e2e`.
