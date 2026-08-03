package store

import (
	"errors"
	"testing"
)

// The column exists, defaults to "nothing to say", and reads back through the
// same thread projection the sidebar uses.
func TestThreadWorktreeSetupStateDefaultsToEmpty(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t-setup-default", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	got, err := s.GetThread("t-setup-default")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if got.WorktreeSetupState != WorktreeSetupStateNone {
		t.Fatalf("WorktreeSetupState = %q, want empty", got.WorktreeSetupState)
	}
}

// The CHECK is the schema's own guard; the Go validator is what turns a typo
// into a typed error instead of a raw constraint failure. Both must refuse.
func TestSetThreadWorktreeSetupStateRefusesUnknownStates(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t-setup-invalid", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	err := s.SetThreadWorktreeSetupState("t-setup-invalid", "succeeded")
	if !errors.Is(err, ErrInvalidWorktreeSetupState) {
		t.Fatalf("SetThreadWorktreeSetupState(succeeded) = %v, want ErrInvalidWorktreeSetupState", err)
	}
	if _, err := s.db.Exec(
		`UPDATE threads SET worktree_setup_state = 'succeeded' WHERE id = ?`, "t-setup-invalid",
	); err == nil {
		t.Fatal("raw UPDATE past the enum was accepted; CHECK constraint missing")
	}
}

// State coverage is not transition coverage: the writer has to handle every
// call sequence, including the ones that walk back through empty.
func TestThreadWorktreeSetupStateTransitions(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t-setup-transitions", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	readState := func() string {
		t.Helper()
		got, err := s.GetThread("t-setup-transitions")
		if err != nil {
			t.Fatalf("get thread: %v", err)
		}
		return got.WorktreeSetupState
	}
	set := func(state string) {
		t.Helper()
		if err := s.SetThreadWorktreeSetupState("t-setup-transitions", state); err != nil {
			t.Fatalf("set %q: %v", state, err)
		}
	}

	// set → clear → set: a retry after a failure, then a second failure.
	set(WorktreeSetupStateRunning)
	if got := readState(); got != WorktreeSetupStateRunning {
		t.Fatalf("after kickoff = %q", got)
	}
	set(WorktreeSetupStateFailed)
	if got := readState(); got != WorktreeSetupStateFailed {
		t.Fatalf("after failure = %q", got)
	}
	// failed → '' is what a successful retry writes.
	set(WorktreeSetupStateRunning)
	set(WorktreeSetupStateNone)
	if got := readState(); got != WorktreeSetupStateNone {
		t.Fatalf("after successful retry = %q", got)
	}
	set(WorktreeSetupStateRunning)
	set(WorktreeSetupStateFailed)
	if got := readState(); got != WorktreeSetupStateFailed {
		t.Fatalf("after second failure = %q", got)
	}
	// Cancellation clears without ever passing through failed.
	set(WorktreeSetupStateRunning)
	set(WorktreeSetupStateNone)
	if got := readState(); got != WorktreeSetupStateNone {
		t.Fatalf("after cancellation = %q", got)
	}
}

func TestSetThreadWorktreeSetupStateRefusesAnUnknownThread(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetThreadWorktreeSetupState("t-setup-missing", WorktreeSetupStateFailed); err == nil {
		t.Fatal("writing a state for a nonexistent thread reported success")
	}
}

// The startup sweep: a process death mid-setup leaves 'running' rows whose
// worktree state nobody can vouch for, and 'failed' is what that means.
func TestSweepRunningThreadWorktreeSetups(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"t-sweep-running", "t-sweep-failed", "t-sweep-idle"} {
		if err := s.CreateThread(makeThread(id, "claude")); err != nil {
			t.Fatalf("create thread %s: %v", id, err)
		}
	}
	if err := s.SetThreadWorktreeSetupState("t-sweep-running", WorktreeSetupStateRunning); err != nil {
		t.Fatalf("seed running: %v", err)
	}
	if err := s.SetThreadWorktreeSetupState("t-sweep-failed", WorktreeSetupStateFailed); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	swept, err := s.SweepRunningThreadWorktreeSetups()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1", swept)
	}
	want := map[string]string{
		"t-sweep-running": WorktreeSetupStateFailed,
		"t-sweep-failed":  WorktreeSetupStateFailed,
		"t-sweep-idle":    WorktreeSetupStateNone,
	}
	for id, wantState := range want {
		got, err := s.GetThread(id)
		if err != nil {
			t.Fatalf("get thread %s: %v", id, err)
		}
		if got.WorktreeSetupState != wantState {
			t.Fatalf("%s state = %q, want %q", id, got.WorktreeSetupState, wantState)
		}
	}

	// Idempotent: a second boot with nothing running sweeps nothing.
	swept, err = s.SweepRunningThreadWorktreeSetups()
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if swept != 0 {
		t.Fatalf("second sweep = %d, want 0", swept)
	}
}

// UpdateThread rewrites the row's mutable columns; the setup state is written
// by its own narrow accessor and must survive an unrelated whole-row update
// (a workspace switch commits one while a run is live).
func TestUpdateThreadPreservesWorktreeSetupState(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("t-setup-preserved", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.SetThreadWorktreeSetupState(thread.ID, WorktreeSetupStateRunning); err != nil {
		t.Fatalf("set running: %v", err)
	}
	thread.Title = "renamed"
	if err := s.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	got, err := s.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if got.WorktreeSetupState != WorktreeSetupStateRunning {
		t.Fatalf("WorktreeSetupState = %q after UpdateThread, want running", got.WorktreeSetupState)
	}
}
