package store

import (
	"errors"
	"fmt"
)

// The three values threads.worktree_setup_state admits (migration v47). They
// are declared here rather than left as bare literals at the call sites so the
// CHECK constraint and the Go writers cannot drift.
const (
	// WorktreeSetupStateNone is "nothing to say": the recipe never ran for
	// this thread's worktree, it succeeded, it was cancelled, or the thread
	// has since moved off that worktree. All four are the same absence.
	WorktreeSetupStateNone = ""
	// WorktreeSetupStateRunning is written at kickoff. A row still carrying it
	// at startup is crash residue — see SweepRunningThreadWorktreeSetups.
	WorktreeSetupStateRunning = "running"
	// WorktreeSetupStateFailed is the one state a restart must preserve: the
	// worktree exists and is usable, but the recipe did not finish, so the
	// sidebar advertises it and the retry affordance stays reachable.
	WorktreeSetupStateFailed = "failed"
)

// ErrInvalidWorktreeSetupState is returned for a state outside the enum,
// before SQLite would report it as a raw CHECK failure.
var ErrInvalidWorktreeSetupState = errors.New("store: invalid worktree setup state")

// SetThreadWorktreeSetupState writes the thread's durable worktree-setup
// state.
//
// It deliberately leaves updated_at alone: a setup transition is system work,
// not user activity, and the sidebar sorts by updated_at — bumping it would
// jump the thread to the top on a background state change (same rule the
// worktree-removal reattach sweep follows).
func (s *Store) SetThreadWorktreeSetupState(threadID, state string) error {
	if !validWorktreeSetupState(state) {
		return fmt.Errorf("%w: %q", ErrInvalidWorktreeSetupState, state)
	}
	result, err := s.db.Exec(
		`UPDATE threads SET worktree_setup_state = ? WHERE id = ?`,
		state, threadID,
	)
	if err != nil {
		return fmt.Errorf("store: set worktree setup state for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: set worktree setup state for %s", threadID))
}

// SweepRunningThreadWorktreeSetups settles every thread left mid-setup by a
// previous app instance and reports how many it settled. A run only exists
// inside a live process, so a 'running' row at startup means the app died with
// the recipe in flight: the worktree's provisioning state is unknown, which is
// exactly what 'failed' means here. The counterpart of the workflow engine's
// FailRunningWorkItemUnits.
func (s *Store) SweepRunningThreadWorktreeSetups() (int64, error) {
	result, err := s.db.Exec(
		`UPDATE threads SET worktree_setup_state = ? WHERE worktree_setup_state = ?`,
		WorktreeSetupStateFailed, WorktreeSetupStateRunning,
	)
	if err != nil {
		return 0, fmt.Errorf("store: sweep running worktree setups: %w", err)
	}
	swept, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: sweep running worktree setups: %w", err)
	}
	return swept, nil
}

func validWorktreeSetupState(state string) bool {
	switch state {
	case WorktreeSetupStateNone, WorktreeSetupStateRunning, WorktreeSetupStateFailed:
		return true
	default:
		return false
	}
}
