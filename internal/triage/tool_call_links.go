package triage

import (
	"fmt"

	"agent-overflow/internal/store"
)

// Tool-call links: what the router remembers about the tool_call rows a
// live session has persisted, so the per-event parent validation
// (shouldDropParentID) and scope placement (turnIndexForScope) do not
// re-read rows the session wrote itself.
//
// A link records the two placement facts a later event asks about: the
// row's parent and its turn. Neither moves once the row is written (a
// persisted row's parent is never reparented, see persistToolCallLaunch;
// an empty one can still be filled, which the write re-caches). Rows persisted
// before the session started, or evicted from the bound, cost one
// primary-key read the first time they are asked about and are cached
// from then on. Only tool_call rows are cached: they are the only rows a
// parent link may point at, and a non-tool_call answer ends the walk that
// asked.
//
// A conversation cut deletes rows whose links may be cached;
// ForgetToolCallLinks drops the thread's links so a later event cannot
// place a row at a deleted scope's turn.

// maxToolCallLinksPerThread bounds the links one thread holds. An entry
// costs about 150 bytes (two id strings, a turn index, map overhead), so
// the bound is about 300 KiB for a thread at the limit. The rows asked
// about are the scopes that live rows attach to: agent launches and the
// launch chain above them. Those stay resident because a hit marks the
// entry referenced and eviction skips referenced entries once (a clock),
// so a session with 100 concurrent agents keeps every launch cached while
// thousands of leaf tool calls cycle through the rest of the ring.
const maxToolCallLinksPerThread = 2048

// toolCallLink is one cached tool_call row's placement, and whether it
// anchors a subagent card (store.SubagentAnchorable).
type toolCallLink struct {
	parentID   string
	turnIndex  int
	anchorable bool
	referenced bool
}

// toolCallLinks is a bounded map with clock eviction. Guarded by r.mu
// through its owning threadState.
type toolCallLinks struct {
	byID map[string]*toolCallLink
	ring []string
	hand int
}

func (l *toolCallLinks) get(id string) (toolCallLink, bool) {
	link := l.byID[id]
	if link == nil {
		return toolCallLink{}, false
	}
	link.referenced = true
	return *link, true
}

func (l *toolCallLinks) put(id, parentID string, turnIndex int, anchorable bool) {
	if link := l.byID[id]; link != nil {
		link.parentID = parentID
		link.turnIndex = turnIndex
		link.anchorable = anchorable
		link.referenced = true
		return
	}
	if l.byID == nil {
		l.byID = make(map[string]*toolCallLink)
	}
	link := &toolCallLink{parentID: parentID, turnIndex: turnIndex, anchorable: anchorable}
	if len(l.ring) < maxToolCallLinksPerThread {
		l.ring = append(l.ring, id)
		l.byID[id] = link
		return
	}
	// Two passes at most: the first clears every referenced bit it
	// passes, so the second finds an unreferenced slot.
	for {
		victim := l.byID[l.ring[l.hand]]
		if victim != nil && victim.referenced {
			victim.referenced = false
			l.hand = (l.hand + 1) % len(l.ring)
			continue
		}
		delete(l.byID, l.ring[l.hand])
		l.ring[l.hand] = id
		l.byID[id] = link
		l.hand = (l.hand + 1) % len(l.ring)
		return
	}
}

// itemLink is the placement of any row a link lookup found: a cached
// tool_call, or the row the fallback read returned.
type itemLink struct {
	kind       string
	parentID   string
	turnIndex  int
	anchorable bool
}

// noteToolCallLink caches a just-persisted row's placement. Rows that are
// not tool calls are not cached. A stopped thread is not given state
// back: a write after teardown (a host-synthesized settle) would
// otherwise leave an entry nothing sweeps.
func (r *Router) noteToolCallLink(item store.Item) {
	if item.Kind != itemKindToolCall || item.ThreadID == "" || item.ID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id := r.identityIfPresent(item.ThreadID); id != nil && id.stopped {
		return
	}
	r.state(item.ThreadID).toolCalls.put(item.ID, item.ParentID, item.TurnIndex, store.SubagentAnchorable(item.Kind, item.ToolName))
}

// lookupItemLink answers where a row sits and what kind it is, from the
// cache when the session wrote or already asked about it, otherwise from
// one primary-key read whose tool_call answer is cached.
func (r *Router) lookupItemLink(threadID, itemID string) (itemLink, bool, error) {
	r.mu.Lock()
	var link toolCallLink
	cached := false
	if st := r.threadStateIfPresent(threadID); st != nil {
		link, cached = st.toolCalls.get(itemID)
	}
	r.mu.Unlock()
	if cached {
		return itemLink{kind: itemKindToolCall, parentID: link.parentID, turnIndex: link.turnIndex, anchorable: link.anchorable}, true, nil
	}
	row, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil || !found {
		return itemLink{}, found, err
	}
	r.noteToolCallLink(row)
	return itemLink{kind: row.Kind, parentID: row.ParentID, turnIndex: row.TurnIndex,
		anchorable: store.SubagentAnchorable(row.Kind, row.ToolName)}, true, nil
}

// subagentAnchorFor names the anchor a row under parentID claims on its
// store write (store.Item.SubagentAnchor): the parent itself when it is a
// stored row that anchors a card. The store then keeps the parent chain's
// stamps with keyed writes. A parent not yet stored, a row that names
// itself, or a failed lookup name none: the store marks the chain and
// recomputes it, which serves the same cards at a higher cost.
func (r *Router) subagentAnchorFor(threadID, itemID, parentID string) string {
	if parentID == "" || parentID == itemID {
		return ""
	}
	link, found, err := r.lookupItemLink(threadID, parentID)
	if err != nil || !found || !link.anchorable {
		return ""
	}
	return parentID
}

// ForgetToolCallLinks drops the thread's cached links. The app calls it
// after a conversation cut deletes rows while the session stays live
// (Codex thread/revert, the claude-tui native revert): a deleted scope's
// cached turn would otherwise place a late event in a turn index the next
// turn reuses.
func (r *Router) ForgetToolCallLinks(threadID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.threadStateIfPresent(threadID); st != nil {
		st.toolCalls = toolCallLinks{}
		st.firstChildProbed = nil
	}
}

// ToolCallLinkCountForTest reports how many links the thread caches.
// Exported for the conversation-cut tests in `internal/app`.
func (r *Router) ToolCallLinkCountForTest(threadID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.threadStateIfPresent(threadID); st != nil {
		return len(st.toolCalls.byID)
	}
	return 0
}

// shouldDropParentID decides whether an item's parent_id should be
// dropped before persistence. The spec invariant is that parent_id
// ultimately points to a tool_call row, but text/thinking deltas from
// a subagent can arrive before the parent Task tool_call is persisted,
// so a missing parent is NOT grounds for dropping the link. Instead
// we guard against three real corruption patterns:
//
//  1. Self-reference (parent_id == item.id).
//  2. A cycle discovered by walking existing parent_id links back to
//     the same row.
//  3. A parent row that EXISTS but is not a tool_call (the invariant
//     violation the spec actually cares about: a text item attached
//     to another text item).
//
// The walk reads links through lookupItemLink, so a chain the session
// wrote costs no store read.
//
// Returns (true, reason) on drop; (false, "") when the link is either
// valid or refers to a yet-unseen row that may arrive later. Lookup
// failures downgrade to (false, ""): a transient store error never
// blocks persistence.
func (r *Router) shouldDropParentID(threadID, itemID, parentID string) (bool, string) {
	if parentID == itemID {
		return true, "self reference"
	}
	var seenBuf [4]string
	seen := append(seenBuf[:0], itemID)
	current := parentID
	for hops := 0; hops < 16; hops++ {
		for _, id := range seen {
			if id == current {
				return true, "cycle detected"
			}
		}
		seen = append(seen, current)
		parent, found, err := r.lookupItemLink(threadID, current)
		if err != nil {
			// Transient lookup error: keep the link, the store write
			// will surface any hard error.
			return false, ""
		}
		if !found {
			// Parent hasn't been persisted yet (common for subagent
			// text deltas arriving before the Task tool_call row).
			// Leave the link: the row may materialise shortly.
			return false, ""
		}
		if parent.kind != itemKindToolCall {
			return true, fmt.Sprintf("parent kind %q is not tool_call", parent.kind)
		}
		if parent.parentID == "" {
			return false, ""
		}
		current = parent.parentID
	}
	return true, "parent chain too deep"
}
