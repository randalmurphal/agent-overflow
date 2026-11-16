package app

import (
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/store"
)

// A spawn completes when its worker is at rest, not when its turn first ends
// (docs/specs/agent-thread-tools.md, "What settles a request"). A request of
// kind spawn, and a send to a thread its caller spawned, whose turn ends
// as it would settle `finished` while the thread is not at rest
// (threadRest) is held here instead: its receipt stays `running`, out of
// the turn-end gate, and settles `finished` with the thread's last message
// once the thread rests. Every other end of its turn settles as before: an
// error, an interruption, a Stop and an explicit thread_reply. A send or
// ask to a thread the caller did not spawn settles on its turn's end.
//
// The hold is memory. Whatever can bring a thread to rest reaches the Agents
// rows' drain (noteWorkerLiveness, noteWorkerWaiting, a request's change),
// which asks for a pass over the holds (noteRestWaitsMayChange); a pass
// reads each held thread's rest fresh and settles those at rest. The boot
// holds again what the last run held: a running receipt whose turn ended as
// it would settle `finished` (holdRestingReceiptsAtBoot), and its first
// pass runs once the loops, watches and live workers are recovered.

// restWaits is the requests whose settlement waits on their thread's rest.
type restWaits struct {
	mu sync.Mutex
	// held maps a thread to its held tokens, each with the index of the
	// turn its request's message was read in: the answer is the thread's
	// last message from that turn on.
	held map[string]map[string]int
	// checking is a pass running; again asks it for another once done.
	checking bool
	again    bool
}

// restHold is one held request.
type restHold struct {
	thread string
	token  string
	turn   int
}

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

// restSettlement is how settlement, what the end of receipt's turn turn
// answers, applies: as it is, for any settlement but `finished` and for a
// receipt that does not wait for rest; with the thread's last message now,
// for one whose thread is at rest; or not yet (hold), for one whose thread
// is not, which is then held and leaves the turn-end gate. The caller holds
// the token's settle lock and writes the settlement it returns.
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
	a.holdForRest(restHold{thread: thread, token: receipt.Token, turn: turn})
	rest, err := a.threadRest(thread)
	if err != nil {
		// The turn-end gate keeps it; the next turn end or session end
		// reads again.
		a.releaseRestHold(thread, receipt.Token)
		return settlement, false, err
	}
	if rest.unrest != "" {
		a.forgetReceiptRunning(thread, receipt.Token)
		// The rest this read missed may already have been reported, before
		// the hold was there to see it.
		a.noteRestWaitsMayChange()
		return store.ThreadRequestSettlement{}, true, nil
	}
	a.releaseRestHold(thread, receipt.Token)
	at := settlement.SettledAt
	settlement, err = a.restAnswer(thread, turn)
	if at != 0 {
		settlement.SettledAt = at
	}
	return settlement, false, err
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

func (a *App) holdForRest(hold restHold) {
	w := &a.restWaits
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.held == nil {
		w.held = map[string]map[string]int{}
	}
	tokens := w.held[hold.thread]
	if tokens == nil {
		tokens = map[string]int{}
		w.held[hold.thread] = tokens
	}
	tokens[hold.token] = hold.turn
}

// releaseRestHold drops one hold: its request settled, or is no longer
// the thread's to settle.
func (a *App) releaseRestHold(threadID, token string) {
	w := &a.restWaits
	w.mu.Lock()
	defer w.mu.Unlock()
	tokens := w.held[threadID]
	if tokens == nil {
		return
	}
	delete(tokens, token)
	if len(tokens) == 0 {
		delete(w.held, threadID)
	}
}

// restHeld reports whether token's request waits for its thread's rest.
func (a *App) restHeld(threadID, token string) bool {
	w := &a.restWaits
	w.mu.Lock()
	defer w.mu.Unlock()
	_, held := w.held[threadID][token]
	return held
}

// restHeldThread reports whether a request on threadID waits for its rest.
func (a *App) restHeldThread(threadID string) bool {
	w := &a.restWaits
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.held[threadID]) > 0
}

func (a *App) restHolds() []restHold {
	w := &a.restWaits
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]restHold, 0, len(w.held))
	for thread, tokens := range w.held {
		for token, turn := range tokens {
			out = append(out, restHold{thread: thread, token: token, turn: turn})
		}
	}
	return out
}

// noteRestWaitsMayChange asks for a pass over the held requests, on its own
// goroutine, one at a time: a request held now may be at rest. Nothing is
// read while nothing is held.
func (a *App) noteRestWaitsMayChange() {
	w := &a.restWaits
	w.mu.Lock()
	if len(w.held) == 0 {
		w.mu.Unlock()
		return
	}
	if w.checking {
		w.again = true
		w.mu.Unlock()
		return
	}
	w.checking = true
	w.mu.Unlock()
	if !a.goThreadRequestSettling(a.runRestPasses) {
		w.mu.Lock()
		w.checking = false
		w.mu.Unlock()
	}
}

func (a *App) runRestPasses() {
	w := &a.restWaits
	for {
		a.settleRestedReceipts()
		w.mu.Lock()
		if !w.again || a.lifeCtx().Err() != nil {
			w.checking, w.again = false, false
			w.mu.Unlock()
			return
		}
		w.again = false
		w.mu.Unlock()
	}
}

// settleRestedReceipts settles every held request whose thread is at rest
// now, together, so a caller several of them answer is told once. A thread
// liveWorkers records as not at rest by its own state is not read: its
// record changes, and asks for another pass, when it rests. A read that
// fails leaves the request held for the next pass.
func (a *App) settleRestedReceipts() {
	holds := a.restHolds()
	rested := map[string]bool{}
	works := make([]threadSettlementWork, 0, len(holds))
	for _, hold := range holds {
		atRest, read := rested[hold.thread]
		if !read {
			if !a.liveWorker(hold.thread) {
				rest, err := a.threadRest(hold.thread)
				if err != nil {
					log.Printf("thread tools: read whether %s is at rest for request %s: %v", hold.thread, hold.token, err)
				} else {
					atRest = rest.unrest == ""
				}
			}
			rested[hold.thread] = atRest
		}
		if !atRest {
			continue
		}
		work, err := a.settleReceiptAtRest(hold)
		if err != nil {
			log.Printf("thread tools: settle request %s on %s at rest: %v", hold.token, hold.thread, err)
			continue
		}
		works = append(works, work)
	}
	a.runThreadSettlementWorks(works)
}

// settleReceiptAtRest settles one held request `finished` with its thread's
// last message, unless something else settled it first, which drops the
// hold.
func (a *App) settleReceiptAtRest(hold restHold) (threadSettlementWork, error) {
	unlock := a.threadRequestSettleLock(hold.token)
	defer unlock()
	receipt, found, err := a.deliveredReceiptLocked(hold.token)
	if err != nil {
		return threadSettlementWork{}, err
	}
	if !found || receipt.TargetThreadID != hold.thread || receipt.State != store.ThreadReceiptRunning {
		a.releaseRestHold(hold.thread, hold.token)
		return threadSettlementWork{}, nil
	}
	settlement, err := a.restAnswer(hold.thread, hold.turn)
	if err != nil {
		return threadSettlementWork{}, err
	}
	work, _, err := a.settleReadReceiptLocked(receipt, []string{store.ThreadReceiptRunning}, settlement, dropScratchThread)
	return work, err
}

// heldReceiptAtBoot reports whether a receipt the last run left running is
// one it held for its thread's rest, and holds it again: its turn ended as
// it would settle `finished`, and it waits for rest. Its first pass runs
// once the boot recovered what the thread may wait on (startThreadRequestSweeps).
func (a *App) heldReceiptAtBoot(receipt store.ThreadRequestReceipt) (bool, error) {
	if receipt.State != store.ThreadReceiptRunning || receipt.TurnID == "" {
		return false, nil
	}
	index, ok := threadRequestTurnIndex(receipt.TargetThreadID, receipt.TurnID)
	if !ok {
		return false, nil
	}
	turn, found, err := a.store.GetTurnByThreadIndex(receipt.TargetThreadID, index)
	if err != nil || !found || turn.CompletedAt == nil {
		return false, err
	}
	if turn.StopReason == "interrupted" || turn.StopReason == "error" || turn.ErrorMessage != "" {
		return false, nil
	}
	awaits, err := a.receiptAwaitsRest(receipt)
	if err != nil || !awaits {
		return false, err
	}
	a.holdForRest(restHold{thread: receipt.TargetThreadID, token: receipt.Token, turn: index})
	return true, nil
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
