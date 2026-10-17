package app

import (
	"context"
	"log"

	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// Agent-to-agent work raises no unread dot (docs/specs/standing-agents.md
// ruling 30). When a spawned thread's turn answers a request from the thread
// that spawned it, and the answer reached that caller on this computer, the
// spawned thread is marked read through that turn's completion. A pinned
// thread keeps its dot.
//
// A turn-end settlement has its turn complete already and stamps at once. A
// thread_reply settles while its turn still runs: stamping then would leave
// the turn's completion past the mark, so the turn is held in answeredReads
// and stamped when the turn observer sees it end. A session that ends
// leaving the turn without a completion drops the hold, and so does a
// restart, which loses it.

// markAnsweredRead stamps, or holds for its turn's end, the read mark a
// settled receipt earned. The settlement read that its target was spawned
// by its source (LocalSettlement.AnsweredBySpawned). Only an answer the
// target's own turn gave counts: a reply, that turn's final message, or its
// error or interruption. A cancel, and a settlement for a message the
// target never read, carry no turn and mark nothing.
func (a *App) markAnsweredRead(receipt store.ThreadRequestReceipt) {
	switch receipt.State {
	case store.ThreadReceiptReplied, store.ThreadReceiptFinished, store.ThreadReceiptErrored, store.ThreadReceiptInterrupted:
	default:
		return
	}
	if receipt.TargetThreadID == "" || receipt.SourceThreadID == "" || receipt.TurnID == "" {
		return
	}
	index, ok := threadRequestTurnIndex(receipt.TargetThreadID, receipt.TurnID)
	if !ok {
		return
	}
	turn, found, err := a.store.GetTurnByThreadIndex(receipt.TargetThreadID, index)
	if err != nil {
		log.Printf("thread tools: read turn %d of %s for its answer's read mark: %v", index, receipt.TargetThreadID, err)
		return
	}
	if !found || turn.CompletedAt == nil {
		// The turn is running, or its row is not written yet: the receipt
		// binds to a turn as its message is delivered, before the turn row
		// lands. Held, then read again, since a turn that ended between
		// the read above and the hold found no hold to finish.
		a.holdAnsweredRead(receipt.TargetThreadID, index)
		a.finishAnsweredReads(receipt.TargetThreadID, false)
		return
	}
	a.stampAnsweredRead(receipt.TargetThreadID, *turn.CompletedAt)
}

// stampAnsweredRead writes the mark through at unless the thread is pinned,
// read at the stamp, and announces the row when it moved.
func (a *App) stampAnsweredRead(threadID string, at int64) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("thread tools: read %s for its answer's read mark: %v", threadID, err)
		return
	}
	if thread.PinnedAt != nil {
		return
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), markThreadReadTimeout)
	defer cancel()
	row, changed, err := a.store.MarkThreadReadThrough(ctx, threadID, at)
	if err != nil {
		log.Printf("thread tools: mark %s read through its answer: %v", threadID, err)
		return
	}
	if changed {
		a.threadAnnouncer().AnnounceWithin(a.lifeCtx(), a.emitEvent, triage.ThreadActionFull, row.ID)
	}
}

func (a *App) holdAnsweredRead(threadID string, turnIndex int) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	if a.threadRequests.answeredReads == nil {
		a.threadRequests.answeredReads = map[string]map[int]bool{}
	}
	turns := a.threadRequests.answeredReads[threadID]
	if turns == nil {
		turns = map[int]bool{}
		a.threadRequests.answeredReads[threadID] = turns
	}
	turns[turnIndex] = true
}

// hasAnsweredReads is the turn observer's gate for held marks.
func (a *App) hasAnsweredReads(threadID string) bool {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	return len(a.threadRequests.answeredReads[threadID]) > 0
}

// finishAnsweredReads stamps each held turn of threadID that has completed
// and keeps the rest, a turn whose row is not written yet included, for
// their own end. A session end drops the holds of turns it left without a
// completion: nothing will complete them.
func (a *App) finishAnsweredReads(threadID string, sessionEnded bool) {
	a.threadRequests.mu.Lock()
	held := a.threadRequests.answeredReads[threadID]
	turns := make([]int, 0, len(held))
	for index := range held {
		turns = append(turns, index)
	}
	a.threadRequests.mu.Unlock()
	for _, index := range turns {
		turn, found, err := a.store.GetTurnByThreadIndex(threadID, index)
		if err != nil {
			log.Printf("thread tools: read turn %d of %s for its answer's read mark: %v", index, threadID, err)
			continue
		}
		switch {
		case found && turn.CompletedAt != nil:
			a.forgetAnsweredRead(threadID, index)
			a.stampAnsweredRead(threadID, *turn.CompletedAt)
		case sessionEnded:
			a.forgetAnsweredRead(threadID, index)
		}
	}
}

func (a *App) forgetAnsweredRead(threadID string, turnIndex int) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	turns := a.threadRequests.answeredReads[threadID]
	delete(turns, turnIndex)
	if len(turns) == 0 {
		delete(a.threadRequests.answeredReads, threadID)
	}
}
