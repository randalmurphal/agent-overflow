package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// writeExitMarkerClaudeScript writes a mock Claude CLI that answers the
// first stdin line with init + result and writes a marker file on exit.
//
// The marker is what makes "the subprocess is gone" observable to a
// test: the script leaves it behind when its read loop ends, which
// happens when the close cascade shuts stdin (or signals the group).
// Asserting on the session map alone would pass a teardown that dropped
// the entry and left the process running, which is the exact leak
// archive-stops-the-session exists to close.
func writeExitMarkerClaudeScript(t *testing.T, dir, markerPath string) string {
	t.Helper()
	script := `#!/bin/bash
set -u
marker=` + shellSingleQuoteForBackground(markerPath) + `
trap 'printf exited > "$marker"' EXIT
idx=0
while IFS= read -r line; do
  case $idx in
    0)
      printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-archive","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
      printf '%s\n' '{"type":"result","subtype":"success","is_error":false}'
      ;;
    *) : ;;
  esac
  idx=$((idx+1))
done
exit 0
`
	path := filepath.Join(dir, "mock-claude-exit-marker.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock claude script: %v", err)
	}
	return path
}

// startExitMarkerClaudeSession brings a real (mocked) Claude session up
// on the thread and returns the path of the marker its subprocess
// writes when it exits.
func startExitMarkerClaudeSession(t *testing.T, app *App, bus *capturedEventBus, threadID string) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "provider-exited")
	binary := writeExitMarkerClaudeScript(t, t.TempDir(), marker)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(threadID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(threadID, "hello", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)
	return marker
}

// TestArchiveThreadClosesLiveProviderSession is the load-bearing case:
// archiving a thread with a live session takes the entry out of the
// session map AND ends the subprocess behind it, rather than flipping a
// column and leaving the process running for the life of the host.
//
// Driven through the mocked CLI so the real close path runs — the
// provider process, its stdin close, and the group signal cascade — not
// a nil provider handle a broken teardown would slip past.
//
// The thread is left holding a running background tool call on purpose.
// That is precisely the state the idle reaper refuses to touch; archive
// is an explicit stop and proceeds anyway, which is the difference
// between the two mechanisms.
func TestArchiveThreadClosesLiveProviderSession(t *testing.T) {
	app, bus := setupE2EApp(t)

	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	marker := startExitMarkerClaudeSession(t, app, bus, thread.ID)

	if _, live := app.sessionManager().get(thread.ID); !live {
		t.Fatal("expected a live session before archiving")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("provider subprocess exited before the archive; the test proves nothing")
	}

	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID:           "bg-archive-1",
		ThreadID:     thread.ID,
		TurnIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		IsBackground: true,
		Summary:      "Bash: long-running dev server",
		ToolName:     "Bash",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed running background row: %v", err)
	}

	if err := app.ArchiveThread(thread.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}

	if _, stillLive := app.sessionManager().get(thread.ID); stillLive {
		t.Fatal("archived thread still holds a registered session")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("provider subprocess survived the archive (no exit marker): %v", err)
	}
	archived, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !archived.Archived {
		t.Fatal("thread is not archived")
	}
}

// TestArchiveThreadMidTurnSettlesTheOpenTurn covers the state a reader
// is left looking at when they archive while the agent is working. The
// close routes through the shared teardown, whose triage cleanup
// synthesizes a turn-complete for the in-flight turn, so the turn row
// lands with a completed_at instead of staying open and re-rendering the
// thread as working on the next load.
func TestArchiveThreadMidTurnSettlesTheOpenTurn(t *testing.T) {
	app, _ := setupE2EApp(t)

	thread := e2eThread("thread-archive-midturn", string(provider.Claude), t.TempDir())
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Open a turn through the router exactly as a live provider would,
	// then register a session for the thread. No subprocess here: this
	// case is about what the timeline records, and closeProviderSession
	// is a no-op on an entry with no provider handle.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		TurnID:    "turn-midturn",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("open turn: %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Liveness: newSessionLiveness(time.Now()),
	})

	if err := app.ArchiveThread(thread.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}

	if _, stillLive := app.sessionManager().get(thread.ID); stillLive {
		t.Fatal("archived thread still holds a registered session")
	}
	turns, err := app.store.ListRecentTurns(thread.ID, 1)
	if err != nil {
		t.Fatalf("ListRecentTurns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turn count = %d, want 1", len(turns))
	}
	if turns[0].CompletedAt == nil {
		t.Fatal("archive left the in-flight turn open; the thread would still render as working")
	}
}

// TestArchiveThreadKeepsSessionWhenATurnStartedAfterTheRequest is the
// stop-time re-check. ArchiveThread stamps the request instant before it
// queues on the per-thread action lock, and a send already holding that
// lock can start a turn while the archive waits — the archive is stale
// by the time it runs, and must not kill work the reader asked for after
// they archived.
//
// The gap itself is a lock wait and not addressable from a test, so the
// turn carries a start ahead of the request instant, which is the same
// fact the re-check reads.
func TestArchiveThreadKeepsSessionWhenATurnStartedAfterTheRequest(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-archive-reengaged")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-after",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: time.Now().Add(time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Liveness: newSessionLiveness(time.Now()),
	})

	if err := app.ArchiveThread(thread.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}

	if _, live := app.sessionManager().get(thread.ID); !live {
		t.Fatal("a stale archive killed a session whose turn started after the archive request")
	}
	// The archive itself still stands: re-engagement cancels the stop,
	// not the reader's statement about where the thread belongs.
	archived, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !archived.Archived {
		t.Fatal("re-engagement wrongly cancelled the archive flag")
	}
}

// TestArchiveThreadStopsSessionWhoseTurnStartedBeforeTheRequest is the
// other half of the same discriminator. A turn that was already running
// when the reader archived is exactly what archive exists to stop, so
// its presence must not read as re-engagement.
func TestArchiveThreadStopsSessionWhoseTurnStartedBeforeTheRequest(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-archive-midflight")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-before",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: time.Now().Add(-time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Liveness: newSessionLiveness(time.Now()),
	})

	if err := app.ArchiveThread(thread.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	if _, live := app.sessionManager().get(thread.ID); live {
		t.Fatal("archive left a session running whose turn predates the archive request")
	}
}

// TestArchiveThreadWithoutSessionStaysARowFlip pins the common case:
// archiving a thread nobody is running touches the session map and
// nothing else, and still archives.
func TestArchiveThreadWithoutSessionStaysARowFlip(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-archive-cold")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if err := app.ArchiveThread(thread.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}

	archived, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !archived.Archived {
		t.Fatal("thread is not archived")
	}
	if _, live := app.sessionManager().get(thread.ID); live {
		t.Fatal("archiving a cold thread registered a session")
	}
}

// TestUnarchiveThreadDoesNotResurrectTheSession pins the return trip.
// Unarchive restores sidebar visibility and nothing else — no session,
// no subprocess. A thread that comes back is as cold as any thread the
// reaper closed, and the next send is what starts a process.
func TestUnarchiveThreadDoesNotResurrectTheSession(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-unarchive")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Liveness: newSessionLiveness(time.Now()),
	})

	if err := app.ArchiveThread(thread.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	if _, live := app.sessionManager().get(thread.ID); live {
		t.Fatal("archive did not close the session")
	}

	restored, err := app.UnarchiveThread(thread.ID)
	if err != nil {
		t.Fatalf("UnarchiveThread: %v", err)
	}
	if restored.Archived {
		t.Fatal("unarchive did not clear the archived flag")
	}
	if _, live := app.sessionManager().get(thread.ID); live {
		t.Fatal("unarchive started a provider session; a restored thread stays cold until the reader sends")
	}
}

// TestSendAfterArchiveStartsAColdSession is the behavioral half of the
// same rule, through the real start path: once archive has closed the
// session, the next send lazy-starts a fresh subprocess exactly as it
// would on a thread that never had one. Being archived makes the thread
// no less usable, and nothing auto-restarts it either.
func TestSendAfterArchiveStartsAColdSession(t *testing.T) {
	app, bus := setupE2EApp(t)

	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	startExitMarkerClaudeSession(t, app, bus, thread.ID)

	first, live := app.sessionManager().get(thread.ID)
	if !live {
		t.Fatal("expected a live session before archiving")
	}

	if err := app.ArchiveThread(thread.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	if _, stillLive := app.sessionManager().get(thread.ID); stillLive {
		t.Fatal("archive did not close the session")
	}

	if err := app.SendMessage(thread.ID, "still here", nil); err != nil {
		t.Fatalf("SendMessage after archive: %v", err)
	}
	second, restarted := app.sessionManager().get(thread.ID)
	if !restarted {
		t.Fatal("send after archive did not start a session")
	}
	if second.Token == first.Token {
		t.Fatal("send after archive reused the closed session's token; it must be a fresh cold start")
	}
}
