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

	"agent-overflow/internal/provider"
)

func TestSendTurnStartFormat(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	// Call the actual Send method, which issues a turn/start JSON-RPC request.
	// With the cat-backed session the request echoes back as a server request
	// (has both id and method), handleServerRequest sees "turn/start" as unknown
	// and returns a JSON-RPC error which becomes the sendRequest response.
	// Send returns an error from the RPC layer, which is expected here.
	_ = s.Send(context.Background(), "hello", provider.SendOptions{})

	// Drain the event channel: the echoed server request triggers
	// writeErrorResponse, whose echo arrives as a response (routed to pending).
	// The original turn/start echo may also produce events.
	// Verify the session didn't panic and readLoop is healthy by writing
	// a known notification that produces a deterministic event.
	if err := s.writeNotification("turn/started", map[string]any{
		"turn": map[string]any{"id": "turn-after-send"},
	}); err != nil {
		t.Fatalf("writeNotification: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	// Skip any stale events from the Send echo chain.
	for evt.TurnID != "turn-after-send" {
		evt = codexWaitEvent(t, eventCh)
	}
	if evt.Kind != provider.EventTurnStart {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventTurnStart)
	}
}

// TestTurnStartEmittedExactlyOncePerTurn exercises Bug B6: one user
// turn must produce exactly one EventTurnStart. Pre-fix, Send's RPC
// response emitter and dispatchLine's turn/started notification path
// both fired. We use a silent subprocess (sleep) so Send's request
// write does NOT echo back — this gives us a stable window to inject
// the RPC response via the pending channel without racing cat's echo
// of the request.
func TestTurnStartEmittedExactlyOncePerTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "cat > /dev/null; sleep 60"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { proc.Close() })

	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel: cancel,
	}
	s.setRootThreadID("ctx-thread")
	go s.readLoop()

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- s.Send(context.Background(), "hi", provider.SendOptions{})
	}()

	// Poll until Send has registered its pending channel, then inject a
	// successful result. The silent subprocess never echoes anything so
	// the pending channel is quiet until we write to it.
	var ch chan json.RawMessage
	var rpcID int64
	deadline := time.After(3 * time.Second)
pollPending:
	for {
		select {
		case <-deadline:
			t.Fatal("Send never registered a pending RPC id")
		default:
		}
		s.mu.Lock()
		for id, c := range s.pending {
			rpcID = id
			ch = c
			s.mu.Unlock()
			break pollPending
		}
		s.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	rpcResp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"turn":{"id":"turn-42"}}}`, rpcID)
	ch <- json.RawMessage(rpcResp)

	select {
	case err := <-sendDone:
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Send did not return")
	}

	// Fire the turn/started notification via a direct dispatchLine
	// call — the subprocess is silent so we can't rely on readLoop to
	// pick up a stdin-written line.
	notifLine := []byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-42"}}}`)
	s.dispatchLine(notifLine)

	turnStarts := 0
	drainDeadline := time.After(500 * time.Millisecond)
drain:
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventTurnStart && evt.TurnID == "turn-42" {
				turnStarts++
			}
		case <-drainDeadline:
			break drain
		}
	}
	if turnStarts != 1 {
		t.Fatalf("turnStart emissions for turn-42 = %d, want exactly 1 (Bug B6 regression)", turnStarts)
	}
}

// TestTurnStartOnlyNotificationStillEmits ensures that when the RPC
// response path is removed (the fix), a lone notification still surfaces
// EventTurnStart. Codex always sends turn/started after turn/start, so
// the notification path is load-bearing.
func TestTurnStartOnlyNotificationStillEmits(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	// Only the notification — no RPC response at all.
	notif := `{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-99"}}}`
	if err := s.proc.WriteLine([]byte(notif)); err != nil {
		t.Fatalf("write notif: %v", err)
	}

	var got provider.ProviderEvent
	select {
	case got = <-eventCh:
	case <-time.After(2 * time.Second):
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
	s, eventCh := newTestCodexSession(t)

	notif := `{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-dup"}}}`
	for i := 0; i < 2; i++ {
		if err := s.proc.WriteLine([]byte(notif)); err != nil {
			t.Fatalf("write notif %d: %v", i, err)
		}
	}

	count := 0
	deadline := time.After(1 * time.Second)
drain:
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventTurnStart && evt.TurnID == "turn-dup" {
				count++
			}
		case <-deadline:
			break drain
		}
	}
	if count != 1 {
		t.Fatalf("turnStart emissions = %d, want exactly 1 (dedup regression)", count)
	}
}

func TestCodexSend(t *testing.T) {
	s, _ := newTestCodexSession(t)

	// Send calls sendRequest("turn/start"). With cat, this goes through the
	// echo cycle and returns an error (unknown method). Send propagates it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.Send(ctx, "hello world", provider.SendOptions{})
	// Expected to fail because cat echo + handleServerRequest produces error response.
	if err == nil {
		t.Fatal("expected error from Send via cat echo")
	}
}

func TestCodexInterruptStartupSendsEmptyTurnID(t *testing.T) {
	// Codex's wire protocol treats an empty turn_id as a "startup
	// interrupt" — the app-server submits Op::Interrupt to the core
	// and responds immediately with `{}`. We must NOT gate on a
	// non-empty activeTurnID; just send the RPC and let upstream
	// handle the dispatch-window case. See
	// codex-rs/app-server/src/codex_message_processor.rs:7790-7849.
	capturePath := t.TempDir() + "/request.json"
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			IFS= read -r line || exit 1
			printf '%s\n' "$line" > "$CAPTURE_PATH"
			printf '{"jsonrpc":"2.0","id":1,"result":{}}\n'
		`},
		Env: map[string]string{"CAPTURE_PATH": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn recorder: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		// activeTurnID intentionally left empty — this is the
		// dispatch window case.
		pending: make(map[int64]chan json.RawMessage),
		onEvent: func(provider.ProviderEvent) {},
		cancel:  cancel,
	}
	s.setRootThreadID("codex-thread-1")
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		_ = proc.Close()
	})

	interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer interruptCancel()
	if err := s.Interrupt(interruptCtx); err != nil {
		t.Fatalf("Interrupt during dispatch window: %v", err)
	}

	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured request: %v", err)
	}
	var frame struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal captured request: %v", err)
	}
	if frame.Method != "turn/interrupt" {
		t.Fatalf("method = %q, want turn/interrupt", frame.Method)
	}
	if frame.Params.ThreadID != "codex-thread-1" {
		t.Fatalf("threadId = %q, want codex-thread-1", frame.Params.ThreadID)
	}
	if frame.Params.TurnID != "" {
		t.Fatalf("turnId = %q, want empty (startup interrupt sentinel)", frame.Params.TurnID)
	}
}

func TestCodexInterruptWithActiveTurn(t *testing.T) {
	s, _ := newTestCodexSession(t)

	// Simulate turn/started by setting activeTurnID.
	s.mu.Lock()
	s.activeTurnID = "turn-1"
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Just verify Interrupt attempts the RPC — actual response is
	// noise from cat's echo behaviour. The startup-interrupt path is
	// covered separately by TestCodexInterruptStartupSendsEmptyTurnID.
	_ = s.Interrupt(ctx)
}

func TestCodexInterruptSendsThreadAndTurnID(t *testing.T) {
	capturePath := t.TempDir() + "/request.json"
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			IFS= read -r line || exit 1
			printf '%s\n' "$line" > "$CAPTURE_PATH"
			printf '{"jsonrpc":"2.0","id":1,"result":{}}\n'
		`},
		Env: map[string]string{"CAPTURE_PATH": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn recorder: %v", err)
	}

	s := &Session{
		proc:         proc,
		threadID:     testThread,
		activeTurnID: "turn-1",
		pending:      make(map[int64]chan json.RawMessage),
		onEvent:      func(provider.ProviderEvent) {},
		cancel:       cancel,
	}
	s.setRootThreadID("codex-thread-1")
	go s.readLoop()

	t.Cleanup(func() {
		cancel()
		_ = proc.Close()
	})

	interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer interruptCancel()

	if err := s.Interrupt(interruptCtx); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured request: %v", err)
	}

	var frame struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal captured request: %v", err)
	}

	if frame.Method != "turn/interrupt" {
		t.Fatalf("method = %q, want turn/interrupt", frame.Method)
	}
	if frame.Params.ThreadID != "codex-thread-1" {
		t.Fatalf("params.threadId = %q, want codex-thread-1", frame.Params.ThreadID)
	}
	if frame.Params.TurnID != "turn-1" {
		t.Fatalf("params.turnId = %q, want turn-1", frame.Params.TurnID)
	}
}

func TestSendImageOnlyTurnStartFormat(t *testing.T) {
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
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	s, err := NewSession(context.Background(), testThread, Config{
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
			if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
				t.Fatalf("write mock script: %v", err)
			}

			s, err := NewSession(context.Background(), testThread, Config{
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
			if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
				t.Fatalf("write mock script: %v", err)
			}

			s, err := NewSession(context.Background(), testThread, Config{
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
