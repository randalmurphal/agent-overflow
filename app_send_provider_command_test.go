package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"

	"context"
)

// writeStdinCapturingClaudeBinary mints a fake `claude` that appends every
// stdin line to capturePath before answering. The outbound user envelope is
// the only place the slash guard is observable — the guard rewrites the
// content on its way to the CLI, so nothing in the store or on the event bus
// can tell a guarded send from an unguarded one.
func writeStdinCapturingClaudeBinary(t *testing.T, capturePath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-capture.sh")
	script := `#!/bin/sh
set -eu
while IFS= read -r line; do
    printf '%s\n' "$line" >> ` + capturePath + `
    case "$line" in
        *'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write capturing claude binary: %v", err)
	}
	return path
}

// capturedUserMessageTexts returns the concatenated text blocks of every
// outbound `user` envelope the fake CLI has seen so far, in order.
func capturedUserMessageTexts(t *testing.T, capturePath string) []string {
	t.Helper()
	raw, err := os.ReadFile(capturePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read capture: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var envelope struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			continue
		}
		if envelope.Type != "user" {
			continue
		}
		var text strings.Builder
		for _, block := range envelope.Message.Content {
			if block.Type == "text" {
				text.WriteString(block.Text)
			}
		}
		out = append(out, text.String())
	}
	return out
}

func waitForCapturedUserMessages(t *testing.T, capturePath string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		texts := capturedUserMessageTexts(t, capturePath)
		if len(texts) >= want {
			return texts
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d outbound user messages; got %d: %q", want, len(texts), texts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// installCapturingClaudeSession registers a live Claude session on the App
// whose subprocess records everything written to its stdin.
func installCapturingClaudeSession(t *testing.T, app *App, thread store.Thread, capturePath string) {
	t.Helper()
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{Binary: writeStdinCapturingClaudeBinary(t, capturePath), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.mu.Lock()
	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "provider-command-token",
		claude:   sess,
	}
	app.mu.Unlock()
}

func newClaudeThreadForProviderCommandTest(t *testing.T, app *App, id string) store.Thread {
	t.Helper()
	thread := testThread(id)
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return thread
}

// TestSendMessageWithOptions_ProviderCommandTransitionsOnOneThread is the
// transition test the flag needs: the SAME thread sends the SAME text twice,
// once marked as a provider command and once not, and the two must reach the
// CLI differently. A per-send flag that leaked into session state — or one
// that was never plumbed at all — passes a single-state test and fails this
// one.
func TestSendMessageWithOptions_ProviderCommandTransitionsOnOneThread(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})

	thread := newClaudeThreadForProviderCommandTest(t, app, "thread-provider-command")
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	installCapturingClaudeSession(t, app, thread, capturePath)

	if _, err := app.SendMessageWithOptions(thread.ID, "/usage", SendMessageOptions{ProviderCommand: true}); err != nil {
		t.Fatalf("flagged SendMessageWithOptions: %v", err)
	}
	texts := waitForCapturedUserMessages(t, capturePath, 1)
	if texts[0] != "/usage" {
		t.Fatalf("flagged send reached the CLI as %q, want %q — the slash guard must not fire on a deliberate command", texts[0], "/usage")
	}

	if _, err := app.SendMessageWithOptions(thread.ID, "/usage", SendMessageOptions{}); err != nil {
		t.Fatalf("unflagged SendMessageWithOptions: %v", err)
	}
	texts = waitForCapturedUserMessages(t, capturePath, 2)
	if texts[1] != "\n/usage" {
		t.Fatalf("unflagged send reached the CLI as %q, want %q — the previous send's opt-in must not persist", texts[1], "\n/usage")
	}
}

// TestRegisterQueueItem_ProviderCommandSurvivesTheFlushBoundary pins the
// queue round-trip: a command typed while a turn is running is queued as a
// payload and dispatched later, and the opt-in has to travel with it. The
// direct-send path cannot cover this — the flag is resolved from the decoded
// payload at dispatch time, on the other side of a JSON boundary.
func TestRegisterQueueItem_ProviderCommandSurvivesTheFlushBoundary(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := newClaudeThreadForProviderCommandTest(t, app, "thread-queued-provider-command")
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	installCapturingClaudeSession(t, app, thread, capturePath)

	// An open turn is what makes the message queue rather than send
	// directly, and on Claude it is also the eager-persist branch.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventTurnStart: %v", err)
	}

	queued, err := app.RegisterQueueItem(thread.ID, "/usage", SendMessageOptions{ProviderCommand: true})
	if err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	if !queued.ProviderCommand {
		t.Fatal("the returned queue item must mirror the opt-in so a client rendering the queue shows a command, not prose")
	}

	texts := waitForCapturedUserMessages(t, capturePath, 1)
	if texts[0] != "/usage" {
		t.Fatalf("queued command reached the CLI as %q, want %q — the opt-in must cross the queue payload", texts[0], "/usage")
	}
}

// TestRegisterQueueItem_UnflaggedQueuedTextStaysGuarded is the other half of
// the queue transition: an ordinary message that merely opens with a slash
// must still be guarded after the same round trip.
func TestRegisterQueueItem_UnflaggedQueuedTextStaysGuarded(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := newClaudeThreadForProviderCommandTest(t, app, "thread-queued-plain-slash")
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	installCapturingClaudeSession(t, app, thread, capturePath)

	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventTurnStart: %v", err)
	}

	queued, err := app.RegisterQueueItem(thread.ID, "/usage", SendMessageOptions{})
	if err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	if queued.ProviderCommand {
		t.Fatal("an unflagged register must not report itself as a provider command")
	}

	texts := waitForCapturedUserMessages(t, capturePath, 1)
	if texts[0] != "\n/usage" {
		t.Fatalf("unflagged queued text reached the CLI as %q, want %q", texts[0], "\n/usage")
	}
}
