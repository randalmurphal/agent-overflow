# `internal/usagecost`

Pure, stdlib-only read-time pricing for token usage that has no wire-reported cost. `internal/usageledger` is the only caller and owns row selection and aggregation.

- Claude wire-reported cost is never repriced here.
- Estimates are never persisted. Every query prices rows from `rates.json` by the row's own UTC day, so a rate change reprices exactly the usage it applied to.
- `Price` returns `ok=false` for unknown models, usage before a model's first dated rate, and cache writes on a model with no published write rate. Callers must preserve that uncertainty instead of treating zero as a known price.
- Model matching is exact first, then alias, then a documented date suffix. Add explicit entries for dotted Codex versions whose family would otherwise fall through incorrectly.

## Updating `rates.json`

- New model: add its entry with no `from`. Earlier usage of that model prices automatically.
- Price change: append `{"from": "YYYY-MM-DD", ...}` with the date the new price took effect. Never edit an earlier entry.
- Alias pointer moved: append `{"from": "YYYY-MM-DD", "model": ...}` the same way.
- A zero `cacheWrite` means no published write rate; rows with cache writes stay unpriced.
- Verify against the URLs in `sources` and update the hand-computed regression cases in `pricing_test.go`.
