# internal/threadtransfer

Coordinates fixed conversation handoffs through the durable ownership protocol.
Provider snapshots and application authorization remain with their owners. The
complete state machine is in
[conversation-transfer.md](../../docs/specs/conversation-transfer.md).

- Read source intent, recipient offers, archive identity, and cancellation from
  durable private records. Peers never supply local archive paths.
- Authorize the destination before snapshotting. Once an archive is bound to a
  transfer, retries use that immutable archive rather than current workspace
  content.
- Keep source execution disabled for a move until destination activation or
  acknowledged cancellation is durable. Persist source retirement before
  releasing the activation proof. Resolve unknown replies through status calls
  after restart.
- Preparation validates uploaded content in operation scratch space. It must not
  modify live provider files or publish ownership. Only installation plus the
  store's atomic history/completion transaction may activate the destination.
- A prepared destination cannot be discarded without source cancellation proof.
  Cancellation cannot revoke an activation proof already accepted.
- Accepted work uses application-lifetime contexts. `Jobs` bounds active work,
  resumes durable candidates after restart, and joins workers during close.
  Wake callbacks must not run transfer work synchronously.
- Remove private archives and scratch data only after durable completion or
  acknowledged cancellation. Keep cleanup retryable across restart.
- Reset an upload checkpoint only while the destination is unprepared and the
  immutable expected digest is still available. Valid but unacceptable content
  remains a reported refusal.

`ErrPending` means asynchronous preparation is still running. It is scheduler
state rather than an error to present to the user.
