# Claude model catalog

This package merges Claude's probe-reported picker rows into AO's shipped model
catalog and caches by `provider.ProbeCacheKey`. It does not spawn the CLI.

The served list has two jobs. As a lookup it keeps every shipped and learned
model, so a thread on a model the CLI stopped listing still resolves its
capabilities; `DropBinary` is the only subtraction path. As a picker it offers
what the latest non-empty answer lists, in the CLI's order, with the CLI's
`default` row marked `IsDefault`; every other row is marked `Unlisted`. The
application layer clears `Unlisted` for models that ran in the last week, so a
row one degraded answer omits stays selectable while it is in use. An answer
with no models is no answer: the previous one stands, and with none the
shipped list is served unmarked. An identity without an entry is served the
newest entry learned from the same binary.

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
