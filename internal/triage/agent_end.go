package triage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"agent-overflow/internal/store"
)

// Agent-owned rows (docs/architecture/turn-lifecycle.md, §Agent-owned
// rows). A background agent owns every row under its transcript root,
// whichever turn wrote it. A turn's end, a Stop and the crash sweep settle
// only the rows no agent owns; the agent's end settles its own: its
// completion sibling is written with store.UpsertAgentEnd, which settles
// every row the agent left open in the same transaction.

// agentEnd is what persisting an agent's completion sibling does to the
// rows the agent left open.
type agentEnd struct {
	// rootID is the agent's transcript root, where every round's rows
	// are parented.
	rootID string
	// streamStatus is what a stream the router still holds settles to.
	streamStatus string
	rule         store.AgentEndRule
}

// newAgentEnd is the end an agent's completion status makes. A completed
// agent's open text completes and a tool call it left running reads as a
// turn's unresolved tool does. A stopped agent's rows read "stopped"; an
// agent that failed, or died with its session, leaves them "interrupted",
// as a crashed turn does.
func newAgentEnd(rootID, status, source string) agentEnd {
	switch {
	case status == statusCompleted:
		return agentEnd{rootID: rootID, streamStatus: statusCompleted,
			rule: store.AgentEndRule{StreamingCompletes: true, Summarise: ForceCloseSummary}}
	case status == statusKilled && source != "session_died":
		return agentEnd{rootID: rootID, streamStatus: statusErrored,
			rule: store.AgentEndRule{Summarise: stoppedSummary}}
	default:
		return agentEnd{rootID: rootID, streamStatus: statusErrored,
			rule: store.AgentEndRule{Summarise: interruptedSummary}}
	}
}

// persistAgentEndLocked writes an agent's completion sibling and ends the
// agent; the caller holds the thread's drain lock. The streams the router
// still holds for the agent's rows settle first, the way a turn's end
// settles its own; the rows queued behind them persist next, since they
// precede the end; then the sibling is written with every row still open
// under the agent settled in its transaction. A failed stream settle does
// not hold the sibling back: the store settles that row with it.
func (r *Router) persistAgentEndLocked(item store.Item, payload *store.Payload, end agentEnd) error {
	threadID := item.ThreadID
	streamErr := r.settleAgentStreams(threadID, end)
	queueErr := r.drainQueueLocked(threadID, idleScopeRow)

	if item.ParentID != "" {
		if dropped, reason := r.shouldDropParentID(threadID, item.ID, item.ParentID); dropped {
			log.Printf("triage: dropping parent_id %q on item %s: %s", item.ParentID, item.ID, reason)
			item.ParentID = ""
		}
	}
	persisted, settled, err := r.store.UpsertAgentEnd(item, payload, end.rootID, end.rule, time.Now().UnixMilli())
	if err != nil {
		return errors.Join(streamErr, queueErr, fmt.Errorf("end agent %s/%s: %w", threadID, end.rootID, err))
	}
	rows := make([]store.Item, 0, 1+len(settled))
	rows = append(rows, persisted)
	for _, row := range settled {
		rows = append(rows, row.Item)
	}
	r.emitItemUpserts(threadID, rows)
	for _, row := range rows {
		r.metrics.ItemsPersisted.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("kind", row.Kind)))
	}
	if payload != nil {
		r.metrics.PayloadsPersisted.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("kind", payload.Kind)))
	}
	return errors.Join(streamErr, queueErr)
}

// settleAgentStreams settles the streams the router holds for the rows
// end's agent owns, through the same row settle a turn's end uses, and
// releases their counts without draining: the caller drains. A thread
// with no open stream asks the store nothing.
func (r *Router) settleAgentStreams(threadID string, end agentEnd) error {
	if !r.hasActiveStreamingItem(threadID) {
		return nil
	}
	streams, err := r.store.AgentStreams(threadID, end.rootID)
	if err != nil {
		return err
	}
	var errs []error
	for _, stream := range streams {
		ref, held := r.takeAgentStream(threadID, stream)
		if !held {
			continue
		}
		switch stream.Kind {
		case itemKindThinking:
			err = r.settleStreamingThinkingRow(threadID, stream.ID, end.streamStatus, end.rule.Summarise, "", false)
		default:
			err = r.settleStreamingTextRow(threadID, stream.ID, end.streamStatus, end.rule.Summarise, "", false, nil)
		}
		r.mu.Lock()
		r.decStreamingCounts(threadID, ref.scope)
		r.mu.Unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("settle agent stream %s/%s: %w", threadID, stream.ID, err))
		}
	}
	return errors.Join(errs...)
}

// takeAgentStream takes the live stream that writes stream's row, if the
// router holds it: the entry its key names, when that entry writes this
// row.
func (r *Router) takeAgentStream(threadID string, stream store.AgentStream) (activeStreamBlock, bool) {
	key := activeStreamKey(stream.TurnIndex, stream.ParentID, stream.ProviderItemID)
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return activeStreamBlock{}, false
	}
	active, refs := st.activeTextBlocks, st.activeTextBlockRefs
	if stream.Kind == itemKindThinking {
		active, refs = st.activeThinkingBlocks, st.activeThinkingBlockRefs
	}
	ref, ok := refs[key]
	if !ok || !active[key] || ref.itemID != stream.ID {
		return activeStreamBlock{}, false
	}
	delete(active, key)
	delete(refs, key)
	return ref, true
}

// agentOwnedStreamScopes reports which scopes of the thread's open streams
// and queued rows an agent owns, for a turn boundary to leave alone. A
// thread whose streams and rows are all top-level asks the store nothing.
func (r *Router) agentOwnedStreamScopes(threadID string) (map[string]bool, error) {
	r.mu.Lock()
	var scopes []string
	if st := r.threadStateIfPresent(threadID); st != nil {
		seen := make(map[string]bool)
		add := func(scope string) {
			if scope != "" && !seen[scope] {
				seen[scope] = true
				scopes = append(scopes, scope)
			}
		}
		for key, ref := range st.activeTextBlockRefs {
			if st.activeTextBlocks[key] {
				add(ref.scope)
			}
		}
		for key, ref := range st.activeThinkingBlockRefs {
			if st.activeThinkingBlocks[key] {
				add(ref.scope)
			}
		}
		for _, queued := range st.interruptQueue {
			add(queued.item.ParentID)
		}
	}
	r.mu.Unlock()
	if len(scopes) == 0 {
		return nil, nil
	}
	owned, err := r.store.AgentOwnedScopes(threadID, scopes)
	if err != nil {
		return nil, fmt.Errorf("agent-owned stream scopes of %s: %w", threadID, err)
	}
	return owned, nil
}

// dropTurnStreamsLocked forgets the open streams a turn boundary ends
// without settling, releasing each one's count: those of turnIndex, or of
// every turn when allTurns is set, except the ones an agent owns. Caller
// holds r.mu.
func (r *Router) dropTurnStreamsLocked(threadID string, st *threadState, turnIndex int, allTurns bool, agentScopes map[string]bool) {
	drop := func(active map[string]bool, refs map[string]activeStreamBlock) {
		for key, ref := range refs {
			if (!allTurns && ref.turnIndex != turnIndex) || agentScopes[ref.scope] {
				continue
			}
			if active[key] {
				r.decStreamingCounts(threadID, ref.scope)
			}
			delete(active, key)
			delete(refs, key)
			delete(st.streamingPathRefsLast, ref.itemID)
		}
	}
	drop(st.activeTextBlocks, st.activeTextBlockRefs)
	drop(st.activeThinkingBlocks, st.activeThinkingBlockRefs)
}
