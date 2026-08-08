package sessionimport

import (
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// usageRows projects one settled turn's per-model usage deltas into
// append-only usage_ledger rows — the same projection triage's
// appendUsageLedger makes, minus its live-only pieces (the workflow
// resolver, and the thread lookup this writer already holds).
//
// Two rules an import must not get wrong:
//
//   - CreatedAt is the turn's own completion time, never now(). Every
//     usage surface buckets on that column, so a restamped import would
//     report a year of history as today's spend.
//   - CostSource is always "none". Neither session file carries a
//     wire-reported cost: Claude's `total_cost_usd` lives on the
//     stream-json result envelope, which transcripts do not contain, and
//     Codex has no cost on its wire at all. "none" is what makes
//     GetUsageStats price these rows from internal/usagecost at query
//     time instead of reading a zero as a real total.
func usageRows(
	thread store.Thread,
	turnID string,
	completedAt int64,
	modelUsage []provider.ModelTokenUsage,
) []store.UsageLedgerRow {
	if len(modelUsage) == 0 {
		return nil
	}
	rows := make([]store.UsageLedgerRow, 0, len(modelUsage))
	for _, usage := range modelUsage {
		if usage.Model == "" && usage.TokenUsage.IsZero() {
			continue
		}
		rows = append(rows, store.UsageLedgerRow{
			CreatedAt:                completedAt,
			ThreadID:                 thread.ID,
			ProjectID:                thread.ProjectID,
			TurnID:                   turnID,
			Provider:                 provider.UsageProviderFamily(thread.Provider),
			Model:                    usage.Model,
			InputTokens:              usage.InputTokens,
			OutputTokens:             usage.OutputTokens,
			CacheReadInputTokens:     usage.CacheReadInputTokens,
			CacheCreationInputTokens: usage.CacheCreationInputTokens,
			ReasoningOutputTokens:    usage.ReasoningOutputTokens,
			CostSource:               "none",
		})
	}
	return rows
}
