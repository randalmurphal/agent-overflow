# Claude model catalog

This package merges Claude's probe-reported picker rows into AO's shipped model
catalog and caches by `provider.ProbeCacheKey`. It does not spawn the CLI.

The wire is an enrichment source, not a complete catalog. Preserve shipped
order and models, including learned wire-only models. `DropBinary` is the only
subtraction path.

The shipped catalog owns model names and context windows. The wire owns feature
flags for rows it reports. Add wire-only models, deriving names from slugs and
windows from the nearest catalog family. Without a match, use 200k and widen
only when `[1m]` is explicit.

Never promote an unavailable effort default to a costlier tier.
`SupportsAutoMode` remains `*bool`; nil means unknown, and consumers restrict
Auto only on explicit false. Drift is a deduplicated maintainer signal, not a
user-facing error. Never infer capability or removal from alias text or absence.
