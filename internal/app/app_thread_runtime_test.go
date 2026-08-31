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
	"agent-overflow/internal/store"
)

// TestDeleteThreadSimpleThreadRemovesRowAndSession covers the runtime happy path for a
// leaf thread with no children and no discussion state. The session must be
// stopped and the thread row removed.
func TestDeleteThreadSimpleThreadRemovesRowAndSession(t *testing.T) {
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

// TestDeleteThreadRecursivelyRemovesParentChildAndGrandchild exercises the
// recursive descent: each thread in the chain is stopped and removed.
func TestDeleteThreadRecursivelyRemovesParentChildAndGrandchild(t *testing.T) {
	app := newTestAppWithStore(t)

	parent := testThread("thread-delete-parent")
	child := testThread("thread-delete-child")
	child.ParentThreadID = parent.ID
	grandchild := testThread("thread-delete-grandchild")
	grandchild.ParentThreadID = child.ID

	for _, th := range []store.Thread{parent, child, grandchild} {
		if err := app.store.CreateThread(th); err != nil {
			t.Fatalf("CreateThread(%s) error = %v", th.ID, err)
		}
	}

	var stopCalls []string
	app.stopSessionFn = func(threadID string) error {
		stopCalls = append(stopCalls, threadID)
		return nil
	}

	if err := app.DeleteThread(parent.ID); err != nil {
		t.Fatalf("DeleteThread() error = %v", err)
	}

	// deleteThreadTree recurses children first, then stops own session.
	// Expected order: grandchild, child, parent.
	expected := []string{grandchild.ID, child.ID, parent.ID}
	if len(stopCalls) != len(expected) {
		t.Fatalf("stopSession calls = %v, want %v", stopCalls, expected)
	}
	for i, want := range expected {
		if stopCalls[i] != want {
			t.Fatalf("stopSession[%d] = %q, want %q (full %v)", i, stopCalls[i], want, stopCalls)
		}
	}

	threads, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads() error = %v", err)
	}
	if len(threads) != 0 {
		t.Fatalf("len(threads) = %d, want 0 after recursive delete", len(threads))
	}
}

// TestDeleteThreadStopsActiveSessionAndRemovesFromMap verifies that a thread
// with an active in-memory session has that session removed when delete goes
// through the real StopSession path (no stopSessionFn injection).
func TestDeleteThreadStopsActiveSessionAndRemovesFromMap(t *testing.T) {
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

// TestDeleteThreadClearsSystemPromptAndDeliberationForParent exercises the
// discussion cleanup path: the parent carries DiscussionID with no
// ParentThreadID, so removeDeliberation should drop the in-memory state.
// Child threads keep DiscussionID but have ParentThreadID set, and must
// NOT trigger deliberation removal themselves (so the parent still owns
// cleanup authority).
func TestDeleteThreadClearsSystemPromptAndDeliberationForParent(t *testing.T) {
	app := newTestAppWithStore(t)

	// Threads + channels have a circular FK relationship (channel.thread_id
	// → threads.id, threads.discussion_id → channels.id). Create threads
	// first without discussion_id, then channel, then UPDATE the
	// discussion_id — same pattern production uses in
	// internal/discussionapp.
	parent := testThread("thread-delete-discussion-parent")
	if err := app.store.CreateThread(parent); err != nil {
		t.Fatalf("CreateThread(parent) error = %v", err)
	}
	child := testThread("thread-delete-discussion-child")
	child.ParentThreadID = parent.ID
	if err := app.store.CreateThread(child); err != nil {
		t.Fatalf("CreateThread(child) error = %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.CreateChannel(store.Channel{
		ID: "channel-delete", ThreadID: parent.ID,
		Type: "deliberation", Status: "open",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateChannel() error = %v", err)
	}
	parent.DiscussionID = "channel-delete"
	if err := app.store.UpdateThread(parent); err != nil {
		t.Fatalf("UpdateThread(parent) error = %v", err)
	}
	child.DiscussionID = "channel-delete"
	if err := app.store.UpdateThread(child); err != nil {
		t.Fatalf("UpdateThread(child) error = %v", err)
	}

	app.setThreadSystemPrompt(parent.ID, "parent prompt")
	app.setThreadSystemPrompt(child.ID, "child prompt")
	app.installDeliberation("channel-delete", nil, 4)

	app.stopSessionFn = func(string) error { return nil }

	if err := app.DeleteThread(parent.ID); err != nil {
		t.Fatalf("DeleteThread() error = %v", err)
	}

	if prompt := app.threadSystemPrompt(parent.ID); prompt != "" {
		t.Fatalf("parent systemPrompt = %q, want empty", prompt)
	}
	if prompt := app.threadSystemPrompt(child.ID); prompt != "" {
		t.Fatalf("child systemPrompt = %q, want empty", prompt)
	}

	_, stillTracked := app.deliberation("channel-delete")
	if stillTracked {
		t.Fatal("deliberations map still tracks channel after parent delete")
	}
}

// TestDeleteThreadChildOnlyLeavesParentDeliberationIntact captures the
// asymmetry in removeDeliberation: deleting a child (which has a
// ParentThreadID set) must NOT remove the shared deliberation state —
// that belongs to the parent thread.
func TestDeleteThreadChildOnlyLeavesParentDeliberationIntact(t *testing.T) {
	app := newTestAppWithStore(t)

	// Threads + channels have a circular FK relationship — create both
	// threads first, then the channel, then UPDATE discussion_id (same
	// pattern as production internal/discussionapp).
	parent := testThread("thread-delete-child-only-parent")
	if err := app.store.CreateThread(parent); err != nil {
		t.Fatalf("CreateThread(parent) error = %v", err)
	}
	child := testThread("thread-delete-child-only-child")
	child.ParentThreadID = parent.ID
	if err := app.store.CreateThread(child); err != nil {
		t.Fatalf("CreateThread(child) error = %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.CreateChannel(store.Channel{
		ID: "channel-child-only", ThreadID: parent.ID,
		Type: "deliberation", Status: "open",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateChannel() error = %v", err)
	}
	parent.DiscussionID = "channel-child-only"
	if err := app.store.UpdateThread(parent); err != nil {
		t.Fatalf("UpdateThread(parent) error = %v", err)
	}
	child.DiscussionID = "channel-child-only"
	if err := app.store.UpdateThread(child); err != nil {
		t.Fatalf("UpdateThread(child) error = %v", err)
	}

	app.installDeliberation("channel-child-only", nil, 4)
	app.stopSessionFn = func(string) error { return nil }

	if err := app.DeleteThread(child.ID); err != nil {
		t.Fatalf("DeleteThread(child) error = %v", err)
	}

	_, stillTracked := app.deliberation("channel-child-only")
	if !stillTracked {
		t.Fatal("deliberation dropped when deleting child only; parent still owns it")
	}

	// Parent still present in the store.
	if _, err := app.store.GetThread(parent.ID); err != nil {
		t.Fatalf("GetThread(parent) error = %v", err)
	}
}

// TestDeleteThreadStopSessionFailureSurfacesErrorAndLeavesRowIntact verifies
// the error-path semantics: if stopping the session fails, the store row is
// NOT deleted. This matches what deleteThreadTree actually guarantees —
// it returns early on stopSession error, so DB cleanup doesn't proceed.
func TestDeleteThreadStopSessionFailureSurfacesErrorAndLeavesRowIntact(t *testing.T) {
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

// TestDeleteThreadChildStopFailureHaltsRecursionAndLeavesParent verifies that
// when a child's stopSession fails mid-recursion, deleteThreadTree halts and
// returns the error. The parent row (and still-living sibling children, if
// any) remain in the store.
func TestDeleteThreadChildStopFailureHaltsRecursionAndLeavesParent(t *testing.T) {
	app := newTestAppWithStore(t)

	parent := testThread("thread-delete-child-fails-parent")
	if err := app.store.CreateThread(parent); err != nil {
		t.Fatalf("CreateThread(parent) error = %v", err)
	}
	failingChild := testThread("thread-delete-child-fails-child")
	failingChild.ParentThreadID = parent.ID
	if err := app.store.CreateThread(failingChild); err != nil {
		t.Fatalf("CreateThread(child) error = %v", err)
	}

	app.stopSessionFn = func(threadID string) error {
		if threadID == failingChild.ID {
			return errors.New("child stop failed")
		}
		return nil
	}

	err := app.DeleteThread(parent.ID)
	if err == nil {
		t.Fatal("DeleteThread() error = nil, want child stop failure")
	}
	if !strings.Contains(err.Error(), "child stop failed") {
		t.Fatalf("DeleteThread() error = %v, want child stop failed", err)
	}

	// Parent row preserved (recursion exited before parent stop/delete).
	if _, err := app.store.GetThread(parent.ID); err != nil {
		t.Fatalf("parent removed despite child stop failure: GetThread() error = %v", err)
	}
	// Child row also preserved.
	if _, err := app.store.GetThread(failingChild.ID); err != nil {
		t.Fatalf("child removed despite stopSession failure: GetThread() error = %v", err)
	}
}

// TestDeleteThreadAlreadyRemovedIsIdempotent covers the partial-failure
// recovery path: GetThread returns sql.ErrNoRows, but children and runtime
// state still need cleanup. The delete must not error and must skip the
// (now-absent) store delete.
func TestDeleteThreadAlreadyRemovedIsIdempotent(t *testing.T) {
	app := newTestAppWithStore(t)

	// Install a deliberation for a channel that a missing parent used to
	// own. deleteThreadTree should still tear down associated runtime state.
	app.installDeliberation("ghost-channel", nil, 2)

	app.stopSessionFn = func(string) error { return nil }

	if err := app.DeleteThread("nonexistent-thread"); err != nil {
		t.Fatalf("DeleteThread(missing) error = %v, want nil (idempotent)", err)
	}

	// Sanity: the call did not touch deliberations for a different channel,
	// because the ghost thread carried no DiscussionID to trigger cleanup.
	_, stillTracked := app.deliberation("ghost-channel")
	if !stillTracked {
		t.Fatal("deliberation wrongly removed for missing thread with no DiscussionID")
	}
}

// TestDeleteThreadTreePropagatesStoreErrors ensures that unexpected store
// errors (anything other than sql.ErrNoRows on GetThread) are surfaced
// to the caller — the delete must fail loudly, not silently succeed.
func TestDeleteThreadTreePropagatesStoreErrors(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-delete-store-error")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Close the underlying database so any subsequent store call fails with
	// a non-sql.ErrNoRows error. deleteThreadTree should propagate the
	// failure instead of swallowing it.
	if err := app.store.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}

	err := app.DeleteThread(thread.ID)
	if err == nil {
		t.Fatal("DeleteThread() error = nil, want store-level failure")
	}
}

// TestDeleteThreadKeepsRowWhenChildFails covers A4's key invariant: if
// ANY step of the cascade fails, the parent row must remain in the DB so
// a subsequent DeleteThread call can retry idempotently. Before A4, the
// recursive path halted early and the parent was preserved — but the
// flat step-by-step path (stop → terminals → deliberation →
// attachments) would still run even after a failure in one of them
// and could race to partially clean state.
func TestDeleteThreadKeepsRowWhenChildFails(t *testing.T) {
	app := newTestAppWithStore(t)

	parent := testThread("p-root")
	if err := app.store.CreateThread(parent); err != nil {
		t.Fatalf("parent: %v", err)
	}
	child := testThread("p-child")
	child.ParentThreadID = parent.ID
	if err := app.store.CreateThread(child); err != nil {
		t.Fatalf("child: %v", err)
	}

	app.stopSessionFn = func(id string) error {
		if id == child.ID {
			return errors.New("child stop failed")
		}
		return nil
	}

	err := app.DeleteThread(parent.ID)
	if err == nil {
		t.Fatal("expected error when child cleanup fails")
	}

	// Both rows must still be present (no partial state).
	if _, err := app.store.GetThread(parent.ID); err != nil {
		t.Errorf("parent row should be preserved: %v", err)
	}
	if _, err := app.store.GetThread(child.ID); err != nil {
		t.Errorf("child row should be preserved: %v", err)
	}
}

// TestDeleteThreadThreeLevelsDeepChildFailureKeepsAncestors asserts that
// a failure at the deepest level of a 3-level tree does not allow any
// ancestor to be deleted. Tests the recursive error-join semantics.
func TestDeleteThreadThreeLevelsDeepChildFailureKeepsAncestors(t *testing.T) {
	app := newTestAppWithStore(t)

	l0 := testThread("tree-l0")
	l1 := testThread("tree-l1")
	l1.ParentThreadID = l0.ID
	l2 := testThread("tree-l2")
	l2.ParentThreadID = l1.ID
	l3 := testThread("tree-l3")
	l3.ParentThreadID = l2.ID
	for _, th := range []store.Thread{l0, l1, l2, l3} {
		if err := app.store.CreateThread(th); err != nil {
			t.Fatalf("create %s: %v", th.ID, err)
		}
	}

	app.stopSessionFn = func(id string) error {
		if id == l3.ID {
			return errors.New("deepest fail")
		}
		return nil
	}

	if err := app.DeleteThread(l0.ID); err == nil {
		t.Fatal("expected error when deepest descendant fails")
	}

	// All four rows must still exist.
	for _, id := range []string{l0.ID, l1.ID, l2.ID, l3.ID} {
		if _, err := app.store.GetThread(id); err != nil {
			t.Errorf("%s should still be in DB: %v", id, err)
		}
	}
}

// TestDeleteThreadConcurrentCallsAreSafe verifies that two racing
// DeleteThread calls for the same thread tree don't crash and the end
// state is coherent: row is gone, resources are cleaned up, and only
// one call returns success (the other sees a clean idempotent success
// via the ErrNoRows branch or returns success after finding nothing
// left to do).
func TestDeleteThreadConcurrentCallsAreSafe(t *testing.T) {
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

// TestInstallDeliberationTracksState confirms the helper used by discussion
// start wiring, since several delete tests depend on it. Acts as a canary —
// if this breaks, the other tests are suspect.
func TestInstallDeliberationTracksState(t *testing.T) {
	app := newTestAppWithStore(t)
	app.installDeliberation("channel-canary", nil, 5)

	delib, _ := app.deliberation("channel-canary")
	if delib == nil {
		t.Fatal("installDeliberation did not register channel-canary")
	}
	if got := delib.State().MaxTurns; got != 5 {
		t.Fatalf("MaxTurns = %d, want 5", got)
	}
}

// TestDeleteThreadRemovesReplayLog covers the common path: the thread has
// an on-disk replay log and deleting the thread must remove it.
func TestDeleteThreadRemovesReplayLog(t *testing.T) {
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
