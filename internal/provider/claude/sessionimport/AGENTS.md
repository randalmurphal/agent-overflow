# Claude session history reader

This package reads Claude session files into `internal/importir`. It is
read-only, starts no process, and receives both the projects directory and
session path from the caller. Tests use only temporary homes and fixture rows.

## Bounded loading

`LoadSession` performs a streaming skeleton pass and retains the open file.
Callers must close it. `ConvertActiveBranch` then decodes only the selected
branch with `ReadAt`. Do not decode the whole file or all branches.

Listing uses bounded head and tail reads. A line over 16 MiB is skipped with a
warning so later records remain readable. A file over 1 GiB is refused before
reading. Missing timestamps inherit from source timestamps; never call
`time.Now` for imported history.

The import DAG drops progress and sidechain rows, keeps the first duplicate
UUID, joins compact boundaries through `logicalParentUuid`, and rejects cyclic
branches. Use the shared sessionfork parent rules rather than copying them.

## Conversion

Emit the same `provider.ProviderEvent` vocabulary as the live parser. Preserve
source UUIDs, timestamps, provider tool IDs, subagent parent IDs, structured
tool results, API-error classification, model profile, and accumulated usage.
Imported blocks contain whole content, not deltas.

Missing externalized tool output is explicit
`itemmeta.ImportUnavailableKey` metadata. Unknown system subtypes warn and
skip; one malformed record must not fail the session. Do not put large content
in event metadata.

Branch selection, transcript shapes, and observed compatibility behavior are
documented in [claude-wire.md](../../../../docs/references/claude-wire.md).
