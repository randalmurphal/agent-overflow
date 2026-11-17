package app

import (
	"context"
	"fmt"
	"log"
	"sync"
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
// walk to end ends the archive while the thread is still archived
// (store.OweRemoteArchives), in the transaction that owes the paired
// computers the archives of its workers there. A failure leaves the archive
// unfinished, for the next boot to finish.
func (a *App) endArchiveWalk(threadID string) error {
	w := &a.archiveWalks
	w.mu.Lock()
	if w.running[threadID] > 1 {
		w.running[threadID]--
		w.mu.Unlock()
		return nil
	}
	delete(w.running, threadID)
	w.mu.Unlock()
	if _, err := a.store.OweRemoteArchives(threadID); err != nil {
		return fmt.Errorf("record that the archive of %s finished; the next start finishes it: %w", threadID, err)
	}
	return nil
}

// pendingArchives owns the boot walk over the archives a crash stopped
// before their walk ended.
type pendingArchives struct {
	once sync.Once
	wg   sync.WaitGroup
}

// startPendingArchives finishes, in the background, every archive whose
// walk did not end (store.ListUnfinishedArchives): it archives the workers
// each such thread owns and ends its archive, as finishArchive does. The
// thread's own work stopped with the process, so nothing of it is stopped
// again. It waits for the first client's catalog reads
// (awaitFirstReadsSettled).
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
		outcome := a.finishArchive(a.resumeArchiveWalk(id))
		if outcome.cleanup != nil {
			log.Printf("app: pending archive of %s: %v", id, outcome.cleanup)
		}
		for _, worker := range outcome.workers {
			log.Printf("app: pending archive of %s: worker %s not archived: %v", id, worker.threadID, worker.err)
		}
	}
}

// resumeArchiveWalk reads, under the thread's action lock as an archive
// does, the workers an unfinished archive goes on to archive, and records
// its walk. A thread reopened since has no workers the walk may archive
// (store.ArchiveOwnedWorker's fence) and its archive is already ended.
func (a *App) resumeArchiveWalk(id string) restingThreadWork {
	unlock := a.threadLocks().Lock(id)
	defer unlock()
	work := restingThreadWork{threadID: id}
	a.beginArchiveWalk(id)
	a.readArchivedWorkers(&work)
	return work
}

// waitPendingArchives joins the boot walk. Shutdown cancels the app
// context first, and the walk stops before its next thread.
func (a *App) waitPendingArchives() {
	a.pendingArchives.wg.Wait()
}
