package app

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Owner operations: what archiving or deleting a thread owes the workers it
// spawned on paired computers (docs/specs/standing-agents.md, Owned
// workers). The write that decides one owes it in its own transaction
// (store.CommitDeleteClaim, store.OweRemoteArchives, the unarchive's
// supersede), one row per spawn, under the spawn's next sequence
// (store/thread_owner_ops.go). The door that decided it makes one bounded
// attempt for a person waiting on it, and the request sweep delivers the
// rest until the other computer answers (deliverOwedOwnerOps). A refusal or
// an unreachable computer never undoes what was decided here.
//
// The other computer admits each through its own archive and delete, fenced
// by the spawn's link: the archive by store.ArchiveLinkedWorker, which takes
// each sequence once, the delete by its claim (store.ClaimLinkedDelete).

// threadOwnerOpTool is the forwarded write tool that carries one owner
// operation. A computer that predates it answers that it is not a thread
// tools write call (threadAgentPeerOlder).
const threadOwnerOpTool = "agent_owner_op"

// What the other computer answers an owner operation. applied, gone and
// not_owned settle it; pending, unfinished and refused leave it owed.
const (
	// ownerOpApplied: admitted, or admitted before under this sequence.
	ownerOpApplied = "applied"
	// ownerOpPending: the spawn's thread is still being made.
	ownerOpPending = "pending"
	// ownerOpGone: no such spawn of the caller's there, or its thread is
	// deleted or moved away.
	ownerOpGone = "gone"
	// ownerOpNotOwned: the spawn's link no longer owns the thread there; a
	// delete then ends the link.
	ownerOpNotOwned = "not_owned"
	// ownerOpRefused: that computer refuses it for now, Reason says why.
	ownerOpRefused = "refused"
	// ownerOpUnfinished: admitted, and a worker below the thread there was
	// not archived or deleted; Failures names each. The same operation sent
	// again walks again (store.ArchiveLinkedWorker, the delete's committed
	// claim), so the spawner's ledger is the one retry.
	ownerOpUnfinished = "unfinished"
)

// threadOwnerOpArgs are agent_owner_op's arguments.
type threadOwnerOpArgs struct {
	// Spawn is the spawn's token, which the other computer's receipt keeps.
	Spawn string `json:"spawn"`
	// Seq is the operation's sequence on the spawn; a computer takes each
	// once and an older one after a newer changes nothing.
	Seq int64 `json:"seq"`
	// Op is store.OwnerOpArchive, Delete, Release or Supersede.
	Op string `json:"op"`
}

// threadOwnerOpAnswer is agent_owner_op's answer.
type threadOwnerOpAnswer struct {
	Outcome string `json:"outcome"`
	// Reason says why a refused operation was refused.
	Reason string `json:"reason,omitempty"`
	// Failures name, for an unfinished operation, the workers under the
	// thread there that its archive or delete could not reach.
	Failures []WorkerFailure `json:"failures,omitempty"`
}

// ownerOpReach is when a door delivers the owner operations it owed.
type ownerOpReach int

const (
	// reachLater leaves them to the request sweep: a door no person waits
	// on, or one serving a paired computer's own operation.
	reachLater ownerOpReach = iota
	// reachNow makes one attempt at each before the door returns, bounded
	// by threadAgentPeerListTimeout, and reports the ones that did not land.
	reachNow
)

// ownerOpDelivery is the source half's in-process state. Zero value ready.
type ownerOpDelivery struct {
	mu sync.Mutex
	// sending holds the spawns an attempt is under way for: one attempt per
	// spawn at a time, the door's or the sweep's.
	sending map[string]bool
	// draining is set while a sweep pass delivers.
	draining bool
}

// --- The worker's computer: admitting an operation ---

// serveThreadOwnerOp answers agent_owner_op for the device owner.
func (a *App) serveThreadOwnerOp(ctx context.Context, owner string, call ThreadPeerCall) (ThreadPeerReply, error) {
	source := strings.TrimSpace(call.Source.ThreadID)
	var args threadOwnerOpArgs
	err := json.Unmarshal(call.Args, &args)
	if err != nil || !entityid.Valid(source) || !entityid.Valid(args.Spawn) || args.Seq < 1 ||
		!slices.Contains([]string{store.OwnerOpArchive, store.OwnerOpDelete, store.OwnerOpRelease, store.OwnerOpSupersede}, args.Op) {
		return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"An owner operation must name the thread it came from, its spawn, its sequence and one of archive, delete, release or supersede.", err)
	}
	answer, err := a.admitOwnerOp(ctx, owner, source, args)
	if err != nil {
		return ThreadPeerReply{}, a.publicThreadToolError(call.Tool, source, err)
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return ThreadPeerReply{}, fmt.Errorf("thread tools: encode %s answer: %w", call.Tool, err)
	}
	return ThreadPeerReply{Result: encoded}, nil
}

// admitOwnerOp carries out one owner operation the device owner sent from
// its thread source, for the spawn args names. Only that device's spawn,
// from that thread, is reached (store.ReachSpawnLink, the lineage the
// retention floor kept included); any other is gone, which tells the caller
// nothing it did not know.
func (a *App) admitOwnerOp(_ context.Context, owner, source string, args threadOwnerOpArgs) (threadOwnerOpAnswer, error) {
	now := time.Now().UnixMilli()
	reach, found, err := a.store.ReachSpawnLink(args.Spawn, owner, source, now)
	if err != nil {
		return threadOwnerOpAnswer{}, err
	}
	if !found {
		return threadOwnerOpAnswer{Outcome: ownerOpGone}, nil
	}
	worker := reach.TargetThreadID
	if worker == "" {
		if reach.Open() {
			// Its creation runs: it names the thread, or loses to a
			// settlement and discards it (store.SetThreadReceiptTarget).
			return threadOwnerOpAnswer{Outcome: ownerOpPending}, nil
		}
		// Settled with no thread, which nothing can name now: interrupted
		// before it named one, or its thread was deleted or moved away.
		return threadOwnerOpAnswer{Outcome: ownerOpGone}, nil
	}
	held, err := a.ownerOpReachesThread(worker, args.Op)
	if err != nil {
		return threadOwnerOpAnswer{}, err
	}
	if !held {
		return threadOwnerOpAnswer{Outcome: ownerOpGone}, nil
	}
	switch args.Op {
	case store.OwnerOpArchive:
		return a.admitLinkedArchive(worker, owner, args), nil
	case store.OwnerOpDelete:
		return a.admitLinkedDelete(worker, owner, source, args)
	case store.OwnerOpRelease:
		released, err := a.store.ReleaseSpawnLink(args.Spawn, owner, source)
		if err != nil {
			return threadOwnerOpAnswer{}, err
		}
		a.announceReleasedWorkers(released)
		return threadOwnerOpAnswer{Outcome: ownerOpApplied}, nil
	default:
		linked, err := a.store.SupersedeLink(args.Spawn, owner, source, args.Seq)
		if err != nil {
			return threadOwnerOpAnswer{}, err
		}
		if !linked {
			return threadOwnerOpAnswer{Outcome: ownerOpNotOwned}, nil
		}
		return threadOwnerOpAnswer{Outcome: ownerOpApplied}, nil
	}
}

// ownerOpReachesThread reports whether an owner operation op can still act
// on threadID here: a thread this computer holds, or for a delete one whose
// delete committed, which it joins. A thread deleted or moved away is gone.
func (a *App) ownerOpReachesThread(threadID, op string) (bool, error) {
	_, err := a.store.GetOwnedThread(threadID)
	var transfer *store.ThreadTransferError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &transfer) && transfer.Moved:
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	if op != store.OwnerOpDelete {
		return false, nil
	}
	_, committed, err := a.store.ThreadDeleteClaim(threadID)
	return committed, err
}

// admitLinkedArchive archives worker for the link's owner through the
// archive's own path (runArchive), fenced by the link and taking args.Seq
// once, then finishes it here: its session stops, its workers here are
// archived, and what it owes further computers is left to the sweep. A
// worker below it that could not be archived makes the answer unfinished,
// naming it; the owner sends the same operation again, which walks again
// while the worker is still archived under it.
func (a *App) admitLinkedArchive(worker, owner string, args threadOwnerOpArgs) threadOwnerOpAnswer {
	work, err := a.runArchive(worker, archiveRequest{link: &linkedArchive{token: args.Spawn, device: owner, seq: args.Seq}})
	switch {
	case errors.Is(err, errOwnerOpTaken):
		return threadOwnerOpAnswer{Outcome: ownerOpApplied}
	case errors.Is(err, store.ErrWorkerNotOwned):
		return threadOwnerOpAnswer{Outcome: ownerOpNotOwned}
	case threadGone(err):
		return threadOwnerOpAnswer{Outcome: ownerOpGone}
	case err != nil:
		return ownerOpRefusal("archive", worker, err)
	}
	outcome := a.finishArchive(work, reachLater)
	if cause := outcome.cleanup.Unwrap(); cause != nil {
		log.Printf("thread tools: archive %s for its spawner on another computer: %v", worker, cause)
	}
	if len(outcome.workers) > 0 {
		return threadOwnerOpAnswer{Outcome: ownerOpUnfinished, Failures: a.workerFailures(outcome.workers)}
	}
	return threadOwnerOpAnswer{Outcome: ownerOpApplied}
}

// admitLinkedDelete deletes worker for the link's owner through the
// delete's own claim, verify, commit and destroy (deleteThreads), the claim
// fenced by the link. A worker the link no longer owns is kept, and its
// link ended: its spawner is gone, so nothing may make it that spawner's
// worker again. A refusal leaves the delete to be sent again. A worker
// below it that could not be destroyed keeps the threads above it
// committed, the worker included, and makes the answer unfinished, naming
// it: the owner sends the delete again, which joins the committed delete.
func (a *App) admitLinkedDelete(worker, owner, source string, args threadOwnerOpArgs) (threadOwnerOpAnswer, error) {
	outcome, err := a.deleteThreads(deleteRequest{seeds: []string{worker}, link: &store.LinkFence{Token: args.Spawn, DeviceID: owner}})
	switch {
	case errors.Is(err, store.ErrWorkerNotOwned):
		released, err := a.store.ReleaseSpawnLink(args.Spawn, owner, source)
		if err != nil {
			return threadOwnerOpAnswer{}, err
		}
		a.announceReleasedWorkers(released)
		return threadOwnerOpAnswer{Outcome: ownerOpNotOwned}, nil
	case err != nil:
		return ownerOpRefusal("delete", worker, err), nil
	case len(outcome.failed) > 0:
		return threadOwnerOpAnswer{Outcome: ownerOpUnfinished, Failures: a.workerFailures(outcome.failed)}, nil
	case !slices.Contains(outcome.deleted, worker):
		return threadOwnerOpAnswer{Outcome: ownerOpGone}, nil
	}
	return threadOwnerOpAnswer{Outcome: ownerOpApplied}, nil
}

// ownerOpRefusal is a refused owner operation, its reason in words fit for
// the other computer: the cause stays in this computer's log.
func ownerOpRefusal(verb, threadID string, err error) threadOwnerOpAnswer {
	log.Printf("thread tools: %s %s for its spawner on another computer: %v", verb, threadID, err)
	var transfer *store.ThreadTransferError
	var claimed *store.DeleteClaimedError
	reason := "it could not be carried out there; that computer's log says why"
	switch {
	case errors.As(err, &transfer):
		reason = "it is being transferred to another computer"
	case errors.As(err, &claimed), errors.Is(err, store.ErrThreadDeleting):
		reason = "it is being deleted there"
	}
	return threadOwnerOpAnswer{Outcome: ownerOpRefused, Reason: reason}
}

// announceReleasedWorkers announces the threads whose spawn links ended:
// each is no paired computer's worker any more, and has its sidebar row
// here unless something else owns it, and the prompts its link held for
// the spawner go where its prompts route now (handBackLinkPrompts). Every
// write that ends links hands what it released here: a session end
// (identity's OnLinksReleased), the
// retention sweep's staleness release, and an owner's release or a delete
// that found its worker given a sidebar row.
func (a *App) announceReleasedWorkers(threadIDs []string) {
	for _, threadID := range threadIDs {
		a.announcePlaced(threadID)
	}
	a.handBackLinkPrompts(threadIDs)
}

// --- The spawner's computer: delivering what is owed ---

// ownerOpVerb is how a report names what op does to a worker.
func ownerOpVerb(op string) string {
	if op == store.OwnerOpArchive {
		return "archived"
	}
	return "deleted"
}

// ownerOpAttempt is how one attempt at an owed operation ended.
type ownerOpAttempt struct {
	// settled: answered for good, or replaced by a newer operation.
	settled bool
	// older: the computer predates owner operations; dropped.
	older bool
	// unpaired: the pairing with the computer ended; dropped.
	unpaired bool
	// reason says why a pending operation did not land, in a person's words.
	reason string
	// members are the workers below its worker that an unfinished operation
	// did not reach, as that computer named them.
	members []WorkerFailure
}

// deliverOwnerOps delivers what a door owed, as reach says, and reports
// what did not land: a worker whose computer did not take it yet, pending,
// or one that predates owner operations. targets are the owing spawns,
// read while they still existed (ownerOpTargets); nil reads them now.
func (a *App) deliverOwnerOps(ops []store.OwnerOp, targets map[string]store.ThreadRequest, reach ownerOpReach) []workerFailure {
	if len(ops) == 0 {
		return nil
	}
	// The sweep sleeps on the ledger's schedule, which these rows changed.
	defer a.nudgeThreadRequestSweep()
	if reach != reachNow {
		return nil
	}
	if targets == nil {
		targets = a.ownerOpTargets(ops)
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), threadAgentPeerListTimeout)
	defer cancel()
	attempts := make([]ownerOpAttempt, len(ops))
	var wg sync.WaitGroup
	limit := make(chan struct{}, ownerOpInlineFanOut)
	for i, op := range ops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
				attempts[i] = a.deliverOwnerOp(ctx, op)
			case <-ctx.Done():
				attempts[i] = ownerOpAttempt{}
			}
		}()
	}
	wg.Wait()
	names := a.remoteComputerNames()
	var failures []workerFailure
	for i, op := range ops {
		attempt := attempts[i]
		if attempt.settled {
			continue
		}
		computer := names[op.ComputerID]
		if computer == "" {
			computer = "The other computer"
		}
		for _, member := range attempt.members {
			failures = append(failures, workerFailure{threadID: member.ThreadID, title: member.Title, computer: computer, computerID: op.ComputerID, pending: true,
				err: fmt.Errorf("%s: %s; it is %s there once that computer takes it", computer, member.Error, ownerOpVerb(op.Op))})
		}
		if len(attempt.members) > 0 {
			continue
		}
		target := targets[op.Token]
		failure := workerFailure{threadID: target.TargetThreadID, title: target.TargetThreadTitle, computer: computer, computerID: op.ComputerID,
			pending: !attempt.older && !attempt.unpaired}
		switch {
		case attempt.older:
			failure.err = fmt.Errorf("%s runs an older version, so the worker there was not %s", computer, ownerOpVerb(op.Op))
		case attempt.unpaired:
			failure.err = fmt.Errorf("%s is no longer connected, so the worker there was not %s", computer, ownerOpVerb(op.Op))
		case attempt.reason != "":
			failure.err = fmt.Errorf("%s: %s; it is %s there once that computer takes it", computer, attempt.reason, ownerOpVerb(op.Op))
		default:
			failure.err = fmt.Errorf("%s did not answer in time; it is %s there once that computer answers", computer, ownerOpVerb(op.Op))
		}
		failures = append(failures, failure)
	}
	return failures
}

// ownerOpInlineFanOut bounds a door's attempts in flight at once.
const ownerOpInlineFanOut = 8

// ownerOpTargets reads the spawns ops are owed for, for a report to name
// their workers: a delete reads them before its destroy drops them with
// their caller.
func (a *App) ownerOpTargets(ops []store.OwnerOp) map[string]store.ThreadRequest {
	targets := make(map[string]store.ThreadRequest, len(ops))
	for _, op := range ops {
		row, found, err := a.store.GetThreadRequest(op.Token)
		if err != nil {
			log.Printf("thread tools: read spawn %s for its owner operation: %v", op.Token, err)
		}
		if found {
			targets[op.Token] = row
		}
	}
	return targets
}

// deliverOwnerOp makes one attempt at op: marked sent before its call, and
// settled when its computer answered it for good, predates owner operations
// or is no longer paired (threadPairingEnded), rescheduled on the retry
// ladder otherwise. An attempt that
// finds another under way for the spawn, or op replaced by a newer
// operation, does nothing.
func (a *App) deliverOwnerOp(ctx context.Context, op store.OwnerOp) ownerOpAttempt {
	if !a.beginOwnerOpSend(op.Token) {
		return ownerOpAttempt{}
	}
	defer a.endOwnerOpSend(op.Token)
	marked, err := a.store.MarkOwnerOpSent(op.Token, op.Seq)
	if err != nil {
		log.Printf("thread tools: owner operation %s for spawn %s: %v", op.Op, op.Token, err)
		return ownerOpAttempt{reason: "it could not be recorded here"}
	}
	if !marked {
		return ownerOpAttempt{settled: true}
	}
	answer, err := a.callOwnerOp(ctx, op)
	var code string
	if err != nil {
		code, _, _ = threadErrorDetails("owner operation", err)
	}
	switch {
	case errors.Is(err, errThreadAgentPeerOlder):
		a.settleOwnerOp(op)
		return ownerOpAttempt{older: true}
	case err != nil && threadPairingEnded(code):
		// Nothing can deliver it until the person pairs the computer again.
		a.settleOwnerOp(op)
		return ownerOpAttempt{unpaired: true}
	case err != nil:
		a.rescheduleOwnerOp(op)
		return ownerOpAttempt{}
	}
	switch answer.Outcome {
	case ownerOpApplied, ownerOpGone, ownerOpNotOwned:
		a.settleOwnerOp(op)
		return ownerOpAttempt{settled: true}
	case ownerOpUnfinished:
		names := make([]string, 0, len(answer.Failures))
		for _, failure := range answer.Failures {
			names = append(names, cmp.Or(failure.Title, failure.ThreadID))
		}
		reason := fmt.Sprintf("the threads under it there were not all %s yet (%s)", ownerOpVerb(op.Op), strings.Join(names, ", "))
		a.rescheduleOwnerOp(op)
		return ownerOpAttempt{reason: reason, members: answer.Failures}
	case ownerOpPending:
		a.rescheduleOwnerOp(op)
		return ownerOpAttempt{reason: "its thread there is still being made"}
	case ownerOpRefused:
		reason := "refused for now: " + answer.Reason
		a.rescheduleOwnerOp(op)
		return ownerOpAttempt{reason: reason}
	default:
		reason := fmt.Sprintf("answered %q", answer.Outcome)
		a.rescheduleOwnerOp(op)
		return ownerOpAttempt{reason: reason}
	}
}

// callOwnerOp sends op to its computer.
func (a *App) callOwnerOp(ctx context.Context, op store.OwnerOp) (threadOwnerOpAnswer, error) {
	if a.backends == nil {
		return threadOwnerOpAnswer{}, errors.New("no paired computers are reachable")
	}
	encoded, err := json.Marshal(threadOwnerOpArgs{Spawn: op.Token, Seq: op.Seq, Op: op.Op})
	if err != nil {
		return threadOwnerOpAnswer{}, fmt.Errorf("encode %s: %w", threadOwnerOpTool, err)
	}
	var reply ThreadPeerReply
	if err := a.backends.CallThreadPeer(ctx, op.ComputerID, "ThreadToolCall", &reply, ThreadPeerCall{
		Tool: threadOwnerOpTool, Args: encoded, Source: a.threadAgentCaller(op.SourceThreadID),
	}); err != nil {
		if threadAgentPeerOlder(err) {
			return threadOwnerOpAnswer{}, errThreadAgentPeerOlder
		}
		return threadOwnerOpAnswer{}, err
	}
	var answer threadOwnerOpAnswer
	if err := json.Unmarshal(reply.Result, &answer); err != nil {
		return threadOwnerOpAnswer{}, fmt.Errorf("read %s from %s: %w", threadOwnerOpTool, op.ComputerID, err)
	}
	return answer, nil
}

func (a *App) settleOwnerOp(op store.OwnerOp) {
	if _, err := a.store.SettleOwnerOp(op.Token, op.Seq); err != nil {
		log.Printf("thread tools: settle owner operation %s for spawn %s: %v", op.Op, op.Token, err)
	}
}

// rescheduleOwnerOp sets op's next attempt on the retry ladder: 5 s after
// the first, doubling to threadPollLateCap.
func (a *App) rescheduleOwnerOp(op store.OwnerOp) {
	delay := threadPollNormalDelay
	for range op.Attempts {
		if delay *= 2; delay >= threadPollLateCap {
			delay = threadPollLateCap
			break
		}
	}
	if _, err := a.store.RescheduleOwnerOp(op.Token, op.Seq, time.Now().Add(delay).UnixMilli()); err != nil {
		log.Printf("thread tools: reschedule owner operation %s for spawn %s: %v", op.Op, op.Token, err)
	}
}

func (a *App) beginOwnerOpSend(token string) bool {
	d := &a.ownerOps
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sending[token] {
		return false
	}
	if d.sending == nil {
		d.sending = make(map[string]bool)
	}
	d.sending[token] = true
	return true
}

func (a *App) endOwnerOpSend(token string) {
	d := &a.ownerOps
	d.mu.Lock()
	delete(d.sending, token)
	d.mu.Unlock()
}

// deliverOwedOwnerOps is the request sweep's delivery of what is owed by
// now: one pass at a time, off the sweep's goroutine because a computer
// that does not answer holds its call for the call's timeout, the computers
// threadRequestPollFanOut at a time and each one's operations in turn.
func (a *App) deliverOwedOwnerOps(now time.Time) {
	if a.backends == nil || !a.beginOwnerOpDrain() {
		return
	}
	ops, err := a.store.DueOwnerOps(now.UnixMilli(), threadRequestPollBatch)
	if err != nil || len(ops) == 0 {
		a.endOwnerOpDrain()
		if err != nil {
			logThreadRequestSweep("read due owner operations", err)
		}
		return
	}
	if a.lifeCtx().Err() != nil || !a.threadRequestsWG.Go(func() {
		defer a.endOwnerOpDrain()
		a.drainOwnerOps(ops)
	}) {
		a.endOwnerOpDrain()
	}
}

// drainOwnerOps delivers ops, grouped by computer.
func (a *App) drainOwnerOps(ops []store.OwnerOp) {
	var order []string
	byComputer := make(map[string][]store.OwnerOp)
	for _, op := range ops {
		if _, seen := byComputer[op.ComputerID]; !seen {
			order = append(order, op.ComputerID)
		}
		byComputer[op.ComputerID] = append(byComputer[op.ComputerID], op)
	}
	var wg sync.WaitGroup
	limit := make(chan struct{}, threadRequestPollFanOut)
	for _, computerID := range order {
		select {
		case limit <- struct{}{}:
		case <-a.lifeCtx().Done():
			wg.Wait()
			return
		}
		wg.Go(func() {
			defer func() { <-limit }()
			for _, op := range byComputer[computerID] {
				if a.lifeCtx().Err() != nil {
					return
				}
				ctx, cancel := context.WithTimeout(a.lifeCtx(), threadPollCallTimeout)
				a.deliverOwnerOp(ctx, op)
				cancel()
			}
		})
	}
	wg.Wait()
}

func (a *App) beginOwnerOpDrain() bool {
	d := &a.ownerOps
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.draining {
		return false
	}
	d.draining = true
	return true
}

func (a *App) endOwnerOpDrain() {
	d := &a.ownerOps
	d.mu.Lock()
	d.draining = false
	d.mu.Unlock()
	// The rows this pass rescheduled are the sweep's next deadline, and the
	// sweep left them out of its schedule while the pass held them.
	a.nudgeThreadRequestSweep()
}

// ownerOpDrainInFlight reports whether a delivery pass holds the rows it
// read.
func (a *App) ownerOpDrainInFlight() bool {
	d := &a.ownerOps
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.draining
}
