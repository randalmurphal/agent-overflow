package triage

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// PersistAndRegisterPendingQuietFlushSendWithExpectation publishes the quiet
// row and its correlation record under the same lock used to capture echo
// placement. An unconfirmed row must never look like a stable predecessor
// merely because the echo arrived between its persist and registration.
func (r *Router) PersistAndRegisterPendingQuietFlushSendWithExpectation(
	threadID, queueItemID string,
	item store.Item,
	responseTurnIndex int,
	enqueuedAt int64,
	expect PendingSendExpectation,
) error {
	anchor := r.flushAnchor(threadID)
	anchor.Lock()
	defer anchor.Unlock()
	if err := r.PersistItemQuiet(item, nil); err != nil {
		return err
	}
	r.RegisterPendingQuietFlushSendWithExpectation(threadID, queueItemID, item, responseTurnIndex, enqueuedAt, expect)
	return nil
}

// userMessageConfirmation retains first-echo facts only while its write is
// pending. Retries and teardown replay this decision, never the current tail.
type userMessageConfirmation struct {
	Row          store.Item
	Placement    bool
	Promoted     bool
	BoundaryID   string
	Group        []store.Item
	CaptureError error
}

// Caller holds flushAnchor. The drain lock fences handed-off async settles
// through predecessor capture; unconfirmed rows are never stable predecessors.
func (r *Router) captureUserConfirmation(threadID string, pending *pendingSend, now int64) {
	if pending.Confirmation != nil {
		return
	}
	plan := &userMessageConfirmation{Row: store.Item{ID: pending.AOItemID, ThreadID: threadID, TurnIndex: pending.TurnIndex}}
	pending.Confirmation = plan
	deferred := pending.DeferredItem != nil
	if deferred {
		plan.Row = *pending.DeferredItem
		plan.Row.CreatedAt = now
	} else if pending.QuietItem != nil {
		plan.Row = *pending.QuietItem
	}
	if plan.Row.CreatedAt == 0 {
		plan.Row.CreatedAt = now
	}
	plan.Row.UpdatedAt = now
	plan.Placement = deferred || (pending.Shape == sendShapeFlush && !pending.AnchoredAtInterrupt)
	plan.Promoted = pending.Shape == sendShapeFlush && pending.AnchoredAtInterrupt && !pending.WasDeferred
	if !plan.Placement && !plan.Promoted {
		return
	}
	drain := r.drainLock(threadID)
	drain.Lock()
	defer drain.Unlock()
	queued := r.hasQueuedInterruptItems(threadID)
	if queued {
		if err := r.drainInterruptQueueLocked(threadID, false); err != nil {
			plan.CaptureError = err
			return
		}
	}
	if plan.Promoted && queued {
		plan.Placement = true
	}
	exclude := []string{pending.AOItemID}
	plan.Group = []store.Item{plan.Row}
	r.mu.Lock()
	for _, entry := range r.pendingSendsLocked(threadID) {
		exclude = append(exclude, entry.AOItemID)
		if plan.Promoted && plan.Placement && !entry.EchoConsumed && entry.Shape == sendShapeFlush && entry.AnchoredAtInterrupt && !entry.WasDeferred && entry.QuietItem != nil && entry.QuietItem.TurnIndex == plan.Row.TurnIndex {
			plan.Group = append(plan.Group, store.Item{ID: entry.AOItemID})
		}
	}
	r.mu.Unlock()
	plan.BoundaryID, plan.CaptureError = r.store.CaptureUserPlacementBoundary(threadID, plan.Row.TurnIndex, exclude)

	if plan.Promoted && !plan.Placement && plan.BoundaryID == "" {
		plan.BoundaryID = pending.AOItemID
	}
}

// commitUserConfirmation is shared by the echo and the session-death heal.
// Provider consumption is already proven; failure never makes this resendable.
func (r *Router) commitUserConfirmation(threadID string, pending *pendingSend, now int64) (store.Item, error) {
	plan := pending.Confirmation
	if plan == nil {
		return store.Item{}, fmt.Errorf("triage: missing first-echo confirmation for %s", pending.AOItemID)
	}
	if plan.CaptureError != nil {
		return store.Item{}, fmt.Errorf("triage: first-echo boundary unavailable: %w", plan.CaptureError)
	}
	transform := func(meta string, boundary int) (string, error) {
		merged, err := usermessage.MergeProviderIDs(meta, pending.EchoProviderItemID, pending.EchoParentUUID)
		if err != nil {
			return "", err
		}
		if plan.Promoted {
			merged, err = itemmeta.MarkPromotedAtInterrupt(merged)
			if err != nil {
				return "", err
			}
			if boundary < 0 {
				return "", fmt.Errorf("triage: negative promoted confirmation boundary %d", boundary)
			}
			return itemmeta.MarkPromotedEchoBoundary(merged, boundary)
		}
		return merged, nil
	}
	var rows []store.Item
	if plan.Placement {
		var err error
		group := plan.Group
		if len(group) > 1 {
			// A sibling's own echo supersedes its provisional group position.
			// Captured membership is a candidate list, never permission to
			// undo a later confirmation or a teardown's restoration claim.
			eligible := make(map[string]bool, len(group)-1)
			r.mu.Lock()
			for _, entry := range r.pendingSendsLocked(threadID) {
				if !entry.EchoConsumed {
					eligible[entry.AOItemID] = true
				}
			}
			r.mu.Unlock()
			group = []store.Item{group[0]}
			for _, sibling := range plan.Group[1:] {
				if eligible[sibling.ID] {
					group = append(group, sibling)
				}
			}
		}
		rows, err = r.store.PlaceUserItemsAfterBoundary(threadID, plan.Row.TurnIndex, plan.BoundaryID, group, transform, now)
		if err != nil {
			return store.Item{}, err
		}
	} else {
		row, err := r.store.UpdateItemMetaAtBoundary(threadID, pending.AOItemID, plan.BoundaryID, transform, now)
		if err != nil {
			return store.Item{}, err
		}
		rows = []store.Item{row}
	}
	for _, row := range rows {
		r.emitItemUpsert(row)
	}
	if pending.DeferredItem != nil {
		r.metrics.ItemsPersisted.Add(context.Background(), 1, metric.WithAttributes(attribute.String("kind", rows[0].Kind)))
	}
	if plan.Placement && userTextCountsAsThreadActivity(rows[0]) {
		r.bumpThreadActivityForUserText(threadID, now)
	}
	return rows[0], nil
}

func (r *Router) healUserConfirmation(threadID string, pending *pendingSend) error {
	row, err := r.commitUserConfirmation(threadID, pending, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if pending.QueueItemID != "" && !pending.AnchorRecordedAtEcho {
		r.mu.Lock()
		hook := r.flushUserTextConfirmed
		r.mu.Unlock()
		if hook != nil {
			hook(threadID, row)
		}
	}
	return r.store.UpdateMessageAnchorProviderIDs(threadID, pending.AOItemID, pending.EchoProviderItemID, pending.EchoParentUUID)
}
