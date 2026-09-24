package triage

import (
	"context"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func (r *Router) updateApprovalItem(item store.Item, request provider.ApprovalRequest, decision string, now int64) error {
	for {
		item.Decision = decision
		if item.ToolName == "" {
			item.ToolName = request.ToolName
		}
		if item.Summary == "" {
			item.Summary = approvalSummary(request)
		}
		// An amended approval renders the input the user accepted.
		if decision == "amended" && len(request.Input) > 0 {
			if refreshed := approvalSummary(request); refreshed != "" {
				item.Summary = refreshed
			}
		}
		if approvalDeclinesExecution(decision) && item.Status != statusCompleted && item.Status != statusErrored {
			item.Status = statusDeclined
		}
		if approvalLosesExecution(decision) && item.Status != statusCompleted && item.Status != statusDeclined {
			item.Status = statusErrored
		}
		item.UpdatedAt = now
		item.SubagentAnchor = r.subagentAnchorFor(item.ThreadID, item.ID, item.ParentID)

		persisted, changed, err := r.store.UpdateItemIfRevision(item)
		if err != nil {
			return fmt.Errorf("approval item update: %w", err)
		}
		if changed {
			r.emitItemUpsert(persisted)
			r.metrics.ItemsPersisted.Add(context.Background(), 1, metric.WithAttributes(attribute.String("kind", persisted.Kind)))
			r.takeApprovalDecision(item.ThreadID, item.ID)
			return nil
		}
		var found bool
		item, found, err = r.store.GetThreadItem(item.ThreadID, item.ID)
		if err != nil {
			return fmt.Errorf("approval item reread: %w", err)
		}
		if !found {
			return fmt.Errorf("approval item disappeared during update")
		}
	}
}
