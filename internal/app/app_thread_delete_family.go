package app

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
)

// The phases of one family delete (familyDelete): claim, verify, commit and
// destroy, as the head of app_thread_delete.go describes them.

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
	// holds hold what the commit owes for the door's attempt
	// (deleteThreads makes and releases them).
	holds *ownerOpHolds
	// unstopped is what the destroy could not stop of the work its threads
	// started elsewhere.
	unstopped deleteUnstopped
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
	committed, err := op.app.store.CommitDeleteClaim(op.id, op.holds.owing())
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
		// No reader lists it now, and its destroy removes its turn rows,
		// with any whose end the store did not hold.
		op.app.triage.ForgetUnrecordedTurnEnds(member.ThreadID)
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
	ports := op.app.threadDeletePorts(&op.unstopped)
	ports.ChunkPause = op.req.pause
	if hook := op.app.deletePortsForTest; hook != nil {
		hook(&ports)
	}
	unlock := op.app.threadLocks().Lock(member.ThreadID)
	defer unlock()
	return op.app.threadApplication().Destroy(member.ThreadID, ports)
}
