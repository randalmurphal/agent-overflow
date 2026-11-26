package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/trackertools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// The app's answers to the tracker tools about the calling thread
// (trackertools.Host): whether it may write, the person's approval of a
// write naming several tickets, the write's timeline row, and the board a
// create goes through.
//
// The approval is a prompt in triage's pending approvals, of kind
// tracker-write (triage.ApprovalKindTrackerWrite), which triage never
// routes to a spawner: the person answers it, in the thread's composer on
// any screen, and a worker with no sidebar row reaches them through its
// spawner's Agents row as any prompt routed to nobody does. The waiter
// here is its one outcome: settled once, by the first of the person's
// answer (RespondToApproval), trackerApprovalWait, and the call's end.
// An answer that loses is refused as already handled; a wait that ends
// first withdraws the prompt from every screen. Nothing is persisted: an
// app that stops ends the call, and nothing was sent.

// trackerApprovalWait is how long a tracker write waits for the person.
const trackerApprovalWait = 15 * time.Minute

// trackerApprovalPrefix begins the request ID of a tracker write's
// approval, which RespondToApproval answers here and not to a provider,
// and the ID of a tracker write's timeline row; both end with the call's
// ID.
const trackerApprovalPrefix = "tracker:"

// The refusals a tracker write's approval ends with.
const (
	codeTrackerDenied   = "tracker_write_denied"
	codeTrackerUnseen   = "tracker_write_unanswered"
	codeTrackerReadOnly = "tracker_read_only"
)

// trackerApprovals are the tracker writes waiting on the person, by
// request ID.
type trackerApprovals struct {
	mu      sync.Mutex
	waiting map[string]chan bool
	// wait, when set, replaces trackerApprovalWait (tests).
	wait time.Duration
}

// open registers id's waiter.
func (t *trackerApprovals) open(id string) chan bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.waiting == nil {
		t.waiting = map[string]chan bool{}
	}
	ch := make(chan bool, 1)
	t.waiting[id] = ch
	return ch
}

// settle settles id's waiter with approved, reporting whether this was
// its one settlement.
func (t *trackerApprovals) settle(id string, approved bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ch, ok := t.waiting[id]
	if !ok {
		return false
	}
	delete(t.waiting, id)
	ch <- approved
	return true
}

func (t *trackerApprovals) waitFor() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.wait > 0 {
		return t.wait
	}
	return trackerApprovalWait
}

type trackerToolsHost struct{ a *App }

func (h trackerToolsHost) Writable(threadID string) error {
	thread, err := h.a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("tracker tools: read thread %s: %w", threadID, err)
	}
	if provider.RuntimeMode(thread.RuntimeMode) == provider.RuntimeReadOnly {
		return errorsx.Public(codeTrackerReadOnly, "This thread is read-only, so it cannot change tickets; ask the user to change its access.", nil)
	}
	return nil
}

func (h trackerToolsHost) Approve(ctx context.Context, threadID string, approval trackertools.Approval) error {
	a := h.a
	if a.triage == nil {
		return errors.New("tracker tools: no router to raise the approval")
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return errorsx.Public("tracker_session_inactive", "The agent session is no longer active. Resume the conversation before using the tracker tools.", nil)
	}
	id := trackerApprovalPrefix + approval.ID
	input, err := json.Marshal(approval)
	if err != nil {
		return fmt.Errorf("tracker tools: encode the approval: %w", err)
	}
	// The person approves or declines; nothing else (a session-wide
	// allow, a cancel of the turn) applies to a tracker write.
	decisions := []json.RawMessage{json.RawMessage(`"accept"`), json.RawMessage(`"decline"`)}
	meta, err := json.Marshal(provider.ApprovalRequest{
		RequestID: id, ThreadID: threadID, Kind: triage.ApprovalKindTrackerWrite, ToolName: "ticket_update",
		Title: fmt.Sprintf("Change %d tickets", len(approval.Tickets)), Input: input, AvailableDecisions: &decisions,
	})
	if err != nil {
		return fmt.Errorf("tracker tools: encode the approval: %w", err)
	}
	waiter := a.trackerMCP.approvals.open(id)
	if err := a.triage.HandleSynthetic(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: threadID, ItemID: id, Meta: meta, Timestamp: time.Now(), SessionToken: sess.Token,
	}); err != nil {
		a.trackerMCP.approvals.settle(id, false)
		return fmt.Errorf("tracker tools: raise the approval: %w", err)
	}
	if !a.trackerApprovalOpen(threadID, id) && a.trackerMCP.approvals.settle(id, false) {
		// Triage keeps no prompt of a session that no longer holds the
		// thread: nobody could answer this one.
		return errorsx.Public("tracker_session_inactive", "The agent session is no longer active. Resume the conversation before using the tracker tools.", nil)
	}
	timer := time.NewTimer(a.trackerMCP.approvals.waitFor())
	defer timer.Stop()
	var ended error
	select {
	case approved := <-waiter:
		if approved {
			return nil
		}
		return errorsx.Public(codeTrackerDenied, "The user denied this change; nothing was sent.", nil)
	case <-timer.C:
		ended = errorsx.Public(codeTrackerUnseen, fmt.Sprintf("The user did not answer within %d minutes; nothing was sent.", int(a.trackerMCP.approvals.waitFor()/time.Minute)), nil)
	case <-ctx.Done():
		ended = ctx.Err()
	}
	if !a.trackerMCP.approvals.settle(id, false) {
		// The person answered as the wait ended: their answer stands.
		if <-waiter {
			return nil
		}
		return errorsx.Public(codeTrackerDenied, "The user denied this change; nothing was sent.", nil)
	}
	// Withdrawn from every screen: no answer can reach it now.
	a.resolveTrackerApproval(threadID, id, 0, "expired")
	return ended
}

// trackerApprovalOpen reports whether the tracker write's prompt id is
// open on thread, for a screen to answer.
func (a *App) trackerApprovalOpen(threadID, id string) bool {
	return slices.ContainsFunc(a.triage.PendingInteractiveRequests(threadID).Approvals, func(r provider.ApprovalRequest) bool { return r.RequestID == id })
}

// resolveTrackerApproval takes the tracker write's prompt id of thread out
// of the pending approvals and tells every screen, with decision.
func (a *App) resolveTrackerApproval(threadID, id string, occurrence provider.PromptOccurrence, decision string) {
	meta, err := json.Marshal(map[string]any{"requestId": id, "decision": decision})
	if err != nil {
		log.Printf("tracker tools: encode the resolution of %s: %v", id, err)
		return
	}
	if err := a.triage.HandleSynthetic(provider.ProviderEvent{
		Kind: provider.EventApprovalResolved, ThreadID: threadID, ItemID: id, Meta: meta, Timestamp: time.Now(), PromptOccurrence: occurrence,
	}); err != nil {
		log.Printf("tracker tools: resolve the approval %s of %s: %v", id, threadID, err)
	}
}

// respondToTrackerWrite is the person's answer to a tracker write's
// approval: it settles the waiting call, and no provider is written.
func (a *App) respondToTrackerWrite(threadID string, response provider.ApprovalResponse) error {
	claim, claimed := a.claimPrompt(threadID, response.RequestID, response.Occurrence)
	if !claimed {
		return fmt.Errorf("approval %s: %w", response.RequestID, transport.ErrAlreadyHandled)
	}
	decision := provider.NormalizeApprovalDecision(response)
	if decision != "approved" && decision != "declined" {
		a.releasePrompt(claim)
		return fmt.Errorf("approval %s: a tracker write is approved or denied, not %q", response.RequestID, response.Decision)
	}
	if !a.trackerMCP.approvals.settle(response.RequestID, decision == "approved") {
		// The call ended first, and withdraws the prompt.
		a.releasePrompt(claim)
		return fmt.Errorf("approval %s: %w", response.RequestID, transport.ErrAlreadyHandled)
	}
	a.resolveTrackerApproval(threadID, response.RequestID, claim.Ref.Occurrence, decision)
	return nil
}

func (h trackerToolsHost) Wrote(threadID string, row trackertools.WriteRow) {
	a := h.a
	if a.triage == nil {
		return
	}
	meta, err := json.Marshal(struct {
		Kind string `json:"kind"`
		trackertools.WriteRow
	}{Kind: "tracker_write", WriteRow: row})
	if err != nil {
		log.Printf("tracker tools: encode the timeline row of %s: %v", threadID, err)
		return
	}
	title := fmt.Sprintf("Changed %d tickets", len(row.Tickets))
	switch {
	case row.Created:
		title = "Created " + row.Tickets[0].Key
	case len(row.Tickets) == 1:
		title = "Changed " + row.Tickets[0].Key
	}
	if err := a.triage.HandleSynthetic(provider.ProviderEvent{
		Kind: provider.EventNotification, ThreadID: threadID, ItemID: trackerApprovalPrefix + row.ID, Content: title, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		// The write stands and its answer reaches the agent; the row the
		// store refused is shown as unsaved (triage's report).
		log.Printf("tracker tools: the timeline row of %s: %v", threadID, err)
	}
}

func (h trackerToolsHost) CreateBoard(threadID, connection, project string) (string, bool, error) {
	a := h.a
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", false, fmt.Errorf("tracker tools: read thread %s: %w", threadID, err)
	}
	svc, err := a.trackerService()
	if err != nil {
		return "", false, err
	}
	choices, err := svc.BoardChoices()
	if err != nil {
		return "", false, err
	}
	other := ""
	for _, c := range choices {
		if c.Connection != connection || !strings.EqualFold(c.TrackerProject, project) {
			continue
		}
		if c.Project == thread.ProjectID {
			return c.Board, true, nil
		}
		if other == "" {
			other = c.Board
		}
	}
	return other, other != "", nil
}

var _ trackertools.Host = trackerToolsHost{}
