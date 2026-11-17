package app

import (
	"context"
	"fmt"
	"log"
	"sync"

	"agent-overflow/internal/store"
)

// An archive is durable at its flip, which marks it unfinished
// (store.setThreadArchivedTx); its walk (finishArchive) archives what the
// thread owns and then ends it (endArchiveWalk). A boot finishes every
// archive a crash stopped before its walk ended (startPendingArchives).

// archiveWalks counts, by thread, the archive walks under way. A thread
// archived, reopened and archived again while the first walk still runs
// has two; only the last to end ends the archive, so a crash before the
// newer walk ends still finishes it at the next boot.
type archiveWalks struct {
	mu      sync.Mutex
	running map[string]int
}

// beginArchiveWalk records a walk of threadID's archive. Its caller holds
// the thread's action lock, in which the archive committed.
func (a *App) beginArchiveWalk(threadID string) {
	w := &a.archiveWalks
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running == nil {
		w.running = make(map[string]int)
	}
	w.running[threadID]++
}

// endArchiveWalk records the end of a walk of threadID's archive. The last
// walk to end owes the paired computers the archives of its workers there
// while the thread is still archived (store.OweRemoteArchives), and in the
// same transaction ends the archive when the walk finished, having read
// the workers it owns; it returns what it owed for its caller to deliver
// (deliverOwnerOps). A walk that did not finish, or a failure, leaves the
// archive unfinished, for the next boot to finish.
//
// It holds the thread's action lock across the decision and the end: a new
// archive commits its mark and records its walk under that lock
// (runArchive), so it lands before the decision, which then counts it, or
// after the end, which then leaves its mark alone.
func (a *App) endArchiveWalk(threadID string, finished bool) ([]store.OwnerOp, error) {
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	return a.endArchiveWalkLocked(threadID, finished)
}

// endArchiveWalkLocked is endArchiveWalk for a caller that holds the
// thread's action lock.
func (a *App) endArchiveWalkLocked(threadID string, finished bool) ([]store.OwnerOp, error) {
	w := &a.archiveWalks
	w.mu.Lock()
	if w.running[threadID] > 1 {
		w.running[threadID]--
		w.mu.Unlock()
		return nil, nil
	}
	delete(w.running, threadID)
	w.mu.Unlock()
	if hook := a.archiveWalkEndingForTest; hook != nil {
		hook(threadID)
	}
	owed, err := a.store.OweRemoteArchives(threadID, finished)
	if err != nil {
		return nil, fmt.Errorf("record that the archive of %s finished; the next start finishes it: %w", threadID, err)
	}
	return owed, nil
}

// pendingArchives owns the boot walk over the archives a crash stopped
// before their walk ended.
type pendingArchives struct {
	once sync.Once
	wg   sync.WaitGroup
}

// startPendingArchives finishes, in the background, every archive whose
// walk did not end (store.ListUnfinishedArchives), through the archive's
// own path (resumeArchiveLocked, finishArchive): what the thread still has
// of the work an archive stops is stopped, a remote command's cancel owed
// when its computer does not take it, its workers are archived and the
// archive ends. Nothing runs at boot, so the session stop finds nothing.
// It waits for the first client's catalog reads (awaitFirstReadsSettled).
func (a *App) startPendingArchives() {
	if a.store == nil {
		return
	}
	a.pendingArchives.once.Do(func() {
		a.pendingArchives.wg.Add(1)
		go func() {
			defer a.pendingArchives.wg.Done()
			ctx := a.lifeCtx()
			if err := a.awaitFirstReadsSettled(ctx); err != nil {
				return
			}
			a.finishPendingArchives(ctx)
		}()
	})
}

// finishPendingArchives finishes each unfinished archive in turn, logging
// what it could not do: each archive stands, and archiving the thread again
// retries a worker it could not archive.
func (a *App) finishPendingArchives(ctx context.Context) {
	ids, err := a.store.ListUnfinishedArchives()
	if err != nil {
		log.Printf("app: pending archives: %v", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		a.resumeArchive(id)
	}
}

// resumeArchive finishes one unfinished archive: it stops what the thread
// still has of the work the archive stops, then walks its workers and ends
// the archive (finishArchive). A thread reopened since is left alone.
func (a *App) resumeArchive(id string) {
	work, err := a.resumeArchiveLocked(id)
	if err != nil {
		log.Printf("app: pending archive of %s: %v", id, err)
		return
	}
	if !work.captured {
		return
	}
	outcome := a.finishArchive(work, reachLater)
	if outcome.cleanup != nil {
		log.Printf("app: pending archive of %s: %v", id, outcome.cleanup)
	}
	for _, worker := range outcome.workers {
		log.Printf("app: pending archive of %s: worker %s not archived: %v", id, worker.threadID, worker.err)
	}
	for _, unread := range outcome.unread {
		log.Printf("app: pending archive of %s: %s: %v", id, unread.threadID, unread.err)
	}
}

// waitPendingArchives joins the boot walk. Shutdown cancels the app
// context first, and the walk stops before its next thread.
func (a *App) waitPendingArchives() {
	a.pendingArchives.wg.Wait()
}
