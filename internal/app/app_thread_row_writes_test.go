package app

import (
	"context"
	"database/sql"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil"
)

// A writer that read the thread row and then writes it must write only the
// columns it owns: a write to another column that lands between its read and
// its write (a followed worktree move, a rename) has to survive.

// storeWriteFrame matches a goroutine frame inside a store write.
var storeWriteFrame = regexp.MustCompile(`agent-overflow/internal/store\.\(\*Store\)\.(Update|Set|Rename|Mark|Create|Record|Clear|apply)`)

// landWhileStoreWriteWaits holds SQLite's write lock with landSQL applied but
// not committed, runs op, and commits once op waits inside a store write.
// By then op has read the row, before landSQL was visible to it, so a write
// of the whole row it read would undo landSQL.
func landWhileStoreWriteWaits(t *testing.T, dbPath, opFrame string, op func() error, landSQL string, args ...any) {
	t.Helper()
	blocker, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open blocker: %v", err)
	}
	t.Cleanup(func() { _ = blocker.Close() })
	blocker.SetMaxOpenConns(1)
	ctx := context.Background()
	held, err := blocker.Conn(ctx)
	if err != nil {
		t.Fatalf("blocker conn: %v", err)
	}
	defer func() { _ = held.Close() }()
	if _, err := held.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("take write lock: %v", err)
	}
	if _, err := held.ExecContext(ctx, landSQL, args...); err != nil {
		_, _ = held.ExecContext(ctx, "ROLLBACK")
		t.Fatalf("land %q: %v", landSQL, err)
	}
	done := make(chan error, 1)
	go func() { done <- op() }()
	waiting := awaitStoreWriteIn(opFrame, done)
	if _, err := held.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatalf("commit landed write: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("op: %v", err)
	}
	if !waiting {
		t.Fatalf("%s never waited on the store's write lock", opFrame)
	}
}

// awaitStoreWriteIn reports whether a goroutine running fn reached a store
// write before done delivered. done is left for the caller to read.
func awaitStoreWriteIn(fn string, done chan error) bool {
	buf := make([]byte, 1<<20)
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		n := runtime.Stack(buf, true)
		if n == len(buf) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(stack, fn) && storeWriteFrame.MatchString(stack) {
				return true
			}
		}
		select {
		case err := <-done:
			done <- err
			return false
		case <-time.After(time.Millisecond):
		}
	}
	return false
}

// A `/effort` the live session adopted writes the effort and nothing else.
func TestEffortSyncKeepsAMoveThatLandedAfterItsRead(t *testing.T) {
	t.Parallel()
	app, dbPath := newTestAppWithStorePath(t)
	thread := testThread("thread-effort-move")
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	const token = "token-effort-move"
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: token})
	elsewhere := thread.WorkspacePath + "-elsewhere"
	landWhileStoreWriteWaits(t, dbPath, "syncThreadEffortFromWire", func() error {
		app.syncThreadEffortFromWire(thread.ID, token, provider.EffortLow, false)
		return nil
	}, `UPDATE threads SET workspace_path = ? WHERE id = ?`, elsewhere, thread.ID)
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.WorkspacePath != elsewhere || stored.ReasoningEffort != string(provider.EffortLow) {
		t.Fatalf("row = workspace %q effort %q, want %q and low", stored.WorkspacePath, stored.ReasoningEffort, elsewhere)
	}
}

// A checkout move writes the workspace, worktree and branch and nothing
// else; create and switch commit through the same commitThreadCheckout.
func TestCheckoutMoveKeepsARenameThatLandedAfterItsRead(t *testing.T) {
	t.Parallel()
	app, dbPath := newTestAppWithStorePath(t)
	repo := initMainGitRepo(t)
	testutil.RunGit(t, repo, "branch", "feature/rename-meanwhile")
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	thread := testThread("thread-checkout-rename")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	var worktree string
	landWhileStoreWriteWaits(t, dbPath, "AttachThreadWorktree", func() error {
		updated, err := app.AttachThreadWorktree(thread.ID, "feature/rename-meanwhile")
		worktree = updated.WorktreePath
		return err
	}, `UPDATE threads SET title = ? WHERE id = ?`, "Renamed meanwhile", thread.ID)
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, worktree, true) })
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.Title != "Renamed meanwhile" || !samePath(stored.WorkspacePath, worktree) || worktree == "" {
		t.Fatalf("row = title %q workspace %q, want the rename and %q", stored.Title, stored.WorkspacePath, worktree)
	}
}
