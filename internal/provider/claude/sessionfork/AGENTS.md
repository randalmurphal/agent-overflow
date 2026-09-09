# Claude native session files

This package reads, forks, relocates, and copies Claude session JSONL plus its
opaque sidecar subtree. Callers inject the projects root and decide when an
operation is allowed. This package owns safe parsing, chain selection, UUID
rewrites, path encoding, and crash-safe file writes.

## Reading and fork cuts

Stream JSONL under the 16 MiB line limit. Never load a whole transcript into
memory or count every `type:"user"` row as a user turn; tool results use that
type too. Parent traversal must use the shared parent and logical-parent
resolvers.

Canonicalize workspace paths before computing Claude's project slug. Preserve
the truncate-and-hash encoding for long paths. Refuse ambiguous or missing
resume anchors rather than fabricating a session.

`WriteForkFileThroughUUID` takes `ForkCut`; keep path and identity inputs on
that struct. Remap the new root session identity while preserving message UUID
chains needed for resume. Deferred `system/api_error` rows are the only rows
re-chained to their file predecessor. A successful `/compact` echo must rewind
to the compact boundary's `logicalParentUuid`; otherwise timeline rollback
and provider context diverge.

## Relocation and transfer

`RelocateSession` is the copy phase and overwrites a stale destination.
`RemoveSessionTranscript` is the later delete phase. Callers must copy before
committing a workspace change and delete the old transcript only after commit.
Validate transcript basenames before deriving or removing a sidecar directory.

Transfers include the full opaque sidecar subtree. Unsupported links and
incomplete copies are explicit errors. Independent copies receive a new native
root identity but preserve message UUIDs, parent chains, content, and opaque
sidecars. A pending fork copy stops exactly at its resolved cursor and never
retires the borrowed parent identity.

Native homes and destination workspaces are always injected. Planned
destinations canonicalize an existing parent before slugging. See
[conversation-transfer.md](../../../../docs/specs/conversation-transfer.md)
for operation ownership and installation order.
