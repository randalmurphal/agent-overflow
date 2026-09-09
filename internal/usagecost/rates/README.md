# Usage pricing snapshots

Provider-reported per-turn costs take precedence. These catalogs price settled
rows that contain tokens only; they never price pending usage. Codex cumulative
thread estimates remain separate because they have no reliable per-turn USD split.

To publish updated fallback rates:

1. Copy the latest JSON to a new dated snapshot and retain the old snapshots.
2. Verify standard USD rates and aliases against the official URLs in `sources`.
   For a new model, set `"backfill": true` on its rate once verified to cover
   earlier usage, normally its launch price. A later price change must not be
   marked for backfill unless it also applies to that earlier usage.
3. Update `CurrentVersion` and the published-rate regression cases.

New ledger rows store `CurrentVersion`; queries keep different versions separate.
Migration 93 pins older rows to the initial snapshot because earlier releases did
not retain a pricing basis. When a pinned snapshot cannot price a model, reads
consult the first later snapshot containing it. A rate marked `backfill` fills
those missing estimates on the next query, without rewriting tokens, importing
history again, or adding ledger rows. Existing priced estimates stay pinned.

The first matching snapshot must explicitly permit backfill; reads never skip an
ineligible price to use a later price or alias target. Retain that first snapshot
so a subsequent price change cannot change backfilled history. If earlier pricing
is uncertain or differs, leave backfill disabled until it can be established.
If verification comes later, enable backfill on that first matching snapshot
without changing its rates.
Aliases use `{ "model": "target-id", "backfill": true }` when both the
historical target and its rates are verified. Otherwise omit `backfill`; the
target model's approval alone does not establish a moving alias's old target.
Do not redirect an old alias to a new model.

A missing cache-write class can likewise use the first later published write
rate when marked for backfill, while retaining the original other rates and
alias target. Provider-reported costs, including zero, and pending usage never
use this fallback.

Only exact IDs, explicit aliases and date-suffixed snapshots of known IDs match.
Unknown variants stay unpriced. A zero cache-write rate means no published rate;
a row with cache writes in that class remains unpriced.

These are estimates, not invoices. The ledger lacks per-request service tier,
context tier, regional routing and cache TTL. Claude cache writes retain the
one-hour estimate. Prefer provider reports when they include those adjustments.
