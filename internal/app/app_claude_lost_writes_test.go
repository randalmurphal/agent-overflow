package app

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/triage"
)

// Claude sends system/session_writes_lost only as it exits with status 0,
// which is usually because AO closed it. Every way a session ends must put
// exactly one error row on the thread.

const lostWritesEnvelope = `{"type":"system","subtype":"session_writes_lost","error_code":"ENOSPC","lost":["transcript","global_config"],"transcript_session_ids":["sess-lost"],"uuid":"u-1","session_id":"sess-lost"}`

const lostWritesText = "Claude could not save the conversation transcript and its settings file (~/.claude.json) because the disk is full."

// lostWritesOnCloseScript writes the envelope once its stdin closes, as the
// CLI does when AO closes the session.
const lostWritesOnCloseScript = `#!/bin/sh
printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-lost","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"2.1.321"}'
while IFS= read -r _; do :; done
printf '%s\n' '` + lostWritesEnvelope + `'
exit 0
`

// lostWritesSelfExitScript exits on its own. Its init carries no session
// id, so no resume cursor exists and the death does not auto-reconnect into
// a second process that would report its own loss.
const lostWritesSelfExitScript = `#!/bin/sh
printf '%s\n' '{"type":"system","subtype":"init","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"2.1.321"}'
sleep 0.2
printf '%s\n' '` + lostWritesEnvelope + `'
exit 0
`

func startLostWritesSession(t *testing.T, script string) (*App, string) {
	t.Helper()
	app, bus := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "claude-lost-writes.sh")
	mockexec.Write(t, binary, script)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	return app, thread.ID
}

func lostWritesErrorRows(t *testing.T, app *App, threadID string) int {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var rows int
	for _, item := range items {
		if item.Kind == triage.ItemKindError && strings.HasPrefix(item.Summary, lostWritesText) {
			rows++
		}
	}
	return rows
}

func TestClaudeLostWritesReportedOnceAfterStop(t *testing.T) {
	t.Parallel()
	app, threadID := startLostWritesSession(t, lostWritesOnCloseScript)
	if err := app.StopSession(threadID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if got := lostWritesErrorRows(t, app, threadID); got != 1 {
		t.Fatalf("lost-writes error rows after Stop = %d, want 1", got)
	}
	// A second Stop finds no session and reports nothing more.
	if err := app.StopSession(threadID); err != nil {
		t.Fatalf("second StopSession: %v", err)
	}
	if got := lostWritesErrorRows(t, app, threadID); got != 1 {
		t.Fatalf("lost-writes error rows after a second Stop = %d, want 1", got)
	}
}

func TestClaudeLostWritesReportedOnceAfterIdleReap(t *testing.T) {
	t.Parallel()
	app, threadID := startLostWritesSession(t, lostWritesOnCloseScript)
	app.reapIdleSessions(time.Now().Add(idleReapThreshold + time.Hour))
	if _, present := app.sessionManager().get(threadID); present {
		t.Fatal("the idle reaper left the session running")
	}
	if got := lostWritesErrorRows(t, app, threadID); got != 1 {
		t.Fatalf("lost-writes error rows after the idle reaper = %d, want 1", got)
	}
}

func TestClaudeLostWritesReportedOnceAfterSelfExit(t *testing.T) {
	t.Parallel()
	app, threadID := startLostWritesSession(t, lostWritesSelfExitScript)
	waitUntil(t, 10*time.Second, func() bool {
		_, present := app.sessionManager().get(threadID)
		return !present && lostWritesErrorRows(t, app, threadID) > 0
	})
	app.drainProviderEvents(threadID, "test")
	if err := app.StopSession(threadID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if got := lostWritesErrorRows(t, app, threadID); got != 1 {
		t.Fatalf("lost-writes error rows after a self-exit = %d, want 1", got)
	}
}
