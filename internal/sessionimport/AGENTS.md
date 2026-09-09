# Session import

This package scans provider-native history, plans neutral import rows, and
commits each session transactionally to the store. Provider readers emit
`internal/importir.Event`; this package must match live triage row shapes
without running triage or starting providers.

## Import identity and ownership

Import identity is `(provider, source_session_id)`. Provider item and payload
IDs are thread-local. Scope every derived payload, turn, and source identity to
the destination thread.

Scanning excludes already imported, transfer-reserved, and spawned-child native
sessions. Recheck native ownership under the appropriate lock before creating
or refreshing a thread. Explicit provider forks remain importable and preserve
their raw parent session ID; reconcile lineage independently of import order
and never guess an absent or cyclic parent.

Callers inject both provider homes. Project resolution prefers an exact project
row before repository coverage and registered worktrees. Preserve the native
session's working directory as the thread workspace.

## Transaction and row contracts

`ImportOne` is all-or-nothing. If history application fails after thread
creation, remove the partial thread and its import usage rows so the session can
be offered again.

Every item-producing event requires `SourceUUID`. Tool completion updates the
launch identified by provider item ID; only an explicit import-unavailable
marker permits synthesizing a missing launch. Preserve structured file-change
payloads before deriving summaries. Imported content blocks are settled whole
blocks, and turn completion force-closes unresolved tools as live triage does.

Turn indices start at 1 so a resumed thread can allocate its first live turn at
0. Item indices restart per turn. Reject a batch whose scoped turn ID already
exists before insertion. Seal every imported turn with a source-derived
completion time; imported history must never resemble crash recovery.

Usage rows use `CostSource:"none"` and the turn timestamp because native
history has usage but no authoritative cost.

## Refresh

The thread cursor and source cursor are distinct. Refuse refresh after local
history diverges. `PlanUpdate` performs all conversion and validation without
writes; `ApplyUpdate` accepts only an applicable plan.

Claude refresh follows the descendant branch containing the recorded leaf UUID.
Codex refresh validates file size, first-line fingerprint, and declared history
mode before trusting a byte offset. A changed or unknown current source identity
fails closed as source divergence.

`parity_test.go` compares provider fixtures through live triage and import.
Golden fixtures cover provider-only shapes. `make import-corpus-smoke` is the
manual format-drift gate and must use copies of provider homes.
