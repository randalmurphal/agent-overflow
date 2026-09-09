# Conversation transfers

One dialog serves transfer entry points from thread menus and the composer.
Keep the operation explicit: Move preserves conversation identity; Copy creates
an independent native session and leaves the original usable.

The controller lives in `stores/conversationTransfers.svelte.ts`; this
directory owns presentation and form behavior. Do not duplicate transfer
protocol state in components.

- Lock accepted source and destination coordinates for an operation. Nested
  project creation stays on the selected destination computer.
- Keep offline computers visible and surface errors beside the affected
  operation.
- Never place offer grants in component state, browser storage, logs, or error
  text.
- Capability versions gate available operations. Unknown versions do not issue
  transfer RPCs.
- Mount the bounded status list only while expanded. Preserve server ordering
  and limits.
- On reconnect, merge the snapshot with per-computer events received while the
  read was in flight. Do not overwrite newer rows or drop recovered operations.
- Cancellation controls must reflect protocol state: recipient setup can be
  discarded before preparation; prepared recipients require source
  cancellation; committed moves can only finish.

Protocol and persistence behavior are specified in
[`conversation-transfer.md`](../../../../../docs/specs/conversation-transfer.md).
