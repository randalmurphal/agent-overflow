package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// ListPendingInteractiveRequests returns live approval and structured-input
// prompts for a thread so a pane that missed the original event can hydrate
// its composer controls.
func (a *App) ListPendingInteractiveRequests(threadID string) (provider.PendingInteractiveRequests, error) {
	snapshot := provider.PendingInteractiveRequests{
		Approvals:  []provider.ApprovalRequest{},
		UserInputs: []provider.UserInputRequest{},
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return snapshot, nil
	}
	if a.triage == nil {
		return snapshot, nil
	}
	return a.triage.PendingInteractiveRequests(threadID), nil
}

// RespondToApproval forwards an interactive response to the active provider session.
func (a *App) RespondToApproval(threadID string, response provider.ApprovalResponse) error {
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		err := fmt.Errorf("no active session for thread %s", threadID)
		a.emitApprovalFailure(threadID, response.RequestID, err)
		return err
	}

	providerSess := sess.providerSession()
	if providerSess == nil {
		err := fmt.Errorf("session has no provider")
		a.emitApprovalFailure(threadID, response.RequestID, err)
		return err
	}
	// Approval responses write to provider stdin; stamp activity
	// before the write so the idle reaper can't tear down a session
	// the user just answered a permission prompt on. The follow-up
	// EventApprovalResolved bumps again via sessionEventHandler — this
	// guards the window before that lands.
	sess.liveness.bumpActivity(time.Now())
	if err := providerSess.RespondToApproval(context.Background(), response); err != nil {
		a.emitApprovalFailure(threadID, response.RequestID, err)
		return err
	}

	decision := provider.NormalizeApprovalDecision(response)
	// Forward updatedInput through the resolution event so triage can
	// refresh the tool_call summary to reflect the MODIFIED input on an
	// "amended" decision (spec invariant: an amended tool row shows the
	// input that will actually run, not the original).
	var updatedInput json.RawMessage
	if len(response.UpdatedInput) > 0 {
		updatedInput = response.UpdatedInput
	}
	a.emitApprovalResolution(threadID, response.RequestID, decision, updatedInput)
	return nil
}

// RespondToUserInput forwards structured answers to the active provider session.
func (a *App) RespondToUserInput(threadID string, response provider.UserInputResponse) error {
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		err := fmt.Errorf("no active session for thread %s", threadID)
		a.emitUserInputFailure(threadID, response.RequestID, err)
		return err
	}

	providerSess := sess.providerSession()
	if providerSess == nil {
		err := fmt.Errorf("session has no provider")
		a.emitUserInputFailure(threadID, response.RequestID, err)
		return err
	}
	// Same rationale as RespondToApproval: bump before stdin write so
	// the reaper doesn't kill a session mid-structured-input round-trip.
	sess.liveness.bumpActivity(time.Now())
	if err := providerSess.RespondToUserInput(context.Background(), response); err != nil {
		a.emitUserInputFailure(threadID, response.RequestID, err)
		return err
	}

	decision := "answered"
	if response.Decision == "decline" || response.Decision == "cancel" {
		decision = "declined"
	}
	a.emitUserInputResolution(threadID, response.RequestID, decision, response.Answers)
	return nil
}

func (a *App) emitApprovalResolution(threadID, requestID, decision string, updatedInput json.RawMessage) {
	metaFields := map[string]any{
		"requestId": requestID,
		"decision":  decision,
	}
	if len(updatedInput) > 0 {
		metaFields["updatedInput"] = updatedInput
	}
	meta, _ := json.Marshal(metaFields)
	evt := provider.ProviderEvent{
		Kind:      provider.EventApprovalResolved,
		ThreadID:  threadID,
		ItemID:    requestID,
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
			RequestID: requestID,
			Decision:  decision,
		})
	}
}

func (a *App) emitApprovalFailure(threadID, requestID string, err error) {
	a.emit("provider:approval", provider.ApprovalEvent{
		Action:    "fail",
		ThreadID:  threadID,
		RequestID: requestID,
		Decision:  "failed",
		Detail:    err.Error(),
	})
}

func (a *App) emitUserInputResolution(threadID, requestID, decision string, answers map[string]provider.UserInputAnswer) {
	meta, _ := json.Marshal(map[string]any{
		"requestId": requestID,
		"decision":  decision,
		"answers":   answers,
	})
	evt := provider.ProviderEvent{
		Kind:      provider.EventUserInputResolved,
		ThreadID:  threadID,
		ItemID:    requestID,
		Meta:      meta,
		Timestamp: time.Now(),
	}
	if a.triage != nil {
		if err := a.triage.Handle(evt); err != nil {
			log.Printf("respond to user input triage update failed: %v", err)
		}
	} else {
		a.emit("provider:user_input", provider.UserInputEvent{
			Action:    "resolve",
			ThreadID:  threadID,
			RequestID: requestID,
			Decision:  decision,
		})
	}
}

func (a *App) emitUserInputFailure(threadID, requestID string, err error) {
	a.emit("provider:user_input", provider.UserInputEvent{
		Action:    "fail",
		ThreadID:  threadID,
		RequestID: requestID,
		Decision:  "failed",
		Detail:    err.Error(),
	})
}
