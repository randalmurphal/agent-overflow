//go:build !providersmoke

package app

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/provider/claude/sessionfork"
)

// A Claude slice is written before the thread's session_ref moves to it.
// These tests fail that ref write and require the slice to be removed, so
// no session file outlives an attempt the store never committed.

// rejectSessionRefWrites makes every session_ref update of threadID fail
// on the database at dbPath until the test ends.
func rejectSessionRefWrites(t *testing.T, dbPath, threadID string) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := raw.Exec(`DROP TRIGGER reject_session_ref`); err != nil {
			t.Error(err)
		}
		if err := raw.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := raw.Exec(`CREATE TRIGGER reject_session_ref BEFORE UPDATE OF session_ref ON threads WHEN NEW.id = '` +
		threadID + `' BEGIN SELECT RAISE(ABORT, 'injected session ref failure'); END`); err != nil {
		t.Fatal(err)
	}
}

func TestConversationRollbackRemovesSliceWhenSessionRefWriteFails(t *testing.T) {
	app, dbPath := newTestAppWithStorePath(t)
	thread, workspace := setupSliceIDsThread(t, app)
	sourcePath, err := sessionfork.LocateSessionFile(testProviderProjectsDir(t), thread.SessionRef, workspace)
	if err != nil {
		t.Fatalf("locate source session: %v", err)
	}
	rejectSessionRefWrites(t, dbPath, thread.ID)

	err = rollbackToMessage(app, thread.ID, "user:2")
	if err == nil || !strings.Contains(err.Error(), "injected session ref failure") {
		t.Fatalf("rollback error = %v, want the session ref write failure", err)
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(sourcePath), "*.jsonl"))
	if err != nil {
		t.Fatalf("glob session files: %v", err)
	}
	if !slices.Equal(files, []string{sourcePath}) {
		t.Fatalf("session files = %v, want only the source %s; the uncommitted slice must be removed", files, sourcePath)
	}
	after := mustGetThread(t, app, thread.ID)
	if after.SessionRef != thread.SessionRef {
		t.Fatalf("session ref = %q, want the untouched %q", after.SessionRef, thread.SessionRef)
	}
	if _, found, err := app.store.GetThreadItem(thread.ID, "user:2"); err != nil || !found {
		t.Fatalf("failed rollback removed user:2 (found=%v err=%v)", found, err)
	}
}

func TestMaterializeImportedClaudeBranchRemovesCutWhenSessionRefWriteFails(t *testing.T) {
	app, dbPath := newTestAppWithStorePath(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeBranchedSession(t, importFixtureClaudeBranchy)

	threads := importClaudeSessionWithLegacyAbandonedBranch(t, app, home, importFixtureClaudeBranchy)
	_, abandoned, ok := threadsBySessionRef(threads)
	if !ok {
		t.Fatalf("threads = %+v, want exactly one with a session ref", threads)
	}
	before := claudeSessionFiles(t, home)
	rejectSessionRefWrites(t, dbPath, abandoned.ID)

	if got := app.materializeImportedClaudeBranch(abandoned); got.SessionRef != "" {
		t.Fatalf("sessionRef = %q, want none when the ref write fails", got.SessionRef)
	}
	if stored := mustGetThread(t, app, abandoned.ID); stored.SessionRef != "" {
		t.Fatalf("stored sessionRef = %q, want the row left alone", stored.SessionRef)
	}
	if after := claudeSessionFiles(t, home); !slices.Equal(before, after) {
		t.Fatalf("transcripts = %v, want %v; the uncommitted cut must be removed", after, before)
	}
}
