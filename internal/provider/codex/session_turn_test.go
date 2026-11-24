package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

func TestSendTurnStartFormat(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)
	s := fx.session(testThread, "codex-thread-1", nil)

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- s.Send(context.Background(), "hello", provider.SendOptions{})
	}()

	frame := fx.nextRequest("turn/start")
	params, _ := frame["params"].(map[string]any)
	if params["threadId"] != "codex-thread-1" {
		t.Errorf("turn/start threadId = %v, want codex-thread-1", params["threadId"])
	}
	input, _ := params["input"].([]any)
	first, _ := firstOrNil(input).(map[string]any)
	if first["type"] != "text" || first["text"] != "hello" {
		t.Errorf("turn/start input = %v, want one text item \"hello\"", params["input"])
	}
	fx.respond(int64(frame["id"].(float64)), map[string]any{"turn": map[string]any{"id": "turn-1"}})
	if err := <-sendDone; err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func firstOrNil(items []any) any {
	if len(items) == 0 {
		return nil
	}
	return items[0]
}

// TestTurnStartEmittedExactlyOncePerTurn exercises Bug B6: one user
// turn must produce exactly one EventTurnStart. Pre-fix, Send's RPC
// response emitter and the turn/started notification path both fired.
func TestTurnStartEmittedExactlyOncePerTurn(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)
	eventCh := make(chan provider.ProviderEvent, 100)
	s := fx.session(testThread, "ctx-thread", func(evt provider.ProviderEvent) {
		eventCh <- evt
	})

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- s.Send(context.Background(), "hi", provider.SendOptions{})
	}()
	fx.respond(int64(fx.nextRequest("turn/start")["id"].(float64)), map[string]any{"turn": map[string]any{"id": "turn-42"}})

	select {
	case err := <-sendDone:
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
	case <-time.After(fixtureWait):
		t.Fatal("Send did not return")
	}

	fx.feed([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"ctx-thread","turn":{"id":"turn-42"}}}`))
	// The completion is emitted after any turn start either path produced,
	// so it bounds the drain without waiting out a quiet period.
	fx.feed([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"ctx-thread","turn":{"id":"turn-42","status":"completed"}}}`))

	turnStarts := countTurnStartsUntilComplete(t, eventCh, "turn-42")
	if turnStarts != 1 {
		t.Fatalf("turnStart emissions for turn-42 = %d, want exactly 1 (Bug B6 regression)", turnStarts)
	}
}

// TestTurnStartOnlyNotificationStillEmits ensures that when the RPC
// response path is removed (the fix), a lone notification still surfaces
// EventTurnStart. Codex always sends turn/started after turn/start, so
// the notification path is load-bearing.
func TestTurnStartOnlyNotificationStillEmits(t *testing.T) {
	t.Parallel()
	s, eventCh := newTestCodexSession(t)

	// Only the notification — no RPC response at all.
	feed(t, s, []byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"codex-thread-1","turn":{"id":"turn-99"}}}`))

	var got provider.ProviderEvent
	select {
	case got = <-eventCh:
	default:
		t.Fatal("no event emitted from lone notification")
	}
	if got.Kind != provider.EventTurnStart {
		t.Fatalf("kind: got %q, want EventTurnStart", got.Kind)
	}
	if got.TurnID != "turn-99" {
		t.Fatalf("turnID: got %q, want turn-99", got.TurnID)
	}
}

// TestTurnStartIdempotentOnDuplicateNotification covers the rarer case
// where the provider re-sends turn/started (e.g. recovery). The second
// emission must be suppressed so the router still sees one turn.
func TestTurnStartIdempotentOnDuplicateNotification(t *testing.T) {
	t.Parallel()
	s, eventCh := newTestCodexSession(t)

	notif := `{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"codex-thread-1","turn":{"id":"turn-dup"}}}`
	for i := 0; i < 2; i++ {
		feed(t, s, []byte(notif))
	}
	feed(t, s, []byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"codex-thread-1","turn":{"id":"turn-dup","status":"completed"}}}`))

	count := countTurnStartsUntilComplete(t, eventCh, "turn-dup")
	if count != 1 {
		t.Fatalf("turnStart emissions = %d, want exactly 1 (dedup regression)", count)
	}
}

// countTurnStartsUntilComplete counts EventTurnStart for turnID until that
// turn's EventTurnComplete arrives.
func countTurnStartsUntilComplete(t *testing.T, eventCh <-chan provider.ProviderEvent, turnID string) int {
	t.Helper()
	count := 0
	deadline := time.After(fixtureWait)
	for {
		select {
		case evt := <-eventCh:
			if evt.TurnID != turnID {
				continue
			}
			if evt.Kind == provider.EventTurnStart {
				count++
			} else if evt.Kind == provider.EventTurnComplete {
				return count
			}
		case <-deadline:
			t.Fatalf("no turn completion for %s", turnID)
			return count
		}
	}
}

// Send returns the app-server's turn/start refusal to its caller.
func TestCodexSendReturnsTurnStartError(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)
	s := fx.session(testThread, "codex-thread-1", nil)

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- s.Send(context.Background(), "hello world", provider.SendOptions{})
	}()
	fx.respondError(int64(fx.nextRequest("turn/start")["id"].(float64)), -32600, "turn refused")

	if err := <-sendDone; err == nil || !strings.Contains(err.Error(), "turn refused") {
		t.Fatalf("Send error = %v, want the turn/start refusal", err)
	}
}

// interruptFrame runs Interrupt on a fixture session, answers its
// turn/interrupt, and returns the request's params.
func interruptFrame(t *testing.T, activeTurnID string) map[string]any {
	t.Helper()
	fx := newConnFixture(t)
	s := fx.session(testThread, "codex-thread-1", nil)
	s.turn.activeTurnID = activeTurnID

	done := make(chan error, 1)
	go func() { done <- s.Interrupt(context.Background()) }()
	frame := fx.nextRequest("turn/interrupt")
	fx.respond(int64(frame["id"].(float64)), map[string]any{})
	if err := <-done; err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	params, _ := frame["params"].(map[string]any)
	return params
}

// Codex's wire protocol treats an empty turn_id as a "startup interrupt" —
// the app-server submits Op::Interrupt to the core and responds immediately
// with `{}`. We must NOT gate on a non-empty activeTurnID; just send the RPC
// and let upstream handle the dispatch-window case. See
// codex-rs/app-server/src/codex_message_processor.rs:7790-7849.
func TestCodexInterruptSendsThreadAndTurnID(t *testing.T) {
	t.Parallel()
	for _, activeTurnID := range []string{"turn-1", ""} {
		t.Run("active turn "+activeTurnID, func(t *testing.T) {
			params := interruptFrame(t, activeTurnID)
			if params["threadId"] != "codex-thread-1" {
				t.Fatalf("params.threadId = %v, want codex-thread-1", params["threadId"])
			}
			if turnID, _ := params["turnId"].(string); turnID != activeTurnID {
				t.Fatalf("params.turnId = %v, want %q", params["turnId"], activeTurnID)
			}
		})
	}
}

func TestSendImageOnlyTurnStartFormat(t *testing.T) {
	t.Parallel()
	capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"turn/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-image\"}}}"
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-123\"}}}"
    fi
done
`, capturePath)
	scriptPath := filepath.Join(t.TempDir(), "codex")
	mockexec.Write(t, scriptPath, script)

	s, err := NewSession(context.Background(), testThread, Config{
		Reaper:         procutiltest.Reaper(),
		Binary:         scriptPath,
		Model:          "test-model",
		WorkDir:        "/tmp",
		ApprovalPolicy: "on-request",
		Sandbox:        "workspace-write",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	err = s.Send(context.Background(), "", provider.SendOptions{
		Attachments: []provider.ImageAttachment{{
			ID:       "att-1",
			Filename: "snap.png",
			MimeType: "image/png",
			Size:     8,
			Path:     "/tmp/att-1/snap.png",
		}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	_ = s.Close()

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	var turnStart map[string]any
	for _, line := range strings.Split(string(captured), "\n") {
		if !strings.Contains(line, `"method":"turn/start"`) {
			continue
		}
		if err := json.Unmarshal([]byte(line), &turnStart); err != nil {
			t.Fatalf("unmarshal turn/start: %v", err)
		}
		break
	}
	if turnStart == nil {
		t.Fatalf("captured no turn/start request: %s", string(captured))
	}
	params := turnStart["params"].(map[string]any)
	if params["approvalPolicy"] != "on-request" {
		t.Fatalf("approvalPolicy = %v, want on-request", params["approvalPolicy"])
	}
	sandboxPolicy := params["sandboxPolicy"].(map[string]any)
	if sandboxPolicy["type"] != "workspaceWrite" {
		t.Fatalf("sandboxPolicy.type = %v, want workspaceWrite", sandboxPolicy["type"])
	}
	input := params["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input length = %d, want image-only input", len(input))
	}
	imageInput := input[0].(map[string]any)
	if imageInput["type"] != "localImage" {
		t.Fatalf("input type = %v, want localImage", imageInput["type"])
	}
	wantPath := "/tmp/att-1/snap.png"
	if imageInput["path"] != wantPath {
		t.Fatalf("image path = %v, want %s", imageInput["path"], wantPath)
	}
}

func TestSessionSendIncludesRuntimeAccessPolicyForEveryMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		approvalPolicy  string
		sandbox         string
		wantSandboxType string
	}{
		{
			name:            "approval-required",
			approvalPolicy:  "untrusted",
			sandbox:         "read-only",
			wantSandboxType: "readOnly",
		},
		{
			name:            "auto-accept-edits",
			approvalPolicy:  "on-request",
			sandbox:         "workspace-write",
			wantSandboxType: "workspaceWrite",
		},
		{
			name:            "full-access",
			approvalPolicy:  "never",
			sandbox:         "danger-full-access",
			wantSandboxType: "dangerFullAccess",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
			script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"turn/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-runtime\"}}}"
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-123\"}}}"
    fi
done
`, capturePath)
			scriptPath := filepath.Join(t.TempDir(), "codex")
			mockexec.Write(t, scriptPath, script)

			s, err := NewSession(context.Background(), testThread, Config{
				Reaper:         procutiltest.Reaper(),
				Binary:         scriptPath,
				Model:          "test-model",
				WorkDir:        "/tmp",
				ApprovalPolicy: tc.approvalPolicy,
				Sandbox:        tc.sandbox,
			}, func(provider.ProviderEvent) {})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })

			if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
				t.Fatalf("Send: %v", err)
			}
			_ = s.Close()

			captured, err := os.ReadFile(capturePath)
			if err != nil {
				t.Fatalf("read capture: %v", err)
			}
			var turnStart map[string]any
			for _, line := range strings.Split(string(captured), "\n") {
				if !strings.Contains(line, `"method":"turn/start"`) {
					continue
				}
				if err := json.Unmarshal([]byte(line), &turnStart); err != nil {
					t.Fatalf("unmarshal turn/start: %v", err)
				}
				break
			}
			if turnStart == nil {
				t.Fatalf("captured no turn/start request: %s", string(captured))
			}
			params := turnStart["params"].(map[string]any)
			if params["approvalPolicy"] != tc.approvalPolicy {
				t.Fatalf("approvalPolicy = %v, want %s", params["approvalPolicy"], tc.approvalPolicy)
			}
			sandboxPolicy := params["sandboxPolicy"].(map[string]any)
			if sandboxPolicy["type"] != tc.wantSandboxType {
				t.Fatalf("sandboxPolicy.type = %v, want %s", sandboxPolicy["type"], tc.wantSandboxType)
			}
		})
	}
}

func TestSessionSendIncludesCollaborationMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		mode     provider.InteractionMode
		wantMode string
	}{
		{name: "plan", mode: provider.ModePlan, wantMode: "plan"},
		{name: "chat clears plan mode", mode: provider.ModeChat, wantMode: "default"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
			script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"turn/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-collab\"}}}"
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-123\"}}}"
    fi
done
`, capturePath)
			scriptPath := filepath.Join(t.TempDir(), "codex")
			mockexec.Write(t, scriptPath, script)

			s, err := NewSession(context.Background(), testThread, Config{
				Reaper:          procutiltest.Reaper(),
				Binary:          scriptPath,
				Model:           "test-model",
				WorkDir:         "/tmp",
				ReasoningEffort: "high",
			}, func(provider.ProviderEvent) {})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })

			if err := s.Send(context.Background(), "hello", provider.SendOptions{InteractionMode: tc.mode}); err != nil {
				t.Fatalf("Send: %v", err)
			}
			_ = s.Close()

			captured, err := os.ReadFile(capturePath)
			if err != nil {
				t.Fatalf("read capture: %v", err)
			}
			var turnStart map[string]any
			for _, line := range strings.Split(string(captured), "\n") {
				if !strings.Contains(line, `"method":"turn/start"`) {
					continue
				}
				if err := json.Unmarshal([]byte(line), &turnStart); err != nil {
					t.Fatalf("unmarshal turn/start: %v", err)
				}
				break
			}
			if turnStart == nil {
				t.Fatalf("captured no turn/start request: %s", string(captured))
			}
			params := turnStart["params"].(map[string]any)
			collaborationMode := params["collaborationMode"].(map[string]any)
			if collaborationMode["mode"] != tc.wantMode {
				t.Fatalf("collaborationMode.mode = %v, want %s", collaborationMode["mode"], tc.wantMode)
			}
			settings := collaborationMode["settings"].(map[string]any)
			if settings["model"] != "test-model" {
				t.Fatalf("settings.model = %v, want test-model", settings["model"])
			}
			if settings["reasoning_effort"] != "high" {
				t.Fatalf("settings.reasoning_effort = %v, want high", settings["reasoning_effort"])
			}
			if settings["developer_instructions"] != nil {
				t.Fatalf("settings.developer_instructions = %v, want nil built-in preset", settings["developer_instructions"])
			}
		})
	}
}

// TestSessionSendIncludesApprovalsReviewerForEveryMode proves the reviewer
// rides every turn/start, not just the handshake. Codex keeps the reviewer as
// thread state until something overwrites it, so a turn that omits it inherits
// the previous runtime mode's choice — which is how a thread switched OUT of
// auto keeps auto-approving its own escalations.
func TestSessionSendIncludesApprovalsReviewerForEveryMode(t *testing.T) {
	t.Parallel()
	for _, mode := range provider.AllRuntimeModes {
		t.Run(string(mode), func(t *testing.T) {
			capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
			want := codexApprovalsReviewer(mode)
			threadResult := fmt.Sprintf(`{\"thread\":{\"id\":\"mock-thread-123\"},\"approvalsReviewer\":\"%s\"}`, want)
			cfg := ConfigFromOptions(provider.SessionOptions{
				Provider:    "codex",
				RuntimeMode: mode,
				WorkDir:     "/tmp",
			})
			cfg.Binary = codexReviewerEchoScript(t, capturePath, threadResult)
			cfg.Reaper = procutiltest.Reaper()

			s, err := NewSession(context.Background(), testThread, cfg, func(provider.ProviderEvent) {})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
				t.Fatalf("Send: %v", err)
			}
			_ = s.Close()

			params := codexCapturedRequest(t, capturePath, "turn/start")
			if params["approvalsReviewer"] != want {
				t.Errorf("turn/start approvalsReviewer = %v, want %q", params["approvalsReviewer"], want)
			}
		})
	}
}
