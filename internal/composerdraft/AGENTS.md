# `internal/composerdraft`

Pure construction of persisted composer-draft payloads. Callers own storage, attachment lookup, and send orchestration.

Use `FromUserItem` only when attachment ids already belong to the target thread;
cross-thread callers clone attachments first and use `FromParts`. Both builders
clear terminal chips because stored user items do not carry composer-only
terminal context.

`MergeParts` restores messages before existing draft text, deduplicates
attachment ids by first occurrence, preserves the current draft's terminal
chips, and lets an existing pending plan win. Update all builders and round-trip
tests when the persisted draft shape changes.
