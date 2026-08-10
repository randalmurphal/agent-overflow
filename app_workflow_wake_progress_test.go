package main

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// The app half of a `notify:` route (K1): the engine says a decorated gate was
// taken, and the run's bound thread hears about it without anything parking.

func (h *wakeHarness) notify(event engine.NotifyEvent) {
	h.app.afterWorkflowEngineEvent("workflow:gate-notify", event)
	h.drain()
}

func waveNotify(itemID string, attempt int) engine.NotifyEvent {
	return engine.NotifyEvent{
		ItemID: itemID, ProjectID: defaultTestProjectID,
		PhaseID: "wave", Attempt: attempt,
		Decision: string(def.DecisionLoop), Target: "wave",
	}
}

func TestWorkflowGateNotifyWakesTheBoundThreadWithoutParking(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-progress")
	item := h.boundRun(t, "progress-run", thread.ID, engine.StateRunning, "")
	if err := h.app.store.UpdateWorkItemWorkspace(item.ID, "/work/progress", "ao/progress", "main"); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{"status": "done", "outputs": map[string]any{
		"verdict": "pass", "landed": "D40-D50",
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.phase(t, item.ID, "wave", 12, "completed", "phase-thread", encoded)

	h.notify(waveNotify(item.ID, 12))

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("progress wakes = %d, want one", len(sends))
	}
	for _, want := range []string{
		`Run "progress-run" (workflow "build") is running`,
		`This run finished phase "wave" (attempt 12) and continued: the gate chose "loop" to phase "wave".`,
		`Workspace: "/work/progress" on branch "ao/progress".`,
		`- "verdict": "pass"`,
		`- "landed": "D40-D50"`,
		"Nothing is waiting on a reply — the run is still going.",
	} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("progress wake missing %q:\n%s", want, sends[0])
		}
	}
	// The run is untouched: a progress wake reports, it never parks.
	stored, err := h.app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(engine.StateRunning) || stored.Reason != "" {
		t.Fatalf("progress wake moved the run: state=%q reason=%q", stored.State, stored.Reason)
	}
}

// Every wave of a loop is its own news. Coalescing must separate them, or the
// mechanism built to report progress would report the first lap and go quiet.
func TestWorkflowGateNotifyCoalescesPerTraversalNotPerRoute(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-waves")
	item := h.boundRun(t, "waves-run", thread.ID, engine.StateRunning, "")
	for attempt := 1; attempt <= 3; attempt++ {
		h.phase(t, item.ID, "wave", attempt, "completed", "phase-thread",
			json.RawMessage(`{"status":"done","outputs":{"verdict":"pass"}}`))
	}

	h.notify(waveNotify(item.ID, 1))
	h.notify(waveNotify(item.ID, 2))
	// The same traversal announced twice — a replay, not a wave — is one ask.
	h.notify(waveNotify(item.ID, 2))
	h.notify(waveNotify(item.ID, 3))

	sends, _, _, _ := h.snapshot()
	if len(sends) != 3 {
		t.Fatalf("progress wakes = %d, want one per traversal:\n%s", len(sends), strings.Join(sends, "\n---\n"))
	}
	for index, attempt := range []int{1, 2, 3} {
		if !strings.Contains(sends[index], phraseForAttempt(attempt)) {
			t.Fatalf("wake %d does not report attempt %d:\n%s", index, attempt, sends[index])
		}
	}
}

func phraseForAttempt(attempt int) string {
	return `phase "wave" (attempt ` + string(rune('0'+attempt)) + `)`
}

// A called run's notify surfaces as the ROOT's progress wake, naming the
// descendant — the same rule a descendant's park follows, for the same reason:
// only a root binds a thread.
func TestWorkflowGateNotifyFromACalledRunSurfacesAtTheRoot(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-campaign-progress")
	root := h.boundRun(t, "campaign-root", thread.ID, engine.StateRunning, "")
	child := store.WorkItem{
		ID: "campaign-wave-2", ProjectID: defaultTestProjectID, Goal: "wave 2", WorkflowID: "wave",
		WorkflowScope: "shared", State: string(engine.StateRunning), Source: engine.WorkItemSourceCall,
		ParentItemID: root.ID, ParentPhaseID: "advance", ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	}
	if err := h.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	h.phase(t, child.ID, "review", 1, "completed", "wave-thread",
		json.RawMessage(`{"status":"done","outputs":{"verdict":"pass"}}`))

	h.notify(engine.NotifyEvent{
		ItemID: child.ID, ProjectID: child.ProjectID, PhaseID: "review", Attempt: 1,
		Decision: string(def.DecisionAdvance), Target: "fix",
	})

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("progress wakes = %d, want one at the root", len(sends))
	}
	for _, want := range []string{
		`Run "campaign-root" (workflow "build") is running`,
		`A called run one level down (run "campaign-wave-2", workflow "wave") finished phase "review" (attempt 1) and continued: the gate chose "advance" to phase "fix".`,
		`Call chain: "campaign-root" → "campaign-wave-2".`,
		`- "verdict": "pass"`,
	} {
		if !strings.Contains(sends[0], want) {
			t.Fatalf("root progress wake missing %q:\n%s", want, sends[0])
		}
	}
}

// An unbound run has no thread to wake, and progress is not an interruption:
// notify is simply inert there rather than reaching for the OS notification a
// run that needs a human gets.
func TestWorkflowGateNotifyIsInertForAnUnboundRun(t *testing.T) {
	h := newWakeHarness(t)
	item := h.run(t, "unbound-progress", engine.StateRunning, "")
	h.phase(t, item.ID, "wave", 1, "completed", "phase-thread",
		json.RawMessage(`{"status":"done","outputs":{"verdict":"pass"}}`))

	h.notify(waveNotify(item.ID, 1))

	sends, queued, events, _ := h.snapshot()
	if len(sends) != 0 || len(queued) != 0 {
		t.Fatalf("an unbound run was woken: sends=%d queued=%d", len(sends), len(queued))
	}
	for _, name := range events {
		if strings.HasPrefix(name, "notification:") {
			t.Fatalf("progress rang the desktop: %s", name)
		}
	}
}

// A progress wake never delays or fails the run it reports on: a record it
// cannot read costs the outputs, not the message.
func TestWorkflowGateNotifyStillReportsWhenTheAttemptIsUnreadable(t *testing.T) {
	h := newWakeHarness(t)
	thread := h.chatThread(t, "origin-unreadable")
	item := h.boundRun(t, "unreadable-run", thread.ID, engine.StateRunning, "")
	// No phase row at all: the notify names an attempt this store has never
	// seen, which is what a pruned or racing record looks like.
	h.notify(waveNotify(item.ID, 7))

	sends, _, _, _ := h.snapshot()
	if len(sends) != 1 {
		t.Fatalf("progress wakes = %d, want the traversal reported anyway", len(sends))
	}
	if strings.Contains(sends[0], "What it produced") {
		t.Fatalf("wake invented outputs for an attempt it could not read:\n%s", sends[0])
	}
	if !strings.Contains(sends[0], `finished phase "wave" (attempt 7)`) {
		t.Fatalf("wake dropped the fact it did have:\n%s", sends[0])
	}
}
