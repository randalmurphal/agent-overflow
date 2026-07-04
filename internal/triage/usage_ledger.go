// Package triage — usage-ledger persistence for settled turns.
// Providers deliver per-turn per-model usage deltas on the turn-complete
// meta; this file projects them into append-only store rows. Context-meter
// state is a separate concern (usage_compaction.go).

package triage

import (
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// appendUsageLedger persists one ledger row per model for a settled
// turn. Called from BOTH turn-settle paths — settleTurnRow (first
// settlement) and persistLateTurnPayload (multi-result cascade / soft
// round-close fold) — because the provider parsers emit per-turn DELTAS:
// a second result for the same logical turn carries only usage that
// accrued since the first settle, so appending on every non-empty event
// is additive-correct while the turns row keeps first-write-wins for
// display.
//
// Failure here is logged, not returned: the turn itself persisted fine
// and accounting is supplementary — but it is an error (data we want),
// never silently dropped.
func (r *Router) appendUsageLedger(evt provider.ProviderEvent, turnID string, meta turnCompleteMeta, now int64) {
	if len(meta.ModelUsage) == 0 {
		return
	}
	// Nested/subagent turn completes never carry ModelUsage today
	// (Claude folds subagent spend into the parent's modelUsage; Codex
	// child lifecycle events attach no usage) — the guard keeps a future
	// nested producer from double-counting against the parent's rows.
	if evt.ParentToolUseID != "" {
		return
	}

	attribution, err := r.store.GetThreadContextSettings(evt.ThreadID)
	if err != nil {
		// Thread row unavailable (already deleted mid-settle). Persist
		// unattributed rather than dropping spend.
		log.Printf("triage: usage ledger attribution for %s: %v", evt.ThreadID, err)
	}

	rows := make([]store.UsageLedgerRow, 0, len(meta.ModelUsage))
	for _, m := range meta.ModelUsage {
		rows = append(rows, store.UsageLedgerRow{
			CreatedAt:                now,
			ThreadID:                 evt.ThreadID,
			ProjectID:                attribution.ProjectID,
			TurnID:                   turnID,
			Provider:                 provider.UsageProviderFamily(attribution.Provider),
			Model:                    m.Model,
			InputTokens:              m.InputTokens,
			OutputTokens:             m.OutputTokens,
			CacheReadInputTokens:     m.CacheReadInputTokens,
			CacheCreationInputTokens: m.CacheCreationInputTokens,
			ReasoningOutputTokens:    m.ReasoningOutputTokens,
			CostUSD:                  m.TotalCostUSD,
		})
	}
	if err := r.store.AppendUsage(rows); err != nil {
		log.Printf("triage: usage ledger append for %s turn %s: %v", evt.ThreadID, turnID, err)
	}
}
