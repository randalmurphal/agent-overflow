package app

import (
	"database/sql"
	"errors"
	"log"

	"agent-overflow/internal/store"
)

// Deleting a thread deletes the workers it owns, down the lineage, each by
// its own delete (docs/specs/standing-agents.md, "Owned workers"). A
// spawned thread with a sidebar row is the person's and stays.

// DeleteThread tears down the thread and drops its row, with the workers
// it owns: each is deleted the way this delete deletes the thread, under
// its own lock, once this thread's session has stopped and before its row
// is marked (threadapp.DeletePorts.DeleteWorkers).
//
// Every dropped row is broadcast as `deleted` so a second attached client
// drops the same rows and their caches without a refresh. The fan-out rides
// the delete port's per-row Deleted callback.
//
// The thread's own delete does not wait on its workers': a worker whose
// delete fails is named in the result, and it is standalone once the
// thread's row is gone.
//
//ao:scope threads:operate
//ao:route thread
func (a *App) DeleteThread(id string) (DeleteThreadResult, error) {
	unlock := a.threadLocks().Lock(id)
	defer unlock()
	// A stale thread action must discover its new owner. Only host-local
	// project/retention cleanup may discard a confirmed move's old cache.
	if err := a.threadApplication().CheckMutable(id); err != nil {
		return DeleteThreadResult{}, err
	}
	cascade := newWorkerCascade(id)
	err := a.deleteThreadCascading(id, cascade)
	return cascade.result(a), err
}

// DeleteThreadResult is what a delete left undone. The zero value is a
// complete delete.
type DeleteThreadResult struct {
	// WorkersNotDeleted names each worker under the thread, at any depth,
	// whose delete failed. It stays, standalone in the sidebar once the
	// thread above it is gone; deleting it again retries it.
	WorkersNotDeleted []WorkerFailure `json:"workersNotDeleted,omitempty"`
}

// WorkerFailure is one worker an archive or a delete of the thread above
// it could not carry out.
type WorkerFailure struct {
	ThreadID string `json:"threadId"`
	Title    string `json:"title"`
	Error    string `json:"error"`
}

type workerFailure struct {
	threadID string
	err      error
}

// workerFailures renders failures for a binding, titled by the rows they
// name. A worker its delete hid has no row to read, and goes untitled.
func (a *App) workerFailures(failures []workerFailure) []WorkerFailure {
	var out []WorkerFailure
	for _, worker := range failures {
		failure := WorkerFailure{ThreadID: worker.threadID, Error: worker.err.Error()}
		if row, err := a.store.GetThread(worker.threadID); err == nil {
			failure.Title = row.Title
		}
		out = append(out, failure)
	}
	return out
}

// workerCascade is one delete's walk down the workers it deletes: the
// threads whose action locks the walk holds, which it neither waits on nor
// deletes a second time, and the workers whose deletes failed.
type workerCascade struct {
	held   map[string]bool
	failed []workerFailure
}

// newWorkerCascade starts a walk whose caller holds the action locks of
// held.
func newWorkerCascade(held ...string) *workerCascade {
	cascade := &workerCascade{held: make(map[string]bool, len(held))}
	for _, id := range held {
		cascade.held[id] = true
	}
	return cascade
}

func (c *workerCascade) result(a *App) DeleteThreadResult {
	return DeleteThreadResult{WorkersNotDeleted: a.workerFailures(c.failed)}
}

// logFailures is the report of a delete no person is waiting on, or one
// whose door has nowhere to name a worker.
func (c *workerCascade) logFailures(threadID string) {
	for _, worker := range c.failed {
		log.Printf("app: delete thread %s: worker %s was not deleted: %v", threadID, worker.threadID, worker.err)
	}
}

// deleteThreadCascading deletes threadID, whose action lock the caller
// holds, with the workers it owns, recording in cascade those it could not
// delete.
func (a *App) deleteThreadCascading(threadID string, cascade *workerCascade) error {
	return a.threadApplication().DeleteThread(threadID, a.threadDeletePorts(cascade))
}

// deleteThreadLocked deletes threadID, whose action lock the caller holds,
// with the workers it owns. A worker that could not be deleted is logged.
func (a *App) deleteThreadLocked(threadID string) error {
	cascade := newWorkerCascade(threadID)
	err := a.deleteThreadCascading(threadID, cascade)
	cascade.logFailures(threadID)
	return err
}

// deleteThread is DeleteThread for the app's own deletes, which have no
// person to name a worker to: a worker that could not be deleted is logged.
func (a *App) deleteThread(id string) error {
	unlock := a.threadLocks().Lock(id)
	defer unlock()
	if err := a.threadApplication().CheckMutable(id); err != nil {
		return err
	}
	return a.deleteThreadLocked(id)
}

// deleteOwnedWorkers is the delete's DeleteWorkers port: it deletes each
// worker ownerID owns, archived or not, through deleteWorker. A worker the
// walk holds is the caller's to delete. A worker whose delete fails is
// recorded and the others still go.
func (a *App) deleteOwnedWorkers(ownerID string, cascade *workerCascade) error {
	workers, err := a.store.ListOwnedWorkers(ownerID)
	if err != nil {
		return err
	}
	for _, id := range workers {
		if cascade.held[id] {
			continue
		}
		if err := a.deleteWorker(id, ownerID, cascade); err != nil {
			cascade.failed = append(cascade.failed, workerFailure{threadID: id, err: err})
		}
	}
	return nil
}

// deleteWorker deletes one worker for its owner's delete under the worker's
// action lock, as the binding deletes a thread, and in turn the workers it
// owns. A worker given a sidebar row or another owner since the owner's
// delete read it is left alone (threadapp.Service.DeleteWorker), and one
// deleted since is gone already: neither is a failure.
func (a *App) deleteWorker(id, ownerID string, cascade *workerCascade) error {
	unlock := a.threadLocks().Lock(id)
	defer unlock()
	cascade.held[id] = true
	defer delete(cascade.held, id)
	err := a.threadApplication().CheckMutable(id)
	if err == nil {
		err = a.threadApplication().DeleteWorker(id, ownerID, a.threadDeletePorts(cascade))
	}
	if errors.Is(err, store.ErrWorkerNotOwned) || errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrThreadGone) {
		return nil
	}
	return err
}
