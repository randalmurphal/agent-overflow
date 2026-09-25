package triage

import (
	"log"

	"agent-overflow/internal/store"
)

// Subagent cards: the store handle every row with a parent is written
// with (store.OpenSubagentCard). The store feeds a card from the rows
// written with it and writes the result to the anchors' stamps when the
// card is flushed. The router keeps one card per parent for a live
// session and flushes the thread's cards before each anchor push
// (refreshWireItems: the quiet-point timer, turn completion and
// teardown). Before a first child's anchors are pushed
// (emitFirstChildAnchors) it flushes that child's chain, and before an
// agent's completion sibling or stop notification is written
// (settleSubagentCard) the agent's chain: the cards of other agents wait
// for the quiet point. A thread without a live session opens a card per
// write and closes it after, which flushes what the card reaches.

// maxSubagentCardsPerThread bounds the cards a thread keeps open. A card
// costs a few hundred bytes; the parents a session writes under are its
// agent launches and the tool calls they nest, and a card is released
// when its agent completes. At the bound an idle card is closed, and
// opened again if its parent writes again.
const maxSubagentCardsPerThread = 256

// subagentCardEntry is a cached card. uses counts the writes holding it;
// a retired entry is closed by the last of them. Guarded by r.mu.
type subagentCardEntry struct {
	card    *store.SubagentCard
	uses    int
	retired bool
}

// withSubagentCard runs write with the card for rows under parentID, or
// with nil for a top-level row.
func (r *Router) withSubagentCard(threadID, parentID string, write func(*store.SubagentCard) error) error {
	if parentID == "" {
		return write(nil)
	}
	entry, err := r.acquireSubagentCard(threadID, parentID)
	if err != nil {
		return err
	}
	defer r.releaseSubagentCard(entry)
	return write(entry.card)
}

func (r *Router) acquireSubagentCard(threadID, parentID string) (*subagentCardEntry, error) {
	r.mu.Lock()
	if st := r.threadStateIfPresent(threadID); st != nil {
		if entry := st.subagentCards[parentID]; entry != nil {
			entry.uses++
			r.mu.Unlock()
			return entry, nil
		}
	}
	r.mu.Unlock()
	// Opened outside r.mu: it reads the parent's chain.
	card, err := r.store.OpenSubagentCard(threadID, parentID)
	if err != nil {
		return nil, err
	}
	entry := &subagentCardEntry{card: card, uses: 1}
	var idle []*subagentCardEntry
	r.mu.Lock()
	if id := r.identityIfPresent(threadID); id == nil || id.stopped {
		// No live session keeps the card: it is closed after this write.
		entry.retired = true
	} else {
		st := r.state(threadID)
		if cached := st.subagentCards[parentID]; cached != nil {
			// A concurrent write opened it first; this card wrote nothing.
			cached.uses++
			idle = append(idle, entry)
			entry = cached
		} else {
			if len(st.subagentCards) >= maxSubagentCardsPerThread {
				for key, other := range st.subagentCards {
					if other.uses == 0 {
						delete(st.subagentCards, key)
						other.retired = true
						idle = append(idle, other)
						break
					}
				}
			}
			if st.subagentCards == nil {
				st.subagentCards = make(map[string]*subagentCardEntry)
			}
			st.subagentCards[parentID] = entry
		}
	}
	r.mu.Unlock()
	closeSubagentCards(threadID, idle)
	return entry, nil
}

func (r *Router) releaseSubagentCard(entry *subagentCardEntry) {
	r.mu.Lock()
	entry.uses--
	closeNow := entry.retired && entry.uses == 0
	r.mu.Unlock()
	if closeNow {
		closeSubagentCards(entry.card.ThreadID(), []*subagentCardEntry{entry})
	}
}

// retireSubagentCardsLocked takes the thread's cards out of the cache:
// the idle ones are returned for the caller to close once it releases
// r.mu, and the ones in use are closed by their last write. parentID
// names one card, or "" for all. Callers hold r.mu.
func (st *threadState) retireSubagentCardsLocked(parentID string) []*subagentCardEntry {
	var idle []*subagentCardEntry
	for key, entry := range st.subagentCards {
		if parentID != "" && key != parentID {
			continue
		}
		delete(st.subagentCards, key)
		entry.retired = true
		if entry.uses == 0 {
			idle = append(idle, entry)
		}
	}
	return idle
}

// closeSubagentCards closes cards, which flushes what each reaches. A
// failed flush keeps the accumulators for the thread's next flush.
func closeSubagentCards(threadID string, entries []*subagentCardEntry) {
	for _, entry := range entries {
		if err := entry.card.Close(); err != nil {
			log.Printf("triage: close subagent card %s/%s: %v", threadID, entry.card.ParentID(), err)
		}
	}
}

// flushSubagentCards writes the thread's pending card accumulators to
// their anchors' stamps and returns the anchors whose stamp changed. A
// failure keeps the accumulators for the next flush.
func (r *Router) flushSubagentCards(threadID string) []string {
	if r.store == nil {
		return nil
	}
	changed, err := r.store.FlushSubagentCards(threadID)
	if err != nil {
		log.Printf("triage: flush subagent cards of %s: %v", threadID, err)
	}
	return changed
}

// flushSubagentChain writes the pending card accumulators on parentID's
// chain to their stamps (store.FlushSubagentChain). A failure keeps them
// for the next flush.
func (r *Router) flushSubagentChain(threadID, parentID string) {
	if r.store == nil {
		return
	}
	if _, err := r.store.FlushSubagentChain(threadID, parentID); err != nil {
		log.Printf("triage: flush the subagent cards of %s/%s: %v", threadID, parentID, err)
	}
}

// subagentCardOpen reports whether rows were written under launchID
// that its stamp may not hold yet: the router keeps a card for it, or the
// store holds unflushed changes to its stamp (from a nested agent's
// rows). Memory only; an ordinary tool's completion costs no statement.
func (r *Router) subagentCardOpen(threadID, launchID string) bool {
	if r.store == nil {
		return false
	}
	r.mu.Lock()
	st := r.threadStateIfPresent(threadID)
	cached := st != nil && st.subagentCards[launchID] != nil
	r.mu.Unlock()
	return cached || r.store.SubagentCardPending(threadID, launchID)
}

// settleSubagentCard runs before an agent's completion sibling or stop
// notification is written: the launch's chain is flushed, so the sibling
// reads the card its launch ends with, and the launch's own card leaves
// the cache. A later row under it opens the card again.
func (r *Router) settleSubagentCard(threadID, launchID string) {
	r.flushSubagentChain(threadID, launchID)
	r.mu.Lock()
	var idle []*subagentCardEntry
	if st := r.threadStateIfPresent(threadID); st != nil {
		idle = st.retireSubagentCardsLocked(launchID)
	}
	r.mu.Unlock()
	closeSubagentCards(threadID, idle)
}
