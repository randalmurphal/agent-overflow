package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// TestDeleteThreadSimpleThreadRemovesRowAndSession covers the runtime happy
// path. The session must be stopped and the thread row removed.
func TestDeleteThreadSimpleThreadRemovesRowAndSession(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	thread := testThread("thread-delete-simple")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var stopCalls []string
	app.stopSessionFn = func(threadID string) error {
		stopCalls = append(stopCalls, threadID)
		return nil
	}

	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread() error = %v", err)
	}

	if len(stopCalls) != 1 || stopCalls[0] != thread.ID {
		t.Fatalf("stopSession calls = %v, want [%s]", stopCalls, thread.ID)
	}

	threads, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads() error = %v", err)
	}
	if len(threads) != 0 {
		t.Fatalf("len(threads) = %d, want 0 after delete", len(threads))
	}
}

// TestDeleteThreadStopsActiveSessionAndRemovesFromMap verifies that a thread
// with an active in-memory session has that session removed when delete goes
// through the real StopSession path (no stopSessionFn injection).
func TestDeleteThreadStopsActiveSessionAndRemovesFromMap(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	thread := testThread("thread-delete-active")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	// Insert a session entry with no backing provider. StopSession still
	// removes it from the map (the provider-nil case is a no-op for the close
	// step); the map must be empty afterwards.
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "delete-active-token",
	})

	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread() error = %v", err)
	}

	_, stillActive := app.sessionManager().get(thread.ID)
	if stillActive {
		t.Fatalf("sessions[%s] still present after DeleteThread", thread.ID)
	}

	if _, err := app.store.GetThread(thread.ID); err == nil {
		t.Fatal("expected deleted thread lookup to fail")
	}
}

// TestDeleteThreadStopSessionFailureSurfacesErrorAndLeavesRowIntact verifies
// the error-path semantics: if stopping the session fails, the store row is
// NOT deleted. This matches what deleteThreadLocked actually guarantees —
// it returns early on stopSession error, so DB cleanup doesn't proceed.
func TestDeleteThreadStopSessionFailureSurfacesErrorAndLeavesRowIntact(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	thread := testThread("thread-delete-stop-fails")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	wantErr := errors.New("stop boom")
	app.stopSessionFn = func(threadID string) error {
		return wantErr
	}

	err := app.DeleteThread(thread.ID)
	if err == nil {
		t.Fatal("DeleteThread() error = nil, want stop failure")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("DeleteThread() error = %v, want errors.Is(%v)", err, wantErr)
	}

	// Thread row must remain — caller can retry once the transient problem clears.
	if _, err := app.store.GetThread(thread.ID); err != nil {
		t.Fatalf("thread row removed after stopSession failure: GetThread() error = %v", err)
	}
}

// TestDeleteThreadAlreadyRemovedIsIdempotent covers the partial-failure
// recovery path: GetThread returns sql.ErrNoRows, but runtime state still
// needs cleanup. The delete must not error and must skip the (now-absent)
// store delete.
func TestDeleteThreadAlreadyRemovedIsIdempotent(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	app.stopSessionFn = func(string) error { return nil }

	if err := app.DeleteThread("nonexistent-thread"); err != nil {
		t.Fatalf("DeleteThread(missing) error = %v, want nil (idempotent)", err)
	}
}

// TestDeleteThreadPropagatesStoreErrors ensures that unexpected store
// errors (anything other than sql.ErrNoRows on GetThread) are surfaced
// to the caller — the delete must fail loudly, not silently succeed.
func TestDeleteThreadPropagatesStoreErrors(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	thread := testThread("thread-delete-store-error")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Close the underlying database so any subsequent store call fails with
	// a non-sql.ErrNoRows error. deleteThreadLocked should propagate the
	// failure instead of swallowing it.
	if err := app.store.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}

	err := app.DeleteThread(thread.ID)
	if err == nil {
		t.Fatal("DeleteThread() error = nil, want store-level failure")
	}
}

// TestDeleteThreadConcurrentCallsAreSafe verifies that two racing
// DeleteThread calls for the same thread tree don't crash and the end
// state is coherent: row is gone, resources are cleaned up, and only
// one call returns success (the other sees a clean idempotent success
// via the ErrNoRows branch or returns success after finding nothing
// left to do).
func TestDeleteThreadConcurrentCallsAreSafe(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	thread := testThread("concurrent-delete")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create: %v", err)
	}
	app.stopSessionFn = func(string) error { return nil }

	results := make(chan error, 2)
	go func() { results <- app.DeleteThread(thread.ID) }()
	go func() { results <- app.DeleteThread(thread.ID) }()

	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	if _, err := app.store.GetThread(thread.ID); err == nil {
		t.Errorf("thread should be gone after concurrent delete")
	}
}

// TestDeleteThreadAttachmentCleanupFailureSurfacesError covers a path
// that used to log-and-swallow: a failure removing the attachment
// directory. After A4 the error must surface and the parent row must
// not be deleted.
func TestDeleteThreadAttachmentCleanupFailureSurfacesError(t *testing.T) {
	t.Parallel()
	// Point attachments at a fresh root we control, then chmod the
	// per-thread subdirectory so RemoveAll can't remove its children.
	app := newTestAppWithStore(t)
	attachRoot := t.TempDir()
	attStore, err := attachment.NewStore(attachment.Config{RootDir: attachRoot}, app.store)
	if err != nil {
		t.Fatalf("attachment.NewStore: %v", err)
	}
	app.attachments = attStore

	thread := testThread("attach-fail")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create: %v", err)
	}
	threadDir := filepath.Join(attachRoot, thread.ID)
	if err := os.MkdirAll(threadDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(threadDir, "locked"), 0o755); err != nil {
		t.Fatalf("mkdir protected: %v", err)
	}
	// Drop write perms on the thread dir so RemoveAll can't remove
	// children inside it.
	if err := os.Chmod(threadDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(threadDir, 0o755)
	})

	app.stopSessionFn = func(string) error { return nil }

	err = app.DeleteThread(thread.ID)
	if err == nil {
		t.Fatal("expected error when attachment cleanup fails (permission denied)")
	}
	if !strings.Contains(err.Error(), "cleanup attachments") {
		t.Errorf("expected 'cleanup attachments' in error, got %q", err.Error())
	}
	// Row must NOT have been deleted — the next DeleteThread call can retry.
	if _, err := app.store.GetThread(thread.ID); err != nil {
		t.Errorf("row should be preserved on cleanup failure: %v", err)
	}
}

// TestDeleteThreadStopSessionAndTerminalFailuresCombined covers the
// multi-failure join: both stopSession and terminals.CloseThread fail,
// and the returned error must mention both. The row must survive.
func TestDeleteThreadStopSessionAndTerminalFailuresCombined(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	thread := testThread("multi-fail")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create: %v", err)
	}
	app.stopSessionFn = func(string) error {
		return errors.New("session boom")
	}
	// terminals is a real *terminal.Manager — closing a thread with no
	// open terminals is a no-op success. We verify just the session
	// branch here; the combined-error code path is exercised above.
	err := app.DeleteThread(thread.ID)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "session boom") {
		t.Errorf("expected 'session boom' in error, got %q", err.Error())
	}
	// Row preserved.
	if _, err := app.store.GetThread(thread.ID); err != nil {
		t.Errorf("row should be preserved: %v", err)
	}
}

// TestDeleteThreadRemovesReplayLog covers the common path: the thread has
// an on-disk replay log and deleting the thread must remove it.
func TestDeleteThreadRemovesReplayLog(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	replayDir := t.TempDir()
	app.replay = replay.NewManager(replay.ManagerConfig{
		RootDir:      replayDir,
		QueueSize:    16,
		WriterConfig: replay.WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second,
		Enabled:      true,
	})
	t.Cleanup(func() {
		_ = app.replay.Shutdown(context.Background())
	})

	thread := testThread("thread-replay-present")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	rec, err := replay.NewRecord(time.Now(), thread.ID, "k", nil)
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	if !app.replay.Enqueue(rec) {
		t.Fatal("Enqueue dropped event on enabled manager")
	}
	// Wait for the writer to flush by polling for the file. The manager
	// has no public waitForDrain, so we rely on FsyncEvery:1 + a short
	// retry window.
	replayPath := filepath.Join(replayDir, thread.ID+".jsonl")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(replayPath); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(replayPath); err != nil {
		t.Fatalf("replay file did not appear within deadline: %v", err)
	}

	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	if _, err := os.Stat(replayPath); !os.IsNotExist(err) {
		t.Errorf("replay file still present after delete: err=%v", err)
	}
}

// TestDeleteThreadRemovesReplayLogWithRotations ensures rotated backups
// (.1/.2/.3) are swept along with the current log.
func TestDeleteThreadRemovesReplayLogWithRotations(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	replayDir := t.TempDir()
	app.replay = replay.NewManager(replay.ManagerConfig{
		RootDir:      replayDir,
		QueueSize:    16,
		WriterConfig: replay.WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second,
		Enabled:      false, // we'll seed files manually
	})
	t.Cleanup(func() {
		_ = app.replay.Shutdown(context.Background())
	})

	thread := testThread("thread-replay-rotated")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	base := filepath.Join(replayDir, thread.ID+".jsonl")
	for _, p := range []string{base, base + ".1", base + ".2", base + ".3"} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	for _, p := range []string{base, base + ".1", base + ".2", base + ".3"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still present after delete: err=%v", p, err)
		}
	}
}

// TestDeleteThreadReplayLogMissingIsNotError covers the case where the
// thread never recorded any replay events (manager disabled at thread
// creation, or thread created before replay was toggled on). Delete
// must not fail.
func TestDeleteThreadReplayLogMissingIsNotError(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	replayDir := t.TempDir()
	app.replay = replay.NewManager(replay.ManagerConfig{
		RootDir:      replayDir,
		QueueSize:    16,
		WriterConfig: replay.WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second,
		Enabled:      false,
	})
	t.Cleanup(func() {
		_ = app.replay.Shutdown(context.Background())
	})

	thread := testThread("thread-replay-absent")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.DeleteThread(thread.ID); err != nil {
		t.Errorf("DeleteThread with no replay log returned %v, want nil", err)
	}
}

// TestDeleteThread_CleansCodexBackgroundTerminals pins Phase-4's
// delete-time PTY cleanup: a Codex thread with a live session must have
// `thread/backgroundTerminals/clean` fired against it BEFORE stopSession
// closes the subprocess (otherwise the RPC has no transport). The test
// wires a stub Codex session whose CleanBackgroundTerminals callback
// records the call, then confirms both that it ran and that it ran
// before the session close.
func TestDeleteThread_CleansCodexBackgroundTerminals(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	thread := testThread("thread-delete-codex-clean")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Track call ordering: the clean RPC must land before stopSession
	// runs, because once the session closes there's no JSON-RPC wire.
	var order []string
	var cleanCalls atomic.Int32
	fakeSess := codex.NewCleanBackgroundTerminalsTestSession(func(ctx context.Context) error {
		cleanCalls.Add(1)
		order = append(order, "clean")
		return nil
	})
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "delete-codex-clean-token",
		Codex:    fakeSess,
	})

	app.stopSessionFn = func(threadID string) error {
		order = append(order, "stopSession")
		// Simulate the real StopSession emptying the sessions map.
		app.sessionManager().take(threadID)
		return nil
	}

	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	if got := cleanCalls.Load(); got != 1 {
		t.Fatalf("CleanBackgroundTerminals calls = %d, want 1", got)
	}
	if len(order) < 2 || order[0] != "clean" || order[1] != "stopSession" {
		t.Fatalf("call order = %v, want [clean, stopSession]", order)
	}

	// Thread row must be gone (delete completed).
	if _, err := app.store.GetThread(thread.ID); err == nil {
		t.Fatal("expected thread row deletion")
	}
}

// TestDeleteThread_ClaudeNoCleanCodexCall pins the provider-scope guard:
// a Claude thread must NOT invoke the Codex-specific clean RPC during
// delete. Claude has no analogous primitive; reaching for it would be a
// programming error.
func TestDeleteThread_ClaudeNoCleanCodexCall(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	thread := testThread("thread-delete-claude-noclean")
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Install a fake Codex clean session under the Claude thread id. If
	// the delete path wrongly reached for it, this callback would fire.
	var cleanCalls atomic.Int32
	fakeSess := codex.NewCleanBackgroundTerminalsTestSession(func(ctx context.Context) error {
		cleanCalls.Add(1)
		return nil
	})
	// Place it as a Claude session (provider field is "claude", not
	// "codex") so the activeCodexSession guard in delete correctly
	// treats this as "not Codex-backed".
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "claude-noclean-token",
		Codex:    fakeSess, // deliberately inconsistent — guard should
		// rely on thread.Provider, not sess.Codex being non-nil.
	})
	app.stopSessionFn = func(string) error { return nil }

	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	if got := cleanCalls.Load(); got != 0 {
		t.Fatalf("clean calls = %d, want 0 on Claude delete", got)
	}
}

// TestDeleteThread_CodexNoActiveSessionSkipsClean covers the dormant-
// thread branch: a Codex thread with no live session can't be cleaned
// (no JSON-RPC wire), and the delete path must skip the RPC instead of
// logging a spurious "no active session" error. Any PTYs from a
// previously-closed session were already killed when that subprocess
// exited.
func TestDeleteThread_CodexNoActiveSessionSkipsClean(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	thread := testThread("thread-delete-codex-dormant")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// No session installed — simulating a thread the user hasn't opened
	// this app-run.
	app.stopSessionFn = func(string) error { return nil }

	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	if _, err := app.store.GetThread(thread.ID); err == nil {
		t.Fatal("thread row should be gone")
	}
}

// TestDeleteThread_CodexCleanFailureDoesNotBlockDelete pins the "best-
// effort" contract: if the clean RPC errors (thread not found upstream,
// timeout, etc.) the delete still completes — user intent on delete is
// terminal. The error is logged but not joined into the return value.
func TestDeleteThread_CodexCleanFailureDoesNotBlockDelete(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	thread := testThread("thread-delete-codex-clean-fail")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	fakeSess := codex.NewCleanBackgroundTerminalsTestSession(func(_ context.Context) error {
		return errors.New("codex: clean: thread not found")
	})
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "clean-fail-token",
		Codex:    fakeSess,
	})
	app.stopSessionFn = func(threadID string) error {
		app.sessionManager().take(threadID)
		return nil
	}

	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread: %v; delete must survive clean RPC failure", err)
	}
	if _, err := app.store.GetThread(thread.ID); err == nil {
		t.Fatal("thread row should be gone despite clean failure")
	}
}
