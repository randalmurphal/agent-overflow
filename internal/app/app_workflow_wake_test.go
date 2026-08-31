package app

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/untrustedtext"
	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
	"agent-overflow/internal/workflow/wake"
	"agent-overflow/internal/workflowapp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type workflowEmitter struct {
	app  *App
	emit func(eventchan.Channel, any)
}

func (e workflowEmitter) Emit(name eventchan.Channel, payload any) {
	if e.app == nil {
		e.emit(name, payload)
		return
	}
	e.app.workflowApplication().HandleEngineEvent(name, payload, e.emit)
}

func (a *App) afterWorkflowEngineEvent(name eventchan.Channel, payload any) {
	a.workflowApplication().AfterEngineEvent(name, payload)
}

func (a *App) afterWorkflowStateEvent(event engine.StateEvent) {
	a.workflowApplication().AfterStateEvent(event)
}

func (a *App) workflowAncestry(item store.WorkItem) ([]store.WorkItem, error) {
	return a.workflowApplication().Ancestry(item)
}

func (a *App) reclaimWorkflowUsageAttention() []store.WorkflowProviderUsageAttentionRecovery {
	return a.workflowApplication().ReclaimUsageAttention()
}

func (a *App) surfaceReclaimedWorkflowUsageAttention(recoveries []store.WorkflowProviderUsageAttentionRecovery) {
	a.workflowApplication().SurfaceReclaimedUsageAttention(recoveries)
}

func (a *App) workflowUsageAttentionForRest(root, parked store.WorkItem) workflowapp.UsageAttentionDecision {
	return a.workflowApplication().UsageAttentionForRest(root, parked)
}

const (
	wakeQueuedPrefix       = workflowapp.WakeQueuedPrefix
	maxFailedUnitNoteRunes = workflowapp.MaxFailedUnitNoteRunes
)

func (a *App) sweepWorkflowUsageAttention() {
	a.surfaceReclaimedWorkflowUsageAttention(a.reclaimWorkflowUsageAttention())
}

// wakeHarness wires the two seams a wake crosses — the ordinary send and the
// flush queue — so a test can assert which one a delivery took without a
// provider process.
type wakeHarness struct {
	app   *App
	mu    sync.Mutex
	sends []string
	// queued holds the messages that reached the flush queue's dispatcher and
	// STOPPED there — the harness stands in for a dispatch worker that has not
	// run yet, which is the state a crash or a session teardown freezes. Nothing
	// is delivered until dispatchQueued runs their callbacks.
	queued      []string
	queuedItems []triage.QueuedFlushItem
	events      []string
	errorTexts  []string
}

func newWakeHarness(t *testing.T) *wakeHarness {
	t.Helper()
	app, _ := setupE2EApp(t)
	h := &wakeHarness{app: app}
	app.sendMessageFn = func(threadID, content string, _ []string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.sends = append(h.sends, content)
		return nil
	}
	app.triage.SetFlushDispatcher(func(_ string, items []triage.QueuedFlushItem) {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, item := range items {
			h.queued = append(h.queued, item.Message)
			h.queuedItems = append(h.queuedItems, item)
		}
	})
	app.testEmitHook = func(name string, data any) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, name)
		if event, ok := data.(engine.ErrorEvent); ok {
			h.errorTexts = append(h.errorTexts, event.Error)
		}
	}
	app.configDir = t.TempDir()
	return h
}

func (h *wakeHarness) snapshot() (sends, queued, events, errorTexts []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sends...), append([]string(nil), h.queued...),
		append([]string(nil), h.events...), append([]string(nil), h.errorTexts...)
}

// drain waits for the app's wake worker, which runs off the engine's emit
// goroutine and therefore is not finished when the transition returns.
func (h *wakeHarness) drain() { h.app.workflowApplication().WaitWake() }

// dispatchQueued completes the queued messages the way the real dispatch worker
// does once it has written them to the provider: it runs each item's
// durability settlement (dispatchFlushWithGeneration's success path). Until this
// runs, a queued wake has been composed and handed off but not delivered.
func (h *wakeHarness) dispatchQueued() {
	h.mu.Lock()
	pending := h.queuedItems
	h.queuedItems = nil
	h.mu.Unlock()
	for _, item := range pending {
		item.Settlement.Settle()
	}
}

func (h *wakeHarness) chatThread(t *testing.T, id string) store.Thread {
	t.Helper()
	thread := store.Thread{
		ID: id, ProjectID: defaultTestProjectID, ProjectPath: "/tmp/project",
		Title: id, Provider: string(provider.Claude), Model: "sonnet",
		Mode: threadmode.ModeChat, CreatedAt: 1, UpdatedAt: 1,
	}
	if err := h.app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	return thread
}

func (h *wakeHarness) run(t *testing.T, id string, state engine.State, reason engine.Reason) store.WorkItem {
	t.Helper()
	item := store.WorkItem{
		ID: id, ProjectID: defaultTestProjectID, Goal: "Ship " + id,
		WorkflowID: "build", WorkflowScope: "shared", State: string(state),
		Reason: string(reason), Source: "manual",
		CreatedAt: time.Now().UnixMilli(), StartedAt: time.Now().UnixMilli(),
	}
	if err := h.app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	return item
}

func (h *wakeHarness) phase(t *testing.T, itemID, phaseID string, attempt int, status, threadID string, envelope json.RawMessage) {
	t.Helper()
	if err := h.app.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: itemID, PhaseID: phaseID, Attempt: attempt, ThreadID: threadID,
		InputEnvelope: json.RawMessage(`{}`), OutputEnvelope: envelope,
		Status: status, StartedAt: 10, EndedAt: 11,
	}); err != nil {
		t.Fatal(err)
	}
}

// parkedPhase is the shape an ENGINE-diagnosed park leaves: no thread, no
// envelope — nothing ran a turn — and the cause as the whole account.
func (h *wakeHarness) parkedPhase(t *testing.T, itemID, phaseID string, attempt int, cause string) {
	t.Helper()
	h.phase(t, itemID, phaseID, attempt, "running", "", nil)
	if err := h.app.store.CompleteWorkItemPhase(itemID, phaseID, attempt, nil, nil, "parked", cause, 0, 11); err != nil {
		t.Fatal(err)
	}
}

func (h *wakeHarness) usageLimitedPhase(t *testing.T, itemID string, scopeID store.WorkflowProviderUsageScopeID) {
	t.Helper()
	h.phase(t, itemID, "work", 1, "running", "", nil)
	if err := h.app.store.CompleteWorkItemPhase(
		itemID, "work", 1, nil, nil, "parked", "provider usage limit reached", scopeID, 11,
	); err != nil {
		t.Fatal(err)
	}
}

func (h *wakeHarness) usageScope(t *testing.T, providerName, accountID string, generation uint64) store.WorkflowProviderUsageScopeID {
	t.Helper()
	id, err := h.app.store.OpenWorkflowProviderUsageScope(providerName, accountID, generation, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWorkflowWakeDeliversToAnIdleBoundThread(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-thread")
	item := h.run(t, "wake-done", engine.StateDone, "")
	h.phase(t, item.ID, "verify", 1, "completed", "phase-thread",
		json.RawMessage(`{"status":"done","outputs":{"ok":true}}`))
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	// A draft the user has typed but not sent must survive a background wake.
	if err := h.app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID: thread.ID, Content: "half-written thought", UpdatedAt: 5,
	}); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	})
	h.drain()

	sends, queued, _, _ := h.snapshot()
	if len(sends) != 1 || len(queued) != 0 {
		t.Fatalf("idle delivery took sends=%d queued=%d, want one ordinary send", len(sends), len(queued))
	}
	for _, want := range []string{`Run "wake-done"`, "is done", `Phase "verify"`, "nothing is waiting on a reply"} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("wake missing %q:\n%s", want, sends[0])
		}
	}
	draft, found, err := h.app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || draft.Content != "half-written thought" {
		t.Fatalf("wake destroyed the composer draft: found=%v draft=%+v", found, draft)
	}
}

func TestWorkflowWakeQueuesIntoABusyBoundThread(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-busy")
	h.app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "live"})
	item := h.run(t, "wake-question", engine.StateNeedsHuman, engine.ReasonQuestion)
	h.phase(t, item.ID, "plan", 1, "parked", "phase-thread",
		json.RawMessage(`{"status":"question","question":"Which base branch?"}`))
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()

	sends, queued, _, _ := h.snapshot()
	if len(queued) != 1 || len(sends) != 0 {
		t.Fatalf("live-session delivery took sends=%d queued=%d, want the queued path", len(sends), len(queued))
	}
	for _, want := range []string{"needs-human (question)", "Which base branch?", "does not continue until this is resolved"} {
		if !strings.Contains(queued[0], want) {
			t.Fatalf("queued wake missing %q:\n%s", want, queued[0])
		}
	}
}

func TestWorkflowUsageLimitStormNotifiesOncePerAccountAndWatcher(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-usage")
	scope := h.usageScope(t, string(provider.Claude), "acct-a", 4)
	park := func(id string, scopeID store.WorkflowProviderUsageScopeID) store.WorkItem {
		item := h.run(t, id, engine.StateNeedsHuman, engine.ReasonProviderUsageLimited)
		if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
			t.Fatal(err)
		}
		h.usageLimitedPhase(t, item.ID, scopeID)
		h.app.afterWorkflowStateEvent(engine.StateEvent{
			ItemID: item.ID, ProjectID: item.ProjectID,
			From: engine.StateRunning, To: engine.StateNeedsHuman,
		})
		h.drain()
		return item
	}

	first := park("usage-a", scope)
	park("usage-b", scope)
	if sends, _, _, _ := h.snapshot(); len(sends) != 1 {
		t.Fatalf("two same-account parks produced %d messages, want one", len(sends))
	}

	// An explicit recovery action re-arms attention but does not check or clear
	// provider availability. A failure arriving after that action is actionable
	// new work and gets one new message.
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: first.ID, ProjectID: first.ProjectID,
		From: engine.StateNeedsHuman, To: engine.StateRunning,
	})
	h.drain()
	park("usage-late", scope)
	if sends, _, _, _ := h.snapshot(); len(sends) != 2 {
		t.Fatalf("late post-resume park produced %d total messages, want two", len(sends))
	}

	// Account identity is part of the correlation key. A different account's
	// refusal cannot be hidden behind the first account's notification.
	other := h.usageScope(t, string(provider.Claude), "acct-b", 5)
	park("usage-other-account", other)
	if sends, _, _, _ := h.snapshot(); len(sends) != 3 {
		t.Fatalf("different-account park produced %d total messages, want three", len(sends))
	}
}

func TestWorkflowUsageAttentionQueuedResumeRaceCannotSilenceLateFailure(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-usage-busy")
	h.app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "live"})
	scope := h.usageScope(t, string(provider.Claude), "acct", 9)
	park := func(id string) store.WorkItem {
		item := h.run(t, id, engine.StateNeedsHuman, engine.ReasonProviderUsageLimited)
		if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
			t.Fatal(err)
		}
		h.usageLimitedPhase(t, item.ID, scope)
		h.app.afterWorkflowStateEvent(engine.StateEvent{
			ItemID: item.ID, ProjectID: item.ProjectID,
			From: engine.StateRunning, To: engine.StateNeedsHuman,
		})
		h.drain()
		return item
	}

	first := park("queued-usage-a")
	park("queued-usage-b")
	if _, queued, _, _ := h.snapshot(); len(queued) != 1 {
		t.Fatalf("same outage queued %d messages before delivery, want one", len(queued))
	}

	// Resume lands while the first alert is still waiting behind the watcher’s
	// active turn. Rearming invalidates that claim before a later long-running
	// phase reports the same limit.
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: first.ID, ProjectID: first.ProjectID,
		From: engine.StateNeedsHuman, To: engine.StateRunning,
	})
	h.drain()
	park("queued-usage-late")
	if _, queued, _, _ := h.snapshot(); len(queued) != 2 {
		t.Fatalf("late failure queued %d total messages, want a new generation", len(queued))
	}

	// The stale callback runs first but cannot promote across the resume. The
	// second callback records the current generation, which suppresses another
	// same-outage park until the next action.
	h.dispatchQueued()
	park("queued-usage-after-delivery")
	if _, queued, _, _ := h.snapshot(); len(queued) != 2 {
		t.Fatalf("delivered current generation failed to suppress: queued=%d", len(queued))
	}
}

func TestWorkflowUsageAttentionBootSweepRedeliversAnUnsettledClaim(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-usage-restart")
	scope := h.usageScope(t, string(provider.Codex), "acct", 12)
	makePark := func(id string) store.WorkItem {
		item := h.run(t, id, engine.StateNeedsHuman, engine.ReasonProviderUsageLimited)
		if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
			t.Fatal(err)
		}
		h.usageLimitedPhase(t, item.ID, scope)
		return item
	}
	stranded := makePark("usage-before-restart")
	later := makePark("usage-after-restart")

	if _, claimed, err := h.app.store.ClaimWorkflowProviderUsageAttention(
		scope, thread.ID, stranded.ID, "lost-process-token", time.Now().UnixMilli(),
	); err != nil {
		t.Fatal(err)
	} else if !claimed {
		t.Fatal("initial delivery claim was unexpectedly suppressed")
	}

	// The in-memory delivery disappeared with the old process. Startup clears
	// that reservation and re-surfaces its source run; failing closed here would
	// leave every same-scope park permanently quiet.
	h.app.sweepWorkflowUsageAttention()
	h.drain()
	if sends, _, _, _ := h.snapshot(); len(sends) != 1 || !strings.Contains(sends[0], stranded.ID) {
		t.Fatalf("boot sweep sends = %v, want one redelivery for %s", sends, stranded.ID)
	}

	// Redelivery settles the current generation, so another run that reports
	// the same provider-account outage stays coalesced until an explicit action.
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: later.ID, ProjectID: later.ProjectID,
		From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()
	if sends, _, _, _ := h.snapshot(); len(sends) != 1 {
		t.Fatalf("same outage after boot redelivery produced %d messages, want one", len(sends))
	}
}

func TestWorkflowUsageAttentionBootSweepReselectsAnAffectedRunWhenSourceResolved(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-usage-reselect")
	scope := h.usageScope(t, string(provider.Codex), "acct", 16)
	makePark := func(id string) store.WorkItem {
		item := h.run(t, id, engine.StateNeedsHuman, engine.ReasonProviderUsageLimited)
		if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
			t.Fatal(err)
		}
		h.usageLimitedPhase(t, item.ID, scope)
		return item
	}
	original := makePark("usage-recovery-original")
	mixed := h.run(t, "usage-recovery-a-mixed", engine.StateNeedsHuman, engine.ReasonUnitFailed)
	if err := h.app.store.UpdateWorkItemOriginThread(mixed.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.phase(t, mixed.ID, "fan", 1, "parked", "", nil)
	if err := h.app.store.CreateWorkItemUnits([]store.WorkItemUnit{
		{ItemID: mixed.ID, PhaseID: "fan", Attempt: 1, UnitID: "limited", UnitIndex: 0,
			Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed, ProviderUsageScopeID: scope},
		{ItemID: mixed.ID, PhaseID: "fan", Attempt: 1, UnitID: "agent-failure", UnitIndex: 1,
			Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed},
	}); err != nil {
		t.Fatal(err)
	}
	stillParked := makePark("usage-recovery-z-still-parked")
	if _, claimed, err := h.app.store.ClaimWorkflowProviderUsageAttention(
		scope, thread.ID, original.ID, "lost-source-token", time.Now().UnixMilli(),
	); err != nil || !claimed {
		t.Fatalf("initial claim claimed=%v err=%v", claimed, err)
	}
	if _, claimed, err := h.app.store.ClaimWorkflowProviderUsageAttention(
		scope, thread.ID, stillParked.ID, "suppressed-token", time.Now().UnixMilli(),
	); err != nil || claimed {
		t.Fatalf("second run was not suppressed by the original claim: claimed=%v err=%v", claimed, err)
	}
	if err := h.app.store.UpdateWorkItemState(
		original.ID, string(engine.StateCancelled), "", time.Now().UnixMilli(),
	); err != nil {
		t.Fatal(err)
	}

	h.app.sweepWorkflowUsageAttention()
	h.drain()
	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 || !strings.Contains(sends[0], stillParked.ID) ||
		strings.Contains(sends[0], original.ID) || strings.Contains(sends[0], mixed.ID) {
		t.Fatalf("boot recovery sends = %v, want the still-parked run %s", sends, stillParked.ID)
	}
}

func TestWorkflowUsageAttentionComposerRecoverySettlesTheBootClaim(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-usage-draft")
	h.app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "live"})
	scope := h.usageScope(t, string(provider.Claude), "acct", 13)
	item := h.run(t, "usage-restored-to-draft", engine.StateNeedsHuman, engine.ReasonProviderUsageLimited)
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.usageLimitedPhase(t, item.ID, scope)

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID,
		From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()
	h.mu.Lock()
	if len(h.queuedItems) != 1 {
		h.mu.Unlock()
		t.Fatalf("usage wake queue items = %d, want 1", len(h.queuedItems))
	}
	queued := h.queuedItems[0]
	h.mu.Unlock()

	// The harness dispatcher took ownership of the in-memory item. Put that
	// exact item back at the session-death boundary so the production recovery
	// path persists its text and settles both durable workflow records.
	h.app.triage.RegisterQueueItem(thread.ID, queued)
	h.app.restoreUnconfirmedQueueOnSessionDeath(thread.ID)
	draft, found, err := h.app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !strings.Contains(draft.Content, item.ID) {
		t.Fatalf("composer recovery did not persist the wake: found=%v draft=%+v", found, draft)
	}
	signature, err := h.app.store.WorkItemWakeSignature(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if signature == "" || strings.HasPrefix(signature, wakeQueuedPrefix) {
		t.Fatalf("composer recovery left wake signature pending: %q", signature)
	}

	h.app.sweepWorkflowUsageAttention()
	h.drain()
	if _, queuedMessages, _, _ := h.snapshot(); len(queuedMessages) != 1 {
		t.Fatalf("boot sweep redelivered a composer-restored alert: queued=%d, want the original one only", len(queuedMessages))
	}
}

func TestWorkflowUsageAttentionBootSweepResurfacesTheParkedDescendant(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-usage-descendant-restart")
	scope := h.usageScope(t, string(provider.Codex), "acct", 14)
	root := h.run(t, "usage-root-running", engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemOriginThread(root.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	root.OriginThreadID = thread.ID
	child := store.WorkItem{
		ID: "usage-child-parked", ProjectID: defaultTestProjectID, Goal: "child",
		WorkflowID: "child", WorkflowScope: "shared", State: string(engine.StateNeedsHuman),
		Reason: string(engine.ReasonProviderUsageLimited), Source: "call",
		ParentItemID: root.ID, ParentPhaseID: "call-child", ParentAttempt: 1,
		CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	h.usageLimitedPhase(t, child.ID, scope)
	usage := h.app.workflowUsageAttentionForRest(root, child)
	if usage.Suppress || usage.Claim == nil {
		t.Fatalf("descendant attention decision = %+v", usage)
	}

	h.app.sweepWorkflowUsageAttention()
	h.drain()
	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 || !strings.Contains(sends[0], `called run "usage-child-parked"`) {
		t.Fatalf("boot recovery did not surface the parked descendant:\n%v", sends)
	}
}

func TestWorkflowCalledChildBirthDoesNotRearmProviderUsageAttention(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-usage-child-birth")
	scope := h.usageScope(t, string(provider.Claude), "acct", 15)
	park := func(id string) {
		item := h.run(t, id, engine.StateNeedsHuman, engine.ReasonProviderUsageLimited)
		if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
			t.Fatal(err)
		}
		h.usageLimitedPhase(t, item.ID, scope)
		h.app.afterWorkflowStateEvent(engine.StateEvent{
			ItemID: item.ID, ProjectID: item.ProjectID,
			From: engine.StateRunning, To: engine.StateNeedsHuman,
		})
		h.drain()
	}
	park("usage-before-child")

	root := h.run(t, "running-caller", engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemOriginThread(root.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	child := store.WorkItem{
		ID: "automatic-called-child", ProjectID: defaultTestProjectID, Goal: "child",
		WorkflowID: "child", WorkflowScope: "shared", State: string(engine.StateRunning),
		Source: "call", ParentItemID: root.ID, ParentPhaseID: "call-child",
		ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: child.ID, ProjectID: child.ProjectID, From: "", To: engine.StateRunning,
	})
	h.drain()
	park("usage-after-child")
	if sends, _, _, _ := h.snapshot(); len(sends) != 1 {
		t.Fatalf("automatic child birth rearmed the unresolved outage: sends=%d, want 1", len(sends))
	}

	// A top-level birth is an explicit new start and remains a re-arm boundary.
	started := h.run(t, "explicit-root-start", engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemOriginThread(started.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: started.ID, ProjectID: started.ProjectID, From: "", To: engine.StateRunning,
	})
	h.drain()
	park("usage-after-root-start")
	if sends, _, _, _ := h.snapshot(); len(sends) != 2 {
		t.Fatalf("explicit root start did not rearm attention: sends=%d, want 2", len(sends))
	}
}

func TestWorkflowMixedFanOutFailureIsNeverHiddenByUsageCoalescing(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-mixed")
	scope := h.usageScope(t, string(provider.Codex), "acct", 3)

	usageOnly := h.run(t, "fan-usage-only", engine.StateNeedsHuman, engine.ReasonUnitFailed)
	mixed := h.run(t, "fan-mixed", engine.StateNeedsHuman, engine.ReasonUnitFailed)
	for _, item := range []*store.WorkItem{&usageOnly, &mixed} {
		if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
			t.Fatal(err)
		}
		h.phase(t, item.ID, "fan", 1, "parked", "", nil)
	}
	if err := h.app.store.CreateWorkItemUnits([]store.WorkItemUnit{
		{ItemID: usageOnly.ID, PhaseID: "fan", Attempt: 1, UnitID: "limited", UnitIndex: 0,
			Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed, ProviderUsageScopeID: scope},
		{ItemID: mixed.ID, PhaseID: "fan", Attempt: 1, UnitID: "limited", UnitIndex: 0,
			Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed, ProviderUsageScopeID: scope},
		{ItemID: mixed.ID, PhaseID: "fan", Attempt: 1, UnitID: "real-failure", UnitIndex: 1,
			Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed},
	}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []store.WorkItem{usageOnly, mixed} {
		h.app.afterWorkflowStateEvent(engine.StateEvent{
			ItemID: item.ID, ProjectID: item.ProjectID,
			From: engine.StateRunning, To: engine.StateNeedsHuman,
		})
		h.drain()
	}
	if sends, _, _, _ := h.snapshot(); len(sends) != 2 {
		t.Fatalf("usage-only plus mixed failure produced %d messages, want both surfaced", len(sends))
	}
}

func TestWorkflowWakeFallsBackWhenTheBoundThreadIsGone(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-deleted")
	item := h.run(t, "wake-orphan", engine.StateFailed, engine.ReasonAgentError)
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.app.store.DeleteThread(thread.ID); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateFailed,
	})
	h.drain()

	sends, queued, _, errorTexts := h.snapshot()
	if len(sends) != 0 || len(queued) != 0 {
		t.Fatalf("a deleted thread still received a wake: sends=%d queued=%d", len(sends), len(queued))
	}
	// The fallback is loud: the binding is cleared and the run says so.
	stored, err := h.app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OriginThreadID != "" {
		t.Fatalf("stale binding survived: %q", stored.OriginThreadID)
	}
	if len(errorTexts) == 0 || !strings.Contains(errorTexts[0], "bound thread is gone") {
		t.Fatalf("error events = %v, want the fallback reported", errorTexts)
	}
}

func TestWorkflowWakeIgnoresUnboundAndCalledRuns(t *testing.T) {
	h := newWakeHarness(t)
	unbound := h.run(t, "wake-unbound", engine.StateDone, "")
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: unbound.ID, ProjectID: unbound.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	})
	h.drain()
	if sends, queued, _, _ := h.snapshot(); len(sends) != 0 || len(queued) != 0 {
		t.Fatalf("an unbound run woke a thread: sends=%d queued=%d", len(sends), len(queued))
	}

	// A called run finishing is the caller's business, not a wake of its own.
	child := store.WorkItem{
		ID: "wake-child", ProjectID: defaultTestProjectID, Goal: "called", WorkflowID: "child",
		WorkflowScope: "shared", State: string(engine.StateDone), Source: "call",
		ParentItemID: unbound.ID, ParentPhaseID: "audit", ParentAttempt: 1, CallDepth: 1,
		CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: child.ID, ProjectID: child.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	})
	h.drain()
	if sends, queued, _, _ := h.snapshot(); len(sends) != 0 || len(queued) != 0 {
		t.Fatalf("a called run woke a thread as itself: sends=%d queued=%d", len(sends), len(queued))
	}
}

// A grandchild parking while the root waits is announced at the ROOT — the run
// a human watches and the run that carries the binding.
func TestWorkflowDescendantParkSurfacesAtTheRoot(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-root")
	root := h.run(t, "tree-root", engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemOriginThread(root.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	child := store.WorkItem{
		ID: "tree-child", ProjectID: defaultTestProjectID, Goal: "middle", WorkflowID: "mid",
		WorkflowScope: "shared", State: string(engine.StateRunning), Source: "call",
		ParentItemID: root.ID, ParentPhaseID: "call-mid", ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	grandchild := store.WorkItem{
		ID: "tree-grandchild", ProjectID: defaultTestProjectID, Goal: "deep", WorkflowID: "leaf",
		WorkflowScope: "shared", State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonQuestion),
		Source: "call", ParentItemID: child.ID, ParentPhaseID: "call-leaf", ParentAttempt: 1,
		CallDepth: 2, CreatedAt: 3,
	}
	if err := h.app.store.CreateWorkItem(grandchild); err != nil {
		t.Fatal(err)
	}
	h.phase(t, grandchild.ID, "ask", 1, "parked", "leaf-thread",
		json.RawMessage(`{"status":"question","question":"Which environment?"}`))

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: grandchild.ID, ProjectID: grandchild.ProjectID,
		From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("descendant park produced %d wakes, want exactly one at the root", len(sends))
	}
	for _, want := range []string{
		`Run "tree-root"`, "is waiting", "A called run 2 levels down parked",
		`run "tree-grandchild"`, "needs-human (question)", "Which environment?",
		`cannot continue until called run "tree-grandchild" is resolved`,
	} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("root wake missing %q:\n%s", want, sends[0])
		}
	}
}

// Once the root itself rests, its own transition is the surface; announcing the
// descendant again would be a duplicate.
func TestWorkflowDescendantParkIsSilentOnceTheRootRests(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-settled")
	root := h.run(t, "settled-root", engine.StateNeedsHuman, engine.ReasonChildFailed)
	if err := h.app.store.UpdateWorkItemOriginThread(root.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	child := store.WorkItem{
		ID: "settled-child", ProjectID: defaultTestProjectID, Goal: "called", WorkflowID: "child",
		WorkflowScope: "shared", State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonStuck),
		Source: "call", ParentItemID: root.ID, ParentPhaseID: "audit", ParentAttempt: 1,
		CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: child.ID, ProjectID: child.ProjectID, From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()
	if sends, queued, _, _ := h.snapshot(); len(sends) != 0 || len(queued) != 0 {
		t.Fatalf("descendant park duplicated the root's own surface: sends=%d queued=%d", len(sends), len(queued))
	}
}

// A descendant park is only actionable if the wake carries what the repair verb
// takes: the parked run's id, the waves between it and the root, and the failed
// units of THAT run rather than of the root.
func TestWorkflowDescendantParkCarriesTheChainAndItsOwnFailedUnits(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-campaign")
	root := h.run(t, "campaign-root", engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemOriginThread(root.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	// The root has a failed unit of its own from an earlier wave. It must not be
	// reported as the parked run's, or a repair verb would be pointed at the
	// wrong run.
	if err := h.app.store.CreateWorkItemUnits([]store.WorkItemUnit{{
		ItemID: root.ID, PhaseID: "fan", Attempt: 1, UnitID: "root-unit", UnitIndex: 0,
		Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed, ThreadID: "root-unit-thread",
		Provider: string(provider.Claude), Model: "sonnet",
	}}); err != nil {
		t.Fatal(err)
	}
	parent := root
	for wave := 1; wave <= 2; wave++ {
		child := store.WorkItem{
			ID:            fmt.Sprintf("campaign-wave-%d", wave),
			ProjectID:     defaultTestProjectID,
			Goal:          fmt.Sprintf("wave %d", wave),
			WorkflowID:    "wave",
			WorkflowScope: "shared",
			State:         string(engine.StateRunning),
			Source:        engine.WorkItemSourceCall,
			ParentItemID:  parent.ID,
			ParentPhaseID: "advance",
			ParentAttempt: 1,
			CallDepth:     wave,
			CreatedAt:     int64(wave + 1),
		}
		if err := h.app.store.CreateWorkItem(child); err != nil {
			t.Fatal(err)
		}
		parent = child
	}
	parked := parent
	if err := h.app.store.UpdateWorkItemState(
		parked.ID, string(engine.StateNeedsHuman), string(engine.ReasonUnitFailed), 20,
	); err != nil {
		t.Fatal(err)
	}
	parked.State = string(engine.StateNeedsHuman)
	parked.Reason = string(engine.ReasonUnitFailed)
	h.phase(t, parked.ID, "fan", 1, "parked", "wave-thread",
		json.RawMessage(`{"status":"stuck","reason":"two units failed"}`))
	if err := h.app.store.CreateWorkItemUnits([]store.WorkItemUnit{{
		ItemID: parked.ID, PhaseID: "fan", Attempt: 1, UnitID: "pkg-engine", UnitIndex: 0,
		Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed, ThreadID: "wave-unit-thread",
		Provider: string(provider.Claude), Model: "sonnet",
	}}); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: parked.ID, ProjectID: parked.ProjectID,
		From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("descendant park produced %d wakes, want one", len(sends))
	}
	for _, want := range []string{
		`Call chain: "campaign-root" → "campaign-wave-1" → "campaign-wave-2".`,
		`- "called run": "campaign-wave-2"`,
		`- "called run failed unit": "pkg-engine (thread wave-unit-thread)"`,
		`act on run "campaign-wave-2", not on "campaign-root"`,
	} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("campaign wake missing %q:\n%s", want, sends[0])
		}
	}
	if strings.Contains(sends[0], "root-unit") {
		t.Fatalf("campaign wake reported the root's failed unit as the parked run's:\n%s", sends[0])
	}
}

// The one park that is not a fault: the stop a human asked for. It must not read
// like every other one, at either level of the tree.
func TestWorkflowCheckpointParkReadsAsTheStopThatWasAskedFor(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-checkpoint")
	root := h.run(t, "checkpoint-root", engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemOriginThread(root.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	child := store.WorkItem{
		ID: "checkpoint-wave", ProjectID: defaultTestProjectID, Goal: "wave 2", WorkflowID: "wave",
		WorkflowScope: "shared", State: string(engine.StateNeedsHuman),
		Reason: string(engine.ReasonCheckpoint), Source: engine.WorkItemSourceCall,
		ParentItemID: root.ID, ParentPhaseID: "advance", ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: child.ID, ProjectID: child.ProjectID,
		From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("checkpoint park produced %d wakes, want one", len(sends))
	}
	for _, want := range []string{
		"needs-human (checkpoint)",
		`run "checkpoint-wave" reached the checkpoint and did not start the next one`,
		// The literal command, against the DESCENDANT's id: the run to act on is
		// one the reader has never seen, and "resume" is one of four control
		// verbs it could otherwise be mapped onto.
		"`agent-overflow run resume \"checkpoint-wave\"` takes the call it skipped, or leave it parked.",
	} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("checkpoint wake missing %q:\n%s", want, sends[0])
		}
	}
	if strings.Contains(sends[0], "cannot continue until called run") {
		t.Fatalf("checkpoint wake read as a failure:\n%s", sends[0])
	}
}

func TestWorkflowWakeCarriesNarrativeArtifactAndFailedUnitReferences(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-refs")
	item := h.run(t, "wake-refs", engine.StateNeedsHuman, engine.ReasonUnitFailed)
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.phase(t, item.ID, "fan", 1, "parked", "join-thread", json.RawMessage(`{"status":"stuck","reason":"a unit failed"}`))
	if err := h.app.store.CreateWorkItemUnits([]store.WorkItemUnit{{
		ItemID: item.ID, PhaseID: "fan", Attempt: 1, UnitID: "pkg-store", UnitIndex: 0,
		Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed,
		ThreadID: "unit-thread", Provider: string(provider.Claude), Model: "sonnet",
	}}); err != nil {
		t.Fatal(err)
	}
	narrative, err := workflowrunner.NarrativePath(h.app.workflowDataRoot(), item.ID, "fan", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(narrative), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(narrative, []byte("what happened"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("wakes = %d, want one", len(sends))
	}
	for _, want := range []string{
		`- "narrative":`, `- "phase thread": "join-thread"`,
		`- "failed unit": "pkg-store (thread unit-thread)"`, "a unit failed",
	} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("wake missing %q:\n%s", want, sends[0])
		}
	}
	// References are pointers, never content.
	if strings.Contains(sends[0], "what happened") {
		t.Fatalf("wake inlined narrative content:\n%s", sends[0])
	}
}

// A pause takes its in-flight units down `failed` with an interrupted note —
// there is no interrupted unit status, and `failed` is exactly what the repair
// verbs recover — so the STATUS alone tells an operator who paused a healthy run
// that their own units failed. The note is what keeps the report truthful, and
// it never displaces the id or the thread a repair verb takes.
func TestWorkflowWakeSaysWhyEachFailedUnitRests(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-paused")
	item := h.run(t, "wake-paused", engine.StateNeedsHuman, engine.ReasonPaused)
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.phase(t, item.ID, "fan", 1, "parked", "join-thread", nil)
	// What the engine's `interruptedUnitNote` writes when a pause tears an
	// attempt down: the phase attempt's status, not the run's reason.
	const interrupted = "interrupted with its phase attempt (parked)"
	// A note far past the reference budget still leaves the id and the thread
	// whole, because those are what `run retry-unit` and a thread read take.
	long := "unit outcome error: " + strings.Repeat("stack frame; ", 200)
	if err := h.app.store.CreateWorkItemUnits([]store.WorkItemUnit{
		{ItemID: item.ID, PhaseID: "fan", Attempt: 1, UnitID: "adjudicate", UnitIndex: 0,
			Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed,
			ThreadID: "adjudicate-thread", Provider: string(provider.Claude), Model: "sonnet",
			Feedback: interrupted},
		{ItemID: item.ID, PhaseID: "fan", Attempt: 1, UnitID: "codex-lens", UnitIndex: 1,
			Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed,
			ThreadID: "codex-lens-thread", Provider: string(provider.Claude), Model: "sonnet",
			Feedback: long},
	}); err != nil {
		t.Fatal(err)
	}

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("wakes = %d, want one", len(sends))
	}
	want := `- "failed unit": ` + untrustedtext.Field(
		"adjudicate (thread adjudicate-thread): "+interrupted)
	if !strings.Contains(sends[0], want) {
		t.Fatalf("wake missing %q:\n%s", want, sends[0])
	}
	// The rendered form carries the truncation marker, so a match on it is proof
	// the note was cut rather than passed through whole.
	bounded := untrustedtext.Field("codex-lens (thread codex-lens-thread): " +
		untrustedtext.Truncate(long, maxFailedUnitNoteRunes))
	if !strings.Contains(sends[0], bounded) {
		t.Fatalf("wake did not bound a long unit note:\n%s", sends[0])
	}
}

// A reference is a pointer an agent opens, so one that does not resolve is worse
// than none: the agent spends a tool call learning that. An attempt with no
// narrative on disk therefore carries no narrative reference — while everything
// else it does have still does.
func TestWorkflowWakeOmitsAMissingNarrative(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-missing")
	item := h.run(t, "wake-missing", engine.StateNeedsHuman, engine.ReasonStuck)
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.phase(t, item.ID, "survey", 1, "parked", "phase-thread",
		json.RawMessage(`{"status":"stuck","reason":"nothing to read"}`))

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("wakes = %d, want one", len(sends))
	}
	if strings.Contains(sends[0], `- "narrative":`) {
		t.Fatalf("wake pointed at a narrative nothing wrote:\n%s", sends[0])
	}
	if !strings.Contains(sends[0], `- "phase thread": "phase-thread"`) {
		t.Fatalf("wake dropped the references it does have:\n%s", sends[0])
	}

	// Written, the same run's wake carries it — the omission is about the file,
	// not about this shape of run.
	narrative, err := workflowrunner.NarrativePath(h.app.workflowDataRoot(), item.ID, "survey", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(narrative), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(narrative, []byte("what happened"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The run is resumed before it parks the same way again. Without that the
	// second park is the same ask the thread was already told (K2) and is
	// suppressed — which is the coalescing rule, not this test's subject.
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateNeedsHuman, To: engine.StateRunning,
	})
	h.drain()
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()
	sends, _, _, _ = h.snapshot()
	if len(sends) != 2 || !strings.Contains(sends[1], `- "narrative": "`+narrative+`"`) {
		t.Fatalf("wake omitted a narrative that exists:\n%s", sends[len(sends)-1])
	}
}

// The wake is where a park reaches a supervising agent, so an engine-diagnosed
// one has to carry its diagnosis: the phase authored no envelope, and without
// the cause the message says only that a run stopped.
func TestWorkflowWakeCarriesTheEngineParkCause(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-cause")
	item := h.run(t, "wake-cause", engine.StateNeedsHuman, engine.ReasonSetupFailed)
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	const cause = `provision worktree for item "wake-cause": branch "ao/wave-3" already exists`
	h.parkedPhase(t, item.ID, "implement", 1, cause)

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning,
		To: engine.StateNeedsHuman, Reason: engine.ReasonSetupFailed,
	})
	h.drain()

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("wake sends = %d, want 1", len(sends))
	}
	if !strings.Contains(sends[0], "The engine stopped it here: ") ||
		!strings.Contains(sends[0], untrustedtext.Quote(cause, wake.MaxCauseRunes)) {
		t.Fatalf("wake does not carry the park cause:\n%s", sends[0])
	}
}

// A descendant's park is announced at the root, so the cause travelling with it
// must be the DESCENDANT's — the root ran nothing to have a cause of its own.
func TestWorkflowDescendantWakeCarriesTheDescendantsParkCause(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-cause-root")
	root := h.run(t, "cause-root", engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemOriginThread(root.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	child := store.WorkItem{
		ID: "cause-child", ProjectID: defaultTestProjectID, Goal: "wave", WorkflowID: "wave",
		WorkflowScope: "shared", State: string(engine.StateNeedsHuman),
		Reason: string(engine.ReasonSetupFailed), Source: "call",
		ParentItemID: root.ID, ParentPhaseID: "call-wave", ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	const cause = `cut worktree for item "cause-child": no space left on device`
	h.parkedPhase(t, child.ID, "implement", 1, cause)

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: child.ID, ProjectID: child.ProjectID, From: engine.StateRunning,
		To: engine.StateNeedsHuman, Reason: engine.ReasonSetupFailed,
	})
	h.drain()

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("wake sends = %d, want 1", len(sends))
	}
	if !strings.Contains(sends[0], untrustedtext.Quote(cause, wake.MaxCauseRunes)) {
		t.Fatalf("root wake does not carry the descendant's park cause:\n%s", sends[0])
	}
}

// A descendant's park is composed against the root, and the root has NOT
// finished — so the root's declared outputs have no business on that message.
// For a recursive campaign they are the previous wave's carry-forward values
// (`next-wave-number: 3`), restated on every park deep in the tree as though
// they described the run that just stopped. Same reasoning that already blanks
// the root's own park reason on a descendant wake; the descendant's real
// outputs ride the message as AttemptOutputs.
func TestWorkflowDescendantParkOmitsTheRootsDeclaredOutputs(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-carryforward")
	root := h.run(t, "campaign-root", engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemOriginThread(root.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	// A recursive campaign root: the wave that already finished produced the
	// carry-forward value its workflow declares as an output.
	snapshot := json.RawMessage(
		`{"workflow":{"id":"campaign","outputs":{"next-wave-number":{"from":"advance.next"}}}}`)
	if err := h.app.store.UpdateWorkItemRunStart(root.ID, snapshot, "", "", "", 1); err != nil {
		t.Fatal(err)
	}
	h.phase(t, root.ID, "advance", 1, "completed", "root-thread",
		json.RawMessage(`{"status":"done","outputs":{"next":3}}`))
	child := store.WorkItem{
		ID: "campaign-wave", ProjectID: defaultTestProjectID, Goal: "wave", WorkflowID: "wave",
		WorkflowScope: "shared", State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonQuestion),
		Source: "call", ParentItemID: root.ID, ParentPhaseID: "advance", ParentAttempt: 1,
		CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	h.phase(t, child.ID, "ask", 1, "parked", "wave-thread",
		json.RawMessage(`{"status":"question","question":"Which environment?"}`))

	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: child.ID, ProjectID: child.ProjectID,
		From: engine.StateRunning, To: engine.StateNeedsHuman,
	})
	h.drain()

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("descendant park produced %d wakes, want one", len(sends))
	}
	for _, unwanted := range []string{"\n\nOutputs:", "next-wave-number"} {
		if strings.Contains(sends[0], unwanted) {
			t.Fatalf("descendant wake carried the root's declared outputs (%q):\n%s", unwanted, sends[0])
		}
	}

	// The same outputs on the root's OWN resting wake are exactly right — this
	// is what proves the fixture declares them at all.
	if err := h.app.store.UpdateWorkItemState(root.ID, string(engine.StateDone), "", 20); err != nil {
		t.Fatal(err)
	}
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: root.ID, ProjectID: root.ProjectID,
		From: engine.StateRunning, To: engine.StateDone,
	})
	h.drain()

	sends, _, _, _ = h.snapshot()
	if len(sends) != 2 {
		t.Fatalf("root resting produced %d wakes in total, want 2", len(sends))
	}
	for _, want := range []string{"\n\nOutputs:", `- "next-wave-number": "3"`} {
		if !strings.Contains(sends[1], want) {
			t.Fatalf("the root's own resting wake dropped its declared outputs (%q):\n%s", want, sends[1])
		}
	}
}

// Wake coalescing (K2) and the richer resting body (K3). The two are tested
// together where they meet: what the message CARRIES must not decide whether it
// is a duplicate, so a run whose record grew between two identical parks is
// still one ask.

// park drives one resting transition of a run into its bound thread.
func (h *wakeHarness) park(itemID, projectID string, to engine.State) {
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: itemID, ProjectID: projectID, From: engine.StateRunning, To: to,
	})
	h.drain()
}

// act is the intervening human/agent action every repair verb performs: the run
// goes back to running.
func (h *wakeHarness) act(itemID, projectID string, from engine.State) {
	h.app.afterWorkflowStateEvent(engine.StateEvent{
		ItemID: itemID, ProjectID: projectID, From: from, To: engine.StateRunning,
	})
	h.drain()
}

func (h *wakeHarness) boundRun(t *testing.T, id, threadID string, state engine.State, reason engine.Reason) store.WorkItem {
	t.Helper()
	item := h.run(t, id, state, reason)
	if err := h.app.store.UpdateWorkItemOriginThread(item.ID, threadID); err != nil {
		t.Fatal(err)
	}
	item.OriginThreadID = threadID
	return item
}

func TestWorkflowWakeSuppressesTheSameAskAndSaysSo(t *testing.T) {
	logs := testutil.CaptureLogOutput(t)

	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-coalesce")
	item := h.boundRun(t, "coalesce-run", thread.ID, engine.StateNeedsHuman, engine.ReasonQuestion)
	h.phase(t, item.ID, "plan", 1, "parked", "phase-thread",
		json.RawMessage(`{"status":"question","question":"Which base branch?"}`))

	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)
	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("wakes = %d, want the second identical ask suppressed", len(sends))
	}
	// Suppression is never silent: a wake that did not arrive is otherwise
	// indistinguishable from a wake that was never composed.
	if !strings.Contains(logs.String(), "suppressed a repeat of the wake already delivered") {
		t.Fatalf("suppression left no record:\n%s", logs.String())
	}
	stored, err := h.app.store.WorkItemWakeSignature(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, `phase="plan"`) || !strings.Contains(stored, "attempt=1") {
		t.Fatalf("recorded signature does not name the coordinate it suppressed on: %q", stored)
	}
}

func TestWorkflowWakeDeliversAgainAfterSomebodyActsOnTheRun(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-acted")
	item := h.boundRun(t, "acted-run", thread.ID, engine.StateNeedsHuman, engine.ReasonQuestion)
	h.phase(t, item.ID, "plan", 1, "parked", "phase-thread",
		json.RawMessage(`{"status":"question","question":"Which base branch?"}`))

	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)
	// An answer, a resolve, a resume, a retry, a rerun: all of them return the
	// run to running, which is what spends the record.
	h.act(item.ID, item.ProjectID, engine.StateNeedsHuman)
	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)

	if sends, _, _, _ := h.snapshot(); len(sends) != 2 {
		t.Fatalf("wakes = %d, want the same question re-asked after an action", len(sends))
	}
	if signature, err := h.app.store.WorkItemWakeSignature(item.ID); err != nil || signature == "" {
		t.Fatalf("second delivery left no record: signature=%q err=%v", signature, err)
	}
}

func TestWorkflowWakeAlwaysDeliversAGenuinelyNewState(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		apply func(t *testing.T, h *wakeHarness, item store.WorkItem)
	}{
		{"a new attempt", func(t *testing.T, h *wakeHarness, item store.WorkItem) {
			h.phase(t, item.ID, "plan", 2, "parked", "phase-thread",
				json.RawMessage(`{"status":"question","question":"Which base branch?"}`))
		}},
		// `verify` rather than `implement`: the timeline orders by (started_at,
		// phase_id, attempt) and the harness stamps one start time, so the later
		// phase has to sort later for the fixture to describe a run that advanced.
		{"a different phase", func(t *testing.T, h *wakeHarness, item store.WorkItem) {
			h.phase(t, item.ID, "verify", 1, "parked", "phase-thread",
				json.RawMessage(`{"status":"question","question":"Which base branch?"}`))
		}},
		{"a different question", func(t *testing.T, h *wakeHarness, item store.WorkItem) {
			h.phase(t, item.ID, "plan", 2, "parked", "phase-thread",
				json.RawMessage(`{"status":"question","question":"Which reviewer?"}`))
		}},
		{"a different reason", func(t *testing.T, h *wakeHarness, item store.WorkItem) {
			if err := h.app.store.UpdateWorkItemState(
				item.ID, string(engine.StateNeedsHuman), string(engine.ReasonStuck), 20,
			); err != nil {
				t.Fatal(err)
			}
		}},
		{"a different engine cause", func(t *testing.T, h *wakeHarness, item store.WorkItem) {
			h.parkedPhase(t, item.ID, "plan", 2, "the worktree would not cut")
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newWakeHarness(t)
			thread := h.chatThread(t, "origin-new-state")
			item := h.boundRun(t, "new-state-run", thread.ID, engine.StateNeedsHuman, engine.ReasonQuestion)
			h.phase(t, item.ID, "plan", 1, "parked", "phase-thread",
				json.RawMessage(`{"status":"question","question":"Which base branch?"}`))
			h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)

			testCase.apply(t, h, item)
			h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)

			if sends, _, _, _ := h.snapshot(); len(sends) != 2 {
				t.Fatalf("wakes = %d, want %s to deliver", len(sends), testCase.name)
			}
		})
	}
}

// A descendant resumed and re-parked identically is a new ask at the root: the
// record the root carries is spent by an action anywhere in its tree.
func TestWorkflowWakeClearsTheRootRecordWhenADescendantIsActedOn(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-tree")
	root := h.boundRun(t, "tree-root", thread.ID, engine.StateRunning, "")
	child := store.WorkItem{
		ID: "tree-child", ProjectID: defaultTestProjectID, Goal: "wave", WorkflowID: "wave",
		WorkflowScope: "shared", State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonQuestion),
		Source: engine.WorkItemSourceCall, ParentItemID: root.ID, ParentPhaseID: "advance",
		ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	h.phase(t, child.ID, "ask", 1, "parked", "wave-thread",
		json.RawMessage(`{"status":"question","question":"Which environment?"}`))

	h.park(child.ID, child.ProjectID, engine.StateNeedsHuman)
	h.park(child.ID, child.ProjectID, engine.StateNeedsHuman)
	if sends, _, _, _ := h.snapshot(); len(sends) != 1 {
		t.Fatalf("wakes = %d, want the repeated descendant park suppressed", len(sends))
	}

	h.act(child.ID, child.ProjectID, engine.StateNeedsHuman)
	h.park(child.ID, child.ProjectID, engine.StateNeedsHuman)
	if sends, _, _, _ := h.snapshot(); len(sends) != 2 {
		t.Fatalf("wakes = %d, want the answered-and-re-asked descendant to deliver", len(sends))
	}
}

// K3: the two facts a woken agent used to have to go and fetch — where the work
// lives, and what the parked attempt produced.
func TestWorkflowWakeCarriesTheWorkspaceAndTheGateOutputs(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-rich")
	item := h.boundRun(t, "rich-run", thread.ID, engine.StateNeedsHuman, engine.ReasonGate)
	if err := h.app.store.UpdateWorkItemWorkspace(item.ID, "/work/rich-run", "ao/rich-run", "main"); err != nil {
		t.Fatal(err)
	}
	envelope := map[string]any{"status": "done", "outputs": map[string]any{
		"verdict": "reject", "severity": "high",
		"summary": "the migration drops a column two services still read",
	}}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	h.phase(t, item.ID, "review", 3, "parked", "phase-thread", encoded)

	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("wakes = %d, want one", len(sends))
	}
	for _, want := range []string{
		`Workspace: "/work/rich-run" on branch "ao/rich-run".`,
		`Phase "review" (attempt 3)`,
		`What the parked attempt produced (phase "review", attempt 3):`,
		`- "verdict": "reject"`,
		`- "severity": "high"`,
		`- "summary": "the migration drops a column two services still read"`,
	} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("wake missing %q:\n%s", want, sends[0])
		}
	}
}

// The attempt digest is bounded by the same helper `run inspect` uses, and says
// what it left out — a digest that hides its own truncation is how a reader
// concludes an output does not exist.
func TestWorkflowWakeBoundsTheGateOutputsAndNamesTheDrillDown(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-bounded")
	item := h.boundRun(t, "bounded-run", thread.ID, engine.StateNeedsHuman, engine.ReasonGate)
	outputs := make(map[string]any, maxDigestOutputs+3)
	for index := 0; index < maxDigestOutputs+3; index++ {
		outputs[string(rune('a'+index))] = strings.Repeat("z", maxDigestValueRunes*2)
	}
	encoded, err := json.Marshal(map[string]any{"status": "done", "outputs": outputs})
	if err != nil {
		t.Fatal(err)
	}
	h.phase(t, item.ID, "review", 1, "parked", "phase-thread", encoded)

	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("wakes = %d, want one", len(sends))
	}
	if !strings.Contains(sends[0], "…and 3 more (`agent-overflow run inspect \"bounded-run\" --phase \"review\" --attempt 1`).") {
		t.Fatalf("wake did not state its own overflow with the read that answers it:\n%s", sends[0])
	}
	if !strings.Contains(sends[0], "[truncated]") {
		t.Fatalf("an oversized output value rode the wake whole:\n%s", sends[0])
	}
}

// Only a gate park carries the digest. Every other park either has no decision
// to make or already carries its own account, and outputs on every message
// would put the bytes back that bounding exists to save.
func TestWorkflowWakeOmitsTheAttemptDigestOutsideAGate(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-nondigest")
	item := h.boundRun(t, "nondigest-run", thread.ID, engine.StateNeedsHuman, engine.ReasonStuck)
	encoded, err := json.Marshal(map[string]any{
		"status": "stuck", "reason": "the fixture is missing",
		"outputs": map[string]any{"verdict": "unknown"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.phase(t, item.ID, "review", 1, "parked", "phase-thread", encoded)

	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("wakes = %d, want one", len(sends))
	}
	if strings.Contains(sends[0], "What the parked attempt produced") {
		t.Fatalf("a stuck park carried a gate digest:\n%s", sends[0])
	}
}

// A wake recorded as delivered must be either durably queued or actually
// dispatched — never merely registered. The flush queue lives in process memory
// until the dispatch worker writes the message to the provider, so a record
// written at register time survives a message that a crash, a session teardown,
// or a rollback then discards. That record suppresses the identical wake
// FOREVER (the coalescing rule spends it only when somebody acts on the run), so
// the failure it opens is not a late message but a run nobody is told about
// again.
//
// The trade is the one the guidance slot makes: redeliver over lose.
func TestWorkflowWakeRecordsALiveDeliveryOnlyOnceItDispatches(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-durable")
	h.app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "live"})
	item := h.boundRun(t, "durable-run", thread.ID, engine.StateNeedsHuman, engine.ReasonQuestion)
	h.phase(t, item.ID, "plan", 1, "parked", "phase-thread",
		json.RawMessage(`{"status":"question","question":"Which base branch?"}`))

	// One park, queued and never dispatched: the message is gone with the queue,
	// so the row may hold only a CLAIM — never a delivered signature, which
	// would suppress the ask from here on.
	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)
	if _, queued, _, _ := h.snapshot(); len(queued) != 1 {
		t.Fatalf("queued wakes = %d, want 1", len(queued))
	}
	claim, err := h.app.store.WorkItemWakeSignature(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(claim, wakeQueuedPrefix) {
		t.Fatalf("a message still sitting in the queue was recorded as delivered: %q", claim)
	}

	// So the same ask composes again rather than being suppressed into silence.
	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)
	if _, queued, _, _ := h.snapshot(); len(queued) != 2 {
		t.Fatalf("queued wakes = %d, want the undelivered ask re-composed", len(queued))
	}

	// Once the worker writes them to the provider, the claim is promoted to the
	// real record — and the coalescing rule takes over from there.
	h.dispatchQueued()
	signature, err := h.app.store.WorkItemWakeSignature(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if signature == "" || strings.HasPrefix(signature, wakeQueuedPrefix) {
		t.Fatalf("a dispatched wake left no delivered record: %q", signature)
	}
	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)
	if _, queued, _, _ := h.snapshot(); len(queued) != 2 {
		t.Fatalf("queued wakes = %d, want the repeat of a DELIVERED ask suppressed", len(queued))
	}
}

// The regression the deferred record opened, and the reason the claim exists.
//
// The delivery record lands on the flush-dispatch worker; the "somebody acted,
// so the record is spent" clear lands on the app's wake queue. Nothing orders
// them. So an action taken while the message was still queued used to find
// NOTHING to spend — the record had not been written yet — and then the record
// landed behind it, suppressing the identical park from then on. A bare `run
// resume` of a provider-retries-exhausted run produces exactly this: it continues the
// same attempt, so every field of the signature matches.
//
// The claim written at hand-off is what the action spends, and the promotion is
// a compare-and-set against it, so a spent claim can never become a record.
func TestWorkflowWakeQueuedRecordIsSpentByAnActionBeforeItDispatches(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-overtaken")
	h.app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "live"})
	item := h.boundRun(t, "overtaken-run", thread.ID, engine.StateNeedsHuman, engine.ReasonProviderRetriesExhausted)
	h.phase(t, item.ID, "build", 1, "parked", "phase-thread", nil)

	// (1) The run parks; the wake is queued into a mid-turn thread and sits.
	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)
	if _, queued, _, _ := h.snapshot(); len(queued) != 1 {
		t.Fatalf("queued wakes = %d, want 1", len(queued))
	}

	// (2) Somebody acts before the thread reaches a boundary — an overlay
	// resolve, a `run resume`, an auto-resume timer: all of them land here.
	h.act(item.ID, item.ProjectID, engine.StateNeedsHuman)
	if spent, err := h.app.store.WorkItemWakeSignature(item.ID); err != nil || spent != "" {
		t.Fatalf("the action did not spend the queued wake record: %q (err %v)", spent, err)
	}

	// (3) The turn hits a boundary and the queue finally drains. The message
	// goes out, but it is answering a question that has already been acted on,
	// so it must not become the record that suppresses the next one.
	h.dispatchQueued()
	if recorded, err := h.app.store.WorkItemWakeSignature(item.ID); err != nil || recorded != "" {
		t.Fatalf("a wake overtaken by an action was recorded as delivered: %q (err %v)", recorded, err)
	}

	// (4) The run re-parks identically — same phase, same attempt, same reason,
	// so the same signature. It MUST deliver.
	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)
	if _, queued, _, _ := h.snapshot(); len(queued) != 2 {
		t.Fatalf("queued wakes = %d, want the re-park after an action to deliver", len(queued))
	}
}

// A binding cleared out from under a queued message strands its claim:
// Transition clearing skips unbound roots because it fires on every phase
// advance of every run in the app, so the one place that unbinds has to spend
// the record.
func TestWorkflowWakeStaleBindingSpendsTheRecord(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-unbound-later")
	item := h.boundRun(t, "unbound-later-run", thread.ID, engine.StateNeedsHuman, engine.ReasonQuestion)
	h.phase(t, item.ID, "plan", 1, "parked", "phase-thread",
		json.RawMessage(`{"status":"question","question":"Which base branch?"}`))

	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)
	if signature, err := h.app.store.WorkItemWakeSignature(item.ID); err != nil || signature == "" {
		t.Fatalf("the first wake left no record: %q (err %v)", signature, err)
	}

	// The thread is archived, so the next wake falls back to the unbound
	// surface — and takes the record with it.
	if err := h.app.store.ArchiveThread(thread.ID); err != nil {
		t.Fatal(err)
	}
	h.park(item.ID, item.ProjectID, engine.StateNeedsHuman)

	if signature, err := h.app.store.WorkItemWakeSignature(item.ID); err != nil || signature != "" {
		t.Fatalf("a run that lost its binding kept its wake record: %q (err %v)", signature, err)
	}
}
