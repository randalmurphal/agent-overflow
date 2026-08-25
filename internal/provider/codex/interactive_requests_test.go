package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestApprovalResponseResolvesPendingCodex(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":7,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
	s.dispatchLine(line)

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

func TestCodexCloseResolvesPendingApprovalAsLost(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":9,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
	s.dispatchLine(line)

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				goto closeNow
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}
closeNow:
	if err := s.Close(); err != nil {
		t.Logf("close returned %v (acceptable)", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt, ok := <-eventCh:
			if !ok {
				t.Fatal("event channel closed before approval resolved")
			}
			if evt.Kind != provider.EventApprovalResolved {
				continue
			}
			var meta map[string]any
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta["decision"] != "lost" {
				t.Fatalf("decision = %v, want lost", meta["decision"])
			}
			return
		case <-deadline:
			t.Fatal("pending approval was not resolved on close")
		}
	}
}

// TestCodexInterruptDrainsPendingApproval covers the Interrupt-drain
// path: when the user clicks stop while a sandbox approval is pending,
// the session must emit EventApprovalResolved with decision="cancel" so
// the frontend's approval panel clears immediately. This is the bug-fix
// beyond t3-code's CodexSessionRuntime.interruptTurn, which leaves the
// local Deferred parked.
func TestCodexInterruptDrainsPendingApproval(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	// Need an active turn for Interrupt to attempt the RPC at all —
	// before that gate the function returns "no active turn".
	s.mu.Lock()
	s.turn.activeTurnID = "turn-1"
	s.mu.Unlock()

	line := []byte(`{"jsonrpc":"2.0","id":11,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
	s.dispatchLine(line)

	// Drain incoming events until we see the approval request.
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				goto interrupt
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}
interrupt:
	// Interrupt itself returns an error (cat echoes our request back
	// as a server-request shape, which falls to the default error case
	// — fine for this test). The drain runs regardless.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Interrupt(ctx)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventApprovalResolved {
				continue
			}
			var meta struct {
				RequestID string `json:"requestId"`
				Decision  string `json:"decision"`
			}
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta.RequestID != "11" {
				t.Fatalf("requestId = %q, want 11", meta.RequestID)
			}
			if meta.Decision != "cancel" {
				t.Fatalf("decision = %q, want cancel", meta.Decision)
			}
			return
		case <-deadline:
			t.Fatal("no EventApprovalResolved after Interrupt")
		}
	}
}

// TestCodexInterruptDrainsPendingUserInput is the user-input twin of
// TestCodexInterruptDrainsPendingApproval. The resolved event must
// carry decision="cancel" AND answers={} so the frontend's user-input
// panel clears with a well-formed payload.
func TestCodexInterruptDrainsPendingUserInput(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	s.mu.Lock()
	s.turn.activeTurnID = "turn-1"
	s.mu.Unlock()

	line := []byte(`{"jsonrpc":"2.0","id":12,"method":"item/tool/requestUserInput","params":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}`)
	s.dispatchLine(line)

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputRequest {
				goto interrupt
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no user-input request event")
		}
	}
interrupt:
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Interrupt(ctx)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventUserInputResolved {
				continue
			}
			var meta struct {
				RequestID string         `json:"requestId"`
				Decision  string         `json:"decision"`
				Answers   map[string]any `json:"answers"`
			}
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta.RequestID != "12" {
				t.Fatalf("requestId = %q, want 12", meta.RequestID)
			}
			if meta.Decision != "cancel" {
				t.Fatalf("decision = %q, want cancel", meta.Decision)
			}
			if meta.Answers == nil {
				t.Fatalf("answers field absent; want empty map for user-input variant")
			}
			if len(meta.Answers) != 0 {
				t.Fatalf("answers = %v, want empty map", meta.Answers)
			}
			return
		case <-deadline:
			t.Fatal("no EventUserInputResolved after Interrupt")
		}
	}
}

// TestCodexCloseDrainsPendingUserInputWithAnswers verifies the Close
// drain emits an EventUserInputResolved that carries the empty
// `answers` map alongside the historic decision="lost". The frontend
// type contract requires the field on every UserInputResolved meta.
func TestCodexCloseDrainsPendingUserInputWithAnswers(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":13,"method":"item/tool/requestUserInput","params":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}`)
	s.dispatchLine(line)

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputRequest {
				goto closeNow
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no user-input request event")
		}
	}
closeNow:
	if err := s.Close(); err != nil {
		t.Logf("close returned %v (acceptable)", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt, ok := <-eventCh:
			if !ok {
				t.Fatal("event channel closed before resolved event")
			}
			if evt.Kind != provider.EventUserInputResolved {
				continue
			}
			var meta struct {
				RequestID string         `json:"requestId"`
				Decision  string         `json:"decision"`
				Answers   map[string]any `json:"answers"`
			}
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta.RequestID != "13" {
				t.Fatalf("requestId = %q, want 13", meta.RequestID)
			}
			if meta.Decision != "lost" {
				t.Fatalf("decision = %q, want lost (Close path)", meta.Decision)
			}
			if meta.Answers == nil {
				t.Fatalf("answers field absent; want empty map")
			}
			return
		case <-deadline:
			t.Fatal("no EventUserInputResolved after Close")
		}
	}
}

func TestCodexProviderExitResolvesPendingUserInputAsLost(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":14,"method":"item/tool/requestUserInput","params":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}`)
	s.dispatchLine(line)

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventUserInputRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventUserInputRequest)
	}

	if err := s.proc.Close(); err != nil {
		t.Fatalf("close provider process: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventUserInputResolved {
				continue
			}
			var meta map[string]any
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta["decision"] != "lost" {
				t.Fatalf("decision = %v, want lost", meta["decision"])
			}
			if _, ok := meta["answers"].(map[string]any); !ok {
				t.Fatalf("answers missing or wrong type: %v", meta["answers"])
			}
			return
		case <-deadline:
			t.Fatal("pending user input was not resolved after provider exit")
		}
	}
}

// TestCodexDrainWritesTurnTransitionError verifies the wire shape of
// the JSON-RPC error our drain writes to the Codex app-server. The
// `data.reason = "turnTransition"` field is the magic value Codex uses
// to early-return cleanly on `is_turn_transition_server_request_error`
// (codex-rs/app-server/src/server_request_error.rs) — without it,
// Codex's per-handler fallback paths log "request failed with client
// error" and (for MCP elicitation) pick the wrong action.
func TestCodexDrainWritesTurnTransitionError(t *testing.T) {
	capturePath := t.TempDir() + "/wire.jsonl"
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			while IFS= read -r line; do
				printf '%s\n' "$line" >> "$CAPTURE_PATH"
			done
		`},
		Env: map[string]string{"CAPTURE_PATH": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture sh: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  func(provider.ProviderEvent) {},
		cancel:   cancel,
	}
	s.setRootThreadID("codex-thread-1")
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		_ = proc.Close()
	})

	// Seed a pending approval (rpcID 99) so the drain has something
	// to write a response for.
	s.trackPendingApproval(99, provider.EventApprovalResolved)

	s.drainPendingApprovals("cancel", false, true)

	// Give the capture script a moment to flush, then read.
	deadline := time.Now().Add(2 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(capturePath)
		if len(data) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(data) == 0 {
		t.Fatalf("no wire bytes captured")
	}
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
