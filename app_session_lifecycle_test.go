package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/settings"
)

func TestStaleSessionDisconnectDoesNotRemoveReplacement(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-stale")
	thread.SessionRef = "provider-session-1"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "session-current",
	}

	started := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}

	app.sessionEventHandler(thread.ID, "session-stale", "")(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})

	if _, err := app.SwitchThread(thread.ID); err != nil {
		t.Fatalf("SwitchThread() error = %v", err)
	}
	if err := app.AutoResumeThread(thread.ID); err != nil {
		t.Fatalf("AutoResumeThread() error = %v", err)
	}

	select {
	case threadID := <-started:
		t.Fatalf("unexpected auto-resume after stale disconnect for %s", threadID)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestServiceShutdownClosesSessionsWithoutDeadlock(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-shutdown")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		app.sessionEventHandler(thread.ID, "shutdown-token", ""),
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "shutdown-token",
		claude:   sess,
	}

	done := make(chan error, 1)
	go func() {
		done <- app.ServiceShutdown()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServiceShutdown() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ServiceShutdown")
	}
}

// TestServiceShutdownTreatsSubprocessExitAsCleanClose is the
// counterpart to the interrupt-then-revert "Revert failed: Exit status
// 1" fix in Process.Close — a session whose subprocess exited non-zero
// (e.g. an interrupted Claude CLI) is not a shutdown failure. The
// process is gone, which is what shutdown was asking for. Surfacing
// the exit code at this level caused user-visible errors on flows
// (shutdown, revert, replacement-session start) where the prior
// session had already terminated abnormally.
func TestServiceShutdownTreatsSubprocessExitAsCleanClose(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-shutdown-error")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// `false` exits with code 1 immediately — by the time
	// ServiceShutdown closes the session, cmd.Wait has captured an
	// *exec.ExitError. Pre-fix this propagated up as
	// "close claude session for thread <id>: exit status 1".
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  "false",
			WorkDir: thread.WorkspacePath,
		},
		app.sessionEventHandler(thread.ID, "shutdown-error-token", ""),
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "shutdown-error-token",
		claude:   sess,
	}

	if err := app.ServiceShutdown(); err != nil {
		t.Fatalf("ServiceShutdown() error = %v, want nil (subprocess exit is not a shutdown failure)", err)
	}
}

// TestStartSessionProceedsWhenPriorSubprocessExitsNonZero is the
// counterpart to TestServiceShutdownTreatsSubprocessExitAsCleanClose
// for the start-replacement path. Same root cause: a Claude CLI that
// was interrupted then asked to close exits non-zero, and that exit
// is the goal of stopExistingSessionLocked, not a failure of it.
// Pre-fix, the exit error bubbled out of startSessionNow as "close
// claude session for thread <id>: exit status 1" — visible as
// "Revert failed: Exit status 1" when the same start-replacement
// flow ran under RevertToMessageCheckpoint after an interrupt. The
// replacement also never started, so the user had to retry. Post-fix,
// the replacement starts on the first attempt and the previous
// subprocess's non-zero exit is treated as the clean teardown it
// actually was.
func TestStartSessionProceedsWhenPriorSubprocessExitsNonZero(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	t.Cleanup(func() { _ = app.ServiceShutdown() })

	thread := testThread("thread-start-replace-close-error")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	startMarker := filepath.Join(t.TempDir(), "replacement-started")
	replacementBinary := writeClaudeMarkerBinary(t, startMarker)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": replacementBinary}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	existing, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudeFailOnCloseBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		app.sessionEventHandler(thread.ID, "replace-close-error-token", ""),
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "replace-close-error-token",
		claude:   existing,
	}

	// A stream killed by replacement never delivers the final tick that
	// clears its highlight-seeder state; stopExistingSessionLocked must
	// purge it (unregisterSession can't — the token is taken before the
	// close, so its callback no-ops for this path).
	app.remoteClientProbeFn = func() bool { return true }
	app.observeAssistantTextStream(thread.ID, "stranded-item", "```python\npass", false)
	waitForSeedStates(t, app, 1)

	// A delta still sitting in the triage stream-persist buffer is the
	// other re-registration path: its 250ms flush fires the observer
	// AFTER the purge unless stopExistingSessionLocked drains the
	// buffers first (the replacement path has no CleanupThread to do
	// it). Two deltas: the first creates the streaming row, the second
	// buffers and arms the timer.
	app.ensureTriageRouter()
	for _, content := range []string{"```python\npending", " = 1"} {
		if err := app.triage.Handle(provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: thread.ID,
			Content: content, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("Handle(text delta) error = %v", err)
		}
	}

	if err := app.startSessionNow(thread.ID); err != nil {
		t.Fatalf("startSessionNow() error = %v, want nil (prior subprocess exit is not a close failure)", err)
	}

	if got := app.seedStateCount(); got != 0 {
		t.Fatalf("replacement start must purge stranded seeder states, got %d", got)
	}
	// Past the stream-persist flush window: a timer the drain missed
	// would have re-registered the old stream's state by now.
	time.Sleep(500 * time.Millisecond)
	if got := app.seedStateCount(); got != 0 {
		t.Fatalf("delayed stream flush re-registered purged seeder state: %d", got)
	}

	// The marker write happens inside the shell script before `cat`
	// blocks on stdin; once Spawn returned to NewSession the file
	// must exist. We poll briefly to absorb the cross-process
	// scheduling lag rather than racing fs metadata.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, statErr := os.Stat(startMarker); statErr == nil {
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("stat replacement marker: %v", statErr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("replacement session start marker %s not created within 2s", startMarker)
		}
		time.Sleep(10 * time.Millisecond)
	}

	app.mu.Lock()
	got, ok := app.sessions[thread.ID]
	app.mu.Unlock()
	if !ok {
		t.Fatalf("sessions[%s] missing after replacement start", thread.ID)
	}
	if got.token == "replace-close-error-token" {
		t.Fatal("sessions[thread] still holds the prior token, want replacement")
	}
	if got.claude == nil {
		t.Fatal("sessions[thread].claude = nil, want replacement claude session")
	}
}

func writeClaudePassthroughBinary(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "claude-passthrough.sh")
	script := "#!/bin/sh\ncat >/dev/null\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

// writeClaudeInterruptResponderBinary is a fake-CLI script that
// acknowledges every interrupt control_request with a synthetic
// success control_response. Use this in tests where InterruptTurn
// must take the clean round-trip path — the passthrough binary
// doesn't respond, so it would force the 10s default timeout per
// test. Case alternation accepts either field order because
// json.Marshal on map[string]any sorts keys alphabetically.
func writeClaudeInterruptResponderBinary(t *testing.T) string {
	t.Helper()
	script := `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"type":"control_request"'*'"subtype":"interrupt"'* | *'"subtype":"interrupt"'*'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`
	path := filepath.Join(t.TempDir(), "claude-interrupt-responder.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func writeClaudeFailOnCloseBinary(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "claude-fail-on-close.sh")
	script := "#!/bin/sh\ncat >/dev/null\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func writeClaudeMarkerBinary(t *testing.T, markerPath string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "claude-marker.sh")
	script := "#!/bin/sh\nprintf started >" + shellQuote(markerPath) + "\ncat >/dev/null\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}
