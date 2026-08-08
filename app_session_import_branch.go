package main

import (
	"log"
	"os"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/sessionimport"
	"agent-overflow/internal/store"
)

// materializeImportedClaudeBranch gives an imported, non-active Claude branch
// the resume reference it needs the first time it starts a session.
//
// WHY THE THREAD HAS NONE. A Claude transcript is a DAG, and the importer
// makes one thread per branch. Only one of those threads can carry the
// session id: `claude --resume <id>` reopens the file's ACTIVE branch — the
// chain ending at its last transcript row — so handing the same id to every
// branch would mean continuing an abandoned branch silently appends to a
// different conversation, with two AO threads writing one file. The importer
// therefore gives the ref to the active branch only — and to nothing at all
// when that branch imported no rows (see sessionimport's settleBranches) —
// and records every branch's leaf uuid in thread_import_state.
//
// WHY IT HAPPENS HERE AND NOT AT IMPORT. Cutting a branch out costs a new
// transcript file on disk, and that file shows up in the user's own
// `claude --resume` picker. Doing it at import time would write one per
// abandoned branch across a whole "Import All" — hundreds of phantom sessions
// and a duplicate copy of every transcript prefix — for threads the user may
// never continue. Doing it on first session start costs the same write for
// the branches that are actually used, and nothing for the rest.
//
// WHAT IT WRITES. sessionfork.WriteForkFileThroughUUID keeps everything
// through the branch's leaf in file order and remints every uuid, which is the
// same transform (and the same resulting shape: the leaf is the new file's
// last transcript row, so it is the new file's active branch) that
// fork-at-a-past-message has produced in `app_thread_fork.go` since forking
// existed. Rows of other branches that happen to sit earlier in the file come
// along off-chain, exactly as they do for a fork.
//
// FAILURE IS A DEGRADE, NOT A REFUSAL. When the source transcript is gone or
// the leaf is no longer in it, the thread starts a fresh provider session —
// which is precisely the behaviour it had before this function existed, and
// the timeline the user sees is unaffected because AO's own copy of the
// history is in SQLite. Refusing the start instead would make an imported
// branch unusable because a file it no longer needs was deleted. The failure
// is logged with the leaf and the path so it can be told apart from a thread
// that never had a ref to begin with.
func (a *App) materializeImportedClaudeBranch(t store.Thread) store.Thread {
	if a.store == nil ||
		t.Provider != string(provider.Claude) ||
		t.ImportSource != sessionimport.ProviderClaude ||
		strings.TrimSpace(t.SessionRef) != "" ||
		strings.TrimSpace(t.PendingForkRef) != "" {
		return t
	}

	state, found, err := a.store.GetThreadImportState(t.ID)
	if err != nil {
		log.Printf("start session: read import state for thread %s: %v", t.ID, err)
		return t
	}
	if !found || strings.TrimSpace(state.LeafUUID) == "" || strings.TrimSpace(state.SourcePath) == "" {
		return t
	}
	if _, err := os.Stat(state.SourcePath); err != nil {
		log.Printf(
			"start session: imported branch %s cannot be resumed — its source transcript %s is gone; starting a fresh session",
			t.ID, state.SourcePath)
		return t
	}

	sessionID, path, _, err := sessionfork.WriteForkFileThroughUUID(state.SourcePath, state.LeafUUID, t.Title)
	if err != nil {
		log.Printf(
			"start session: imported branch %s could not be cut from %s at leaf %s; starting a fresh session: %v",
			t.ID, state.SourcePath, state.LeafUUID, err)
		return t
	}

	// A TARGETED write, not a whole-row UpdateThread. `t` was read at the top
	// of startSessionNowWithClaudeResumeAt and everything else about the row is
	// unread here; writing it back would revert any column another writer moved
	// since — an auto-generated title (which lands from a detached goroutine
	// through its own compare-and-swap), an observed branch, a token-usage
	// refresh. UpdateSessionRef writes session_ref, clears the pending fork
	// ref, and leaves updated_at alone, which is the same posture every other
	// session-ref writer takes.
	if _, err := a.store.UpdateSessionRef(t.ID, sessionID); err != nil {
		// The file is written but the row does not point at it. Remove it
		// rather than leave an orphan transcript in the user's Claude home;
		// the next start tries again from the same source.
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			log.Printf("start session: remove orphaned branch session %s: %v", path, removeErr)
		}
		log.Printf("start session: persist imported branch session for thread %s: %v", t.ID, err)
		return t
	}
	t.SessionRef = sessionID
	return t
}
