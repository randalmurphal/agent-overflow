package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
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
//     and moves nothing (store.CheckThreadNotDeleting): a person's door
//     refuses it, and a start the app makes on its own waits for the
//     decision (deleteDecisions).
//  2. Verify. The claimed threads have their work admission closed and
//     drained (threadRestFence.close), the family is claimed again, which
//     takes a worker a drained spawn added, and each thread's refusals run
//     under its action lock alone: a transfer, an assistant home the delete
//     may not remove, the terminal's own gate, and for a project removal a
//     running thread. The threads that claim added go the same way, until
//     none is added. A delete that removes worktrees then settles the
//     worktrees its threads are on (verifyWorktrees): it asks for
//     git:operate and runs the loss-of-work gate of exactly the ones no
//     thread outside it is on. Nothing is stopped or removed; a refusal
//     releases every claim in one transaction (store.ReleaseDeleteClaim).
//  3. Commit. One transaction marks every claimed thread deleting and
//     records, on each thread on a worktree verify settled, that its
//     destroy removes it, unless a thread outside the delete is on it now
//     (store.CommitDeleteClaim). Clients are told the rows are gone. From
//     here the delete only goes forward.
//  4. Destroy. Each committed thread once every thread under it is gone,
//     under its own action lock: torn down (threadapp.TearDown), its
//     worktree removed when its commit recorded that and it is the last
//     thread on it, and dropped (threadapp.Drop). A failure stops that
//     thread and the threads above it, which stay committed for a repeated
//     delete or the boot to finish.
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
// remove. One that another delete under way also takes a thread of refuses
// it when that delete is the older, and is otherwise kept and named.
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
	// RetainedWorktrees are the worktrees the delete would have removed and
	// left on disk, and why: a thread outside the delete reached one after
	// the delete checked it, another delete takes the other thread on it,
	// or its removal failed when it ran (unsaved work that appeared since,
	// or a git refusal). Their threads are deleted.
	RetainedWorktrees []RetainedWorktree `json:"retainedWorktrees,omitempty"`
	// WorktreeCleanupFailures are the worktrees the delete removed whose
	// cleanup after the removal failed, such as a terminal opened in one
	// that would not close. The worktrees and their threads are gone.
	WorktreeCleanupFailures []WorktreeCleanupFailure `json:"worktreeCleanupFailures,omitempty"`
}

// WorktreeCleanupFailure is one worktree a delete removed and what failed
// after it was removed.
type WorktreeCleanupFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
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
	sites, err := a.familyWorktrees(family, removesWorktree)
	if err != nil {
		return DeleteDependents{}, err
	}
	for _, site := range sites {
		if len(site.outside) > 0 {
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
	cleanup  []WorktreeCleanupFailure
}

func (o deleteOutcome) result(a *App) DeleteThreadResult {
	return DeleteThreadResult{WorkersNotDeleted: a.workerFailures(o.failed), RetainedWorktrees: o.retained, WorktreeCleanupFailures: o.cleanup}
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
	for _, failure := range o.cleanup {
		log.Printf("app: %s: worktree %s was removed, but cleaning up after it failed: %s", what, failure.Path, failure.Error)
	}
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
	// members is the family as last claimed, in claim order, and index
	// each member's place in it.
	members []store.DeleteMember
	index   map[string]int
	// retired are the members that are a moved thread's cache or a holder.
	retired map[string]bool
	// sites are the worktrees the delete verified it removes, and kept the
	// worktrees it keeps that its result names.
	sites []deleteSite
	kept  []RetainedWorktree
	// changes counts the claims that found new threads.
	changes int
	// reopens are the admission closures, released when the delete ends.
	reopens []func()
	closed  map[string]bool
}

func (a *App) newFamilyDelete(req deleteRequest) *familyDelete {
	return &familyDelete{
		app:     a,
		req:     req,
		id:      newDeleteOperationID(),
		retired: make(map[string]bool),
		closed:  make(map[string]bool),
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

// claimFamily reads and claims the family (store.ClaimDeleteFamily).
func (op *familyDelete) claimFamily() ([]store.DeleteMember, error) {
	if hook := op.app.deleteFamilyReadForTest; hook != nil {
		hook()
	}
	members, err := op.app.store.ClaimDeleteFamily(op.id, op.req.seeds)
	if err != nil {
		return nil, op.claimRefusal(err)
	}
	return members, nil
}

// deleteDecisions are the running deletes' decisions, by delete operation:
// the claim on a thread's row names the delete that decides it. A delete
// registers its decision before it writes a claim (deleteThreads) and makes
// it as it commits (commit) or, failing that, as it ends, so on a running app a claim names a decision still to come or one
// already made; the boot releases every claim no delete committed before
// anything starts (releaseUndecidedDeletes). A start the app makes on its
// own, a dispatch or a notice's session start, that meets a claim waits for
// the decision holding no lock (waitDeleteDecision); a person's doors refuse
// at once. A delete never waits for a decision. Zero value ready.
type deleteDecisions struct {
	mu      sync.Mutex
	ops     map[string]*deleteDecision
	waiting int // starts waiting for a decision
}

// deleteDecision is one delete's decision: decided closes once it is made,
// and committed says which it was.
type deleteDecision struct {
	decided   chan struct{}
	committed bool
}

// open registers op's decision, still to come.
func (d *deleteDecisions) open(op string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ops == nil {
		d.ops = make(map[string]*deleteDecision)
	}
	if d.ops[op] == nil {
		d.ops[op] = &deleteDecision{decided: make(chan struct{})}
	}
}

// decide makes op's decision, and the starts waiting for it go on. A
// decision is made once; a later call does nothing.
func (d *deleteDecisions) decide(op string, committed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if decision, ok := d.ops[op]; ok {
		decision.committed = committed
		close(decision.decided)
		delete(d.ops, op)
	}
}

// waitDeleteDecision waits, holding no lock, for the decision of the delete
// that claims threadID, and reports whether the start is to try again from
// the top: false when the delete committed the thread, when ctx ended first,
// or when the claim stands with no delete under way to decide it (a boot or
// a delete whose release failed), which leaves the thread's queue to the
// next boot.
func (a *App) waitDeleteDecision(ctx context.Context, threadID string) bool {
	claim, committed, err := a.store.ThreadDeleteClaim(threadID)
	if err != nil || committed || claim == "" {
		// Decided, or unread: the start's own check reads it again.
		return err != nil || !committed
	}
	if hook := a.deleteDecisionReadForTest; hook != nil {
		hook(threadID)
	}
	d := &a.deleteDecisions
	d.mu.Lock()
	decision, open := d.ops[claim]
	if open {
		d.waiting++
	}
	d.mu.Unlock()
	if !open {
		// A decision is made after its claims: a claim standing now has no delete.
		if again, committed, err := a.store.ThreadDeleteClaim(threadID); err == nil && again == claim && !committed {
			log.Printf("app: thread %s is claimed by delete %s, which is not under way; what it queued waits for the next boot", threadID, claim)
			return false
		}
		return true
	}
	defer func() {
		d.mu.Lock()
		d.waiting--
		d.mu.Unlock()
	}()
	select {
	case <-decision.decided:
		if hook := a.deleteDecisionMadeForTest; hook != nil {
			hook(threadID)
		}
		// A committed thread starts nothing, and its row may be gone already.
		return !decision.committed
	case <-ctx.Done():
		return false
	}
}

// claim claims the family (phase 1).
func (op *familyDelete) claim() error {
	members, err := op.claimFamily()
	if err != nil {
		return err
	}
	op.setMembers(members)
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

// setMembers records members as the family, indexed by thread.
func (op *familyDelete) setMembers(members []store.DeleteMember) {
	op.members = members
	op.index = make(map[string]int, len(members))
	for i, member := range members {
		op.index[member.ThreadID] = i
	}
}

// reclaim claims the family again for this delete, which takes the
// threads that joined it since, and returns those it has not verified.
func (op *familyDelete) reclaim() ([]string, error) {
	members, err := op.claimFamily()
	if err != nil {
		return nil, err
	}
	var added []string
	for _, member := range members {
		if _, known := op.index[member.ThreadID]; !known && !member.Committed {
			added = append(added, member.ThreadID)
		}
	}
	op.setMembers(members)
	return added, nil
}

// verify runs every refusal over the claimed family (phase 2), then the
// worktrees' gates. It stops nothing. It goes a frontier at a time, from
// the threads the claim took: each thread of the frontier has its
// admission closed and drained, so a spawn it admitted has named its
// thread; the family is claimed again once, which takes such a thread; and
// each thread of the frontier runs its refusals under its own action lock.
// The threads that claim took are the next frontier.
func (op *familyDelete) verify() error {
	var frontier []string
	for _, member := range op.members {
		if !member.Committed {
			frontier = append(frontier, member.ThreadID)
		}
	}
	for len(frontier) > 0 {
		for _, id := range frontier {
			op.closeAdmission(id)
		}
		added, err := op.reclaim()
		if err != nil {
			return err
		}
		if len(added) > 0 {
			if op.changes++; op.changes > maxFamilyChanges {
				return fmt.Errorf("delete thread %s: the threads under it kept changing; try again", op.req.seeds[0])
			}
		}
		for _, id := range frontier {
			i, ok := op.index[id]
			if !ok {
				continue
			}
			if err := op.verifyMember(op.members[i]); err != nil {
				return err
			}
			if hook := op.app.deleteVerifiedForTest; hook != nil {
				hook(id)
			}
		}
		frontier = added
	}
	if op.req.removeWorktrees != nil {
		if err := op.verifyWorktrees(); err != nil {
			return err
		}
	}
	if op.req.verified != nil {
		return op.req.verified(op.members)
	}
	return nil
}

// verifyWorktrees decides which worktrees the delete removes: those its
// threads are on that no thread outside it is on (familyWorktrees). The
// caller's permission and each one's loss-of-work gate are asked for
// exactly those, and none is asked when there are none. A worktree that a
// thread outside the delete is on stays; when every such thread is another
// delete's, that delete's claim and this one's are compared, so that one of
// the two removes it (sharedWithADelete).
func (op *familyDelete) verifyWorktrees() error {
	sites, err := op.app.familyWorktrees(op.members, op.removes)
	if err != nil {
		return err
	}
	if hook := op.app.deleteOccupantsReadForTest; hook != nil {
		hook()
	}
	var emptied []deleteSite
	for _, site := range sites {
		if len(site.outside) == 0 {
			emptied = append(emptied, site)
			continue
		}
		kept, err := op.sharedWithADelete(site)
		if err != nil {
			return err
		}
		if kept != nil {
			op.kept = append(op.kept, *kept)
		}
	}
	if len(emptied) == 0 {
		return nil
	}
	if err := op.req.removeWorktrees(); err != nil {
		return err
	}
	for _, site := range emptied {
		err := op.app.checkWorktreeRemovable(site)
		var unsaved *errWorktreeUnsaved
		switch {
		case errors.As(err, &unsaved):
			return errorsx.Public("delete_worktree_unsaved", fmt.Sprintf(
				"Nothing was deleted: the worktree %s, which this delete would remove, has unsaved work. Commit or discard it, then delete again.", site.path), err)
		case err != nil:
			return fmt.Errorf("the worktree %s, which this delete would remove, cannot be removed, so nothing was deleted: %w", site.path, err)
		}
	}
	op.sites = emptied
	return nil
}

// sharedWithADelete settles a worktree of the delete that threads outside
// it are on. One of them that no delete takes keeps it, and nothing is
// said. When every one is another delete's, each would leave the worktree
// to the other: the delete whose claim is older (newDeleteOperationID)
// keeps it and names it, and the younger is refused, so its retry, once the
// older ends, finds the worktree its own. A thread a delete committed
// without a claim (store.BeginThreadDelete) counts as older. How far a
// delete has gone with each outside thread comes from the read that found
// it on the worktree (familyWorktrees), so a thread whose row a delete drops
// afterwards is weighed as that read found it.
func (op *familyDelete) sharedWithADelete(site deleteSite) (*RetainedWorktree, error) {
	for _, occupant := range site.outside {
		if occupant.Delete.Claim == "" && !occupant.Delete.Committed {
			return nil, nil
		}
	}
	for _, occupant := range site.outside {
		if occupant.Delete.Claim < op.id {
			return nil, errorsx.Public("delete_worktree_being_deleted", fmt.Sprintf(
				"Nothing was deleted: thread %s, which is also on the worktree %s, is being deleted. Delete again when that ends.",
				op.app.threadName(occupant.ID), site.path), store.ErrThreadDeleting)
		}
	}
	return &RetainedWorktree{Path: site.path, Reason: fmt.Sprintf(
		"thread %s, which is also on it, is being deleted by another delete", op.app.threadName(site.outside[0].ID))}, nil
}

// threadName names a thread to a person: its title, quoted, or its id when
// it has none or cannot be read.
func (a *App) threadName(threadID string) string {
	if row, err := a.store.GetThread(threadID); err == nil && strings.TrimSpace(row.Title) != "" {
		return strconv.Quote(row.Title)
	}
	return threadID
}

// removes reports whether the delete removes member's worktree: one of the
// threads it claimed, as against a committed one it joined, that is a
// conversation of this computer.
func (op *familyDelete) removes(member store.DeleteMember) bool {
	return removesWorktree(member) && !op.retired[member.ThreadID]
}

// removesWorktree reports whether a delete's own claim takes member's
// worktree: a thread it would claim, as against a committed one it would
// join, that is no holder.
func removesWorktree(member store.DeleteMember) bool {
	return !member.Committed && !member.Holder
}

// verifyMember runs member's refusals under its action lock.
func (op *familyDelete) verifyMember(member store.DeleteMember) error {
	unlock := op.app.threadLocks().Lock(member.ThreadID)
	defer unlock()
	return op.check(member)
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
// returns cause: a refused delete changes nothing, and the starts waiting
// for its decision go on as it ends (deleteThreads). A release that fails
// leaves the claims fenced until the next boot releases them, and the starts
// that meet them end, the queue left to that boot; it is said beside cause.
func (op *familyDelete) refuse(cause error) error {
	if err := op.app.store.ReleaseDeleteClaim(op.id); err != nil {
		return errors.Join(cause, fmt.Errorf("and the delete's claim could not be taken back until the app restarts: %w", err))
	}
	return cause
}

// commit commits the claimed family (phase 3) and tells clients its rows
// are gone. A thread on a worktree the delete verified it removes is
// recorded to remove it, unless the commit finds a thread outside the
// delete on it now, and the worktree is then named as kept; no other thread
// removes a worktree.
func (op *familyDelete) commit() error {
	var worktrees store.DeleteWorktrees
	if len(op.sites) > 0 {
		worktrees = store.DeleteWorktrees{
			Site: siteOf(op.sites),
			Inside: func(threadID string) bool {
				_, in := op.index[threadID]
				return in
			},
		}
	}
	committed, err := op.app.store.CommitDeleteClaim(op.id, worktrees)
	if err != nil {
		return err
	}
	// The starts waiting for the decision find the threads committed and
	// start nothing; their queues are their teardowns'.
	op.app.deleteDecisions.decide(op.id, true)
	for _, site := range op.sites {
		if occupant := committed.Occupied[site.path]; occupant != "" {
			op.kept = append(op.kept, RetainedWorktree{Path: site.path, Reason: op.app.keptFor(occupant)})
		}
	}
	for _, id := range committed.Threads {
		i, ok := op.index[id]
		if !ok {
			continue
		}
		member := op.members[i]
		op.members[i].Committed, op.members[i].RemoveWorktrees = true, committed.RemoveWorktrees[id]
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
		hook(committed.Threads)
	}
	return nil
}

// siteOf maps a stored workspace or worktree path to the one of sites it
// names, "" for none, resolving each path once.
func siteOf(sites []deleteSite) func(path string) string {
	byCanonical := make(map[string]string, len(sites))
	for _, site := range sites {
		byCanonical[gitops.CanonicalPath(site.path)] = site.path
	}
	resolved := make(map[string]string)
	return func(path string) string {
		if strings.TrimSpace(path) == "" {
			return ""
		}
		site, seen := resolved[path]
		if !seen {
			site = byCanonical[gitops.CanonicalPath(path)]
			resolved[path] = site
		}
		return site
	}
}

// keptFor is why a worktree the delete would remove is kept: threadID, a
// thread outside the delete, is on it.
func (a *App) keptFor(threadID string) string {
	return fmt.Sprintf("thread %s, which this delete does not take, is on it", a.threadName(threadID))
}

// destroy destroys every committed thread of the family (phase 4), each
// once every thread under it is gone. A failure stops that thread and the
// threads above it, which stay committed. A named thread's own failure is
// the delete's error; any other thread's is named in the outcome.
func (op *familyDelete) destroy() (deleteOutcome, error) {
	outcome := deleteOutcome{retained: slices.Clone(op.kept)}
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
		op.removeEmptiedWorktree(member, outcome)
	}
	unlock = op.app.threadLocks().Lock(member.ThreadID)
	defer unlock()
	return threads.Drop(member.ThreadID, ports)
}

// errWorktreeWaiting leaves a worktree to the destroy of another thread of
// the delete still on it that removes it too.
var errWorktreeWaiting = errors.New("another thread of the delete removes the worktree")

// errWorktreeKept keeps a worktree another thread is on.
type errWorktreeKept struct{ reason string }

func (e *errWorktreeKept) Error() string { return e.reason }

// removeEmptiedWorktree removes the worktree member, a thread whose commit
// recorded that it removes it, is on, once no other thread is. Another
// thread of the delete still on it that removes it too removes it then;
// any other thread on it keeps it, and it is returned to name in the
// result. The removal is Remove Worktree's (removeProjectWorktree), whose
// loss-of-work gate runs again and which reads who is on the worktree
// again with every one of them locked: a worktree that cannot be removed
// now is left on disk and named the same way; its threads are still
// deleted. A removal whose cleanup after git removed the worktree failed
// is named in outcome as such.
func (op *familyDelete) removeEmptiedWorktree(member store.DeleteMember, outcome *deleteOutcome) {
	path := member.WorktreePath
	if path == "" || member.ProjectID == "" {
		return
	}
	others := func(occupants []string) error {
		waiting := false
		for _, id := range occupants {
			if id == member.ThreadID {
				continue
			}
			if i, in := op.index[id]; in {
				other := op.members[i]
				if other.Committed && other.RemoveWorktrees && gitops.SameFilesystemPath(other.WorktreePath, path) {
					waiting = true
					continue
				}
			}
			return &errWorktreeKept{reason: op.app.keptFor(id)}
		}
		if waiting {
			return errWorktreeWaiting
		}
		return nil
	}
	occupants, err := op.app.threadsReferencingWorkspace(path)
	if err == nil {
		err = others(occupants)
	}
	var project store.Project
	if err == nil {
		project, err = op.app.store.GetProject(member.ProjectID)
	}
	if err == nil {
		var found bool
		if _, found, err = op.app.findWorktree(project.Path, path); err == nil && !found {
			// Removed already: by this delete before a restart, or by hand.
			return
		}
	}
	if err == nil {
		_, err = op.app.removeProjectWorktree(member.ProjectID, project.Path, path, worktreeRemovalPolicy{idle: others})
	}
	var kept *errWorktreeKept
	var followUp *errWorktreeRemovedFollowUp
	switch {
	case err == nil, errors.Is(err, errWorktreeWaiting):
		return
	case errors.As(err, &kept):
		outcome.retained = append(outcome.retained, RetainedWorktree{Path: path, Reason: kept.reason})
		return
	case errors.As(err, &followUp):
		log.Printf("app: delete: worktree %s: %v", path, err)
		outcome.cleanup = append(outcome.cleanup, WorktreeCleanupFailure{Path: path, Error: followUp.err.Error()})
		return
	}
	log.Printf("app: delete thread %s: worktree %s left on disk: %v", member.ThreadID, path, err)
	outcome.retained = append(outcome.retained, RetainedWorktree{Path: path, Reason: err.Error()})
}

// deleteSite is one worktree a delete empties.
type deleteSite struct {
	projectID, path string
	// outside are the threads outside the delete on it, each with how far a
	// delete had gone with it, from one read.
	outside []store.ThreadWorkspaceRef
}

// familyWorktrees lists, each once, the worktrees the family members take
// accepts are on, with the threads outside the family on each and their
// delete state, all in one read of the thread rows. A delete
// removes the ones with none (familyDelete.verifyWorktrees), and its
// confirmation counts the same ones (GetDeleteDependents).
func (a *App) familyWorktrees(family []store.DeleteMember, take func(store.DeleteMember) bool) ([]deleteSite, error) {
	in := make(map[string]bool, len(family))
	for _, member := range family {
		in[member.ThreadID] = true
	}
	var sites []deleteSite
	seen := make(map[string]bool)
	for _, member := range family {
		if !take(member) || member.WorktreePath == "" || member.ProjectID == "" {
			continue
		}
		key := member.ProjectID + "\x00" + gitops.CanonicalPath(member.WorktreePath)
		if seen[key] {
			continue
		}
		seen[key] = true
		sites = append(sites, deleteSite{projectID: member.ProjectID, path: member.WorktreePath})
	}
	if len(sites) == 0 {
		return nil, nil
	}
	paths := make([]string, len(sites))
	for i, site := range sites {
		paths[i] = site.path
	}
	occupants, err := a.worktreeApplication().ThreadsReferencingWorkspaces(paths)
	if err != nil {
		return nil, err
	}
	for i := range sites {
		for _, occupant := range occupants[i] {
			if !in[occupant.ID] {
				sites[i].outside = append(sites[i].outside, occupant)
			}
		}
	}
	return sites, nil
}

// newDeleteOperationID mints a delete's id, the claim it writes
// (threads.delete_claim). Ids sort in the order their deletes began, so two
// deletes compare their claims the same way (sharedWithADelete).
func newDeleteOperationID() string {
	return fmt.Sprintf("%020d-%s", time.Now().UnixNano(), entityid.New())
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
