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
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
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
//
// What this keeps true: a tracker write's approval is answered only
// through its own open prompt. RespondToApproval chooses the path by the
// kind of the prompt triage holds open under that thread, request ID and
// occurrence (triage.PromptClaim.Kind), never by the ID's text, and the
// waiter is keyed by thread and request ID, so an answer naming another
// thread finds no waiter. The person sees the whole of every value they
// approve (trackertools.Approval). Approved writes are admitted only
// while the thread is still writable: the tools read its mode again once
// the approval returns (Writable).

// trackerApprovalWait is how long a tracker write waits for the person.
const trackerApprovalWait = 15 * time.Minute

// trackerApprovalPrefix begins the request ID of a tracker write's
// approval and the ID of a tracker write's timeline row; both end with the
// call's ID. It names them only: what answers an approval is its prompt's
// kind.
const trackerApprovalPrefix = "tracker:"

// The refusals a tracker write's approval ends with.
const (
	codeTrackerDenied   = "tracker_write_denied"
	codeTrackerUnseen   = "tracker_write_unanswered"
	codeTrackerReadOnly = "tracker_read_only"
	// codeTrackerChangesOff is a write from a thread whose project's
	// Agents settings turn off Change tickets.
	codeTrackerChangesOff = "tracker_changes_off"
)

// trackerApprovals are the tracker writes waiting on the person, by
// thread and request ID.
type trackerApprovals struct {
	mu      sync.Mutex
	waiting map[[2]string]chan bool
	// wait, when set, replaces trackerApprovalWait (tests).
	wait time.Duration
}

// open registers the waiter of thread's id.
func (t *trackerApprovals) open(threadID, id string) chan bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.waiting == nil {
		t.waiting = map[[2]string]chan bool{}
	}
	ch := make(chan bool, 1)
	t.waiting[[2]string{threadID, id}] = ch
	return ch
}

// settle settles the waiter of thread's id with approved, reporting
// whether this was its one settlement.
func (t *trackerApprovals) settle(threadID, id string, approved bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := [2]string{threadID, id}
	ch, ok := t.waiting[k]
	if !ok {
		return false
	}
	delete(t.waiting, k)
	ch <- approved
	return true
}

// waitText is a wait as the refusal says it: "15 minutes", or "2
// seconds" for a wait under a minute (a harness boot's).
func waitText(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d seconds", int(d/time.Second))
	}
	return fmt.Sprintf("%d minutes", int(d/time.Minute))
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

// WritePolicy refuses a read-only thread, and a thread whose project's
// Agents settings turn off Change tickets or cannot be read
// (trackerapp.AgentRules); otherwise it says whether a change to several
// tickets waits for the person, as those settings say. Every writing tool
// reads it, at its start and again just before it sends.
func (h trackerToolsHost) WritePolicy(threadID string) (trackertools.WritePolicy, error) {
	thread, err := h.a.store.GetThread(threadID)
	if err != nil {
		return trackertools.WritePolicy{}, fmt.Errorf("tracker tools: read thread %s: %w", threadID, err)
	}
	if provider.RuntimeMode(thread.RuntimeMode) == provider.RuntimeReadOnly {
		return trackertools.WritePolicy{}, errorsx.Public(codeTrackerReadOnly, "This thread is read-only, so it cannot change tickets or log time; ask the user to change its access.", nil)
	}
	rules, err := h.agentRules(thread)
	if err != nil {
		return trackertools.WritePolicy{}, err
	}
	if !rules.Changes() {
		return trackertools.WritePolicy{}, errorsx.Public(codeTrackerChangesOff, "This project's Agents settings do not let its threads change tickets or log time, so nothing was sent; ask the user to turn on Change tickets in Settings > Issues > Agents if this change is wanted.", nil)
	}
	return trackertools.WritePolicy{AskSeveral: rules.Asks()}, nil
}

// agentRules are the Agents settings of thread's project; a thread of no
// project has the built-in ones. Rules this build cannot read refuse the
// write: applying the built-in ones could send what the person turned off.
func (h trackerToolsHost) agentRules(thread store.Thread) (trackerapp.AgentRules, error) {
	if thread.ProjectID == "" {
		return trackerapp.AgentRules{}, nil
	}
	svc, err := h.a.trackerService()
	if err != nil {
		return trackerapp.AgentRules{}, err
	}
	rules, err := svc.ProjectAgentRules(thread.ProjectID)
	var unreadable *trackerapp.SetupFileError
	switch {
	case err == nil:
		return rules, nil
	case errors.As(err, &unreadable):
		return trackerapp.AgentRules{}, errorsx.Public("tracker_setup_unreadable", "Nothing was sent: this project's tracker setup cannot be read from "+unreadable.Path+" ("+unreadable.Reason+"), so what its Agents settings allow is not known; ask the user to fix or remove the file.", err)
	case errors.Is(err, trackerapp.ErrSetupNewer):
		return trackerapp.AgentRules{}, errorsx.Public("tracker_setup_newer", "Nothing was sent: a newer version of Agent Overflow saved this project's Agents settings, so this version cannot tell what they allow; ask the user to update the app.", err)
	}
	return trackerapp.AgentRules{}, fmt.Errorf("tracker tools: the Agents settings of project %s: %w", thread.ProjectID, err)
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
		RequestID: id, ThreadID: threadID, Kind: triage.ApprovalKindTrackerWrite, ToolName: approval.Tool,
		Title: approval.Title, Input: input, AvailableDecisions: &decisions,
	})
	if err != nil {
		return fmt.Errorf("tracker tools: encode the approval: %w", err)
	}
	waiter := a.trackerMCP.approvals.open(threadID, id)
	if err := a.triage.HandleSynthetic(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: threadID, ItemID: id, Meta: meta, Timestamp: time.Now(), SessionToken: sess.Token,
	}); err != nil {
		a.trackerMCP.approvals.settle(threadID, id, false)
		return fmt.Errorf("tracker tools: raise the approval: %w", err)
	}
	if !a.trackerApprovalOpen(threadID, id) && a.trackerMCP.approvals.settle(threadID, id, false) {
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
		ended = errorsx.Public(codeTrackerUnseen, fmt.Sprintf("The user did not answer within %s; nothing was sent.", waitText(a.trackerMCP.approvals.waitFor())), nil)
	case <-ctx.Done():
		ended = ctx.Err()
	}
	if !a.trackerMCP.approvals.settle(threadID, id, false) {
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
// approval, claim the claim of its open prompt: it settles the waiting
// call, and no provider is written.
func (a *App) respondToTrackerWrite(threadID string, claim triage.PromptClaim, response provider.ApprovalResponse) error {
	decision := provider.NormalizeApprovalDecision(response)
	if decision != "approved" && decision != "declined" {
		a.releasePrompt(claim)
		return fmt.Errorf("approval %s: a tracker write is approved or denied, not %q", response.RequestID, response.Decision)
	}
	if !a.trackerMCP.approvals.settle(threadID, response.RequestID, decision == "approved") {
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

// Drafted writes the row of a time_draft call that changed the draft,
// once: it names the call, whose entries a card reads live
// (ThreadTimeEntries), and is never rewritten.
func (h trackerToolsHost) Drafted(threadID string, row trackertools.TimeRow) {
	a := h.a
	if a.triage == nil {
		return
	}
	meta, err := json.Marshal(struct {
		Kind string `json:"kind"`
		trackertools.TimeRow
	}{Kind: "time_draft", TimeRow: row})
	if err != nil {
		log.Printf("tracker tools: encode the time draft row of %s: %v", threadID, err)
		return
	}
	if err := a.triage.HandleSynthetic(provider.ProviderEvent{
		Kind: provider.EventNotification, ThreadID: threadID, ItemID: trackerApprovalPrefix + row.ID, Content: "Drafted time for " + trackerapp.DayRange(row.Days),
		Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		// The draft stands and its answer reaches the agent; the row the
		// store refused is shown as unsaved (triage's report).
		log.Printf("tracker tools: the time draft row of %s: %v", threadID, err)
	}
}

// Posted writes the row of a time_draft post that ran, once, with its
// title fixed as the post left it (trackertools.TimePostRow.Title); it is
// never rewritten.
func (h trackerToolsHost) Posted(threadID string, row trackertools.TimePostRow) {
	a := h.a
	if a.triage == nil {
		return
	}
	meta, err := json.Marshal(struct {
		Kind string `json:"kind"`
		trackertools.TimePostRow
	}{Kind: "time_post", TimePostRow: row})
	if err != nil {
		log.Printf("tracker tools: encode the time post row of %s: %v", threadID, err)
		return
	}
	if err := a.triage.HandleSynthetic(provider.ProviderEvent{
		Kind: provider.EventNotification, ThreadID: threadID, ItemID: trackerApprovalPrefix + row.ID, Content: row.Title(),
		Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		// The post stands and its answer reaches the agent; the row the
		// store refused is shown as unsaved (triage's report).
		log.Printf("tracker tools: the time post row of %s: %v", threadID, err)
	}
}

func (h trackerToolsHost) CreateBoards(threadID, project string) ([]trackertools.CreateBoard, error) {
	a := h.a
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, fmt.Errorf("tracker tools: read thread %s: %w", threadID, err)
	}
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	choices, err := svc.BoardChoices()
	if err != nil {
		return nil, err
	}
	var out []trackertools.CreateBoard
	for _, c := range choices {
		if strings.EqualFold(c.TrackerProject, project) {
			out = append(out, trackertools.CreateBoard{Connection: c.Connection, Board: c.Board, Thread: c.Project == thread.ProjectID})
		}
	}
	return out, nil
}

func (h trackerToolsHost) Live(threadID, session string) bool {
	return h.a.trackerMCPServer().HoldsSession(threadID, session)
}

var _ trackertools.Host = trackerToolsHost{}
