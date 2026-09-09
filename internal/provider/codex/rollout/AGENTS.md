# Codex native history reader

This package reads Codex's `state_5.sqlite` index and rollout JSONL into
`internal/importir`. It never starts a provider, writes live Codex state, or
resolves a home directory. Callers always inject `CodexHome`.

Read [codex.md](../../../../docs/references/codex.md) for the on-disk record
sets, history modes, ledger, and upstream source locations.

## Safe listing and parsing

Gate every indexed rollout path through `PathInHome` before touching it.
Open the index read-only with `immutable=1`; do not inspect WAL files or add a
directory-walk fallback. Listing excludes archived, empty, subagent, guardian,
and structurally referenced child threads.

Rollout enums are open. Count and skip unknown or corrupt records while
continuing the session. Preserve exact line offsets with the custom scanner;
complete lines only, under `MaxLineBytes`. A tail parse still pre-scans the
header and history mode. Validate a resume offset against file size and a
preceding newline.

Accept the `session_meta` matching the requested session ID, not the first
embedded header. Profile fields come from their recorded configuration frames,
not usage inference. Events use `line:<start-offset>` as source identity and
the byte after the newline as resume offset.

## Conversion invariants

Choose paginated versus legacy conversion from the declared history mode.
Deduplicate mirrored item and response records. Decode recognized dropped
records enough to detect shape drift. Do not follow `history_base` during
session import; report the missing prefix explicitly.

Correlate every tool lifecycle only by `call_id`. Preserve unresolved or
unavailable results as explicit metadata rather than inventing status. Synthetic
turn IDs include the rollout session ID and remain deterministic for a parse.

The external import ledger is advisory provenance. Derive a recognized source
agent and canonical session ID conservatively; unreadable or unknown data warns
without failing session listing.

## Transfers

`TransferGraph` follows structured collaboration and `history_base`
references under containment, depth, ambiguity, and cycle checks.
`CopyTransferFiles` assigns independent operation-stable IDs and rewrites only
understood identity fields. Preserve opaque content and numeric precision.

Flatten paginated prefix chains into a standalone native rollout before
transfer, keeping byte and ordinal cuts consistent. Preserve historical turn
and item IDs, remap child projection boundaries, and refuse incomplete records
or unsupported history modes.

Tests use temporary homes and hand-built indexes. Never read the developer's
real Codex home.
