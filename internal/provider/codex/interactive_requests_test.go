package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestApprovalResponseResolvesPendingCodex(t *testing.T) {
	t.Parallel()
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":7,"method":"item/commandExecution/requestApproval","params":{"threadId":"codex-thread-1","command":"ls"}}`)
	feed(t, s, line)

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				goto respond
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}
respond:
	if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "7",
		Decision:  "allow",
	}); err != nil {
		t.Fatalf("RespondToApproval: %v", err)
	}

	if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "7",
		Decision:  "allow",
	}); !errors.Is(err, provider.ErrStaleInteractiveRequest) {
		t.Fatalf("second RespondToApproval error = %v, want ErrStaleInteractiveRequest", err)
	}
}

// TestCodexPendingRequestsResolveWhenTheTurnOrSessionEnds covers each drain
// path: a pending approval or user-input request must emit its resolved event
// so the frontend's panel clears. Interrupt resolves it as "cancel" (beyond
// t3-code's CodexSessionRuntime.interruptTurn, which leaves the local Deferred
// parked); Close and a provider exit resolve it as "lost". A user-input
// resolution also carries an empty `answers` map, which the frontend type
// contract requires on every UserInputResolved meta.
func TestCodexPendingRequestsResolveWhenTheTurnOrSessionEnds(t *testing.T) {
	t.Parallel()
	approval := func(id int) []byte {
		return fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%d,"method":"item/commandExecution/requestApproval","params":{"threadId":"codex-thread-1","command":"ls"}}`, id)
	}
	userInput := func(id int) []byte {
		return fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%d,"method":"item/tool/requestUserInput","params":{"threadId":"codex-thread-1","questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}`, id)
	}
	// Interrupt attempts the RPC only with an active turn; before that gate
	// it returns "no active turn".
	withActiveTurn := func(s *Session) *Session {
		s.mu.Lock()
		s.turn.activeTurnID = "turn-1"
		s.mu.Unlock()
		return s
	}
	await := func(t *testing.T, s *Session, eventCh <-chan provider.ProviderEvent, line []byte, kind provider.EventKind) {
		t.Helper()
		feed(t, s, line)
		if evt := codexWaitEvent(t, eventCh); evt.Kind != kind {
			t.Fatalf("kind: got %q, want %q", evt.Kind, kind)
		}
	}
	interrupt := func(t *testing.T, s *Session) {
		t.Helper()
		fixtureFor(t, s).answer("turn/interrupt", `{}`)
		if err := s.Interrupt(context.Background()); err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
	}
	closeSession := func(t *testing.T, s *Session) {
		if err := s.Close(); err != nil {
			t.Logf("close returned %v (acceptable)", err)
		}
	}
	expectResolved := func(t *testing.T, eventCh <-chan provider.ProviderEvent, kind provider.EventKind, requestID, decision string) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case evt := <-eventCh:
				if evt.Kind != kind {
					continue
				}
				var meta map[string]any
				if err := json.Unmarshal(evt.Meta, &meta); err != nil {
					t.Fatalf("unmarshal resolved meta: %v", err)
				}
				if meta["requestId"] != requestID || meta["decision"] != decision {
					t.Fatalf("requestId/decision = %v/%v, want %s/%s", meta["requestId"], meta["decision"], requestID, decision)
				}
				if kind == provider.EventUserInputResolved {
					if answers, ok := meta["answers"].(map[string]any); !ok || len(answers) != 0 {
						t.Fatalf("answers = %v, want an empty map for the user-input variant", meta["answers"])
					}
				}
				return
			case <-deadline:
				t.Fatalf("no %s for request %s", kind, requestID)
			}
		}
	}

	t.Run("close resolves a pending approval as lost", func(t *testing.T) {
		t.Parallel()
		s, eventCh := newTestCodexSession(t)
		await(t, s, eventCh, approval(9), provider.EventApprovalRequest)
		closeSession(t, s)
		expectResolved(t, eventCh, provider.EventApprovalResolved, "9", "lost")
	})
	t.Run("interrupt cancels a pending approval", func(t *testing.T) {
		t.Parallel()
		s, eventCh := newTestCodexSession(t)
		await(t, withActiveTurn(s), eventCh, approval(11), provider.EventApprovalRequest)
		interrupt(t, s)
		expectResolved(t, eventCh, provider.EventApprovalResolved, "11", "cancel")
	})
	t.Run("interrupt cancels a pending user input", func(t *testing.T) {
		t.Parallel()
		s, eventCh := newTestCodexSession(t)
		await(t, withActiveTurn(s), eventCh, userInput(12), provider.EventUserInputRequest)
		interrupt(t, s)
		expectResolved(t, eventCh, provider.EventUserInputResolved, "12", "cancel")
	})
	t.Run("close resolves a pending user input as lost", func(t *testing.T) {
		t.Parallel()
		s, eventCh := newTestCodexSession(t)
		await(t, s, eventCh, userInput(13), provider.EventUserInputRequest)
		closeSession(t, s)
		expectResolved(t, eventCh, provider.EventUserInputResolved, "13", "lost")
	})
	t.Run("provider exit resolves a pending user input as lost", func(t *testing.T) {
		t.Parallel()
		s, eventCh := newTestCodexSession(t)
		await(t, s, eventCh, userInput(14), provider.EventUserInputRequest)
		fixtureFor(t, s).exitAppServer()
		expectResolved(t, eventCh, provider.EventUserInputResolved, "14", "lost")
	})
}

// TestCodexDrainWritesTurnTransitionError verifies the wire shape of
// the JSON-RPC error our drain writes to the Codex app-server. The
// `data.reason = "turnTransition"` field is the magic value Codex uses
// to early-return cleanly on `is_turn_transition_server_request_error`
// (codex-rs/app-server/src/server_request_error.rs) — without it,
// Codex's per-handler fallback paths log "request failed with client
// error" and (for MCP elicitation) pick the wrong action.
func TestCodexDrainWritesTurnTransitionError(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)
	s := fx.session(testThread, "codex-thread-1", nil)

	// Seed a pending approval (rpcID 99) so the drain has something
	// to write a response for.
	s.trackPendingApproval(99, provider.EventApprovalResolved)

	s.drainPendingApprovals("cancel", false, true)

	data := fx.written(1)[0]
	var frame struct {
		ID    int64 `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Reason string `json:"reason"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal captured frame %q: %v", string(data), err)
	}
	if frame.ID != 99 {
		t.Fatalf("frame.id = %d, want 99", frame.ID)
	}
	if frame.Error.Data.Reason != "turnTransition" {
		t.Fatalf("error.data.reason = %q, want \"turnTransition\"", frame.Error.Data.Reason)
	}
	if frame.Error.Code == 0 {
		t.Fatalf("error.code is zero — JSON-RPC error frames must carry a non-zero code")
	}
}

// trackPendingApproval registers an interactive request. Uses the numeric
// JSON-RPC id rendered as a string so dedup (Bug B9) and response routing
// both use the same key — and so writeDrainResponse can parse it back into
// the id the JSON-RPC reply has to carry.
func (s *Session) trackPendingApproval(rpcID int64, resolveKind provider.EventKind, decisions ...*[]json.RawMessage) {
	s.trackPendingApprovalScoped(rpcID, resolveKind, s.rootThreadID(), decisions...)
}
