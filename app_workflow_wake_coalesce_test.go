package main

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
)

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
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

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
	h.app.sessions[thread.ID] = session{provider: string(provider.Claude), token: "live"}
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
// resume` of a retries-exhausted run produces exactly this: it continues the
// same attempt, so every field of the signature matches.
//
// The claim written at hand-off is what the action spends, and the promotion is
// a compare-and-set against it, so a spent claim can never become a record.
func TestWorkflowWakeQueuedRecordIsSpentByAnActionBeforeItDispatches(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-overtaken")
	h.app.sessions[thread.ID] = session{provider: string(provider.Claude), token: "live"}
	item := h.boundRun(t, "overtaken-run", thread.ID, engine.StateNeedsHuman, engine.ReasonRetriesExhausted)
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
// clearTreeWakeSignature skips unbound roots (it fires on every phase advance of
// every run in the app), so the one place that unbinds has to spend the record.
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
