package main

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"

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
