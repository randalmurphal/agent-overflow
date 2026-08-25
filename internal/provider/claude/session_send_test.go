package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestVerifyReplayParentFailsRiskyReplayWithoutVerifiableParent(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "thread-risky",
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		expectedReplayByUUID: map[string]replayExpectation{
			"user-wire": {parent: "leaf-expected", wasRisky: true},
		},
		expectedReplayOrder: []string{"user-wire"},
	}
	meta, _ := json.Marshal(map[string]string{
		"provider_item_id": "user-wire",
	})

	s.verifyReplayParent(provider.ProviderEvent{
		Kind: provider.EventUserText,
		Meta: meta,
	})

	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Kind != provider.EventError {
		t.Fatalf("event kind = %q, want error", events[0].Kind)
	}
	if !strings.Contains(events[0].Content, "could not verify") {
		t.Fatalf("error content = %q, want verification failure", events[0].Content)
	}
}

func TestSendWireFormat(t *testing.T) {
	// Verify the JSON format matches the Claude CLI input protocol.
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]string{
				{
					"type": "text",
					"text": "hello",
				},
			},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var msgType string
	json.Unmarshal(parsed["type"], &msgType)
	if msgType != "user" {
		t.Errorf("type: got %q, want %q", msgType, "user")
	}

	var message struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	json.Unmarshal(parsed["message"], &message)
	if message.Role != "user" {
		t.Errorf("role: got %q, want %q", message.Role, "user")
	}
	if len(message.Content) != 1 {
		t.Fatalf("content blocks: got %d, want 1 (%+v)", len(message.Content), message.Content)
	}
	if block := message.Content[0]; block.Type != "text" || block.Text != "hello" {
		t.Errorf("content block: got %+v, want text block %q", block, "hello")
	}
}

// TestSession_Interrupt_SuccessRoundTrip drives the happy path end to
// end: Interrupt writes an interrupt control_request, the fake CLI
// matches the request_id and replies with subtype=success, and
// Interrupt returns nil. Implicitly validates the wire envelope shape
// because the fake CLI's stdin matcher only fires for
// `"type":"control_request"` + `"subtype":"interrupt"` together (see
// interruptResponderScript). Pre-fix the malformed envelope wouldn't
// match and this test would time out into the kill fallback instead
// of returning nil.
func TestSession_Interrupt_SuccessRoundTrip(t *testing.T) {
	s := newInterruptResponderSession(t, "success", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
}

// TestSession_Interrupt_WireEnvelopeShape captures the line written to
// stdin and asserts the exact envelope shape the SDK protocol
// requires: `{"type":"control_request","request_id":"so-N","request":{"subtype":"interrupt"}}`.
// This is a stronger gate than the SuccessRoundTrip test — a future
// regression that changed the envelope structure but happened to
// still contain the magic substrings would slip past the responder
// script but fail this assertion.
func TestSession_Interrupt_WireEnvelopeShape(t *testing.T) {
	capturePath := t.TempDir() + "/interrupt-line.json"
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			IFS= read -r line || exit 1
			printf '%s\n' "$line" > "$CAPTURE_PATH"
			reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
			printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
		`},
		Env: map[string]string{"CAPTURE_PATH": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:                  proc,
		threadID:              testThread,
		onEvent:               func(provider.ProviderEvent) {},
		cancel:                cancel,
		readDone:              make(chan struct{}),
		controlRequestTimeout: 2 * time.Second,
	}
	go s.readLoop()
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured line: %v", err)
	}
	var frame struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal captured envelope: %v", err)
	}
	if frame.Type != "control_request" {
		t.Errorf("envelope type = %q, want control_request", frame.Type)
	}
	if frame.RequestID == "" {
		t.Errorf("envelope request_id is empty (must be set so control_response can correlate)")
	}
	if frame.Request.Subtype != "interrupt" {
		t.Errorf("envelope request.subtype = %q, want interrupt", frame.Request.Subtype)
	}
}

// TestSession_Interrupt_ErrorResponse confirms that a subtype=error
// response surfaces as a non-nil error whose message contains the
// provider-supplied detail. Negative-asserts that the error is NOT
// classified as a kill so the app layer doesn't accidentally evict
// sessions on benign provider errors (e.g. interrupting between turns
// when no turn is open).
func TestSession_Interrupt_ErrorResponse(t *testing.T) {
	s := newInterruptResponderSession(t, "error", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.Interrupt(ctx)
	if err == nil {
		t.Fatal("Interrupt error: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no active turn") {
		t.Errorf("error should surface provider detail, got: %v", err)
	}
}

// TestSession_Interrupt_TimeoutSurfaces exercises the no-ack path:
// the fake CLI consumes the request and goes silent, Interrupt must
// return a timeout error within the configured window. We deliberately
// do NOT escalate to a process kill — that would also kill backgrounded
// tasks (inverting the documented foreground-only behaviour) and
// silently mask a Claude Code CLI bug. The error surfaces to the user
// as a toast.
func TestSession_Interrupt_TimeoutSurfaces(t *testing.T) {
	s := newInterruptResponderSession(t, "silent", 150*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := s.Interrupt(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Interrupt timeout: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error should mention timeout, got: %v", err)
	}
	if elapsed > 1*time.Second {
		t.Errorf("Interrupt took %s, expected near 150ms (matches sibling StopTask test bound)", elapsed)
	}
	// Session must still be alive — no kill fallback. Backgrounded
	// tasks the user spawned still need to keep running per the wire
	// contract.
	select {
	case <-s.proc.Done():
		t.Fatal("process was killed on Interrupt timeout — should only stop the model, not the session")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSession_Interrupt_CtxCancelSurfaces confirms the ctx.Done branch
// returns the ctx error to the caller without killing the session.
func TestSession_Interrupt_CtxCancelSurfaces(t *testing.T) {
	s := newInterruptResponderSession(t, "silent", 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after the request is in flight but before any response.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := s.Interrupt(ctx)
	if err == nil {
		t.Fatal("Interrupt ctx-cancel: expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error chain should preserve ctx.Canceled, got: %v", err)
	}
	// Session stays alive — see TimeoutSurfaces for the rationale.
	select {
	case <-s.proc.Done():
		t.Fatal("process was killed on ctx-cancel — should only release the request, not stop the session")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestInterruptAckFlowsIntoResultClassification drives the ack
// correlation end to end on a live read loop: Interrupt round-trips
// against a fake CLI that acks and then emits the verbatim 2.1.170
// ede_diagnostic result line (the wire ordering the real CLI uses —
// ack before result, verified 6/6 in the 2026-06-10 spike). The
// resulting EventTurnComplete must classify as a user abort, not a
// hard error — pinning the read-loop handoff
// (handleControlResponseLine → MarkInterruptAcked → parseResult),
// which the parser-only tests can't cover.
func TestInterruptAckFlowsIntoResultClassification(t *testing.T) {
	events := make(chan provider.ProviderEvent, 16)
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			IFS= read -r line || exit 1
			reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
			printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
			printf '%s\n' "$RESULT_LINE"
			sleep 2
		`},
		Env: map[string]string{"RESULT_LINE": ede2_1_170InterruptResultLine},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		parser:   NewParser(),
		onEvent: func(evt provider.ProviderEvent) {
			select {
			case events <- evt:
			default:
			}
		},
		cancel:                cancel,
		readDone:              make(chan struct{}),
		controlRequestTimeout: 2 * time.Second,
	}
	go s.readLoop()
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-events:
			if evt.Kind != provider.EventTurnComplete {
				continue
			}
			meta, ok := evt.TurnComplete.(*provider.WireTurnCompleteMeta)
			if !ok || meta == nil {
				t.Fatalf("TurnComplete payload = %T, want *WireTurnCompleteMeta", evt.TurnComplete)
			}
			if !meta.Aborted {
				t.Fatalf("Aborted = false — interrupt ack did not flow into result classification (StopReason=%q ErrorMessage=%q)", meta.StopReason, meta.ErrorMessage)
			}
			if meta.StopReason != "interrupted" {
				t.Fatalf("StopReason = %q, want interrupted", meta.StopReason)
			}
			if meta.ErrorMessage != "" {
				t.Fatalf("ErrorMessage = %q, want empty", meta.ErrorMessage)
			}
			return
		case <-deadline:
			t.Fatal("no EventTurnComplete within 3s")
		}
	}
}

func TestSessionSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; exit 0; done`},
		Env: map[string]string{
			"CAPTURE": capturePath,
		},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })

	s := &Session{proc: proc}
	const content = "/tmp/agent-overflow/bug-report.jsonl -- please inspect"
	if err := s.Send(context.Background(), content, provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var captured struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &captured); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if captured.Type != "user" || captured.Message.Role != "user" {
		t.Fatalf("captured line = %+v, want user message", captured)
	}
	if len(captured.Message.Content) != 1 {
		t.Fatalf("content blocks: got %d, want 1 (%+v)", len(captured.Message.Content), captured.Message.Content)
	}
	if block := captured.Message.Content[0]; block.Type != "text" || block.Text != content {
		t.Fatalf("content block = %+v, want exact text block %q", block, content)
	}
}

// TestSessionSendStampsUserMessageUUID verifies that a client-supplied
// UserMessageUUID rides the user envelope as the top-level `uuid` — the
// contract app_send.go depends on so a revert can slice the transcript
// by a uuid it knew at send time (see the Send doc comment).
func TestSessionSendStampsUserMessageUUID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; exit 0; done`},
		Env:    map[string]string{"CAPTURE": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })

	const wantUUID = "f47ac10b-58cc-4372-a567-0e02b2c3d479"
	s := &Session{proc: proc}
	if err := s.Send(context.Background(), "hello", provider.SendOptions{UserMessageUUID: wantUUID}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var captured struct {
		Type string `json:"type"`
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &captured); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if captured.Type != "user" {
		t.Fatalf("captured type = %q, want user", captured.Type)
	}
	if captured.UUID != wantUUID {
		t.Fatalf("captured top-level uuid = %q, want %q", captured.UUID, wantUUID)
	}
}

// TestSessionSendOmitsUUIDWhenEmpty verifies the optional-field contract:
// when no UserMessageUUID is supplied, the envelope carries no `uuid` key
// and the CLI assigns its own id (legacy behaviour, learned from the echo).
func TestSessionSendOmitsUUIDWhenEmpty(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; exit 0; done`},
		Env:    map[string]string{"CAPTURE": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })

	s := &Session{proc: proc}
	if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lines[0]), &raw); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if _, present := raw["uuid"]; present {
		t.Fatalf("envelope carried a uuid key with no UserMessageUUID supplied: %s", lines[0])
	}
}

// TestSessionSendRejectsMalformedUUID verifies the validation guard fails
// the send loudly BEFORE any stdin write, so a malformed id never poisons
// the session JSONL with a uuid the revert path can't match.
func TestSessionSendRejectsMalformedUUID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; exit 0; done`},
		Env:    map[string]string{"CAPTURE": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })

	s := &Session{proc: proc}
	err = s.Send(context.Background(), "hello", provider.SendOptions{UserMessageUUID: "not-a-uuid"})
	if err == nil {
		t.Fatal("Send with malformed uuid returned nil, want error")
	}
	if !strings.Contains(err.Error(), "invalid user message uuid") {
		t.Fatalf("Send error = %v, want invalid-user-message-uuid", err)
	}
	if data, readErr := os.ReadFile(capturePath); readErr == nil && len(data) > 0 {
		t.Fatalf("malformed-uuid send wrote to stdin before validation: %q", data)
	}
}

// TestSessionSendRejectsNonCanonicalUUID verifies that a parseable but
// non-canonical id (here, uppercase) is refused rather than silently
// normalized. app_send.go stamps the exact minted string on the user row
// and checkpoint; if Send canonicalized a different string into the
// envelope the pre-stamped row would no longer match the echoed JSONL
// uuid, dropping a pre-echo revert back to the ordinal-walk fallback. The
// guard makes that contract enforceable at the boundary.
func TestSessionSendRejectsNonCanonicalUUID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; exit 0; done`},
		Env:    map[string]string{"CAPTURE": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })

	s := &Session{proc: proc}
	// Uppercase of a valid uuid: uuid.Parse accepts it, but String()
	// round-trips to the lowercase canonical form, so the guard rejects it.
	err = s.Send(context.Background(), "hello", provider.SendOptions{UserMessageUUID: "F47AC10B-58CC-4372-A567-0E02B2C3D479"})
	if err == nil {
		t.Fatal("Send with non-canonical uuid returned nil, want error")
	}
	if !strings.Contains(err.Error(), "not in canonical form") {
		t.Fatalf("Send error = %v, want not-in-canonical-form", err)
	}
	if data, readErr := os.ReadFile(capturePath); readErr == nil && len(data) > 0 {
		t.Fatalf("non-canonical-uuid send wrote to stdin before validation: %q", data)
	}
}

func TestSessionSendSetsPlanPermissionModeBeforeUserMessage(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "fake-claude")
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	script := `#!/bin/sh
set -eu
capture="${CAPTURE_FILE:?}"
while IFS= read -r line; do
    printf '%s\n' "$line" >> "$capture"
    case "$line" in
        *'"set_permission_mode"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
        *'"type":"user"'*)
            exit 0
            ;;
    esac
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}

	s, err := NewSession(context.Background(), testThread, Config{
		Binary:             scriptPath,
		BasePermissionMode: "default",
		InteractionMode:    provider.ModePlan,
		Env: map[string]string{
			"CAPTURE_FILE": capturePath,
		},
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()
	s.controlRequestTimeout = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Send(ctx, "draft a plan", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 2)
	var first struct {
		Type    string `json:"type"`
		Request struct {
			Subtype string `json:"subtype"`
			Mode    string `json:"mode"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal first captured line: %v", err)
	}
	if first.Type != "control_request" || first.Request.Subtype != "set_permission_mode" || first.Request.Mode != "plan" {
		t.Fatalf("first captured line = %+v, want set_permission_mode plan", first)
	}

	var second struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("unmarshal second captured line: %v", err)
	}
	if second.Type != "user" || second.Message.Role != "user" || len(second.Message.Content) != 1 || second.Message.Content[0].Type != "text" || second.Message.Content[0].Text != "draft a plan" {
		t.Fatalf("second captured line = %+v, want user message", second)
	}
	if got := s.getCurrentPermissionMode(); got != "plan" {
		t.Fatalf("currentPermissionMode = %q, want plan", got)
	}
}

func TestSessionSendRestoresBasePermissionModeAfterPlanTurn(t *testing.T) {
	for _, baseMode := range []string{"default", "acceptEdits", "bypassPermissions"} {
		t.Run(baseMode, func(t *testing.T) {
			scriptPath := filepath.Join(t.TempDir(), "fake-claude")
			capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
			script := `#!/bin/sh
set -eu
capture="${CAPTURE_FILE:?}"
users=0
while IFS= read -r line; do
    printf '%s\n' "$line" >> "$capture"
    case "$line" in
        *'"set_permission_mode"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
        *'"type":"user"'*)
            users=$((users + 1))
            if [ "$users" -ge 2 ]; then
                exit 0
            fi
            ;;
    esac
done
`
			if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
				t.Fatalf("write fake claude script: %v", err)
			}

			s, err := NewSession(context.Background(), testThread, Config{
				Binary:             scriptPath,
				BasePermissionMode: baseMode,
				InteractionMode:    provider.ModeChat,
				Env: map[string]string{
					"CAPTURE_FILE": capturePath,
				},
			}, func(provider.ProviderEvent) {})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			defer s.Close()
			s.controlRequestTimeout = 2 * time.Second

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := s.Send(ctx, "draft a plan", provider.SendOptions{InteractionMode: provider.ModePlan}); err != nil {
				t.Fatalf("plan Send: %v", err)
			}
			if err := s.Send(ctx, "implement it", provider.SendOptions{InteractionMode: provider.ModeChat}); err != nil {
				t.Fatalf("chat Send: %v", err)
			}

			lines := waitCapturedLines(t, capturePath, 4)
			var modes []string
			for _, line := range lines {
				var raw struct {
					Type    string `json:"type"`
					Request struct {
						Subtype string `json:"subtype"`
						Mode    string `json:"mode"`
					} `json:"request"`
				}
				if err := json.Unmarshal([]byte(line), &raw); err != nil {
					t.Fatalf("unmarshal captured line %q: %v", line, err)
				}
				if raw.Type == "control_request" && raw.Request.Subtype == "set_permission_mode" {
					modes = append(modes, raw.Request.Mode)
				}
			}
			if want := []string{"plan", baseMode}; !reflect.DeepEqual(modes, want) {
				t.Fatalf("set_permission_mode sequence = %v, want %v", modes, want)
			}
			if got := s.getCurrentPermissionMode(); got != baseMode {
				t.Fatalf("currentPermissionMode = %q, want %q", got, baseMode)
			}
		})
	}
}

// TestSession_StopTask_SuccessRoundTrip drives the happy path end to
// end: StopTask writes a stop_task control_request, the fake CLI
// matches the request_id and replies with subtype=success, and
// StopTask returns nil.
func TestSession_StopTask_SuccessRoundTrip(t *testing.T) {
	s := newStopTaskResponderSession(t, "success", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.StopTask(ctx, "task-abc"); err != nil {
		t.Fatalf("StopTask success: %v", err)
	}
}

// TestSession_StopTask_ErrorResponse confirms that a subtype=error
// response surfaces as a non-nil error whose message contains the
// provider-supplied detail so the caller can render it to the user.
func TestSession_StopTask_ErrorResponse(t *testing.T) {
	s := newStopTaskResponderSession(t, "error", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.StopTask(ctx, "task-bad")
	if err == nil {
		t.Fatal("StopTask error: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "task not found") {
		t.Errorf("error message missing server detail: %v", err)
	}
}

// TestSession_StopTask_Timeout exercises the watchdog: the fake CLI
// consumes the request and goes silent; StopTask must return a
// timeout error within the configured window.
func TestSession_StopTask_Timeout(t *testing.T) {
	// Use a generous test context so the timeout error comes from
	// Session.controlRequestTimeout, not the caller context.
	s := newStopTaskResponderSession(t, "silent", 150*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := s.StopTask(ctx, "task-wait")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("StopTask timeout: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error should mention timeout, got: %v", err)
	}
	// Generous upper bound so flaky CI doesn't trip — we just want to
	// confirm the session didn't sit there for the 10s default.
	if elapsed > 2*time.Second {
		t.Errorf("StopTask took %s, expected near 150ms", elapsed)
	}
}

// TestSession_StopTask_EmptyTaskID fails fast when the caller passes
// a blank task_id — a silent no-op here would strand the per-row UI
// "Stop" button without feedback. TrimSpace covers stray-whitespace
// ids picked up from a UI surface.
//
// The test pins both the error MESSAGE (must mention empty task_id, NOT
// just "timeout") and the elapsed time — without the TrimSpace gate
// the call would write a stop_task to stdin, sit on the controlRequestTimeout
// for seconds, then return a timeout error that still trips `err != nil`
// but masks the programming bug. A tight 300ms ceiling proves we
// rejected the input without ever hitting the wire.
func TestSession_StopTask_EmptyTaskID(t *testing.T) {
	s := newStopTaskResponderSession(t, "silent", 5*time.Second)
	for _, tid := range []string{"", "   ", "\t\n"} {
		start := time.Now()
		err := s.StopTask(context.Background(), tid)
		elapsed := time.Since(start)
		if err == nil {
			t.Errorf("StopTask(%q): expected error, got nil", tid)
			continue
		}
		if !strings.Contains(err.Error(), "empty task_id") {
			t.Errorf("StopTask(%q): error should mention empty task_id, got: %v", tid, err)
		}
		// A missing TrimSpace would write to the fake CLI and wait the
		// full controlRequestTimeout (5s). 300ms is a generous upper bound that
		// still catches the regression.
		if elapsed > 300*time.Millisecond {
			t.Errorf("StopTask(%q) took %s, expected <300ms (suggests the input check fell through to the wire)", tid, elapsed)
		}
	}
}

// TestSession_StopTask_UnknownRequestIDDropped confirms the read loop
// silently drops control_response envelopes whose request_id doesn't
// match any pending StopTask. The in-flight StopTask still reaches
// its timeout and the session keeps processing lines.
func TestSession_StopTask_UnknownRequestIDDropped(t *testing.T) {
	s := newStopTaskResponderSession(t, "stray", 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.StopTask(ctx, "task-x")
	if err == nil {
		t.Fatal("StopTask with stray response: expected timeout, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("unexpected error shape: %v", err)
	}

	// Session must still be alive — a second call should work if we
	// wired one up. We don't have a second responder, so instead
	// confirm the subprocess hasn't died (the stray line didn't take
	// the read loop down).
	select {
	case <-s.proc.Done():
		t.Fatal("session died after stray control_response — read loop must survive unknown request_ids")
	default:
	}
}

// TestSession_StopTask_SubprocessDeathUnblocksCaller pins the behavior
// the readLoop's deferred clearPendingControlRequests buys us: when the CLI
// subprocess exits on its own while a StopTask is parked, the caller
// must unblock promptly with a clean error — NOT sit on its 10-second
// DefaultControlRequestTimeout waiting for a response that will never come.
// Without this guarantee the tray "Stop all" flow would freeze the UI
// for seconds per pending task after an unclean CLI exit.
func TestSession_StopTask_SubprocessDeathUnblocksCaller(t *testing.T) {
	// Fake CLI reads the first line (the stop_task), pauses briefly so
	// the StopTask goroutine is demonstrably parked on its pending
	// channel, then exits with status 0 — no response ever written.
	// This is exactly what an unclean subprocess death looks like to
	// the read loop: io.EOF with a pending caller.
	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/fake-claude"
	script := `#!/bin/sh
set -u
# Drain exactly one line, then exit without writing anything back.
read -r _discard
sleep 0.05
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: scriptPath})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent:  func(evt provider.ProviderEvent) { _ = evt },
		cancel:   cancel,
		readDone: make(chan struct{}),
		// Generous per-call timeout so the fast-unblock comes from the
		// readLoop cleanup, not from the timeout path. A failing
		// regression would wait this entire window.
		controlRequestTimeout: 5 * time.Second,
	}
	go s.readLoop()
	t.Cleanup(func() { _ = s.Close() })

	start := time.Now()
	err = s.StopTask(context.Background(), "task-vanish")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("StopTask after subprocess death: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "session closed") {
		t.Errorf("error should mention session close, got: %v", err)
	}
	// The subprocess exits ~50ms in; the read loop then drains and
	// signals the pending entry via clearPendingControlRequests. 1s is a
	// generous upper bound that still proves we didn't silently wait
	// the full 5s timeout.
	if elapsed > time.Second {
		t.Errorf("StopTask took %s after subprocess death, expected <1s (suggests timeout path, not readLoop signal)", elapsed)
	}
}

// TestSession_StopTask_ConcurrentSameTaskIDDistinctRequestIDs pins the
// per-request correlation contract when the frontend's Stop-all fan-out
// (or a double-click on the per-row Stop button) issues two StopTask
// calls for the SAME task_id within the same session. Each outbound
// control_request must carry a unique request_id so the CLI's reply
// resolves back to the right pending channel — a regression that
// reused a single request_id across concurrent stops on the same
// task_id would race: the first success unblocks BOTH callers if the
// map keyed by task_id, or only one call would land if the second
// overwrote the pending entry.
//
// The allocateControlRequestID counter guarantees uniqueness per
// session; this test pins that behavior end-to-end by tripping the
// fake-CLI to echo back each request_id and checking both StopTask
// calls resolve with no cross-talk.
func TestSession_StopTask_ConcurrentSameTaskIDDistinctRequestIDs(t *testing.T) {
	s := newStopTaskResponderSession(t, "success", 3*time.Second)

	// Two goroutines: both call StopTask with the SAME task_id. Each
	// allocates its own request_id via the seq counter; the fake CLI
	// echoes back whichever request_id it read, and deliverControlResponse
	// routes the two replies to their respective pending channels.
	const task = "task-double"
	done := make(chan error, 2)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done <- s.StopTask(ctx, task)
	}()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done <- s.StopTask(ctx, task)
	}()

	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("concurrent StopTask #%d: %v", i, err)
			}
		case <-time.After(4 * time.Second):
			t.Fatalf("concurrent StopTask #%d never returned — request_id collision suspected", i)
		}
	}

	// The two request_ids must be distinct. allocateControlRequestID
	// bumps controlRequestSeq under a lock, so two serialized StopTask calls
	// (the fake CLI's `read -r` is line-buffered) land at seq=1 and
	// seq=2. Observing seq >= 2 is sufficient — the third slot is
	// unallocated.
	s.controlRequestMu.Lock()
	seq := s.controlRequestSeq
	s.controlRequestMu.Unlock()
	if seq < 2 {
		t.Errorf("controlRequestSeq = %d after two concurrent StopTasks, want >= 2 (each call must allocate)", seq)
	}
}
