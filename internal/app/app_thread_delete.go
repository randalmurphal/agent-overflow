package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"

	"agent-overflow/internal/entityid"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
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
//     and moves nothing (store.CheckThreadNotDeleting).
//  2. Verify. Each claimed thread in turn has its work admission closed and
//     drained (threadRestFence.close), and then, under its action lock
//     alone, the family is claimed again, which takes a worker a drained
//     spawn added, and the thread's refusals run: a transfer, an assistant
//     home the delete may not remove, the terminal's own gate, and for a
//     project removal a running thread. A delete that removes worktrees
//     then asks for git:operate and runs each emptied worktree's
//     loss-of-work gate. Nothing is stopped or removed; a refusal releases
//     every claim in one transaction (store.ReleaseDeleteClaim).
//  3. Commit. One transaction marks every claimed thread deleting with the
//     delete's worktree policy (store.CommitDeleteClaim), and clients are
//     told the rows are gone. From here the delete only goes forward.
//  4. Destroy. Each committed thread once every thread under it is gone,
//     under its own action lock: torn down (threadapp.TearDown), the
//     worktree it was the last thread on removed, and dropped
//     (threadapp.Drop). A failure stops that thread and the threads above
//     it, which stay committed for a repeated delete or the boot to finish.
//
// No action lock is held while another is taken. The boot releases the
// claims no delete committed and finishes every committed family with its
// recorded policy (startPendingThreadDeletes); a delete of a committed
// thread joins it.

// DeleteThread deletes the thread with everything under it, and drops
// their rows. Each committed row is broadcast as `deleted`, so a second
// attached client drops the same rows and their caches without a refresh.
// A thread under it whose destroy failed is named in the result
// (DeleteThreadResult).
//
//ao:scope threads:operate
//ao:route thread
func (a *App) DeleteThread(id string) (DeleteThreadResult, error) {
	outcome, err := a.deleteThreads(deleteRequest{seeds: []string{id}})
	return outcome.result(a), err
}

// DeleteThreadsAndWorktrees is DeleteThread for a person's delete of one or
// more threads of this computer from the sidebar or the archived threads
// list, as one delete: it also removes the worktrees the delete empties, the
// threads' own and those under them, as Remove Worktree does, keeping one a
// thread outside the delete is on. A worktree with unsaved work refuses the
// whole delete, with nothing stopped or deleted. A caller without
// git:operate is refused, with nothing deleted, when there is a worktree to
// remove.
//
//ao:scope threads:operate
//ao:route home
func (a *App) DeleteThreadsAndWorktrees(ctx context.Context, threadIDs []string) (DeleteThreadResult, error) {
	outcome, err := a.deleteThreads(deleteRequest{seeds: threadIDs, removeWorktrees: func() error {
		return a.requireScope(ctx, transport.ScopeGitOperate, "remove the worktrees a delete empties")
	}})
	return outcome.result(a), err
}

// DeleteThreadResult is what a delete left undone. The zero value is a
// complete delete.
type DeleteThreadResult struct {
	// WorkersNotDeleted names each thread under the named ones, at any
	// depth, whose destroy failed, and why. It and the threads above it are
	// committed and hidden until deleting the named threads again, or the
	// next boot, finishes them.
	WorkersNotDeleted []WorkerFailure `json:"workersNotDeleted,omitempty"`
	// RetainedWorktrees are the worktrees the delete emptied and left on
	// disk because their removal failed when it ran: unsaved work that
	// appeared after the delete checked them, or a git refusal. Their
	// threads are deleted.
	RetainedWorktrees []RetainedWorktree `json:"retainedWorktrees,omitempty"`
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
// name.
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

// DeleteDependents is what deleting threads takes with them, read from the
// same projection the delete runs on (store.ListDeleteFamily): the past
// threads and helpers of the assistants whose current threads they are
// (ruling 39), the workers under all of them at any depth, and the
// worktrees a delete from the sidebar or the archived threads list removes.
type DeleteDependents struct {
	PastThreads int `json:"pastThreads"`
	Helpers     int `json:"helpers"`
	Workers     int `json:"workers"`
	// Worktrees are the worktrees the threads and those under them are on
	// that no thread outside the delete is on, each once.
	Worktrees []DeleteWorktree `json:"worktrees"`
}

// DeleteWorktree is one worktree a delete removes, and how many terminals
// open in it the removal closes.
type DeleteWorktree struct {
	ProjectID    string `json:"projectId"`
	WorktreePath string `json:"worktreePath"`
	Terminals    int    `json:"terminals"`
}

// GetDeleteDependents reads what deleting threadIDs, threads of this
// computer, takes with them as one delete, for its confirmation: a worktree
// only threads of the set are on counts once.
//
//ao:scope threads:read
//ao:route home
func (a *App) GetDeleteDependents(threadIDs []string) (DeleteDependents, error) {
	family, err := a.store.ListDeleteFamily(threadIDs)
	if err != nil {
		return DeleteDependents{}, err
	}
	dependents := DeleteDependents{Worktrees: []DeleteWorktree{}}
	for _, member := range family {
		switch member.Kind {
		case store.DeletePastThread:
			dependents.PastThreads++
		case store.DeleteHelper:
			dependents.Helpers++
		case store.DeleteWorker:
			dependents.Workers++
		}
	}
	sites, err := a.emptiedWorktrees(family, func(member store.DeleteMember) bool { return !member.Holder })
	if err != nil {
		return DeleteDependents{}, err
	}
	for _, site := range sites {
		dependents.Worktrees = append(dependents.Worktrees, DeleteWorktree{
			ProjectID: site.projectID, WorktreePath: site.path, Terminals: a.worktreeTerminalCount(site.path),
		})
	}
	return dependents, nil
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
	// removeWorktrees, for a person's delete from the sidebar or the
	// archived threads list, makes the delete remove the worktrees it
	// empties. It is the caller's permission to, asked when there is one.
	removeWorktrees func() error
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
}

// deleteOutcome is what a delete did and could not do after its commit.
type deleteOutcome struct {
	// deleted are the named threads that were this computer's
	// conversations, as against a moved thread's cache or a holder.
	deleted  []string
	failed   []workerFailure
	retained []RetainedWorktree
}

func (o deleteOutcome) result(a *App) DeleteThreadResult {
	return DeleteThreadResult{WorkersNotDeleted: a.workerFailures(o.failed), RetainedWorktrees: o.retained}
}

// logFailures is the report of a delete no person is waiting on, named by
// what.
func (o deleteOutcome) logFailures(what string) {
	for _, worker := range o.failed {
		log.Printf("app: %s: thread %s was not deleted: %v", what, worker.threadID, worker.err)
	}
	for _, kept := range o.retained {
		log.Printf("app: %s: worktree %s was left on disk: %s", what, kept.Path, kept.Reason)
	}
}

// deleteThreads runs one delete of req.seeds and what lives under them,
// holding no action lock on entry.
func (a *App) deleteThreads(req deleteRequest) (deleteOutcome, error) {
	op := a.newFamilyDelete(req)
	defer op.reopen()
	if err := op.claim(); err != nil {
		return deleteOutcome{}, err
	}
	if err := op.verify(); err != nil {
		return deleteOutcome{}, op.refuse(err)
	}
	if err := op.commit(); err != nil {
		return deleteOutcome{}, op.refuse(err)
	}
	return op.destroy()
}

// maxFamilyChanges bounds how often a delete's family may grow while it is
// verified. It grows only when a spawn its threads admitted before the
// claim names its thread, so one change is normal.
const maxFamilyChanges = 8

// familyDelete is one delete's state.
type familyDelete struct {
	app *App
	req deleteRequest
	// id is the operation's, the claim it writes.
	id string
	// members is the family as last claimed, in claim order.
	members []store.DeleteMember
	// verified are the claimed threads whose refusals ran, and retired
	// those that are a moved thread's cache or a holder.
	verified, retired map[string]bool
	// changes counts the claims that found new threads.
	changes int
	// reopens are the admission closures, released when the delete ends.
	reopens []func()
	closed  map[string]bool
}

func (a *App) newFamilyDelete(req deleteRequest) *familyDelete {
	return &familyDelete{
		app:      a,
		req:      req,
		id:       entityid.New(),
		verified: make(map[string]bool),
		retired:  make(map[string]bool),
		closed:   make(map[string]bool),
	}
}

// reopen releases every admission the delete closed.
func (op *familyDelete) reopen() {
	for i := len(op.reopens) - 1; i >= 0; i-- {
		op.reopens[i]()
	}
	op.reopens = nil
}

// closeAdmission closes threadID's admission until the delete ends and
// waits out the work already admitted, so a spawn it made has named its
// thread before the family is read again. No lock may be held.
func (op *familyDelete) closeAdmission(threadID string) {
	if op.closed[threadID] {
		return
	}
	op.closed[threadID] = true
	op.reopens = append(op.reopens, op.app.resting.close(threadID))
}

// claim claims the family (phase 1).
func (op *familyDelete) claim() error {
	members, err := op.app.store.ClaimDeleteFamily(op.id, op.req.seeds)
	if err != nil {
		return op.claimRefusal(err)
	}
	op.members = members
	if hook := op.app.deleteClaimedForTest; hook != nil {
		ids := make([]string, 0, len(members))
		for _, member := range members {
			ids = append(ids, member.ThreadID)
		}
		hook(ids)
	}
	return nil
}

// claimRefusal says which thread of the family another delete holds.
func (op *familyDelete) claimRefusal(err error) error {
	var claimed *store.DeleteClaimedError
	if !errors.As(err, &claimed) {
		return err
	}
	for _, seed := range op.req.seeds {
		if seed == claimed.ThreadID {
			return err
		}
	}
	return fmt.Errorf("thread %s, which the delete of thread %s takes, is already being deleted; nothing was deleted, try again once that delete ends: %w",
		claimed.ThreadID, op.req.seeds[0], store.ErrThreadDeleting)
}

// reclaim claims the family again for this delete, which takes the
// threads that joined it since, and reports whether any did.
func (op *familyDelete) reclaim() (bool, error) {
	members, err := op.app.store.ClaimDeleteFamily(op.id, op.req.seeds)
	if err != nil {
		return false, op.claimRefusal(err)
	}
	grew := false
	for _, member := range members {
		if !slices.ContainsFunc(op.members, func(known store.DeleteMember) bool { return known.ThreadID == member.ThreadID }) {
			grew = true
		}
	}
	op.members = members
	return grew, nil
}

// verify runs every refusal over the claimed family (phase 2), each thread
// under its own action lock with its admission closed and drained, until
// the family stops growing; then the worktrees' gates. It stops nothing.
func (op *familyDelete) verify() error {
	for {
		var next *store.DeleteMember
		for i := range op.members {
			member := op.members[i]
			if !member.Committed && !op.verified[member.ThreadID] {
				next = &member
				break
			}
		}
		if next == nil {
			break
		}
		if err := op.verifyMember(*next); err != nil {
			return err
		}
		if hook := op.app.deleteVerifiedForTest; hook != nil {
			hook(next.ThreadID)
		}
	}
	if op.req.verified != nil {
		if err := op.req.verified(op.members); err != nil {
			return err
		}
	}
	if op.req.removeWorktrees == nil {
		return nil
	}
	sites, err := op.app.emptiedWorktrees(op.members, op.removes)
	if err != nil {
		return err
	}
	if len(sites) == 0 {
		return nil
	}
	if err := op.req.removeWorktrees(); err != nil {
		return err
	}
	for _, site := range sites {
		if err := op.app.checkWorktreeRemovable(site); err != nil {
			return fmt.Errorf("the worktree %s, which this delete would remove, cannot be removed, so nothing was deleted: %w", site.path, err)
		}
	}
	return nil
}

// removes reports whether the delete removes member's worktree: one of the
// threads it claimed, as against a committed one it joined, that is a
// conversation of this computer.
func (op *familyDelete) removes(member store.DeleteMember) bool {
	return !member.Committed && !member.Holder && !op.retired[member.ThreadID]
}

// verifyMember closes member's admission and, under its action lock, claims
// the family again and runs member's refusals.
func (op *familyDelete) verifyMember(member store.DeleteMember) error {
	op.closeAdmission(member.ThreadID)
	unlock := op.app.threadLocks().Lock(member.ThreadID)
	defer unlock()
	grew, err := op.reclaim()
	if err != nil {
		return err
	}
	if grew {
		if op.changes++; op.changes > maxFamilyChanges {
			return fmt.Errorf("delete thread %s: the threads under it kept changing; try again", op.req.seeds[0])
		}
	}
	if err := op.check(member); err != nil {
		return err
	}
	op.verified[member.ThreadID] = true
	return nil
}

// check is a member's refusals, under its action lock: a thread that is
// not this computer's to change (a transfer), an assistant home the delete
// may not remove, a named terminal thread whose shell runs again, and for
// a project removal a running thread.
func (op *familyDelete) check(member store.DeleteMember) error {
	id := member.ThreadID
	named := member.Kind == store.DeleteNamed
	retired, err := op.app.threadApplication().CheckDelete(id, named && op.req.cleanup)
	if err != nil {
		return familyRefusal(member, err)
	}
	if retired {
		op.retired[id] = true
	}
	if _, err := op.app.assistantHomeForDelete(id); err != nil {
		return err
	}
	if named && op.req.admit != nil {
		if err := op.req.admit(id); err != nil {
			return err
		}
	}
	if op.req.project == "" || retired {
		return nil
	}
	reason, err := op.app.threadActivityBlockReason(id)
	if err != nil {
		return fmt.Errorf("delete project: check thread %s activity: %w", id, err)
	}
	switch {
	case reason == "":
		return nil
	case member.ProjectID == op.req.project:
		return fmt.Errorf("cannot delete project while thread %s is running: %s", id, reason)
	default:
		return fmt.Errorf("cannot delete project while worker %s of thread %s is running: %s", id, member.Under, reason)
	}
}

// threadGone is a thread deleted since it was read.
func threadGone(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrThreadGone)
}

// familyRefusal says which thread under the named one refused the delete.
// A transfer's refusal is stated, not wrapped: the dispatcher reports a
// wrapped one as the named thread's own transfer.
func familyRefusal(member store.DeleteMember, err error) error {
	var transfer *store.ThreadTransferError
	switch {
	case member.Kind == store.DeleteNamed || threadGone(err):
		return err
	case errors.As(err, &transfer) && member.Kind == store.DeleteWorker:
		return fmt.Errorf("thread %s, which works under the thread being deleted, is being transferred to another computer (%s), so nothing was deleted; finish or cancel that transfer first", member.ThreadID, transfer.OperationID)
	case errors.As(err, &transfer):
		return fmt.Errorf("thread %s, which lives under this assistant, is being transferred to another computer (%s), so the assistant was not deleted; finish or cancel that transfer first", member.ThreadID, transfer.OperationID)
	case member.Kind == store.DeleteWorker:
		return fmt.Errorf("delete thread %s, which works under the thread being deleted: %w", member.ThreadID, err)
	default:
		return fmt.Errorf("delete thread %s, which lives under the assistant: %w", member.ThreadID, err)
	}
}

// refuse releases every claim of the delete, in one transaction, and
// returns cause: a refused delete changes nothing. A release that fails
// leaves the claims fenced until the next boot releases them, and is said
// beside cause.
func (op *familyDelete) refuse(cause error) error {
	if err := op.app.store.ReleaseDeleteClaim(op.id); err != nil {
		return errors.Join(cause, fmt.Errorf("and the delete's claim could not be taken back until the app restarts: %w", err))
	}
	return cause
}

// commit commits the claimed family (phase 3) and tells clients its rows
// are gone.
func (op *familyDelete) commit() error {
	removeWorktrees := op.req.removeWorktrees != nil
	committed, err := op.app.store.CommitDeleteClaim(op.id, removeWorktrees)
	if err != nil {
		return err
	}
	for i, member := range op.members {
		if !slices.Contains(committed, member.ThreadID) {
			continue
		}
		op.members[i].Committed, op.members[i].RemoveWorktrees = true, removeWorktrees
		if member.Holder {
			// No client lists a holder.
			continue
		}
		op.app.broadcastThreadDeleted(member.ThreadID)
		// Its workers' prompts go where they route now.
		op.app.reroutePromptsFrom(member.ThreadID)
		// It is no live worker now.
		if err := op.app.forgetLiveWorker(member.ThreadID); err != nil {
			log.Printf("agent rows: look up the spawns of live workers after deleting %s: %v", member.ThreadID, err)
		}
	}
	if hook := op.app.deleteCommittedForTest; hook != nil {
		hook(committed)
	}
	return nil
}

// destroy destroys every committed thread of the family (phase 4), each
// once every thread under it is gone. A failure stops that thread and the
// threads above it, which stay committed. A named thread's own failure is
// the delete's error; any other thread's is named in the outcome.
func (op *familyDelete) destroy() (deleteOutcome, error) {
	var outcome deleteOutcome
	for _, member := range op.members {
		if member.Kind == store.DeleteNamed && !op.retired[member.ThreadID] {
			outcome.deleted = append(outcome.deleted, member.ThreadID)
		}
	}
	order := slices.Clone(op.members)
	slices.SortStableFunc(order, func(a, b store.DeleteMember) int { return b.Depth - a.Depth })
	kept := make(map[string]bool)
	var errs []error
	for _, member := range order {
		if !member.Committed {
			continue
		}
		if op.req.ctx != nil && op.req.ctx.Err() != nil {
			return outcome, errors.Join(errs...)
		}
		if kept[member.ThreadID] {
			kept[member.Under] = true
			continue
		}
		err := op.destroyMember(member, &outcome)
		if hook := op.app.deleteDestroyedForTest; hook != nil && err == nil {
			hook(member.ThreadID)
		}
		if err == nil {
			continue
		}
		kept[member.ThreadID], kept[member.Under] = true, true
		if member.Kind == store.DeleteNamed {
			errs = append(errs, err)
			continue
		}
		outcome.failed = append(outcome.failed, workerFailure{threadID: member.ThreadID, err: err})
	}
	return outcome, errors.Join(errs...)
}

// destroyMember tears member down, removes the worktree it was the last
// thread of the delete on, and drops its row, each half under its action
// lock alone.
func (op *familyDelete) destroyMember(member store.DeleteMember, outcome *deleteOutcome) error {
	threads := op.app.threadApplication()
	ports := op.app.threadDeletePorts()
	ports.ChunkPause = op.req.pause
	if hook := op.app.deletePortsForTest; hook != nil {
		hook(&ports)
	}
	unlock := op.app.threadLocks().Lock(member.ThreadID)
	err := threads.TearDown(member.ThreadID, ports)
	unlock()
	if err != nil {
		return err
	}
	if member.RemoveWorktrees && !member.Holder && !op.retired[member.ThreadID] {
		if kept := op.removeEmptiedWorktree(member); kept != nil {
			outcome.retained = append(outcome.retained, *kept)
		}
	}
	unlock = op.app.threadLocks().Lock(member.ThreadID)
	defer unlock()
	return threads.Drop(member.ThreadID, ports)
}

// errWorktreeShared keeps a worktree a thread outside the delete is on.
var errWorktreeShared = errors.New("another thread is on the worktree")

// removeEmptiedWorktree removes the worktree member is on once no other
// thread is: no committed thread waiting for its own destroy, which removes
// it then, and no thread outside the delete, which keeps it. The removal is
// Remove Worktree's (removeProjectWorktree), whose loss-of-work gate runs
// again: a worktree that cannot be removed now is left on disk and returned
// to name in the result; its threads are still deleted.
func (op *familyDelete) removeEmptiedWorktree(member store.DeleteMember) *RetainedWorktree {
	path := member.WorktreePath
	if path == "" || member.ProjectID == "" {
		return nil
	}
	others := func(occupants []string) (waiting, shared bool, err error) {
		for _, id := range occupants {
			if id == member.ThreadID {
				continue
			}
			committed, err := op.app.store.ThreadDeleteCommitted(id)
			if err != nil {
				return false, false, err
			}
			if committed {
				waiting = true
			} else {
				shared = true
			}
		}
		return waiting, shared, nil
	}
	occupants, err := op.app.threadsReferencingWorkspace(path)
	if err == nil {
		var waiting, shared bool
		if waiting, shared, err = others(occupants); err == nil && (waiting || shared) {
			return nil
		}
	}
	var project store.Project
	if err == nil {
		project, err = op.app.store.GetProject(member.ProjectID)
	}
	if err == nil {
		var found bool
		if _, found, err = op.app.findWorktree(project.Path, path); err == nil && !found {
			// Removed already: by this delete before a restart, or by hand.
			return nil
		}
	}
	if err == nil {
		_, err = op.app.removeProjectWorktree(member.ProjectID, project.Path, path, worktreeRemovalPolicy{
			idle: func(occupants []string) error {
				if waiting, shared, err := others(occupants); err != nil {
					return err
				} else if waiting || shared {
					return errWorktreeShared
				}
				return nil
			},
		})
	}
	var busy *errWorktreeBusy
	var followUp *errWorktreeRemovedFollowUp
	switch {
	case err == nil:
		return nil
	case errors.As(err, &busy) && errors.Is(err, errWorktreeShared):
		return nil
	case errors.As(err, &followUp):
		log.Printf("app: delete: worktree %s: %v", path, err)
		return nil
	}
	log.Printf("app: delete thread %s: worktree %s left on disk: %v", member.ThreadID, path, err)
	return &RetainedWorktree{Path: path, Reason: err.Error()}
}

// deleteSite is one worktree a delete empties.
type deleteSite struct {
	projectID, path string
}

// emptiedWorktrees lists, each once, the worktrees of the family members
// take accepts that no thread outside the family is on.
func (a *App) emptiedWorktrees(family []store.DeleteMember, take func(store.DeleteMember) bool) ([]deleteSite, error) {
	in := make(map[string]bool, len(family))
	for _, member := range family {
		in[member.ThreadID] = true
	}
	var sites []deleteSite
	for _, member := range family {
		if !take(member) || member.WorktreePath == "" || member.ProjectID == "" {
			continue
		}
		if slices.ContainsFunc(sites, func(site deleteSite) bool {
			return site.projectID == member.ProjectID && gitops.SameFilesystemPath(site.path, member.WorktreePath)
		}) {
			continue
		}
		occupants, err := a.threadsReferencingWorkspace(member.WorktreePath)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(occupants, func(id string) bool { return !in[id] }) {
			continue
		}
		sites = append(sites, deleteSite{projectID: member.ProjectID, path: member.WorktreePath})
	}
	return sites, nil
}

// checkWorktreeRemovable is the loss-of-work gate of a worktree the delete
// would remove, which changes nothing. A path that is no worktree of its
// project, removed by hand, has nothing to lose.
func (a *App) checkWorktreeRemovable(site deleteSite) error {
	project, err := a.store.GetProject(site.projectID)
	if err != nil {
		return fmt.Errorf("read the project of worktree %s: %w", site.path, err)
	}
	if _, found, err := a.findWorktree(project.Path, site.path); err != nil {
		return fmt.Errorf("validate worktree %s: %w", site.path, err)
	} else if !found {
		return nil
	}
	return a.worktreeLossOfWork(project.Path, site.path, worktreeRemovalPolicy{})
}
