// Package triage — usage-ledger persistence for settled turns.
// Providers deliver per-turn per-model usage deltas on the turn-complete
// meta; this file projects them into append-only store rows. Context-meter
// state is a separate concern (usage_compaction.go).

package triage

import (
	"fmt"
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// appendUsageLedger exchanges reported progress for final provider accounting.
func (r *Router) appendUsageLedger(evt provider.ProviderEvent, turnID string, meta turnCompleteMeta, now int64) (err error) {
	defer func() {
		if err != nil {
			r.emitUsageFailure(evt.ThreadID, err)
		}
	}()
	if len(meta.ModelUsage) == 0 || evt.ParentToolUseID != "" {
		return nil
	}
	rows := r.usageLedgerRows(evt, turnID, meta.ModelUsage, now)
	if err := r.store.AppendUsageAndReconcile(meta.UsageScope, rows); err != nil {
		return fmt.Errorf("usage accounting for %s: %w", evt.ThreadID, err)
	}
	r.throttledEmitUsage(evt.ThreadID, provider.UsageEvent{Action: "progress", ThreadID: evt.ThreadID})
	return nil
}

func (r *Router) handleUsageProgress(evt provider.ProviderEvent) (err error) {
	defer func() {
		if err != nil {
			r.emitUsageFailure(evt.ThreadID, err)
		}
	}()
	if evt.ParentToolUseID != "" {
		return nil
	}
	if evt.UsageProgress == nil {
		return fmt.Errorf("usage progress missing payload")
	}
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("usage progress turn: %w", err)
	}
	progress := evt.UsageProgress
	rows := r.usageLedgerRows(evt, r.persistedTurnID(evt, turnIndex), progress.ModelUsage, eventTimestampMillis(evt))
	changed, err := r.store.PutUsageProgress(progress.Scope, progress.Segment, rows)
	if err != nil {
		return err
	}
	if changed {
		r.throttledEmitUsage(evt.ThreadID, provider.UsageEvent{Action: "progress", ThreadID: evt.ThreadID})
	}
	return nil
}

func (r *Router) usageLedgerRows(evt provider.ProviderEvent, turnID string, models []provider.ModelTokenUsage, now int64) []store.UsageLedgerRow {
	attribution, err := r.store.GetThreadContextSettings(evt.ThreadID)
	if err != nil {
		// Thread row unavailable (already deleted mid-settle). Persist
		// unattributed rather than dropping spend.
		log.Printf("triage: usage ledger attribution for %s: %v", evt.ThreadID, err)
	}
	r.usageResolverMu.RLock()
	resolveWorkItem := r.usageWorkItemResolver
	r.usageResolverMu.RUnlock()
	workItemID := ""
	if resolveWorkItem != nil {
		workItemID = resolveWorkItem(evt.ThreadID)
	}

	rows := make([]store.UsageLedgerRow, 0, len(models))
	for _, m := range models {
		source := ""
		if m.CostReported {
			source = "wire"
		}
		rows = append(rows, store.UsageLedgerRow{
			CreatedAt:                now,
			ThreadID:                 evt.ThreadID,
			ProjectID:                attribution.ProjectID,
			WorkItemID:               workItemID,
			TurnID:                   turnID,
			Provider:                 provider.UsageProviderFamily(attribution.Provider),
			Model:                    m.Model,
			AccountingModel:          m.AccountingModel,
			InputTokens:              m.InputTokens,
			OutputTokens:             m.OutputTokens,
			CacheReadInputTokens:     m.CacheReadInputTokens,
			CacheCreationInputTokens: m.CacheCreationInputTokens,
			ReasoningOutputTokens:    m.ReasoningOutputTokens,
			CostUSD:                  m.TotalCostUSD,
			CostSource:               source,
		})
	}
	return rows
}

func (r *Router) emitUsageFailure(threadID string, err error) {
	log.Printf("triage: persist reported usage for %s: %v", threadID, err)
	r.throttledEmitUsage(threadID, provider.UsageEvent{Action: "progress", ThreadID: threadID,
		Error: "Reported usage could not be saved. Totals may be incomplete."})
}
