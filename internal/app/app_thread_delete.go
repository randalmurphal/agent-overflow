package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
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
	// still belongs to its spawner. Not counted, because only that computer
	// knows which do.
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
		}
	}
	if dependents.RemoteWorkers, err = a.store.HasRemoteSpawns(ids); err != nil {
		return DeleteDependents{}, err
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
}

func (o deleteOutcome) result(a *App) DeleteThreadResult {
	return DeleteThreadResult{WorkersNotDeleted: a.workerFailures(slices.Concat(o.failed, o.remote))}
}

// logFailures is the report of a delete no person is waiting on, named by
// what.
func (o deleteOutcome) logFailures(what string) {
	for _, worker := range slices.Concat(o.failed, o.remote) {
		log.Printf("app: %s: thread %s was not deleted: %v", what, worker.threadID, worker.err)
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
	outcome, err := op.destroy()
	outcome.remote = a.deliverOwnerOps(op.owed, op.owedTargets, req.reach)
	return outcome, err
}

// maxFamilyChanges bounds how often a delete's family may grow while it is
// verified. It grows only when a spawn its threads admitted before the
// claim names its thread, so one change is normal.
const maxFamilyChanges = 8

// familyDelete is one delete's state.
type familyDelete struct {
	app *App
	req deleteRequest
	// id is the operation's, the claim it writes (threads.delete_claim).
	id string
	// members is the family as last claimed, in claim order, and index
	// each member's place in it.
	members []store.DeleteMember
	index   map[string]int
	// retired are the members that are a moved thread's cache or a holder.
	retired map[string]bool
	// changes counts the claims that found new threads.
	changes int
	// reopens are the admission closures, released when the delete ends.
	reopens []func()
	closed  map[string]bool
	// owed are the owner operations the commit owed, and owedTargets the
	// spawns they are for, read before the destroy drops them.
	owed        []store.OwnerOp
	owedTargets map[string]store.ThreadRequest
}

func (a *App) newFamilyDelete(req deleteRequest) *familyDelete {
	return &familyDelete{
		app:     a,
		req:     req,
		id:      entityid.New(),
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
	var members []store.DeleteMember
	var err error
	switch {
	case op.req.link == nil:
		members, err = op.app.store.ClaimDeleteFamily(op.id, op.req.seeds)
	case len(op.req.seeds) == 1:
		members, err = op.app.store.ClaimLinkedDelete(op.id, op.req.seeds[0], *op.req.link)
	default:
		err = errors.New("a delete through a link names one thread")
	}
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

// verify runs every refusal over the claimed family (phase 2). It stops
// nothing. It goes a frontier at a time, from
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
	if op.req.verified != nil {
		return op.req.verified(op.members)
	}
	return nil
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
// are gone.
func (op *familyDelete) commit() error {
	committed, err := op.app.store.CommitDeleteClaim(op.id)
	if err != nil {
		return err
	}
	// The starts waiting for the decision find the threads committed and
	// start nothing; their queues are their teardowns'.
	op.app.deleteDecisions.decide(op.id, true)
	op.owed = committed.OwedOps
	if op.req.reach == reachNow {
		// Its report names the workers by the spawns the destroy drops.
		op.owedTargets = op.app.ownerOpTargets(committed.OwedOps)
	}
	for _, id := range committed.Threads {
		i, ok := op.index[id]
		if !ok {
			continue
		}
		member := op.members[i]
		op.members[i].Committed = true
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
		err := op.destroyMember(member)
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

// destroyMember destroys member under its action lock.
func (op *familyDelete) destroyMember(member store.DeleteMember) error {
	ports := op.app.threadDeletePorts()
	ports.ChunkPause = op.req.pause
	if hook := op.app.deletePortsForTest; hook != nil {
		hook(&ports)
	}
	unlock := op.app.threadLocks().Lock(member.ThreadID)
	defer unlock()
	return op.app.threadApplication().Destroy(member.ThreadID, ports)
}
