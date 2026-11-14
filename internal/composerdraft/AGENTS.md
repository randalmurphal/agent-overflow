# `internal/composerdraft`

Pure construction of persisted composer-draft payloads. Callers own storage, attachment lookup, and send orchestration.

`PartFromUserItem` keeps the stored item's attachment ids, which belong to its
own thread; cross-thread callers clone attachments first and use `FromParts`.
`FromParts` clears terminal chips because stored user items do not carry
composer-only terminal context.

`MergeParts` restores messages before existing draft text, deduplicates
attachment ids by first occurrence, preserves the current draft's terminal
chips, and lets an existing pending plan win. Update all builders and round-trip
tests when the persisted draft shape changes.
