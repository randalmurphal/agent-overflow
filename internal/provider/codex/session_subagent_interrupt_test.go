package codex

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestInterruptSubagentTargetsOwnedChildTurnAndIsIdempotent(t *testing.T) {
	events := make(chan provider.ProviderEvent, 4)
	s, capturePath := newCapturingSession(t, "root-provider-thread")
	s.onEvent = func(event provider.ProviderEvent) { events <- event }
	s.registerChildOwnership("root-provider-thread", "child-provider-thread", "/root/reviewer", "spawn-1")
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"child-provider-thread","turn":{"id":"child-turn-7"}}}`))

	answerer := newPendingAnswerer(s)
	done := make(chan struct {
		stopped bool
		err     error
	}, 1)
	go func() {
		stopped, err := s.InterruptSubagent(context.Background(), "spawn-1")
		done <- struct {
			stopped bool
			err     error
		}{stopped, err}
	}()
	answerer.answer(t, `{}`)
	result := <-done
	if result.err != nil || !result.stopped {
		t.Fatalf("InterruptSubagent = (%v, %v), want (true, nil)", result.stopped, result.err)
	}

	frames := waitForCapturedRawFrames(t, capturePath, 1, 3*time.Second)
	var request struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frames[0], &request); err != nil {
		t.Fatalf("decode interrupt request: %v", err)
	}
	if request.Method != "turn/interrupt" || request.Params.ThreadID != "child-provider-thread" || request.Params.TurnID != "child-turn-7" {
		t.Fatalf("interrupt request = %+v, want child-provider-thread/child-turn-7", request)
	}

	for {
		select {
		case event := <-events:
			if event.Kind == provider.EventSubagentStatus && event.ParentToolUseID == "spawn-1" {
				var meta struct {
					Status string `json:"status"`
				}
				if err := json.Unmarshal(event.Meta, &meta); err != nil {
					t.Fatalf("decode child status event: %v", err)
				}
				if meta.Status != "interrupted" {
					continue
				}
				goto statusObserved
			}
		case <-time.After(3 * time.Second):
			t.Fatal("interrupted child status event not emitted")
		}
	}

statusObserved:
	stopped, err := s.InterruptSubagent(context.Background(), "spawn-1")
	if err != nil || stopped {
		t.Fatalf("repeated InterruptSubagent = (%v, %v), want (false, nil)", stopped, err)
	}
	if got := len(readCapturedRawFrames(t, capturePath)); got != 1 {
		t.Fatalf("repeated stop wrote %d interrupt requests, want 1", got)
	}
}

func TestInterruptSubagentUsesStartupInterruptBeforeTurnStarted(t *testing.T) {
	s, capturePath := newCapturingSession(t, "root-provider-thread")
	s.registerChildOwnership("root-provider-thread", "child-pending", "", "spawn-pending")
	answerer := newPendingAnswerer(s)

	done := make(chan error, 1)
	go func() {
		_, err := s.InterruptSubagent(context.Background(), "spawn-pending")
		done <- err
	}()
	answerer.answer(t, `{}`)
	if err := <-done; err != nil {
		t.Fatalf("InterruptSubagent pending start: %v", err)
	}

	frames := waitForCapturedRawFrames(t, capturePath, 1, 3*time.Second)
	var request struct {
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frames[0], &request); err != nil {
		t.Fatalf("decode startup interrupt request: %v", err)
	}
	if request.Params.ThreadID != "child-pending" || request.Params.TurnID != "" {
		t.Fatalf("startup interrupt params = %+v, want child-pending with empty turnId", request.Params)
	}
}

func TestInterruptSubagentTargetsNestedLaunchWithoutStoppingAncestor(t *testing.T) {
	var targets []string
	s := &Session{
		threadID: "ao-thread",
		collab: sessionCollabState{
			childParentByThread: map[string]string{
				"parent-child": "spawn-parent",
				"nested-child": "spawn-nested",
			},
			childRuntimeByThread: map[string]childRuntimeState{
				"parent-child": {phase: childRuntimeRunning, turnID: "parent-turn", generation: 1},
				"nested-child": {phase: childRuntimeRunning, turnID: "nested-turn", generation: 1},
			},
		},
		onEvent: func(provider.ProviderEvent) {},
		interruptChildTurnFn: func(_ context.Context, childThreadID, turnID string) error {
			targets = append(targets, childThreadID+"/"+turnID)
			return nil
		},
	}
	stopped, err := s.InterruptSubagent(context.Background(), "spawn-nested")
	if err != nil || !stopped {
		t.Fatalf("InterruptSubagent nested = (%v, %v)", stopped, err)
	}
	if len(targets) != 1 || targets[0] != "nested-child/nested-turn" {
		t.Fatalf("nested stop targets = %v, want nested child only", targets)
	}
	s.mu.Lock()
	ancestor := s.collab.childRuntimeByThread["parent-child"]
	s.mu.Unlock()
	if ancestor.phase != childRuntimeRunning || ancestor.turnID != "parent-turn" {
		t.Fatalf("ancestor runtime changed: %+v", ancestor)
	}
}

func TestInterruptSubagentDrainsOnlyTargetChildApprovals(t *testing.T) {
	s := NewInterruptSubagentTestSession("spawn-1", func(context.Context, string, string) error { return nil })
	s.approvals.TrackScoped("root-approval", provider.EventApprovalResolved, nil, "test-root-thread")
	s.approvals.TrackScoped("child-approval", provider.EventApprovalResolved, nil, "test-child-thread")
	s.approvals.TrackScoped("sibling-approval", provider.EventApprovalResolved, nil, "sibling-child-thread")

	stopped, err := s.InterruptSubagent(context.Background(), "spawn-1")
	if err != nil || !stopped {
		t.Fatalf("InterruptSubagent = (%v, %v)", stopped, err)
	}
	if s.approvals.Claim("child-approval", provider.EventApprovalResolved) {
		t.Fatal("target child approval remained pending")
	}
	if !s.approvals.Claim("root-approval", provider.EventApprovalResolved) {
		t.Fatal("target child stop drained the root approval")
	}
	if !s.approvals.Claim("sibling-approval", provider.EventApprovalResolved) {
		t.Fatal("target child stop drained the sibling approval")
	}
}
