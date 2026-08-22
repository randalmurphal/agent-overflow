package claude

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// backgroundTasksResponderScript builds a fake CLI that answers our
// outbound `background_tasks` control_request. The modes cover the four
// shapes the binding must tell apart: a genuine success, a success
// whose payload says the CLI matched no foreground task, a success with
// no payload at all (an older or divergent build), and silence.
func backgroundTasksResponderScript(mode string) string {
	const header = `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"background_tasks"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
`
	const footer = `
            ;;
    esac
done
`
	var body string
	switch mode {
	case "backgrounded":
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"backgrounded":true}}}\n' "$reqid"`
	case "refused":
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"backgrounded":false}}}\n' "$reqid"`
	case "no-payload":
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"`
	case "error":
		body = `            printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"no such tool_use"}}\n' "$reqid"`
	case "silent":
		body = `            : # drop the line deliberately to exercise the timeout path`
	default:
		body = `            : # unknown mode — never happens in tests`
	}
	return header + body + footer
}

func newBackgroundTasksResponderSession(t *testing.T, mode string, timeout time.Duration) *Session {
	t.Helper()
	scriptPath := t.TempDir() + "/fake-claude"
	if err := os.WriteFile(scriptPath, []byte(backgroundTasksResponderScript(mode)), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: scriptPath})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:                  proc,
		threadID:              testThread,
		onEvent:               func(evt provider.ProviderEvent) { _ = evt },
		cancel:                cancel,
		readDone:              make(chan struct{}),
		controlRequestTimeout: timeout,
	}
	go s.readLoop()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestSession_BackgroundTask_SuccessRoundTrip drives the happy path:
// the CLI answers `{backgrounded:true}` and BackgroundTask returns nil.
func TestSession_BackgroundTask_SuccessRoundTrip(t *testing.T) {
	s := newBackgroundTasksResponderSession(t, "backgrounded", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.BackgroundTask(ctx, "toolu_abc"); err != nil {
		t.Fatalf("BackgroundTask: %v", err)
	}
}

// TestSession_BackgroundTask_RefusedPayloadIsAnError is the reason the
// payload is read at all: the CLI answers subtype=success for a
// well-formed request even when it matched no live foreground task, so
// trusting success alone would flip a still-streaming row to
// "backgrounded" in the UI.
func TestSession_BackgroundTask_RefusedPayloadIsAnError(t *testing.T) {
	s := newBackgroundTasksResponderSession(t, "refused", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.BackgroundTask(ctx, "toolu_gone")
	if err == nil {
		t.Fatal("expected an error for backgrounded:false, got nil")
	}
	if !strings.Contains(err.Error(), "refused to background") {
		t.Errorf("error should explain the refusal, got: %v", err)
	}
}

// TestSession_BackgroundTask_MissingPayloadIsAnError: a success with no
// `backgrounded` field states nothing, and reporting an unstated
// outcome as done is the same failure as the refusal above.
func TestSession_BackgroundTask_MissingPayloadIsAnError(t *testing.T) {
	s := newBackgroundTasksResponderSession(t, "no-payload", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.BackgroundTask(ctx, "toolu_abc")
	if err == nil {
		t.Fatal("expected an error for a payload-less success, got nil")
	}
	if !strings.Contains(err.Error(), "did not report") {
		t.Errorf("error should mention the missing result, got: %v", err)
	}
}

// TestSession_BackgroundTask_ErrorResponse surfaces the CLI's own
// message so the UI can render it verbatim.
func TestSession_BackgroundTask_ErrorResponse(t *testing.T) {
	s := newBackgroundTasksResponderSession(t, "error", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.BackgroundTask(ctx, "toolu_bad")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "no such tool_use") {
		t.Errorf("error message missing provider detail: %v", err)
	}
}

// TestSession_BackgroundTask_Timeout exercises the watchdog: a wedged
// CLI must fail loudly inside the configured window rather than parking
// the caller.
func TestSession_BackgroundTask_Timeout(t *testing.T) {
	s := newBackgroundTasksResponderSession(t, "silent", 150*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := s.BackgroundTask(ctx, "toolu_wait")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error should mention timeout, got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("BackgroundTask took %s, expected near 150ms", elapsed)
	}
}

// TestSession_BackgroundTask_EmptyToolUseID fails fast on a blank id
// instead of writing a request the CLI would read as "background
// EVERY foreground task" — the one shape of this subtype AO must never
// send, because the button that reaches it names a single row.
func TestSession_BackgroundTask_EmptyToolUseID(t *testing.T) {
	s := newBackgroundTasksResponderSession(t, "backgrounded", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	err := s.BackgroundTask(ctx, "   ")
	if err == nil {
		t.Fatal("expected an error for a blank tool_use_id, got nil")
	}
	if !strings.Contains(err.Error(), "empty tool_use_id") {
		t.Errorf("error should name the empty tool_use_id, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("blank id took %s — the guard must short-circuit before the wire", elapsed)
	}
}
