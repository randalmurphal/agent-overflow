package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

func TestFailedFinalizeStartClearsTakeoverTransition(t *testing.T) {
	runner := newTestRunner(t, nil, nil, nil)
	runner.takeovers["thread"] = workflowTakeover{itemID: "item", transitioning: true}
	launch, err := engine.FinalizeThread("thread")
	if err != nil {
		t.Fatal(err)
	}
	request := engine.RunRequest{
		Key:    engine.RunKey{ItemID: "item", PhaseID: "phase", Attempt: 2},
		Phase:  def.Phase{ID: "phase", Driver: def.DriverAgent, Shape: "fan-out"},
		Launch: launch,
	}
	if err := runner.Start(t.Context(), request, func() {}, func(engine.Outcome) {}); err == nil {
		t.Fatal("finalize start with unsupported shape succeeded")
	}
	runner.mu.Lock()
	takeover := runner.takeovers["thread"]
	runner.mu.Unlock()
	if takeover.itemID != "item" || takeover.transitioning {
		t.Fatalf("takeover after failed finalize start = %+v, want transition cleared", takeover)
	}
	if err := runner.RegisterTakeover(context.Background(), "item", "thread"); err != nil {
		t.Fatalf("steering re-registration after failed finalize start: %v", err)
	}
}

func TestSchemaRestartedTakeoverSessionIsOwnedByPreparation(t *testing.T) {
	const threadID = "schema-restarted-takeover"
	startCalls, stopCalls := 0, 0
	host := &fakeHost{
		startSessionTakingLock: func(_ context.Context, got string) error {
			startCalls++
			if got != threadID {
				t.Fatalf("start thread = %q, want %q", got, threadID)
			}
			return nil
		},
		stopSession: func(got string) error {
			stopCalls++
			if got != threadID {
				t.Fatalf("stop thread = %q, want %q", got, threadID)
			}
			return nil
		},
	}
	runner := newTestRunner(t, host, nil, nil)
	runner.takeovers[threadID] = workflowTakeover{itemID: "item", schemaAttached: false}

	restarted, err := runner.restartClaudeTakeoverWithSchema(t.Context(), threadID, json.RawMessage(`{"type":"object"}`))
	if err != nil || !restarted || startCalls != 1 {
		t.Fatalf("schema restart = restarted %v, starts %d, err %v", restarted, startCalls, err)
	}
	// The restart's own teardown of the schema-less session goes through the
	// same stopSession seam as every other runner stop.
	if stopCalls != 1 {
		t.Fatalf("schema restart stopped %d sessions, want the schema-less one", stopCalls)
	}
	prepared := preparedWorkflowAgentTurn{threadID: threadID, startedSession: restarted}
	_ = prepared.discard(runner, errors.New("later preparation failure"))
	if stopCalls != 2 {
		t.Fatalf("discard left stop count at %d, want 2 (restart teardown + discard)", stopCalls)
	}
	if schema := runner.schemaForThread(threadID); len(schema) != 0 {
		t.Fatalf("discard retained temporary schema: %s", schema)
	}
}
