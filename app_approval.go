package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/provider"
)

// RespondToApproval forwards an interactive response to the active provider session.
func (a *App) RespondToApproval(threadID string, response provider.ApprovalResponse) error {
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no active session for thread %s", threadID)
	}

	providerSess := sess.providerSession()
	if providerSess == nil {
		return fmt.Errorf("session has no provider")
	}
	if err := providerSess.RespondToApproval(context.Background(), response); err != nil {
		return err
	}

	decision := provider.NormalizeApprovalDecision(response)
	metaFields := map[string]any{
		"requestId": response.RequestID,
		"decision":  decision,
	}
	// Forward updatedInput through the resolution event so triage can
	// refresh the tool_call summary to reflect the MODIFIED input on an
	// "amended" decision (spec invariant: an amended tool row shows the
	// input that will actually run, not the original).
	if len(response.UpdatedInput) > 0 {
		metaFields["updatedInput"] = json.RawMessage(response.UpdatedInput)
	}
	meta, _ := json.Marshal(metaFields)
	evt := provider.ProviderEvent{
		Kind:      provider.EventApprovalResolved,
		ThreadID:  threadID,
		ItemID:    response.RequestID,
		Meta:      meta,
		Timestamp: time.Now(),
	}
	if a.triage != nil {
		if err := a.triage.Handle(evt); err != nil {
			log.Printf("respond to approval triage update failed: %v", err)
		}
	} else {
		a.emit("provider:approval", provider.ApprovalEvent{
			Action:    "resolve",
			ThreadID:  threadID,
			RequestID: response.RequestID,
			Decision:  decision,
		})
	}
	return nil
}
