# `internal/usagecost`

Pure, stdlib-only query-time pricing for token usage that has no wire-reported cost. `internal/usageledger` is the only caller and owns row selection and aggregation.

- Claude wire-reported cost is never repriced here.
- Estimates are never persisted; changing rates must reprice history on the next query.
- `Price` returns `ok=false` for unknown families. Callers must preserve that uncertainty instead of treating zero as a known price.
- Model matching is exact first, then suffix trimming. Add explicit entries for dotted Codex versions whose family would otherwise fall through incorrectly.
- Every rate change needs a hand-computed pricing test and a current authoritative source.

The shared ledger fold must continue to drive both display and workflow budget enforcement.
