package threadtools

import (
	"encoding/json"
	"os"
	"testing"
)

// The fixture's own shape. Documented beside the file itself. The Go test
// reads live; liveStatus is the same case stated as the frontend's input
// to resolveEffectiveThreadStatus, so both must produce want.
type stateFixture struct {
	Cases []struct {
		Name   string `json:"name"`
		Thread struct {
			HasIncompleteTurn         bool   `json:"hasIncompleteTurn"`
			HasFailedTurn             bool   `json:"hasFailedTurn"`
			HasActionableProposedPlan bool   `json:"hasActionableProposedPlan"`
			WorktreeSetupState        string `json:"worktreeSetupState"`
		} `json:"thread"`
		Live struct {
			ActiveTurn        bool `json:"activeTurn"`
			PendingSends      bool `json:"pendingSends"`
			PendingApprovals  int  `json:"pendingApprovals"`
			PendingUserInputs int  `json:"pendingUserInputs"`
		} `json:"live"`
		LiveStatus string `json:"liveStatus"`
		Want       string `json:"want"`
	} `json:"cases"`
}

// TestStateFixture runs the shared table. It is the shared table, and not
// a Go-only one, because the sidebar and these tools must name the same
// state for the same thread.
func TestStateFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/thread_states.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture stateFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("the fixture has no cases")
	}
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			thread := Thread{
				HasIncompleteTurn:         testCase.Thread.HasIncompleteTurn,
				HasFailedTurn:             testCase.Thread.HasFailedTurn,
				HasActionableProposedPlan: testCase.Thread.HasActionableProposedPlan,
				WorktreeSetupState:        testCase.Thread.WorktreeSetupState,
			}
			live := LiveState{
				ActiveTurn:        testCase.Live.ActiveTurn,
				PendingSends:      testCase.Live.PendingSends,
				PendingApprovals:  testCase.Live.PendingApprovals,
				PendingUserInputs: testCase.Live.PendingUserInputs,
			}
			if got := State(thread, live); got != testCase.Want {
				t.Fatalf("State = %q, want %q", got, testCase.Want)
			}
			// The frontend input has to agree with the Go input, or the
			// two tests would be checking different things.
			if got := liveStatusOf(live); got != testCase.LiveStatus {
				t.Fatalf("liveStatus in the fixture is %q, but live derives %q", testCase.LiveStatus, got)
			}
		})
	}
}

// liveStatusOf is what the frontend's getThreadStatus derives from the
// same live inputs, so the fixture's two input columns cannot disagree.
func liveStatusOf(live LiveState) string {
	switch {
	case live.PendingApprovals > 0:
		return StatePendingApproval
	case live.PendingUserInputs > 0:
		return StateAwaitingInput
	case live.ActiveTurn || live.PendingSends:
		return StateRunning
	default:
		return StateIdle
	}
}

// TestStateCoversEveryValue keeps the fixture from losing a state when the
// derivation grows one.
func TestStateCoversEveryValue(t *testing.T) {
	data, err := os.ReadFile("testdata/thread_states.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture stateFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	seen := map[string]bool{}
	for _, testCase := range fixture.Cases {
		seen[testCase.Want] = true
	}
	for _, state := range AllStates {
		if !seen[state] {
			t.Errorf("no fixture case produces %q", state)
		}
	}
}

// TestResting: a wait on a thread id ends when the thread rests, and a
// thread blocked on a person is not resting.
func TestResting(t *testing.T) {
	for state, want := range map[string]bool{
		StateIdle:            true,
		StatePlanReady:       true,
		StateError:           true,
		StateInterrupted:     true,
		StateSetupFailed:     true,
		StateRunning:         false,
		StatePendingApproval: false,
		StateAwaitingInput:   false,
	} {
		if got := Resting(state); got != want {
			t.Errorf("Resting(%q) = %v, want %v", state, got, want)
		}
	}
}
