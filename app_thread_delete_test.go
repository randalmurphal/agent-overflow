package main

import (
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestDeleteThreadSimpleThreadRemovesRowAndSession covers the happy path for a
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
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "delete-active-token",
	}

	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread() error = %v", err)
	}

	app.mu.Lock()
	_, stillActive := app.sessions[thread.ID]
	app.mu.Unlock()
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

	parent := testThread("thread-delete-discussion-parent")
	parent.DiscussionID = "channel-delete"
	if err := app.store.CreateThread(parent); err != nil {
		t.Fatalf("CreateThread(parent) error = %v", err)
	}
	child := testThread("thread-delete-discussion-child")
	child.ParentThreadID = parent.ID
	child.DiscussionID = "channel-delete"
	if err := app.store.CreateThread(child); err != nil {
		t.Fatalf("CreateThread(child) error = %v", err)
	}

	app.setThreadSystemPrompt(parent.ID, "parent prompt")
	app.setThreadSystemPrompt(child.ID, "child prompt")
	app.installDeliberation("channel-delete", 4)

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

	app.mu.Lock()
	_, stillTracked := app.deliberations["channel-delete"]
	app.mu.Unlock()
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

	parent := testThread("thread-delete-child-only-parent")
	parent.DiscussionID = "channel-child-only"
	if err := app.store.CreateThread(parent); err != nil {
		t.Fatalf("CreateThread(parent) error = %v", err)
	}
	child := testThread("thread-delete-child-only-child")
	child.ParentThreadID = parent.ID
	child.DiscussionID = "channel-child-only"
	if err := app.store.CreateThread(child); err != nil {
		t.Fatalf("CreateThread(child) error = %v", err)
	}

	app.installDeliberation("channel-child-only", 4)
	app.stopSessionFn = func(string) error { return nil }

	if err := app.DeleteThread(child.ID); err != nil {
		t.Fatalf("DeleteThread(child) error = %v", err)
	}

	app.mu.Lock()
	_, stillTracked := app.deliberations["channel-child-only"]
	app.mu.Unlock()
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
	app.installDeliberation("ghost-channel", 2)

	app.stopSessionFn = func(string) error { return nil }

	if err := app.DeleteThread("nonexistent-thread"); err != nil {
		t.Fatalf("DeleteThread(missing) error = %v, want nil (idempotent)", err)
	}

	// Sanity: the call did not touch deliberations for a different channel,
	// because the ghost thread carried no DiscussionID to trigger cleanup.
	app.mu.Lock()
	_, stillTracked := app.deliberations["ghost-channel"]
	app.mu.Unlock()
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

// TestInstallDeliberationTracksState confirms the helper used by discussion
// start wiring, since several delete tests depend on it. Acts as a canary —
// if this breaks, the other tests are suspect.
func TestInstallDeliberationTracksState(t *testing.T) {
	app := newTestAppWithStore(t)
	app.installDeliberation("channel-canary", 5)

	app.mu.Lock()
	delib := app.deliberations["channel-canary"]
	app.mu.Unlock()
	if delib == nil {
		t.Fatal("installDeliberation did not register channel-canary")
	}
	if got := delib.State().MaxTurns; got != 5 {
		t.Fatalf("MaxTurns = %d, want 5", got)
	}
}

