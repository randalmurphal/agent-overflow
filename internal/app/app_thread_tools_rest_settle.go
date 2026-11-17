package app

import (
	"fmt"
	"log"
	"maps"
	"strings"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// A spawn completes when its worker is at rest, not when its turn first ends
// (docs/specs/agent-thread-tools.md, "What settles a request"). A request of
// kind spawn, and a send to a thread its caller spawned, whose turn ends as
// it would settle `finished` while the thread is not at rest (threadRest)
// stays `running` with its gate held (receiptGate.held): a phase of the same
// lifecycle, settled by the same observer. While held, each later turn end
// and session end on the thread is read as the request's own would be: an
// error settles it `errored`, an interruption `interrupted` with its cause,
// a person's Stop as a Stop does, and a session that ends mid-turn
// `interrupted` (InterruptSessionEndedWorking). Only a turn that ends with
// the thread at rest settles it `finished`, with the thread's last message
// from the request's turn on.
//
// A turn that ends `finished` short of rest leaves it held. Whatever can
// bring the thread to rest reaches the Agents rows' drain, which asks for a
// pass over the held requests (noteRestWaitsMayChange). A pass reads rest
// from facts complete when it reads them: the thread's own sources, durable
// ones first (workerUnrest), and the recorded state of the workers below
// it. At boot no held request settles at rest until every source is
// recovered into that record (recoverRestAtBoot): a pass asked for before
// then, by a turn end, a recovery event or the boot itself, waits for it.

// restPasses schedules the passes: one at a time, again once done when
// asked meanwhile, none before the boot's recovery (barred), and what that
// recovery has still to do.
type restPasses struct {
	checking bool
	again    bool
	barred   bool
	recovery restRecovery
}

// restRecovery is what the boot's recovery of rest has still to do: the
// sources it has not read and the threads it has not reassessed, each
// tried again by every pass of the request sweep (retryRestRecovery).
type restRecovery struct {
	sources map[string]bool
	threads map[string]bool
}

func (r restRecovery) pending() bool { return len(r.sources)+len(r.threads) > 0 }

// receiptAwaitsRest reports whether receipt settles on its thread's rest
// rather than its turn's end: a spawn, or a send from the thread that
// spawned its target, on this computer or another (the spawn's receipt
// here, its lineage past the retention floor included).
func (a *App) receiptAwaitsRest(receipt store.ThreadRequestReceipt) (bool, error) {
	switch receipt.Kind {
	case store.ThreadRequestSpawn:
		return true, nil
	case store.ThreadRequestSend:
		if receipt.TargetThreadID == "" || receipt.SourceThreadID == "" {
			return false, nil
		}
		return a.store.ThreadSpawnedForSource(receipt.TargetThreadID, receipt.OwnerDeviceID, receipt.SourceThreadID)
	default:
		return false, nil
	}
}

// restSettlement is how settlement, what the end of receipt's own turn
// (turn) answers, applies: as it is, for any settlement but `finished` and
// for a receipt that does not wait for rest; with the thread's last message,
// for one whose thread is at rest; or not yet (held), for one whose thread
// is not, whose gate is then held. A failed read of the rest leaves it held
// for the next pass. The caller holds the token's settle lock and writes the
// settlement it returns.
func (a *App) restSettlement(receipt store.ThreadRequestReceipt, settlement store.ThreadRequestSettlement, turn int) (store.ThreadRequestSettlement, bool, error) {
	if settlement.State != store.ThreadReceiptFinished {
		return settlement, false, nil
	}
	awaits, err := a.receiptAwaitsRest(receipt)
	if err != nil || !awaits {
		return settlement, false, err
	}
	thread := receipt.TargetThreadID
	// Held before its rest is read, so a pass that runs once the thread
	// rests after this read finds it.
	a.holdReceiptGate(thread, receipt.Token, turn, turn)
	atRest, err := a.heldThreadAtRest(thread)
	if err != nil || !atRest {
		// The rest this read missed may already have been reported, before
		// the hold was there to see it.
		a.noteRestWaitsMayChange()
		return store.ThreadRequestSettlement{}, true, err
	}
	at := settlement.SettledAt
	settlement, err = a.restAnswer(thread, turn)
	if at != 0 {
		settlement.SettledAt = at
	}
	return settlement, false, err
}

// heldThreadAtRest reads threadID's rest for a held request, false while
// the boot has not recovered every source of it, and while a stop brings the
// thread to rest (holdRest): the stop cancels what held it before it settles
// the requests on it, which it settles itself.
func (a *App) heldThreadAtRest(threadID string) (bool, error) {
	a.threadRequests.mu.Lock()
	barred := a.threadRequests.rest.barred
	a.threadRequests.mu.Unlock()
	if barred || a.resting.holds(threadID) {
		return false, nil
	}
	rest, err := a.threadRest(threadID)
	return err == nil && rest.unrest == "", err
}

// restAnswer is a held request's answer at its thread's rest: `finished`
// with the thread's last top-level message from the request's turn on.
func (a *App) restAnswer(threadID string, turn int) (store.ThreadRequestSettlement, error) {
	text := ""
	row, found, err := a.store.LastTopLevelItem(threadID, "assistant_text")
	if err != nil {
		return store.ThreadRequestSettlement{}, err
	}
	if found && row.TurnIndex >= turn {
		if text, _, err = a.threadTools().itemBody(row, threadItemWholeBody); err != nil {
			return store.ThreadRequestSettlement{}, err
		}
	}
	if strings.TrimSpace(text) == "" {
		text = "That thread's turn ended without a message."
	}
	return store.ThreadRequestSettlement{
		State:      store.ThreadReceiptFinished,
		Answer:     []byte(text),
		AnswerKind: store.ThreadAnswerFinal,
		SettledAt:  time.Now().UnixMilli(),
	}, nil
}

// holdReceiptGate marks token's gate held: its answer from its own turn
// turn on, the ends of its thread's turns read through through.
func (a *App) holdReceiptGate(threadID, token string, turn, through int) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	tokens := a.receiptGatesLocked(threadID)
	gate := tokens[token]
	if !gate.held {
		gate.held, gate.turn, gate.through = true, turn, through
	}
	tokens[token] = gate
}

// heldReceiptGate is token's gate while it is held.
func (a *App) heldReceiptGate(threadID, token string) (receiptGate, bool) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	gate, found := a.threadRequests.running[threadID][token]
	return gate, found && gate.held
}

// readHeldReceiptTurns records that a held request has read the ends of
// the thread's turns up to through.
func (a *App) readHeldReceiptTurns(threadID, token string, through int) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	if gate, found := a.threadRequests.running[threadID][token]; found && gate.held && through > gate.through {
		gate.through = through
		a.threadRequests.running[threadID][token] = gate
	}
}

// restHeld reports whether token's request waits for its thread's rest.
func (a *App) restHeld(threadID, token string) bool {
	_, held := a.heldReceiptGate(threadID, token)
	return held
}

// heldEnd is what asks a held request to read its thread: a turn end (the
// zero value), a pass over the held requests (pass), or the end of a session
// (sessionEnded, with the session that ended, empty for one not named).
type heldEnd struct {
	pass         bool
	sessionEnded bool
	session      string
}

// settleHeldReceiptLocked reads what a held request's thread did since it
// last read: the end of each turn it ran, any but `finished` settling the
// request as that turn's end settles one; then, with each of them
// `finished`, its rest. The caller holds the token's settle lock and read
// the receipt `running` on gate's thread.
func (a *App) settleHeldReceiptLocked(receipt store.ThreadRequestReceipt, gate receiptGate, end heldEnd) (threadSettlementWork, error) {
	thread := receipt.TargetThreadID
	settlement, decided, through, pending, err := a.heldTurnsSettlement(thread, gate, end)
	if err != nil {
		return threadSettlementWork{}, err
	}
	if !decided {
		a.readHeldReceiptTurns(thread, receipt.Token, through)
		if pending {
			return threadSettlementWork{}, nil
		}
		atRest, err := a.heldThreadAtRest(thread)
		if err != nil || !atRest {
			return threadSettlementWork{}, err
		}
		if settlement, err = a.restAnswer(thread, gate.turn); err != nil {
			return threadSettlementWork{}, err
		}
	}
	work, _, err := a.settleReadReceiptLocked(receipt, []string{store.ThreadReceiptRunning}, settlement, dropScratchThread)
	return work, err
}

// heldTurnsSettlement reads the ends of thread's turns from the last one a
// held request read, oldest first: the first that did not end `finished`
// decides its settlement, as its own turn's end would (turnSettlement). A
// turn a session's end cut is InterruptSessionEndedWorking, with what the
// thread wrote since the request's turn. through is the last turn read
// that ended `finished`; pending says a turn from it on is still to be
// read at its own end: one open, or, for a pass, one interrupted with no
// stop recorded, which its session's end reads.
//
// The last turn read, the request's own at first, is read again: a turn's
// first close is not its outcome. A soft close a trailing error result
// amends, and a turn the model continues after it, read as they are now.
func (a *App) heldTurnsSettlement(thread string, gate receiptGate, end heldEnd) (settlement store.ThreadRequestSettlement, decided bool, through int, pending bool, err error) {
	through = gate.through
	turns, err := a.store.TurnsAfter(thread, gate.through-1)
	if err != nil {
		return settlement, false, through, false, err
	}
	for _, turn := range turns {
		cut := false
		switch {
		case turn.CompletedAt == nil || (a.triage != nil && a.triage.OpenTurnIndex(thread) == turn.TurnIndex):
			if !end.sessionEnded || a.turnOfLiveSession(thread, turn, end.session) {
				return settlement, false, through, true, nil
			}
			cut = true
		case turn.StopReason == "interrupted":
			// A turn its session's end cut records no stop of its own; one a
			// person or a thread stopped does.
			_, stopped := a.turnStopOf(thread, turn)
			if !stopped && end.pass {
				return settlement, false, through, true, nil
			}
			cut = end.sessionEnded && !stopped
		}
		if cut {
			settlement, err = a.heldSessionEndSettlement(thread, gate.turn)
			return settlement, err == nil, through, false, err
		}
		if turn.StopReason == "interrupted" || turn.StopReason == "error" || turn.ErrorMessage != "" {
			settlement, err = a.turnSettlement(thread, turn)
			return settlement, err == nil, through, false, err
		}
		through = turn.TurnIndex
	}
	return settlement, false, through, false, nil
}

// turnOfLiveSession reports whether turn is open in the thread's live
// session, a later one than the session that ended: that session's end
// did not cut it.
func (a *App) turnOfLiveSession(threadID string, turn store.Turn, ended string) bool {
	if ended == "" || a.triage == nil {
		return false
	}
	sess, live := a.sessionManager().get(threadID)
	return live && sess.Token != ended && a.triage.OpenTurnIndex(threadID) == turn.TurnIndex
}

// heldSessionEndSettlement is a held request's answer when its thread's
// session ended mid-turn: interrupted, with the thread's last message from
// the request's turn on.
func (a *App) heldSessionEndSettlement(threadID string, turn int) (store.ThreadRequestSettlement, error) {
	if a.triage != nil {
		// What the cut turn wrote last may still be in the stream buffers.
		if err := a.triage.FlushThread(threadID); err != nil {
			return store.ThreadRequestSettlement{}, err
		}
	}
	written := ""
	row, found, err := a.store.LastTopLevelItem(threadID, "assistant_text")
	if err != nil {
		return store.ThreadRequestSettlement{}, err
	}
	if found && row.TurnIndex >= turn {
		if written, _, err = a.threadTools().itemBody(row, threadItemWholeBody); err != nil {
			return store.ThreadRequestSettlement{}, err
		}
	}
	return interruptedSettlement(threadtools.Cause{Code: threadtools.CauseSessionEndedWorking}, written), nil
}

// noteRestWaitsMayChange asks for a pass over the held requests, on its own
// goroutine, one at a time: a request held now may be at rest. Nothing is
// read while nothing is held, and nothing runs before the boot's recovery,
// whose end runs the pass asked for meanwhile.
func (a *App) noteRestWaitsMayChange() {
	r := &a.threadRequests
	r.mu.Lock()
	if r.rest.barred || r.rest.checking {
		r.rest.again = true
		r.mu.Unlock()
		return
	}
	if !a.anyReceiptHeldLocked() {
		r.mu.Unlock()
		return
	}
	r.rest.checking = true
	r.mu.Unlock()
	if !a.goThreadRequestSettling(a.runRestPasses) {
		r.mu.Lock()
		r.rest.checking = false
		r.mu.Unlock()
	}
}

func (a *App) anyReceiptHeldLocked() bool {
	for _, tokens := range a.threadRequests.running {
		for _, gate := range tokens {
			if gate.held {
				return true
			}
		}
	}
	return false
}

func (a *App) runRestPasses() {
	r := &a.threadRequests
	for {
		a.settleRestedReceipts()
		r.mu.Lock()
		if !r.rest.again || a.lifeCtx().Err() != nil {
			r.rest.checking, r.rest.again = false, false
			r.mu.Unlock()
			return
		}
		r.rest.again = false
		r.mu.Unlock()
	}
}

// heldReceipt names one held request.
type heldReceipt struct {
	thread string
	token  string
}

func (a *App) heldReceipts() []heldReceipt {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	var out []heldReceipt
	for thread, tokens := range a.threadRequests.running {
		for token, gate := range tokens {
			if gate.held {
				out = append(out, heldReceipt{thread: thread, token: token})
			}
		}
	}
	return out
}

// settleRestedReceipts settles every held request whose thread is at rest
// now, together, so a caller several of them answer is told once. Each held
// thread's rest is read fresh (threadRest), once a pass: what liveWorkers
// records only draws rows, so a change it missed cannot hold a request. A
// read that fails leaves the request held for the next pass.
//
// A held request on a thread under a paused assistant does not settle at
// rest: its loops do not fire while assistants are off, and the pause
// settles it. The pass asks the convergence to (askPausedAssistantRequests),
// which reads the setting again under its own lock; it stops nothing itself.
func (a *App) settleRestedReceipts() {
	holds := a.heldReceipts()
	rested := map[string]bool{}
	paused := map[string]bool{}
	askPause := false
	works := make([]threadSettlementWork, 0, len(holds))
	for _, hold := range holds {
		pausedHere, known := paused[hold.thread]
		if !known {
			var err error
			if pausedHere, err = a.pausedAssistantThread(hold.thread); err != nil {
				log.Printf("thread tools: read whether %s is under a paused assistant for request %s: %v", hold.thread, hold.token, err)
				continue
			}
			paused[hold.thread] = pausedHere
		}
		if pausedHere {
			if hook := a.restPausedReadForTest; hook != nil && !known {
				hook(hold.thread)
			}
			askPause = true
			continue
		}
		atRest, read := rested[hold.thread]
		if !read {
			var err error
			if atRest, err = a.heldThreadAtRest(hold.thread); err != nil {
				log.Printf("thread tools: read whether %s is at rest for request %s: %v", hold.thread, hold.token, err)
			}
			rested[hold.thread] = atRest
		}
		if !atRest {
			continue
		}
		work, err := a.settleHeldReceipt(hold, heldEnd{pass: true})
		if err != nil {
			log.Printf("thread tools: settle request %s on %s at rest: %v", hold.token, hold.thread, err)
			continue
		}
		works = append(works, work)
	}
	a.runThreadSettlementWorks(works)
	if askPause {
		a.askPausedAssistantRequests()
	}
}

// settleHeldReceipt is settleHeldReceiptLocked for a held request named by
// its gate, under its settle lock. One something else settled first leaves
// the gate.
func (a *App) settleHeldReceipt(hold heldReceipt, end heldEnd) (threadSettlementWork, error) {
	unlock := a.threadRequestSettleLock(hold.token)
	defer unlock()
	gate, held := a.heldReceiptGate(hold.thread, hold.token)
	if !held {
		return threadSettlementWork{}, nil
	}
	receipt, found, err := a.deliveredReceiptLocked(hold.token)
	if err != nil {
		return threadSettlementWork{}, err
	}
	if !found || receipt.TargetThreadID != hold.thread || receipt.State != store.ThreadReceiptRunning {
		a.forgetReceiptRunning(hold.thread, hold.token)
		return threadSettlementWork{}, nil
	}
	return a.settleHeldReceiptLocked(receipt, gate, end)
}

// heldReceiptAtBoot reads a receipt the last run left running whose turn
// ended: one held for its thread's rest is held again, unless a later turn
// of its thread ended other than `finished`, whose end then settles it.
// held is false for any other receipt, which the boot settles as
// interrupted by the restart; settlement is what one that is no longer
// held settles with, CauseRestart for a turn the restart cut. Its first
// pass runs once the boot recovered what the thread may wait on
// (recoverRestAtBoot).
func (a *App) heldReceiptAtBoot(receipt store.ThreadRequestReceipt) (store.ThreadRequestSettlement, bool, error) {
	restart := interruptedSettlement(threadtools.Cause{Code: threadtools.CauseRestart}, "")
	if receipt.State != store.ThreadReceiptRunning || receipt.TurnID == "" {
		return restart, false, nil
	}
	index, ok := threadRequestTurnIndex(receipt.TargetThreadID, receipt.TurnID)
	if !ok {
		return restart, false, nil
	}
	turn, found, err := a.store.GetTurnByThreadIndex(receipt.TargetThreadID, index)
	if err != nil || !found || turn.CompletedAt == nil {
		return restart, false, err
	}
	if turn.StopReason == "interrupted" || turn.StopReason == "error" || turn.ErrorMessage != "" {
		return restart, false, nil
	}
	awaits, err := a.receiptAwaitsRest(receipt)
	if err != nil || !awaits {
		return restart, false, err
	}
	later, err := a.store.TurnsAfter(receipt.TargetThreadID, index)
	if err != nil {
		return restart, false, err
	}
	// The request answers from its own turn; the boot read the later ones.
	own := index
	for _, turn := range later {
		switch {
		case turn.CompletedAt == nil || turn.StopReason == "interrupted":
			// The restart cut it: no stop of the last run survives it.
			return restart, false, nil
		case turn.StopReason == "error" || turn.ErrorMessage != "":
			settlement, err := a.turnSettlement(receipt.TargetThreadID, turn)
			if err != nil {
				return restart, false, fmt.Errorf("read the errored turn %d: %w", turn.TurnIndex, err)
			}
			return settlement, false, nil
		}
		index = turn.TurnIndex
	}
	a.holdReceiptGate(receipt.TargetThreadID, receipt.Token, own, index)
	return store.ThreadRequestSettlement{}, true, nil
}

// barRestAtBoot keeps every held request from settling at rest until
// recoverRestAtBoot has recovered what each thread may wait on.
func (a *App) barRestAtBoot() {
	a.threadRequests.mu.Lock()
	a.threadRequests.rest.barred = true
	a.threadRequests.mu.Unlock()
}

// restRecoverySources are the durable sources the boot recovers rest from,
// each listing the threads it keeps from rest.
func (a *App) restRecoverySources() []restRecoverySource {
	return []restRecoverySource{
		{"loops and watches", a.store.ThreadsWithLiveAgentWakes},
		{"reminders", a.store.ThreadsWithOpenReminders},
		{"remote commands", a.store.RemoteWatchThreadIDs},
		{"answers on their way", a.store.ThreadsWithAnswerDue},
		{"workflow runs", a.store.ThreadsWithRunningWorkflowRuns},
		{"unanswered questions", a.store.ThreadsWithUnansweredAsyncQuestions},
		{"queued messages", a.store.ListThreadsWithFlushQueueItems},
	}
}

type restRecoverySource struct {
	what string
	read func() ([]string, error)
}

// recoverRestAtBoot records, before any held request settles at rest, every
// thread a recovered source keeps from rest, each reassessed on the
// sweep's goroutine, before its first pass: a loop or watch armed, a
// reminder unfired, a remote command pending, an answer on its way, a
// workflow run reporting to it, a question unanswered, a message queued.
// Then it lifts the bar and runs the pass. What fails is kept and tried
// again by each pass of the sweep (retryRestRecovery), and the bar stays
// until all of it succeeded, so nothing settles at rest on a partial
// record; meanwhile the requests stay held, and their threads' turn ends
// and session ends, and replies, still settle them every way but rest.
func (a *App) recoverRestAtBoot() {
	sources := map[string]bool{}
	for _, source := range a.restRecoverySources() {
		sources[source.what] = true
	}
	a.threadRequests.mu.Lock()
	a.threadRequests.rest.recovery = restRecovery{sources: sources, threads: map[string]bool{}}
	a.threadRequests.mu.Unlock()
	a.retryRestRecovery()
}

// retryRestRecovery reads the sources the boot's recovery has not read and
// reassesses the threads it has not, keeps what fails for the next pass,
// and lifts the bar once nothing is left. Nothing to do once the recovery
// is done. It runs on the sweep's goroutine, one at a time.
func (a *App) retryRestRecovery() {
	r := &a.threadRequests
	r.mu.Lock()
	if !r.rest.recovery.pending() {
		r.mu.Unlock()
		return
	}
	todo := r.rest.recovery
	r.mu.Unlock()
	left := restRecovery{sources: map[string]bool{}, threads: map[string]bool{}}
	threads := maps.Clone(todo.threads)
	for _, source := range a.restRecoverySources() {
		if !todo.sources[source.what] {
			continue
		}
		ids, err := source.read()
		if err != nil {
			log.Printf("thread tools: recover the threads waiting on %s: %v", source.what, err)
			left.sources[source.what] = true
			continue
		}
		for _, id := range ids {
			threads[id] = true
		}
	}
	for id := range threads {
		if _, err := a.reassessWorker(id, true); err != nil {
			log.Printf("thread tools: recover whether %s is at rest: %v", id, err)
			left.threads[id] = true
		}
		// Its rows are framed as any change's are.
		a.noteWorkerWaiting(id)
	}
	r.mu.Lock()
	r.rest.recovery = left
	done := !left.pending()
	if done {
		r.rest.recovery = restRecovery{}
	}
	if done {
		r.rest.barred, r.rest.again = false, false
	}
	r.mu.Unlock()
	if done {
		a.noteRestWaitsMayChange()
	}
}

// restRecoveryPending reports whether the boot's recovery of rest has
// sources or threads left to try again.
func (a *App) restRecoveryPending() bool {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	return a.threadRequests.rest.recovery.pending()
}

// requestWaitingOn is what an open request's thread waits on while its
// request is held for its rest, for thread_status: a local thread's rest
// read now, a remote one's as its computer last reported it. Empty for a
// request not held: its state says what its thread is doing.
func (a *App) requestWaitingOn(row store.ThreadRequest) string {
	if threadRequestSettled(row) || row.TargetThreadID == "" {
		return ""
	}
	if row.TargetComputerID != "" {
		return a.remoteRequestBlocked(row.Token).waitingOn
	}
	if !a.restHeld(row.TargetThreadID, row.Token) {
		return ""
	}
	rest, err := a.threadRest(row.TargetThreadID)
	if err != nil {
		log.Printf("thread tools: read what %s waits on: %v", row.TargetThreadID, err)
		return ""
	}
	return rest.waitingLine()
}
