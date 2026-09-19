# Claude model catalog

This package merges Claude's probe-reported picker rows into AO's shipped model
catalog and caches by `provider.ProbeCacheKey`. It does not spawn the CLI.

The wire is an enrichment source, not a complete catalog. Preserve shipped
order and models, including learned wire-only models. `DropBinary` is the only
subtraction path. An identity without an entry is served the newest entry
learned from the same binary; the shipped list alone is only for a binary that
never reported.

The shipped catalog owns model names and context windows. The wire owns feature
flags for rows it reports. Add wire-only models, deriving names from slugs and
windows from the nearest catalog family. Without a match, use 200k and widen
only when `[1m]` is explicit.

`Export` and `Seed` carry one identity's answer across a restart. `Export`
returns the exact key's entry only, with no same-binary fallback; `Seed`
rebuilds the models, learned models and wire rows `Store` would have produced
from that snapshot, and an empty snapshot creates nothing. A seeded entry
carries no drift report: dedup is per process, so each process's log keeps
reporting a stale catalog. This package cannot see the filesystem, so the caller
proves the record still describes the binary behind the key. The application
layer persists an export per account (`internal/provideraccounts`) and seeds at
boot while the binary identity is unchanged.

`ModelsFor` reports whether the answer came from an entry. That flag is the
catalog's provenance, not a quality claim: absence from an entry-backed answer
still carries no information about a model.

Never promote an unavailable effort default to a costlier tier.
`SupportsAutoMode` remains `*bool`; nil means unknown, and consumers restrict
Auto only on explicit false. Drift is a deduplicated maintainer signal, not a
user-facing error. Never infer capability or removal from alias text or absence.
