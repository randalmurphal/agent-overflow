package app

import (
	"context"
	"errors"
	"log"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Deleting a thread deletes what lives under it: for an assistant's current
// thread its past threads and helpers, and the workers each of them owns,
// down the lineage (docs/specs/standing-agents.md, "Owned workers"). A
// spawned thread with a sidebar row is the person's and stays. Every door
// that deletes, a person's or the app's, runs one operation (familyDelete)
// in four phases, each phase's write the store's (store/thread_delete.go):
//
//  1. Claim. One transaction claims the whole family for the operation
//     (store.ClaimDeleteFamily), refused, with nothing claimed, when another
//     delete holds a claim in it. A claimed thread stays listed, and runs
//     and moves nothing (store.CheckThreadNotDeleting): a person's door
//     refuses it, and a start the app makes on its own waits for the
//     decision (deleteDecisions).
//  2. Verify. The claimed threads have their work admission closed and
//     drained (threadRestFence.close), the family is claimed again, which
//     takes a worker a drained spawn added, and each thread's refusals run
//     under its action lock alone: a transfer, an assistant home the delete
//     may not remove, the terminal's own gate, and for a project removal a
//     running thread. The threads that claim added go the same way, until
//     none is added. Nothing is stopped; a refusal releases every claim in
//     one transaction (store.ReleaseDeleteClaim).
//  3. Commit. One transaction marks every claimed thread deleting
//     (store.CommitDeleteClaim). Clients are told the rows are gone. From
//     here the delete only goes forward.
//  4. Destroy. Each committed thread once every thread under it is gone,
//     under its own action lock (threadapp.Destroy). A failure stops that
//     thread and the threads above it, which stay committed for a repeated
//     delete or the boot to finish.
//
// A delete never removes, checks or waits for a worktree: the worktrees its
// threads were on stay, for the worktree actions to remove (docs/decisions.md).
// No action lock is held while another is taken. The boot releases the
// claims no delete committed and finishes every committed family
// (startPendingThreadDeletes); a delete of a committed thread joins it.

// DeleteThread deletes the thread with everything under it, and drops
// their rows. Each committed row is broadcast as `deleted`, so a second
// attached client drops the same rows and their caches without a refresh.
// A thread under it whose destroy failed is named in the result
// (DeleteThreadResult).
//
//ao:scope threads:operate
//ao:route thread
func (a *App) DeleteThread(id string) (DeleteThreadResult, error) {
	outcome, err := a.deleteThreads(deleteRequest{seeds: []string{id}, reach: reachNow})
	return outcome.result(a), err
}

// DeleteThreads is DeleteThread for a person's delete of one or more
// threads of this computer from the sidebar or the archived threads list,
// as one delete: one claim of everything under them, and one refusal.
//
//ao:scope threads:operate
//ao:route home
func (a *App) DeleteThreads(threadIDs []string) (DeleteThreadResult, error) {
	outcome, err := a.deleteThreads(deleteRequest{seeds: threadIDs, reach: reachNow})
	return outcome.result(a), err
}

// DeleteThreadResult is what a delete left undone. The zero value is a
// complete delete.
type DeleteThreadResult struct {
	// WorkersNotDeleted names each thread under the named ones, at any
	// depth, whose destroy failed, and why. It and the threads above it are
	// committed and hidden until deleting the named threads again, or the
	// next boot, finishes them. A worker on another computer named here
	// either has not taken the delete yet (Pending), which is sent again
	// until it does, or runs a version that cannot take it.
	WorkersNotDeleted []WorkerFailure `json:"workersNotDeleted,omitempty"`
	// CleanupError says, for a person, what the deleted threads started
	// elsewhere that may keep running (threadCleanupError). The delete
	// stands.
	CleanupError string `json:"cleanupError,omitempty"`
}

// WorkerFailure is one worker an archive or a delete of the thread above
// it could not carry out.
type WorkerFailure struct {
	ThreadID string `json:"threadId"`
	Title    string `json:"title"`
	Error    string `json:"error"`
	// Computer names the paired computer a worker there is on, as this
	// computer names it; empty for a worker here.
	Computer string `json:"computer,omitempty"`
	// ComputerID is that computer's id here, empty for a worker here. Two
	// paired computers may share a name; a report groups by this.
	ComputerID string `json:"computerId,omitempty"`
	// Pending is a worker on another computer that has not taken the
	// archive or delete yet: it is sent again until that computer takes
	// it, and Error says why the last attempt did not land.
	Pending bool `json:"pending,omitempty"`
}

type workerFailure struct {
	threadID string
	err      error
	// computer names the paired computer of a worker there and computerID
	// is its id; such a worker's title is the one its spawn recorded.
	// pending is WorkerFailure.Pending.
	computer   string
	computerID string
	title      string
	pending    bool
}

// workerFailures renders failures for a binding, a worker here titled by
// its row.
func (a *App) workerFailures(failures []workerFailure) []WorkerFailure {
	var out []WorkerFailure
	for _, worker := range failures {
		failure := WorkerFailure{ThreadID: worker.threadID, Title: worker.title, Error: worker.err.Error(),
			Computer: worker.computer, ComputerID: worker.computerID, Pending: worker.pending}
		if worker.computer == "" {
			if row, err := a.store.GetThread(worker.threadID); err == nil {
				failure.Title = row.Title
			}
		}
		out = append(out, failure)
	}
	return out
}

// DeleteDependents is what deleting threads takes with them, read from the
// same projection the delete runs on (store.ListDeleteFamily): the past
// threads and helpers of the assistants whose current threads they are
// (ruling 39), and the workers under all of them at any depth.
type DeleteDependents struct {
	PastThreads int `json:"pastThreads"`
	Helpers     int `json:"helpers"`
	Workers     int `json:"workers"`
	// RemoteWorkers is whether any of them spawned a thread on another
	// computer, which the delete deletes there too: a worker there that
	// still belongs to its spawner. A worker that computer last said has a
	// sidebar row there stays and is left out (peerWorkerStandalone); one
	// whose placement is not known here is in. Not counted, because only
	// that computer knows which belong.
	RemoteWorkers bool `json:"remoteWorkers"`
}

// GetDeleteDependents reads what deleting threadIDs, threads of this
// computer, takes with them as one delete, for its confirmation.
//
//ao:scope threads:read
//ao:route home
func (a *App) GetDeleteDependents(threadIDs []string) (DeleteDependents, error) {
	family, err := a.store.ListDeleteFamily(threadIDs)
	if err != nil {
		return DeleteDependents{}, err
	}
	var dependents DeleteDependents
	ids := make([]string, 0, len(family))
	for _, member := range family {
		ids = append(ids, member.ThreadID)
		switch member.Kind {
		case store.DeletePastThread:
			dependents.PastThreads++
		case store.DeleteHelper:
			dependents.Helpers++
		case store.DeleteWorker:
			dependents.Workers++
		case store.DeleteNamed:
		}
	}
	remote, err := a.store.ListRemoteSpawnTokens(ids)
	if err != nil {
		return DeleteDependents{}, err
	}
	dependents.RemoteWorkers = slices.ContainsFunc(remote, func(token string) bool { return !a.peerWorkerStandalone(token) })
	return dependents, nil
}

// peerWorkerStandalone reports whether the last agent_rows answer for the
// spawn token says its thread there has a sidebar row, which keeps it out
// of its spawner's delete there (store.LinkFence). Unknown is false.
func (a *App) peerWorkerStandalone(token string) bool {
	c := &a.threadAgentPeers
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[token]
	return entry != nil && entry.err == nil && entry.listing.Placement == threadtools.PlacementStandalone
}

// deleteThread is DeleteThread for the app's own deletes, which have no
// person to name a worker to: a worker that could not be deleted is logged.
func (a *App) deleteThread(id string) error {
	outcome, err := a.deleteThreads(deleteRequest{seeds: []string{id}})
	outcome.logFailures("delete thread " + id)
	return err
}

// deleteRequest is what one delete names and how.
type deleteRequest struct {
	// seeds are the threads the delete names.
	seeds []string
	// cleanup admits among the seeds a moved thread's cache and a holder
	// (threadapp.CheckDelete), for project removal, retention and boot;
	// other deletes refuse a thread that is not this computer's. Such a
	// seed is deleted without what is under it: a moved thread's workers
	// stayed here.
	cleanup bool
	// project, for a project removal, refuses while a thread the delete
	// takes is running, read under that thread's lock: removing a project
	// never interrupts work.
	project string
	// admit runs under a seed's action lock as the delete verifies it; its
	// error refuses the delete.
	admit func(threadID string) error
	// verified runs once every claimed thread is verified, just before the
	// commit; its error refuses the delete.
	verified func(members []store.DeleteMember) error
	// pause paces the store's drain of each row (store.ChunkPause).
	pause store.ChunkPause
	// ctx, when set, stops the destroy before its next thread once done,
	// leaving the rest committed for the boot.
	ctx context.Context
	// link, for the delete a paired computer's owner sends for its worker,
	// the one seed, claims that worker only while the link owns it
	// (store.ClaimLinkedDelete).
	link *store.LinkFence
	// reach is when the delete delivers what its commit owed the paired
	// computers its threads spawned on.
	reach ownerOpReach
}

// deleteOutcome is what a delete did and could not do after its commit.
type deleteOutcome struct {
	// deleted are the named threads that were this computer's
	// conversations, as against a moved thread's cache or a holder.
	deleted []string
	failed  []workerFailure
	// remote are the workers on other computers the delete did not reach
	// yet, or cannot: it stands here, and is delivered there later.
	remote []workerFailure
	// cleanup is what the destroyed threads started elsewhere that the
	// delete could not stop; the ports logged its causes.
	cleanup threadCleanupError
}

func (o deleteOutcome) result(a *App) DeleteThreadResult {
	return DeleteThreadResult{
		WorkersNotDeleted: a.workerFailures(slices.Concat(o.failed, o.remote)),
		CleanupError:      o.cleanup.message(),
	}
}

// logFailures is the report of a delete no person is waiting on, named by
// what. The work it could not stop is already in the log (threadDeletePorts).
func (o deleteOutcome) logFailures(what string) {
	for _, worker := range slices.Concat(o.failed, o.remote) {
		log.Printf("app: %s: thread %s was not deleted: %v", what, worker.threadID, worker.err)
	}
}

// deleteUnstopped gathers what a delete could not stop of the work its
// threads started elsewhere. Destroy stops each thread's remote work twice:
// an ask the first pass could not stop is tried again, and the last attempt
// per thread stands; a command whose cancel was not delivered is dismissed
// either way and never tried again by the delete, so every pass's count.
type deleteUnstopped struct {
	asks     map[string]error
	order    []string
	commands error
}

// note records one pass over threadID.
func (u *deleteUnstopped) note(threadID string, asks, commands error) {
	if u.asks == nil {
		u.asks = make(map[string]error)
	}
	if _, seen := u.asks[threadID]; !seen {
		u.order = append(u.order, threadID)
	}
	u.asks[threadID] = asks
	u.commands = errors.Join(u.commands, commands)
}

// cleanup is the delete's cleanup error.
func (u *deleteUnstopped) cleanup() threadCleanupError {
	cleanup := threadCleanupError{outcome: "deleted", commands: u.commands}
	for _, id := range u.order {
		cleanup.asks = errors.Join(cleanup.asks, u.asks[id])
	}
	return cleanup
}

// deleteThreads runs one delete of req.seeds and what lives under them,
// holding no action lock on entry.
func (a *App) deleteThreads(req deleteRequest) (deleteOutcome, error) {
	op := a.newFamilyDelete(req)
	defer op.reopen()
	a.deleteDecisions.open(op.id)
	// A delete that ends without committing lets its waiters go: they find
	// its claims released, or standing with no delete when the claim or the
	// release failed. A commit decides earlier (commit).
	defer a.deleteDecisions.decide(op.id, false)
	if err := op.claim(); err != nil {
		return deleteOutcome{}, err
	}
	if err := op.verify(); err != nil {
		return deleteOutcome{}, op.refuse(err)
	}
	if err := op.commit(); err != nil {
		return deleteOutcome{}, op.refuse(err)
	}
	outcome, err := op.destroy()
	outcome.cleanup = op.unstopped.cleanup()
	outcome.remote = a.deliverOwnerOps(op.owed, op.owedTargets, req.reach)
	return outcome, err
}
