# ADR-008: Cost Computation in Provider Adapters

Status: superseded (2026-07-03)
Date: 2026-04-18

> **Superseded.** The client-side pricing table (`CalculateCost` /
> `internal/provider/cost.go`) was removed on 2026-07-03: provider
> adapters attach **wire-reported cost only**. Claude computes cost
> CLI-side and reports it on `result.modelUsage[model].costUSD` /
> `total_cost_usd` (both session-cumulative — the adapter
> snapshot-deltas them per turn; see
> `internal/provider/claude/usage_accounting.go`). Codex reports no
> USD anywhere on its wire, so Codex (and claudetui) rows carry tokens
> only and persist with `cost_source='none'`.
>
> Later the same day, read-time pricing was added on top of this at a
> different layer: `internal/usagecost` holds a hardcoded per-model
> rate table, and `GetUsageStats` (`app_usage.go`) calls it to price
> `cost_source='none'` rows when it aggregates the ledger for display.
> The estimate is computed fresh on every query and never written back
> to `usage_ledger.cost_usd` — a rate-table update reprices all history
> the next time someone looks at usage stats, and Claude's wire cost is
> never touched by this path. The parts of this ADR that survive:
> usage/cost accounting is still attached in the provider adapter (not
> triage), what lands in `usage_ledger.cost_usd` is still wire-reported
> only, and the frontend still never recomputes cost itself — dollar
> totals it renders come from `GetUsageStats`, wire and estimate
> already merged.

## Context

Provider turn-complete events can carry token usage
(`input_tokens`, `output_tokens`, `cache_creation_input_tokens`,
`cache_read_input_tokens`) but not USD cost. Cost is computed from a
per-model pricing table — different for each provider, each model, and
sometimes each pricing tier.

The question: where does `CalculateCost` get called?

- **In the provider adapter**, before attaching turn usage/cost
  accounting to the normalized event stream.
- **In triage**, inside `handleTokenUsage`.
- **In the frontend**, from the turn metadata's raw token counts.

## Decision

Cost computation lives in the provider adapter. `CalculateCost` in
`internal/provider/cost.go` is the pure function; the adapter calls it
when attaching turn usage/cost accounting to turn completion metadata.
Triage does not recompute cost. Context-window `EventTokenUsage` events
are a separate meter signal and are not cost events.

## Rationale

- **Pricing is provider knowledge.** The table maps provider + model
  → per-million-token rate. Putting the table in triage would
  force the provider-agnostic layer to know which models each
  provider supports, including new ones as they ship.
- **Model rerouting.** Claude and Codex both occasionally reroute a
  turn to a different model (rate limits, availability). The
  adapter sees the reroute notification and knows which model's
  pricing to apply to the resulting turn usage/cost metadata. Triage
  would need parallel tracking to catch the reroute.
- **Frontend display, not computation.** The frontend should render
  the cost number it receives, not recompute from token counts.
  Recomputing in the frontend would mean shipping the pricing table
  to the webview, updating it on every new model, and keeping it in
  sync with Go.
- **Cost field is stable output.** Persisted turn usage/cost metadata
  carries the cost computed at the time of emission. Future pricing
  changes don't rewrite historical turns.

Considered alternatives:

- **Triage-level computation.** Rejected: makes triage
  provider-aware. Principle 6 says no.
- **Frontend-level computation.** Rejected: webview pricing table is
  a maintenance burden and a race with backend updates.

## Consequences

- Invariant #15 ("Cost computation lives in provider adapters, not
  triage") is this ADR promoted to invariant.
- Adding a new provider requires adding its pricing table to
  `cost.go` (or a provider-specific sibling). The table is the
  provider's responsibility.
- Adding a new model to an existing provider requires updating the
  pricing table. `cost_test.go` covers the table with fixtures —
  add a case when the table changes.
- Turn usage/cost metadata flows through triage and the replay log
  unchanged, making it stable for historical analysis.
