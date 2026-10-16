package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
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

// A focus stamp whose announcement waits past the stamp's own deadline still
// reaches clients: only the app context ending drops it.
func TestSwitchThreadReadStampAnnouncesAfterItsWriteDeadline(t *testing.T) {
	t.Parallel()
	f := newFollowFixture(t, "thread-switch-late-frame")
	completeTurnForAnnounce(t, f.app, f.thread.ID)
	release := f.app.threadAnnouncer().Hold(f.thread.ID)
	if _, err := f.app.SwitchThread(f.thread.ID); err != nil {
		release()
		t.Fatalf("SwitchThread: %v", err)
	}
	waitAnnounceWaiter(t, f.app, f.thread.ID)
	time.Sleep(markThreadReadTimeout + time.Second)
	release()
	f.app.waitMarkThreadReads()
	frames := f.rows.fullRowsFor(f.thread.ID)
	if len(frames) != 1 || frames[0].LastReadAt == nil {
		t.Fatalf("frames = %d, want the stamped row once the announce lock is free", len(frames))
	}
}

// Shutdown cancels the app context before it joins the stamps, which ends an
// announcement still waiting, so the join is bounded.
func TestSwitchThreadReadStampAnnouncementEndsWithTheApp(t *testing.T) {
	t.Parallel()
	f := newFollowFixture(t, "thread-switch-shutdown")
	completeTurnForAnnounce(t, f.app, f.thread.ID)
	release := f.app.threadAnnouncer().Hold(f.thread.ID)
	defer release()
	if _, err := f.app.SwitchThread(f.thread.ID); err != nil {
		t.Fatalf("SwitchThread: %v", err)
	}
	waitAnnounceWaiter(t, f.app, f.thread.ID)
	f.app.appCancel()
	joined := make(chan struct{})
	go func() {
		f.app.waitMarkThreadReads()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		t.Fatal("the read stamp still waits to announce after the app context ended")
	}
	if frames := f.rows.fullRowsFor(f.thread.ID); len(frames) != 0 {
		t.Fatalf("frames = %d after the app context ended, want 0", len(frames))
	}
}

func waitAnnounceWaiter(t *testing.T, app *App, threadID string) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); app.threadAnnouncer().Refs(threadID) < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no announcement waited on the announce lock")
		}
	}
}

// gateAnnounceReads replaces the app's announcer with one whose reads of
// threadID stop after reading until the returned release runs, and reports on
// read when one has.
func gateAnnounceReads(app *App, threadID string) (read <-chan struct{}, release func()) {
	reached := make(chan struct{}, 1)
	gate := make(chan struct{})
	app.threadAnnouncerOnce.Do(func() {})
	app.threadRowAnnouncer = triage.NewThreadAnnouncer(func(id string) (store.Thread, error) {
		row, err := app.store.GetThread(id)
		if id == threadID {
			select {
			case reached <- struct{}{}:
				<-gate
			default:
			}
		}
		return row, err
	})
	var once sync.Once
	return reached, func() { once.Do(func() { close(gate) }) }
}

// replayedThread applies thread:updated frames as eventsThreadRows does: a
// full or listed row replaces what the client holds, a patch changes only its
// named fields, a deletion drops the row and a full frame for a row the
// client lacks is ignored. It returns the row the client keeps and whether
// it keeps one.
func replayedThread(frames []triage.ThreadUpdateEvent, threadID string, have bool) (store.Thread, bool) {
	var row store.Thread
	for _, evt := range frames {
		id := evt.ID
		if evt.Thread != nil {
			id = evt.Thread.ID
		}
		if id != threadID {
			continue
		}
		switch evt.Action {
		case triage.ThreadActionDeleted:
			have = false
		case triage.ThreadActionListed:
			row, have = *evt.Thread, true
		case triage.ThreadActionFull:
			if have {
				row = *evt.Thread
			}
		case triage.ThreadActionPatch:
			if evt.Title != nil {
				row.Title = *evt.Title
			}
			if evt.Model != nil {
				row.Model = *evt.Model
			}
			if evt.SessionRef != nil {
				row.SessionRef = *evt.SessionRef
			}
		}
	}
	return row, have
}

// awaitAnnounceWaiterOr returns once a second holder waits on threadID's
// announce lock, or with true and its result once done delivers first.
func awaitAnnounceWaiterOr(app *App, threadID string, done <-chan error) (bool, error) {
	for deadline := time.Now().Add(15 * time.Second); app.threadAnnouncer().Refs(threadID) < 2 && time.Now().Before(deadline); {
		select {
		case err := <-done:
			return true, err
		case <-time.After(time.Millisecond):
		}
	}
	return false, nil
}

// The verifier's probe: the focus stamp's row frame read before a rename
// must not leave after the rename's patch, or every client keeps the old
// title.
func TestRenamePatchDoesNotLeaveBeforeARowReadEarlier(t *testing.T) {
	t.Parallel()
	f := newFollowFixture(t, "thread-rename-patch")
	completeTurnForAnnounce(t, f.app, f.thread.ID)
	read, release := gateAnnounceReads(f.app, f.thread.ID)
	defer release()
	if _, err := f.app.SwitchThread(f.thread.ID); err != nil {
		t.Fatalf("SwitchThread: %v", err)
	}
	<-read
	const title = "Renamed while the stamp's frame waited"
	renamed := make(chan error, 1)
	go func() { renamed <- f.app.RenameThread(f.thread.ID, title) }()
	finished, err := awaitAnnounceWaiterOr(f.app, f.thread.ID, renamed)
	release()
	if !finished {
		err = <-renamed
	}
	if err != nil {
		t.Fatalf("RenameThread: %v", err)
	}
	f.app.waitMarkThreadReads()
	row, have := replayedThread(f.rows.snapshot(), f.thread.ID, true)
	if !have || row.Title != title {
		t.Fatalf("client keeps title %q (have %v), want the rename", row.Title, have)
	}
}

// The verifier's probe: two writers of one title announcing in the opposite
// order to their commits. The router's provider rename commits, pauses
// before its announce while RenameThread runs whole, then announces (the
// router's emitThreadPatch is this AnnouncePatch on the shared announcer).
// The last frame must carry the stored title, not the router's.
func TestPatchFramesCarryTheStoredValueNotTheCallers(t *testing.T) {
	t.Parallel()
	f := newFollowFixture(t, "thread-patch-stored-value")
	const providerTitle = "Title the provider chose"
	const userTitle = "Title the user typed"
	if err := f.app.store.UpdateTitle(f.thread.ID, providerTitle); err != nil {
		t.Fatalf("router commit: %v", err)
	}
	if err := f.app.RenameThread(f.thread.ID, userTitle); err != nil {
		t.Fatalf("RenameThread: %v", err)
	}
	routerTitle := providerTitle
	f.app.threadAnnouncer().AnnouncePatch(f.app.emitEvent, f.thread.ID, triage.ThreadUpdateEvent{Title: &routerTitle})
	stored, err := f.app.store.GetThread(f.thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	row, _ := replayedThread(f.rows.snapshot(), f.thread.ID, true)
	if stored.Title != userTitle || row.Title != userTitle {
		t.Fatalf("client title %q, store title %q, want both %q", row.Title, stored.Title, userTitle)
	}
}

// A row frame read before a delete must not leave after the deleted frame:
// the client would hold a row for a thread that no longer exists. The focus
// stamp announces holding no thread lock, so a delete can land while its
// frame waits.
func TestDeletedFrameIsNotFollowedByARowReadEarlier(t *testing.T) {
	t.Parallel()
	f := newFollowFixture(t, "thread-delete-ordering")
	completeTurnForAnnounce(t, f.app, f.thread.ID)
	read, release := gateAnnounceReads(f.app, f.thread.ID)
	defer release()
	if _, err := f.app.SwitchThread(f.thread.ID); err != nil {
		t.Fatalf("SwitchThread: %v", err)
	}
	<-read
	deleted := make(chan error, 1)
	go func() { deleted <- f.app.DeleteThread(f.thread.ID) }()
	finished, err := awaitAnnounceWaiterOr(f.app, f.thread.ID, deleted)
	release()
	if !finished {
		err = <-deleted
	}
	if err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	f.app.waitMarkThreadReads()
	sawDeleted := false
	for _, evt := range f.rows.snapshot() {
		switch {
		case evt.Action == triage.ThreadActionDeleted && evt.ID == f.thread.ID:
			sawDeleted = true
		case sawDeleted && evt.Thread != nil && evt.Thread.ID == f.thread.ID:
			t.Fatalf("a %s row frame left after the deleted frame", evt.Action)
		}
	}
	if !sawDeleted {
		t.Fatal("no deleted frame")
	}
}
