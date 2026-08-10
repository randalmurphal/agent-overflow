package engine

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/workflow/def"
)

// The engine half of a progress wake (K1): a gate takes a `notify:`-decorated
// route, the run continues exactly as it would have, and one event says so.

func (f *fakeEmitter) notifyEvents(itemID string) []NotifyEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []NotifyEvent
	for _, event := range f.events {
		notify, ok := event.payload.(NotifyEvent)
		if event.name == "workflow:gate-notify" && ok && notify.ItemID == itemID {
			result = append(result, notify)
		}
	}
	return result
}

// twoPhaseNotifyWorkflow advances from `work` to `wrap` over a decorated route,
// then finishes. The second phase's route is deliberately undecorated so the
// test can tell "notify fired for the route that carried it" from "notify fires
// at every gate".
func twoPhaseNotifyWorkflow(id string) def.Workflow {
	return def.Workflow{ID: id, Phases: []def.Phase{
		agentPhase("work", nil, []def.Route{{To: "wrap", Notify: true}}),
		agentPhase("wrap", nil, []def.Route{{To: "done"}}),
	}}
}

func TestGateNotifyAnnouncesTheRouteAndTheRunKeepsGoing(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"notify": twoPhaseNotifyWorkflow("notify")}, []string{"project"}, nil)
	item := testItem("notify-run", "project", "notify", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{
		Kind: OutcomeDone, Envelope: json.RawMessage(`{"status":"done","outputs":{"ok":true},"question":null,"reason":null}`),
	})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	notifications := h.emitter.notifyEvents(item.ID)
	if len(notifications) != 1 {
		t.Fatalf("gate-notify events = %d, want exactly one", len(notifications))
	}
	notification := notifications[0]
	// The coordinate is the attempt the gate CONSUMED, not where the run has
	// got to since: the app reads that attempt's outputs to compose the message.
	if notification.PhaseID != "work" || notification.Attempt != 1 {
		t.Fatalf("gate-notify coordinate = %s/%d, want work/1", notification.PhaseID, notification.Attempt)
	}
	if notification.Decision != string(def.DecisionAdvance) || notification.Target != "wrap" {
		t.Fatalf("gate-notify decision = %s -> %s, want advance -> wrap", notification.Decision, notification.Target)
	}
	if notification.ProjectID != "project" {
		t.Fatalf("gate-notify project = %q", notification.ProjectID)
	}

	// The run continued: the next phase started, and nothing parked.
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.PhaseID != "wrap" {
		t.Fatalf("starts = %+v, want the run to have entered wrap", starts)
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")

	// Finishing over the undecorated route announces nothing more, and the run
	// still reaches done.
	h.runner.complete(t, item.ID, Outcome{
		Kind: OutcomeDone, Envelope: json.RawMessage(`{"status":"done","outputs":{"ok":true},"question":null,"reason":null}`),
	})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := len(h.emitter.notifyEvents(item.ID)); got != 1 {
		t.Fatalf("gate-notify events after an undecorated gate = %d, want still one", got)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
}

// A decorated route the run does not continue through announces nothing. Step
// mode is the case that can only happen at run time: the route is a legal
// advance, and the run parks at it anyway — where its PARK is the surface, and a
// progress wake would be the duplicate this whole packet exists to remove.
func TestGateNotifyIsSilentWhenStepModeParksTheAdvance(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"notify": twoPhaseNotifyWorkflow("notify")}, []string{"project"}, nil)
	item := testItem("stepped", "project", "notify", 0)
	item.StepMode = true
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{
		Kind: OutcomeDone, Envelope: json.RawMessage(`{"status":"done","outputs":{"ok":true},"question":null,"reason":null}`),
	})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonGate)
	if got := h.emitter.notifyEvents(item.ID); len(got) != 0 {
		t.Fatalf("gate-notify events = %+v, want none: the run parked rather than continuing", got)
	}
}
