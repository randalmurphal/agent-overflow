package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// Deleting a thread deletes what lives under it: for an assistant's current
// thread its past threads and helpers, and the workers each of them owns,
// down the lineage (docs/specs/standing-agents.md, "Owned workers"). A
// spawned thread with a sidebar row is the person's and stays. Every door
// that deletes, a person's or the app's, runs one operation (familyDelete):
//
//  1. Preflight. What the delete takes is read (store.ListDeleteFamily) and
//     every refusal runs over all of it, before anything is written.
//  2. Claim. Each thread, the named ones first and then down the lineage,
//     has its work admission closed and drained (threadRestFence.close), so
//     a spawn it admitted has named its thread, and then, under its own
//     action lock, its refusals checked again and its claim written: the
//     delete mark, for a worker only while its owner still owns it
//     (store.ClaimThreadDelete). The threads under the claimed ones are read
//     again until none is new. A refusal takes back every claim the delete
//     set, so a refused delete deletes nothing.
//  3. Destroy. A person's delete from the sidebar or the archived threads
//     list removes the worktrees it empties; then each thread is deleted,
//     the deepest first, under its own lock (threadapp.DeleteThread).
//
// No action lock is held while another is taken, and admission stays closed
// until the delete ends. A crash leaves claimed rows, and the boot finishes
// each with what is under it (startPendingThreadDeletes).

// DeleteThread deletes the thread with everything under it, and drops
// their rows. Each dropped row is broadcast as `deleted`, so a second
// attached client drops the same rows and their caches without a refresh.
// A worker whose delete failed after its claim is named in the result
// (DeleteThreadResult).
//
//ao:scope threads:operate
//ao:route thread
func (a *App) DeleteThread(id string) (DeleteThreadResult, error) {
	outcome, err := a.deleteThreads(deleteRequest{seeds: []string{id}})
	return outcome.result(a), err
}

// DeleteThreadAndWorktrees is DeleteThread for a person's delete from the
// sidebar or the archived threads list: it also removes the worktrees the
// delete empties, the thread's and its workers', as Remove Worktree does,
// keeping one another thread is on. A worktree that cannot be removed fails
// the delete of the threads on it: for the named thread nothing is deleted,
// and a worker is named in the result and stays with the threads under it.
// A caller without git:operate is refused, with nothing deleted, when there
// is a worktree to remove.
//
//ao:scope threads:operate
//ao:route thread
func (a *App) DeleteThreadAndWorktrees(ctx context.Context, threadID string) (DeleteThreadResult, error) {
	outcome, err := a.deleteThreads(deleteRequest{seeds: []string{threadID}, removeWorktrees: func() error {
		return a.requireScope(ctx, transport.ScopeGitOperate, "remove the worktrees a delete empties")
	}})
	return outcome.result(a), err
}

// DeleteThreadResult is what a delete left undone. The zero value is a
// complete delete.
type DeleteThreadResult struct {
	// WorkersNotDeleted names each worker under the thread, at any depth,
	// whose delete failed after its claim, and why. One whose worktree could
	// not be removed stays with the threads under it, standalone in the
	// sidebar once the thread above it is gone; one that failed later stays
	// hidden until deleting it again, or the next boot, finishes it.
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
// name. A worker whose delete hid its row has none to read, and goes
// untitled.
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

// DeleteDependents is what deleting a thread takes with it, read from the
// same projection the delete runs on (store.ListDeleteFamily): the past
// threads and helpers of the assistant whose current thread it is (ruling
// 39), the workers under all of them at any depth, and the worktrees a
// delete from the sidebar or the archived threads list removes.
type DeleteDependents struct {
	PastThreads int `json:"pastThreads"`
	Helpers     int `json:"helpers"`
	Workers     int `json:"workers"`
	// Worktrees are the worktrees the thread and those threads are on that
	// no other thread is on, each once.
	Worktrees []DeleteWorktree `json:"worktrees"`
}

// DeleteWorktree is one worktree a delete removes, and how many terminals
// open in it the removal closes.
type DeleteWorktree struct {
	ProjectID    string `json:"projectId"`
	WorktreePath string `json:"worktreePath"`
	Terminals    int    `json:"terminals"`
}

// GetDeleteDependents reads what deleting threadID takes with it, for its
// delete confirmation.
//
//ao:scope threads:read
func (a *App) GetDeleteDependents(threadID string) (DeleteDependents, error) {
	family, err := a.store.ListDeleteFamily([]string{threadID})
	if err != nil {
		return DeleteDependents{}, err
	}
	op := a.newFamilyDelete(deleteRequest{seeds: []string{threadID}})
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
		claimed := &familyMember{DeleteMember: member}
		op.members = append(op.members, claimed)
		op.byID[member.ThreadID] = claimed
	}
	for _, site := range op.worktrees() {
		occupants, err := a.threadsReferencingWorkspace(site.path)
		if err != nil {
			return DeleteDependents{}, err
		}
		if op.shared(occupants) {
			continue
		}
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
	outcome.logFailures(id)
	return err
}

// deleteRequest is what one delete names and how.
type deleteRequest struct {
	// seeds are the threads the delete names.
	seeds []string
	// cleanup admits among the seeds a moved thread's cache and a holder
	// (threadapp.CheckCleanup), for project removal, retention and boot;
	// other deletes refuse a thread that is not this computer's
	// (CheckMutable). A retired seed is deleted without what is under it:
	// a moved thread's workers stayed here.
	cleanup bool
	// removeWorktrees, for a person's delete from the sidebar or the
	// archived threads list, removes the worktrees the delete empties. It
	// is the caller's permission to, asked before the first removal.
	removeWorktrees func() error
	// project, for a project removal, refuses while a thread outside it that
	// the delete takes is running, read under that thread's lock as it is
	// claimed: removing a project never interrupts work.
	project string
	// admit runs under a seed's action lock before its claim; its error
	// refuses the delete.
	admit func(threadID string) error
	// pause paces the store's drain of each row (store.ChunkPause).
	pause store.ChunkPause
}

// deleteOutcome is what a delete could not do after its claims.
type deleteOutcome struct {
	failed []workerFailure
}

func (o deleteOutcome) result(a *App) DeleteThreadResult {
	return DeleteThreadResult{WorkersNotDeleted: a.workerFailures(o.failed)}
}

// logFailures is the report of a delete no person is waiting on.
func (o deleteOutcome) logFailures(threadID string) {
	for _, worker := range o.failed {
		log.Printf("app: delete thread %s: worker %s was not deleted: %v", threadID, worker.threadID, worker.err)
	}
}

// deleteThreads runs one delete of req.seeds and what lives under them,
// holding no action lock on entry.
func (a *App) deleteThreads(req deleteRequest) (deleteOutcome, error) {
	op := a.newFamilyDelete(req)
	defer op.reopen()
	if err := op.preflight(); err != nil {
		return deleteOutcome{}, err
	}
	for _, id := range req.seeds {
		if err := op.claim(store.DeleteMember{ThreadID: id, Kind: store.DeleteNamed}); err != nil {
			return deleteOutcome{}, op.refuse(err)
		}
	}
	return op.run()
}

// maxClaimPasses bounds how often a delete reads again what is under the
// threads it claimed. A pass finds a thread only when one it claimed spawned
// it before its admission closed, so the second pass is normally the last.
const maxClaimPasses = 8

// familyDelete is one delete's state.
type familyDelete struct {
	app *App
	req deleteRequest
	// members are the threads this delete claimed, in claim order: each
	// thread after the one it was reached from.
	members []*familyMember
	byID    map[string]*familyMember
	// left are the threads the delete reached and leaves alone: moved out
	// of their owner's hands before their claim, or gone.
	left map[string]bool
	// reopens are the admission closures, released when the delete ends.
	reopens []func()
	closed  map[string]bool
	// failed are the threads whose delete failed after their claim.
	failed []memberFailure
}

// memberFailure is one claimed thread the delete could not delete.
type memberFailure struct {
	member *familyMember
	err    error
}

// familyMember is one thread a delete claimed.
type familyMember struct {
	store.DeleteMember
	// fresh is a claim this delete set, which a refusal takes back.
	fresh bool
	// retired is a seed that is a moved thread's cache or a holder: nothing
	// under it goes with it.
	retired bool
	// restored is a thread whose worktree could not be removed: its claim
	// was taken back and it stays.
	restored bool
}

func (a *App) newFamilyDelete(req deleteRequest) *familyDelete {
	return &familyDelete{
		app:    a,
		req:    req,
		byID:   make(map[string]*familyMember),
		left:   make(map[string]bool),
		closed: make(map[string]bool),
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
// thread before the thread's workers are read. No lock may be held.
func (op *familyDelete) closeAdmission(threadID string) {
	if op.closed[threadID] {
		return
	}
	op.closed[threadID] = true
	op.reopens = append(op.reopens, op.app.resting.close(threadID))
}

// preflight runs every refusal over what the delete takes, before anything
// is written.
func (op *familyDelete) preflight() error {
	family, err := op.app.store.ListDeleteFamily(op.req.seeds)
	if err != nil {
		return err
	}
	for _, member := range family {
		if _, err := op.check(member); err != nil && !threadGone(err) {
			return err
		}
	}
	return nil
}

// check is a member's refusals: a thread that is not this computer's to
// change (a transfer), an assistant home the delete may not remove, and for
// a project removal a running thread outside the project. retired reports a
// seed the delete takes without what is under it.
func (op *familyDelete) check(member store.DeleteMember) (retired bool, err error) {
	id := member.ThreadID
	threads := op.app.threadApplication()
	named := member.Kind == store.DeleteNamed
	if named && op.req.cleanup {
		retired, err = threads.CheckCleanup(id)
	} else {
		err = threads.CheckMutable(id)
	}
	if err != nil {
		return false, familyRefusal(member, err)
	}
	if named {
		if _, err := op.app.assistantHomeForDelete(id); err != nil {
			return false, err
		}
	}
	if op.req.project != "" && !named && member.ProjectID != op.req.project {
		reason, err := op.app.threadActivityBlockReason(id)
		if err != nil {
			return false, fmt.Errorf("delete project: check worker %s activity: %w", id, err)
		}
		if reason != "" {
			return false, fmt.Errorf("cannot delete project while worker %s of thread %s is running: %s", id, member.Under, reason)
		}
	}
	return retired, nil
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

// claim closes member's admission and claims it under its action lock.
func (op *familyDelete) claim(member store.DeleteMember) error {
	op.closeAdmission(member.ThreadID)
	unlock := op.app.threadLocks().Lock(member.ThreadID)
	err := op.claimLocked(member)
	unlock()
	if hook := op.app.deleteClaimedForTest; hook != nil && err == nil {
		hook(member.ThreadID)
	}
	return err
}

// claimLocked checks member's refusals again and writes its claim, under
// its action lock, which the caller holds. A worker moved out of its
// owner's hands, and a thread deleted since, are left alone with what is
// under them; a named thread deleted since is still deleted, for what it
// left running.
func (op *familyDelete) claimLocked(member store.DeleteMember) error {
	id := member.ThreadID
	named := member.Kind == store.DeleteNamed
	retired, err := op.check(member)
	switch {
	case threadGone(err) && !named:
		op.left[id] = true
		return nil
	case err != nil && !threadGone(err):
		return err
	}
	if named && op.req.admit != nil {
		if err := op.req.admit(id); err != nil {
			return err
		}
	}
	// The worktree the thread is on now, which its lock holds still.
	switch row, err := op.app.store.GetThread(id); {
	case err == nil:
		member.ProjectID, member.WorktreePath = row.ProjectID, row.WorktreePath
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	fence := ""
	if member.Kind == store.DeleteWorker {
		fence = member.Under
	}
	fresh, err := op.app.threadApplication().ClaimDelete(id, fence)
	switch {
	case errors.Is(err, store.ErrWorkerNotOwned), threadGone(err) && !named:
		op.left[id] = true
		return nil
	case threadGone(err):
		retired = true
	case err != nil:
		return err
	}
	claimed := &familyMember{DeleteMember: member, fresh: fresh, retired: retired}
	op.members = append(op.members, claimed)
	op.byID[id] = claimed
	return nil
}

// run claims what lives under the claimed seeds, then deletes it all.
func (op *familyDelete) run() (deleteOutcome, error) {
	if err := op.claimUnder(); err != nil {
		return deleteOutcome{}, op.refuse(err)
	}
	if op.req.removeWorktrees != nil {
		if err := op.removeWorktrees(); err != nil {
			return deleteOutcome{}, op.refuse(err)
		}
	}
	return op.destroy()
}

// claimUnder claims, top-down, every thread under the claimed ones, reading
// them again after each pass until none is new. Each is read after the
// thread above it was claimed with its admission closed, while the spawn
// links that make it a worker still stand.
func (op *familyDelete) claimUnder() error {
	for pass := 0; ; pass++ {
		var walk []string
		for _, member := range op.members {
			if !member.retired {
				walk = append(walk, member.ThreadID)
			}
		}
		family, err := op.app.store.ListDeleteFamily(walk)
		if err != nil {
			return err
		}
		var next []store.DeleteMember
		for _, member := range family {
			if op.byID[member.ThreadID] == nil && !op.left[member.ThreadID] {
				next = append(next, member)
			}
		}
		if len(next) == 0 {
			return nil
		}
		if pass == maxClaimPasses {
			return fmt.Errorf("delete thread %s: the threads under it kept changing; try again", op.req.seeds[0])
		}
		for _, member := range next {
			// One under a thread this pass left stays with it.
			if op.byID[member.Under] == nil {
				continue
			}
			if err := op.claim(member); err != nil {
				return err
			}
		}
	}
}

// refuse takes back every claim the delete set and returns cause: a refused
// delete deletes nothing. A claim it cannot take back keeps its thread
// hidden until the boot finishes its delete, and is said beside cause.
func (op *familyDelete) refuse(cause error) error {
	var errs []error
	for i := len(op.members) - 1; i >= 0; i-- {
		if err := op.release(op.members[i]); err != nil {
			errs = append(errs, err)
		}
	}
	op.members, op.byID = nil, make(map[string]*familyMember)
	if len(errs) > 0 {
		return errors.Join(cause, fmt.Errorf("and the delete could not be taken back: %w", errors.Join(errs...)))
	}
	return cause
}

// release takes back member's claim under its action lock, when this delete
// set it.
func (op *familyDelete) release(member *familyMember) error {
	if !member.fresh {
		return nil
	}
	unlock := op.app.threadLocks().Lock(member.ThreadID)
	defer unlock()
	return op.app.threadApplication().ReleaseDelete(member.ThreadID)
}

// under lists member and every claimed thread reached through it.
func (op *familyDelete) under(member *familyMember) []*familyMember {
	in := map[string]bool{member.ThreadID: true}
	out := []*familyMember{member}
	for _, candidate := range op.members {
		if !in[candidate.ThreadID] && in[candidate.Under] {
			in[candidate.ThreadID] = true
			out = append(out, candidate)
		}
	}
	return out
}

// errWorktreeShared keeps a worktree a thread the delete does not take is
// on.
var errWorktreeShared = errors.New("another thread is on the worktree")

// deleteWorktree is one worktree a delete empties, with the claimed threads
// on it.
type deleteWorktree struct {
	projectID, path string
	members         []*familyMember
}

// worktrees groups the claimed threads by the worktree each is on, each
// worktree once, in claim order.
func (op *familyDelete) worktrees() []*deleteWorktree {
	var sites []*deleteWorktree
	for _, member := range op.members {
		if member.retired || member.WorktreePath == "" || member.ProjectID == "" {
			continue
		}
		var site *deleteWorktree
		for _, candidate := range sites {
			if candidate.projectID == member.ProjectID && gitops.SameFilesystemPath(candidate.path, member.WorktreePath) {
				site = candidate
				break
			}
		}
		if site == nil {
			site = &deleteWorktree{projectID: member.ProjectID, path: member.WorktreePath}
			sites = append(sites, site)
		}
		site.members = append(site.members, member)
	}
	return sites
}

// shared reports whether a thread the delete does not take is on path.
func (op *familyDelete) shared(occupants []string) bool {
	for _, id := range occupants {
		if op.byID[id] == nil {
			return true
		}
	}
	return false
}

// removeWorktrees removes each worktree the claimed threads empty through
// Remove Worktree's remover (removeProjectWorktree), after the sessions on
// it stop: its loss-of-work gate keeps uncommitted or unpushed work, and a
// worktree another thread is on is kept. One that cannot be removed fails
// the delete of the threads on it: each is restored with what is under it.
// The caller's permission is asked before the first removal; its refusal
// is returned, with nothing removed.
func (op *familyDelete) removeWorktrees() error {
	permitted := false
	for _, site := range op.worktrees() {
		// A thread restored after an earlier worktree was kept stays on its
		// own.
		if slices.ContainsFunc(site.members, func(member *familyMember) bool { return member.restored }) {
			continue
		}
		occupants, err := op.app.threadsReferencingWorkspace(site.path)
		if err == nil && op.shared(occupants) {
			continue
		}
		if err == nil && !permitted {
			if err := op.req.removeWorktrees(); err != nil {
				return err
			}
			permitted = true
		}
		if err == nil {
			for _, member := range site.members {
				if stopErr := op.app.stopSession(member.ThreadID); stopErr != nil {
					log.Printf("app: delete thread %s: stop its session before removing worktree %s: %v", member.ThreadID, site.path, stopErr)
				}
			}
			err = op.removeWorktree(site)
		}
		var busy *errWorktreeBusy
		var followUp *errWorktreeRemovedFollowUp
		switch {
		case err == nil:
		case errors.As(err, &busy) && errors.Is(err, errWorktreeShared):
		case errors.As(err, &followUp):
			log.Printf("app: delete: worktree %s: %v", site.path, err)
		default:
			for _, member := range site.members {
				op.restore(member, err)
			}
		}
	}
	return nil
}

// removeWorktree removes one worktree the delete empties.
func (op *familyDelete) removeWorktree(site *deleteWorktree) error {
	project, err := op.app.store.GetProject(site.projectID)
	if err != nil {
		return fmt.Errorf("remove worktree %s: %w", site.path, err)
	}
	_, err = op.app.removeProjectWorktree(site.projectID, project.Path, site.path, worktreeRemovalPolicy{
		idle: func(occupants []string) error {
			if op.shared(occupants) {
				return errWorktreeShared
			}
			return nil
		},
	})
	return err
}

// restore takes back the claims of member and what is under it after its
// worktree could not be removed, and records why on member.
func (op *familyDelete) restore(member *familyMember, cause error) {
	if member.restored {
		return
	}
	for _, kept := range op.under(member) {
		if kept.restored {
			continue
		}
		kept.restored = true
		if err := op.release(kept); err != nil {
			log.Printf("app: delete thread %s: take back its delete after its worktree was kept: %v", kept.ThreadID, err)
		}
	}
	op.failed = append(op.failed, memberFailure{member: member, err: cause})
}

// destroy deletes every claimed thread not restored, the deepest first,
// each under its own action lock. A named thread's failure is the delete's
// error; any other thread's is named in the outcome.
func (op *familyDelete) destroy() (deleteOutcome, error) {
	var errs []error
	for i := len(op.members) - 1; i >= 0; i-- {
		member := op.members[i]
		if member.restored {
			continue
		}
		if err := op.deleteMember(member); err != nil {
			op.failed = append(op.failed, memberFailure{member: member, err: err})
		}
	}
	var outcome deleteOutcome
	for _, failure := range op.failed {
		if failure.member.Kind == store.DeleteNamed {
			errs = append(errs, failure.err)
			continue
		}
		outcome.failed = append(outcome.failed, workerFailure{threadID: failure.member.ThreadID, err: failure.err})
	}
	return outcome, errors.Join(errs...)
}

// deleteMember deletes one claimed thread under its action lock.
func (op *familyDelete) deleteMember(member *familyMember) error {
	unlock := op.app.threadLocks().Lock(member.ThreadID)
	defer unlock()
	ports := op.app.threadDeletePorts()
	ports.ChunkPause = op.req.pause
	return op.app.threadApplication().DeleteThread(member.ThreadID, ports)
}
