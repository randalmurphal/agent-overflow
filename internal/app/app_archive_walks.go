package app

import (
	"fmt"
	"sync"

	"agent-overflow/internal/store"
)

// An archive is durable at its flip; its walk (finishArchive) archives what
// the thread owns and then ends it (endArchiveWalk), owing the paired
// computers the archives of its workers there. Until that end, a walk under
// way in this process (archiveWalks) is what says those archives are on
// their way (owesWorkerEnd).

// archiveWalks counts, by thread, the archive walks under way. A thread
// archived, reopened and archived again while the first walk still runs
// has two; only the last to end ends the archive, so the archives stay on
// their way until the newer walk owes them.
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

// archiveWalking reports whether an archive walk of threadID is under way.
func (a *App) archiveWalking(threadID string) bool {
	w := &a.archiveWalks
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running[threadID] > 0
}

// endArchiveWalk records the end of a walk of threadID's archive. The last
// walk to end ends the archive while the thread is still archived
// (store.OweRemoteArchives), in the transaction that owes the paired
// computers the archives of its workers there, and only then stops counting
// it, so those archives are on their way throughout (owesWorkerEnd). It
// returns what it owed for its caller to deliver (deliverOwnerOps). A
// failure owes nothing; archiving the thread again walks again.
//
// It holds the thread's action lock across the decision and the end: a new
// archive commits and records its walk under that lock (runArchive), so it
// lands before the decision, which then counts it, or after the end, which
// then leaves its count alone.
func (a *App) endArchiveWalk(threadID string) ([]store.OwnerOp, error) {
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	w := &a.archiveWalks
	w.mu.Lock()
	if w.running[threadID] > 1 {
		w.running[threadID]--
		w.mu.Unlock()
		return nil, nil
	}
	w.mu.Unlock()
	if hook := a.archiveWalkEndingForTest; hook != nil {
		hook(threadID)
	}
	owed, err := a.store.OweRemoteArchives(threadID)
	w.mu.Lock()
	delete(w.running, threadID)
	w.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("owe the archives of the workers %s spawned on paired computers; archive it again to retry: %w", threadID, err)
	}
	return owed, nil
}

// owesWorkerEnd reports whether the spawn token's worker on a paired
// computer has an archive or delete on its way: owed in the ledger, or owed
// at the end of its archived caller's walk under way here.
func (a *App) owesWorkerEnd(token string) (bool, error) {
	owed, archivedCaller, err := a.store.OwesWorkerEnd(token)
	if err != nil || owed {
		return owed, err
	}
	return archivedCaller != "" && a.archiveWalking(archivedCaller), nil
}
