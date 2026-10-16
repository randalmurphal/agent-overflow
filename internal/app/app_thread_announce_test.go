package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// A thread row frame carries the row as it is when the frame leaves, never a
// row a writer read earlier: clients keep the last row they receive, and a
// row read before another writer's commit would undo that commit on every
// screen (app_thread_broadcast.go). Each test below keeps one writer's
// announcement waiting, lands a later write, and expects the frame to carry
// both.

// announceRace holds threadID's announce lock until op's announcement waits
// on it, runs land, releases, and waits for op. It fails unless the last
// full row frame for the thread is the stored row, land's write included.
func announceRace(t *testing.T, app *App, threadID string, rows *threadRowRecorder, op, land func() error) {
	t.Helper()
	announcer := app.threadAnnouncer()
	release := announcer.Hold(threadID)
	done := make(chan error, 1)
	go func() { done <- op() }()
	finished := false
	var opErr error
	for deadline := time.Now().Add(15 * time.Second); !finished && announcer.Refs(threadID) < 2 && time.Now().Before(deadline); {
		select {
		case opErr = <-done:
			finished = true
		case <-time.After(time.Millisecond):
		}
	}
	landErr := land()
	release()
	if !finished {
		opErr = <-done
	}
	if opErr != nil || landErr != nil {
		t.Fatalf("op: %v, land: %v", opErr, landErr)
	}
	stored, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	frames := rows.fullRowsFor(threadID)
	if len(frames) == 0 {
		t.Fatal("no thread:updated row frame")
	}
	last := frames[len(frames)-1]
	if !samePath(last.WorkspacePath, stored.WorkspacePath) || last.Branch != stored.Branch || !sameMillis(last.LastReadAt, stored.LastReadAt) {
		t.Fatalf("last frame = workspace %q branch %q lastReadAt %v, want the stored row: workspace %q branch %q lastReadAt %v",
			last.WorkspacePath, last.Branch, millis(last.LastReadAt), stored.WorkspacePath, stored.Branch, millis(stored.LastReadAt))
	}
}

func sameMillis(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func millis(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// stampRead writes a read stamp past a completed turn, as MarkThreadRead
// does, and fails when the stamp changes nothing.
func stampRead(app *App, threadID string) error {
	_, changed, err := app.store.MarkThreadReadNow(context.Background(), threadID)
	if err == nil && !changed {
		err = errors.New("the read stamp changed nothing")
	}
	return err
}

func completeTurnForAnnounce(t *testing.T, app *App, threadID string) {
	t.Helper()
	completedAt := time.Now().UnixMilli() + 60_000
	insertCompletedTurnForAppTest(t, app, threadID, "turn-announce", completedAt-1000, completedAt)
}

// The merged-PR cleanup's reattach: a read stamp landing while the moved
// row's frame waits is in that frame.
func TestWorktreeRemovalFrameCarriesALaterReadStamp(t *testing.T) {
	t.Parallel()
	f := newRemovalFixture(t, "feature-announce")
	completeTurnForAnnounce(t, f.app, f.occupant)
	announceRace(t, f.app, f.occupant, f.rows, func() error {
		_, err := f.app.RemoveOtherWorktree(WorkspaceRef{ProjectID: f.project.ID, WorkspacePath: f.repo}, f.worktree, true)
		return err
	}, func() error { return stampRead(f.app, f.occupant) })
}

// Claude's EnterWorktree, followed by the row.
func TestFollowFrameCarriesALaterReadStamp(t *testing.T) {
	t.Parallel()
	f := newFollowFixture(t, "thread-follow-announce")
	worktree := f.cliWorktree(t, "feature-announce")
	f.app.sessionManager().put(f.thread.ID, session{Provider: string(provider.Claude), Token: "token-live"})
	completeTurnForAnnounce(t, f.app, f.thread.ID)
	meta, err := json.Marshal(provider.WorkspaceChangeMeta{
		Tool: "EnterWorktree", Cwd: worktree, WorktreePath: worktree, Branch: "worktree-feature-announce",
	})
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	handler := f.app.sessionEventHandler(f.thread.ID, "token-live", string(provider.Claude))
	announceRace(t, f.app, f.thread.ID, f.rows, func() error {
		handler(provider.ProviderEvent{Kind: provider.EventWorkspaceChanged, ThreadID: f.thread.ID, ItemID: "tool-wt-1", Meta: meta, Timestamp: time.Now()})
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			for _, row := range f.rows.fullRowsFor(f.thread.ID) {
				if samePath(row.WorkspacePath, worktree) {
					return nil
				}
			}
		}
		return errors.New("the followed row was never announced")
	}, func() error { return stampRead(f.app, f.thread.ID) })
}

// A user-driven checkout move (attach; create and switch share
// commitThreadCheckout). Its session restart reads the row before the
// frame leaves, so the frame must not be that read.
func TestAttachThreadWorktreeFrameCarriesALaterReadStamp(t *testing.T) {
	t.Parallel()
	f := newFollowFixture(t, "thread-attach-announce")
	testutil.RunGit(t, f.repo, "branch", "feature/attach-announce")
	completeTurnForAnnounce(t, f.app, f.thread.ID)
	var updated store.Thread
	announceRace(t, f.app, f.thread.ID, f.rows, func() error {
		var err error
		updated, err = f.app.AttachThreadWorktree(f.thread.ID, "feature/attach-announce")
		return err
	}, func() error { return stampRead(f.app, f.thread.ID) })
	if err := f.app.gitCore().RemoveWorktreeForce(f.repo, updated.WorktreePath, true); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
}

// The focus read stamp's own frame carries a move that landed after the
// stamp: the CI flake's other half, where the stamp's frame overtook the
// merged-PR cleanup's.
func TestSwitchThreadReadStampFrameCarriesALaterMove(t *testing.T) {
	t.Parallel()
	f := newFollowFixture(t, "thread-switch-announce")
	completeTurnForAnnounce(t, f.app, f.thread.ID)
	elsewhere := filepath.Join(f.repo, "elsewhere")
	announceRace(t, f.app, f.thread.ID, f.rows, func() error {
		if _, err := f.app.SwitchThread(f.thread.ID); err != nil {
			return err
		}
		f.app.waitMarkThreadReads()
		return nil
	}, func() error {
		_, changed, err := f.app.store.UpdateThreadCheckout(f.thread.ID, elsewhere, elsewhere, "feature-elsewhere")
		if err == nil && !changed {
			err = fmt.Errorf("the move to %s changed nothing", elsewhere)
		}
		return err
	})
}

// The focus stamp's wait to announce is bounded by markThreadReadTimeout, so
// shutdown's join of the stamps (stopMarkThreadReads) is too.
func TestSwitchThreadReadStampAnnouncementIsBounded(t *testing.T) {
	t.Parallel()
	f := newFollowFixture(t, "thread-switch-bounded")
	completeTurnForAnnounce(t, f.app, f.thread.ID)
	release := f.app.threadAnnouncer().Hold(f.thread.ID)
	defer release()
	if _, err := f.app.SwitchThread(f.thread.ID); err != nil {
		t.Fatalf("SwitchThread: %v", err)
	}
	joined := make(chan struct{})
	go func() {
		f.app.waitMarkThreadReads()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(markThreadReadTimeout + 10*time.Second):
		t.Fatal("the read stamp still waits to announce past markThreadReadTimeout")
	}
	if frames := f.rows.fullRowsFor(f.thread.ID); len(frames) != 0 {
		t.Fatalf("frames = %d while the announce lock was held, want 0", len(frames))
	}
}
