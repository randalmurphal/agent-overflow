package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/transport"
)

// classifyInteractiveResponseError tags a provider-level "this request is no
// longer open" as already-handled at the RPC boundary.
//
// The provider keeps its own request table and refuses a stale id itself, so
// this condition predates several clients — but the client-side filter that
// recognized it matched on the error TEXT, and the transport redacts method
// error text for anything that is not the loopback caller. That filter has
// therefore never worked over the network: a remote client answering a prompt
// the CLI had already cancelled saw an error banner where the desktop saw
// nothing. The typed code crosses the wire intact, so both behave the same.
//
// The wrap keeps the original error, so callers testing for
// provider.ErrStaleInteractiveRequest are unaffected. The failure event is
// still emitted exactly as before — this changes the RPC's classification,
// not the thread's state.
func classifyInteractiveResponseError(err error) error {
	if err == nil || !errors.Is(err, provider.ErrStaleInteractiveRequest) {
		return err
	}
	return fmt.Errorf("%w: %w", transport.ErrAlreadyHandled, err)
}

// releaseInteractiveClaim hands a claimed prompt back when the answer never
// reached the provider. Every failure path between the claim and a successful
// write calls it; missing one wedges the prompt open with nobody able to
// answer it.
func (a *App) releaseInteractiveClaim(threadID, requestID string) {
	if a.triage == nil {
		return
	}
	a.triage.ReleaseInteractiveResponse(threadID, requestID)
}

// ListPendingInteractiveRequests returns live approval and structured-input
// prompts for a thread so a pane that missed the original event can hydrate
// its composer controls.
//
//ao:scope approvals:respond
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
//
// Several clients render the same prompt, so two of them can answer it. The
// router arbitrates: the loser gets transport.ErrAlreadyHandled and no
// failure event, because nothing failed — the question was answered without
// them, which is the state they wanted. Forwarding both answers would send
// the provider a second response for a request it has already resolved.
//
//ao:scope approvals:respond
func (a *App) RespondToApproval(threadID string, response provider.ApprovalResponse) error {
	if a.triage != nil && !a.triage.ClaimApprovalResponse(threadID, response.RequestID) {
		return fmt.Errorf("approval %s: %w", response.RequestID, transport.ErrAlreadyHandled)
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		err := fmt.Errorf("no active session for thread %s", threadID)
		a.releaseInteractiveClaim(threadID, response.RequestID)
		a.emitApprovalFailure(threadID, response.RequestID, err)
		return err
	}

	providerSess := sess.ProviderSession()
	if providerSess == nil {
		err := fmt.Errorf("session has no provider")
		a.releaseInteractiveClaim(threadID, response.RequestID)
		a.emitApprovalFailure(threadID, response.RequestID, err)
		return err
	}
	// Approval responses write to provider stdin; stamp activity
	// before the write so the idle reaper can't tear down a session
	// the user just answered a permission prompt on. The follow-up
	// EventApprovalResolved bumps again via sessionEventHandler — this
	// guards the window before that lands.
	sess.Liveness.BumpActivity(time.Now())
	if err := providerSess.RespondToApproval(context.Background(), response); err != nil {
		// The write never reached the provider, so the prompt is still
		// open. Give the claim back or no client could ever answer it.
		a.releaseInteractiveClaim(threadID, response.RequestID)
		a.emitApprovalFailure(threadID, response.RequestID, err)
		return classifyInteractiveResponseError(err)
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
//
// Arbitrated the same way as RespondToApproval: a form two screens can both
// submit is answered once, and the second submitter is told it arrived second
// rather than handed a failure.
//
//ao:scope approvals:respond
func (a *App) RespondToUserInput(threadID string, response provider.UserInputResponse) error {
	if a.triage != nil && !a.triage.ClaimUserInputResponse(threadID, response.RequestID) {
		return fmt.Errorf("user input %s: %w", response.RequestID, transport.ErrAlreadyHandled)
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		err := fmt.Errorf("no active session for thread %s", threadID)
		a.releaseInteractiveClaim(threadID, response.RequestID)
		a.emitUserInputFailure(threadID, response.RequestID, err)
		return err
	}

	providerSess := sess.ProviderSession()
	if providerSess == nil {
		err := fmt.Errorf("session has no provider")
		a.releaseInteractiveClaim(threadID, response.RequestID)
		a.emitUserInputFailure(threadID, response.RequestID, err)
		return err
	}
	// Same rationale as RespondToApproval: bump before stdin write so
	// the reaper doesn't kill a session mid-structured-input round-trip.
	sess.Liveness.BumpActivity(time.Now())
	if err := providerSess.RespondToUserInput(context.Background(), response); err != nil {
		// The form is still open; hand the claim back (see RespondToApproval).
		a.releaseInteractiveClaim(threadID, response.RequestID)
		a.emitUserInputFailure(threadID, response.RequestID, err)
		return classifyInteractiveResponseError(err)
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
	// Handle (not HandleSynthetic) is deliberate here and in
	// emitUserInputResolution: both are reachable only with a live
	// registered session (the bindings error out at sessionManager().get
	// first), and a live session implies the stopped marker was cleared
	// at start.
	if a.triage != nil {
		if err := a.triage.Handle(evt); err != nil {
			log.Printf("respond to approval triage update failed: %v", err)
		}
	} else {
		a.emit(eventchan.ProviderApproval, provider.ApprovalEvent{
			Action:    "resolve",
			ThreadID:  threadID,
			RequestID: requestID,
			Decision:  decision,
		})
	}
}

func (a *App) emitApprovalFailure(threadID, requestID string, err error) {
	a.emit(eventchan.ProviderApproval, provider.ApprovalEvent{
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
		a.emit(eventchan.ProviderUserInput, provider.UserInputEvent{
			Action:    "resolve",
			ThreadID:  threadID,
			RequestID: requestID,
			Decision:  decision,
		})
	}
}

func (a *App) emitUserInputFailure(threadID, requestID string, err error) {
	a.emit(eventchan.ProviderUserInput, provider.UserInputEvent{
		Action:    "fail",
		ThreadID:  threadID,
		RequestID: requestID,
		Decision:  "failed",
		Detail:    err.Error(),
	})
}
