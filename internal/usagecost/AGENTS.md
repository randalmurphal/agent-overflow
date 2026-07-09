# internal/usagecost/

Hardcoded per-model USD pricing, applied at query time to
`usage_ledger` rows that carry no wire-reported cost (Codex,
claudetui). Stdlib-only, no persistence, no store/provider imports.

## Why this exists

Claude reports cost CLI-side (`result.modelUsage[model].costUSD`), so
Claude's `usage_ledger.cost_usd` is real and is never touched here.
Codex has no cost anywhere on its wire; claudetui's synthesized results
carry none either. Those rows persist tokens only
(`cost_source='none'`). Pricing them requires a rate table — but a
persisted estimate would go stale the moment rates change and there
would be no way to reprice history. Instead, `app_usage.go`'s
`GetUsageStats` calls `Price` fresh on every query and never writes the
result back. An app update with new rates reprices all history the
next time someone looks at usage stats.

## Surface

| Symbol | Purpose |
|---|---|
| `Rate` | Per-million-token pricing: `Input`, `Output`, `CacheRead`, `CacheWrite`. |
| `Price(model, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64) (costUSD float64, ok bool)` | Prices one model's token usage. `ok=false` means the model has no known pricing family — callers must count those tokens as unpriced, not silently treat 0 as a real price. |

## Rate table

`knownRates` in `pricing.go` maps model slug prefixes to `Rate`.
Matching is exact-first, then progressive suffix trim (drop the
trailing `-` or `.` segment each round) until a family prefix matches
— the same algorithm the removed `internal/provider/cost.go` used. See
the doc comment on `knownRates` for the pricing decisions baked into
the numbers: Claude cache-write uses the 1h-TTL rate (not 5m) because
Claude Code pins 1h cache in practice and the ledger can't distinguish
TTL tiers; there is no 200k-context tier because the ledger doesn't
store per-request context size; OpenAI cache-write is 0 unless a model
has an explicit published `CacheWrite` rate, as the GPT-5.6 family
does.

The trim algorithm has a real gap: a future dotted Codex version
without its own entry (e.g. `gpt-5.3-codex`) does NOT fall back to
`gpt-5-codex` — it trims to `gpt-5.3` then `gpt-5`, landing on the
plain non-codex family rate. `TestPrice_DottedCodexVersionMissesFamilyFallback`
regression-guards this; a new dotted `-codex` version needs its own
explicit table entry the day it ships, not an assumption that the
`gpt-5-codex` fallback will catch it.

## Responsibility boundary

- What BELONGS here: the rate table and the pure `Price` function.
- What does NOT belong here:
  - Deciding which `usage_ledger` rows need pricing (that's
    `cost_source='none'` vs `'wire'`, decided in `app_usage.go`).
  - Persisting an estimate anywhere. Estimates are query-time only.
  - Provider or store types. This package must stay stdlib-only so it
    can be imported from the App layer without pulling in either.

## Anti-patterns

- Do NOT persist the result of `Price` into `usage_ledger.cost_usd` or
  any other column. That would defeat the "rate updates reprice
  history" property this package exists for.
- Do NOT add a rate entry without a `Price` test that hand-computes the
  expected dollar amount — see `pricing_test.go`.

## References

- `internal/store/usage_ledger.go` — the ledger schema and
  `QueryUsage` / `QueryUsageDetail` this package's output feeds.
- `app_usage.go` — the only caller; merges wire cost and `Price`
  estimates into `GetUsageStats`'s response.
- `docs/architecture/adrs/ADR-008-cost-computation-in-provider-adapters.md`
  — history of the wire-cost-only decision and why read-time estimation
  was added on top of it instead of reverting it.
