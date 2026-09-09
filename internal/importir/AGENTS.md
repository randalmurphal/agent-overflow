# Session import vocabulary

This package is the neutral boundary between provider-owned history readers and
`internal/sessionimport`. Keep it limited to types shared by those sides. It
may depend on `internal/provider`, but never on a provider subpackage, store,
or triage.

`Event` embeds `provider.ProviderEvent` and adds source coordinates.
`SourceUUID` is required for every item-producing event and becomes
`items.meta.import_source_uuid`. Claude uses the transcript UUID; Codex uses
`line:<record-start-offset>`.

`SourceOffset` is a separate, optional resume cursor. Codex records the byte
after the terminating newline for append-only tail reads. Claude leaves it zero
because its history is a UUID DAG. Do not grow this package into another event
model.
