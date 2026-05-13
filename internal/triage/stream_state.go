package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/pathlinks"
	"agent-overflow/internal/store"
)

type queuedPersistence struct {
	item    store.Item
	payload *store.Payload
}

func (r *Router) ensureTextBlockStarted(threadID string, turnIndex int, scope string) bool {
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeTextBlocks[key] {
		return false
	}
	r.segmentIndexByScope[key] = r.segmentIndexByScope[key] + 1
	r.activeTextBlocks[key] = true
	r.streamingItemCounts[threadID] = r.streamingItemCounts[threadID] + 1
	return true
}

func (r *Router) ensureThinkingBlockStarted(threadID string, turnIndex int, scope string) bool {
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeThinkingBlocks[key] {
		return false
	}
	r.blockIndexByScope[key] = r.blockIndexByScope[key] + 1
	r.activeThinkingBlocks[key] = true
	r.streamingItemCounts[threadID] = r.streamingItemCounts[threadID] + 1
	return true
}

func (r *Router) hasActiveStreamingItem(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streamingItemCounts[threadID] > 0
}

func (r *Router) maybeDeferOrPersist(threadID string, item store.Item, payload *store.Payload) error {
	if !r.hasActiveStreamingItem(threadID) {
		return r.persistItem(item, payload)
	}

	r.mu.Lock()
	r.interruptQueue[threadID] = append(r.interruptQueue[threadID], queuedPersistence{
		item:    item,
		payload: payload,
	})
	r.mu.Unlock()
	return nil
}

func (r *Router) drainInterruptQueueIfIdle(threadID string) {
	if r.hasActiveStreamingItem(threadID) {
		return
	}
	if err := r.drainInterruptQueue(threadID, false); err != nil {
		// drainInterruptQueue logs per-item failures itself; the idle
		// drain has no upstream caller to surface the aggregate to, so
		// swallow it here.
		return
	}
}

// drainInterruptQueue persists every queued item for the thread. The
// queue is handed off before iteration (cleared from the map under the
// lock), so an early return on persist failure would silently strand
// the remaining items. We log each failure and return the first error
// once the full queue has been attempted.
func (r *Router) drainInterruptQueue(threadID string, forceErrored bool) error {
	r.mu.Lock()
	queue := r.interruptQueue[threadID]
	delete(r.interruptQueue, threadID)
	r.mu.Unlock()

	var firstErr error
	for _, queued := range queue {
		item := queued.item
		if forceErrored {
			item.Status = statusErrored
			item.Summary = interruptedSummary(item.Summary)
			item.UpdatedAt = time.Now().UnixMilli()
		}
		if err := r.persistItem(item, queued.payload); err != nil {
			log.Printf("triage: drain persist failed for item %s on thread %s: %v", item.ID, threadID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// settleTurnStreaming collects every active streaming scope in
// (threadID, turnIndex) and calls settleStreamingText /
// settleStreamingThinking on each. The prefix scan runs against the
// full activeTextBlocks / activeThinkingBlocks maps, but both maps are
// pruned aggressively — entries clear on content_block_stop (typical
// Claude path), clearOpenTurn (turn completion), and CleanupThread
// (session teardown). In steady state the map holds ~1-5 entries for
// the current in-flight turn on active threads, so a flat prefix scan
// beats the bookkeeping overhead of a nested map[threadIDTurn]map[scope]bool
// rekey. Revisit if profiling shows this becoming a hotspot.
func (r *Router) settleTurnStreaming(threadID string, turnIndex int, status string) error {
	prefix := fmt.Sprintf("%s|%d|", threadID, turnIndex)

	r.mu.Lock()
	textScopes := make([]string, 0)
	for key, active := range r.activeTextBlocks {
		if active && strings.HasPrefix(key, prefix) {
			textScopes = append(textScopes, strings.TrimPrefix(key, prefix))
		}
	}
	thinkingScopes := make([]string, 0)
	for key, active := range r.activeThinkingBlocks {
		if active && strings.HasPrefix(key, prefix) {
			thinkingScopes = append(thinkingScopes, strings.TrimPrefix(key, prefix))
		}
	}
	r.mu.Unlock()

	for _, scope := range textScopes {
		if err := r.settleStreamingText(threadID, turnIndex, scope, status); err != nil {
			return err
		}
	}
	for _, scope := range thinkingScopes {
		if err := r.settleStreamingThinking(threadID, turnIndex, scope, status); err != nil {
			return err
		}
	}
	return nil
}

func interruptedSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "Interrupted"
	}
	const suffix = " — interrupted"
	if strings.HasSuffix(summary, suffix) {
		return summary
	}
	return summary + suffix
}

func (r *Router) settleStreamingText(threadID string, turnIndex int, scope string, status string) error {
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	active := r.activeTextBlocks[key]
	index := r.segmentIndexByScope[key]
	if active {
		delete(r.activeTextBlocks, key)
		if count := r.streamingItemCounts[threadID]; count > 0 {
			r.streamingItemCounts[threadID] = count - 1
		}
	}
	r.mu.Unlock()
	if !active {
		return nil
	}

	// Defer the drain unconditionally once the counter has been
	// decremented: every early return below (row missing, row already
	// non-streaming) still represents a streaming slot that just
	// closed. Without this, a late settle that sees a non-streaming
	// row would leak whatever was queued behind the streaming lock.
	defer r.drainInterruptQueueIfIdle(threadID)

	itemID := textItemID(turnIndex, scope, index)
	if err := r.flushStreamingItem(threadID, itemID); err != nil {
		return err
	}
	item, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil || !found {
		return err
	}
	if item.Status != statusStreaming {
		return nil
	}
	item.Status = status
	if status == statusErrored {
		item.Summary = interruptedSummary(item.Summary)
	}
	item.UpdatedAt = time.Now().UnixMilli()
	// Stamp the Go-validated path allowlist onto item.Meta before
	// persisting (see enrichPathRefs).
	r.enrichPathRefs(threadID, &item)
	return r.persistItem(item, nil)
}

// enrichPathRefs is the settle-time hook that validates path-shaped
// tokens in the item's summary against the workspace filesystem and
// stores the resulting allowlist on item.Meta. Only assistant_text
// rows are enriched today; user_text and thinking rows render
// through plain-text components and don't consume the allowlist.
//
// Failures are non-fatal: a missing thread record or a malformed
// existing meta JSON falls back to skipping enrichment so the
// settle path stays robust under partial state. The cost (Go regex
// + os.Stat × unique paths) is sub-frame for realistic messages —
// see internal/pathlinks/AGENTS.md for measured ranges.
func (r *Router) enrichPathRefs(threadID string, item *store.Item) {
	if item.Kind != itemKindAssistantText {
		return
	}
	if item.Summary == "" {
		return
	}
	workspacePath := r.workspacePathFor(threadID)
	if workspacePath == "" {
		return
	}
	refs := pathlinks.ExtractAndValidate(workspacePath, item.Summary)
	if len(refs) == 0 {
		return
	}
	merged, err := mergePathRefsIntoMeta(item.Meta, refs)
	if err != nil {
		log.Printf("triage: pathlinks merge meta for %s: %v", item.ID, err)
		return
	}
	item.Meta = merged
}

// workspacePathFor returns the WorkspacePath for threadID, using a
// per-router cache so repeat callers in a single turn (e.g. several
// assistant_text settles back-to-back) don't each pay a SQLite read
// for the same effectively-immutable value. Misses log and return
// empty so the caller skips enrichment cleanly. Eviction is tied to
// CleanupThread (the only point at which the workspace association
// goes away from the router's perspective).
func (r *Router) workspacePathFor(threadID string) string {
	r.mu.Lock()
	cached, ok := r.workspacePathByThread[threadID]
	r.mu.Unlock()
	if ok {
		return cached
	}
	thread, err := r.store.GetThread(threadID)
	if err != nil {
		log.Printf("triage: pathlinks lookup thread %s: %v", threadID, err)
		return ""
	}
	r.mu.Lock()
	r.workspacePathByThread[threadID] = thread.WorkspacePath
	r.mu.Unlock()
	return thread.WorkspacePath
}

// mergePathRefsIntoMeta returns the item.Meta JSON string with a
// `pathRefs` key set to the supplied slice, preserving any other
// existing top-level keys (e.g. `task_id` — its partial index in
// migration v17 must keep working). An empty / whitespace-only input
// is treated as `{}`. Existing values that aren't JSON objects are
// replaced — that's a corruption state the store shouldn't produce,
// but the helper degrades cleanly rather than refusing to persist.
//
// A separate helper exists (rather than reusing the deep-merge
// `mergeItemMetaJSON` patterns in tool_lifecycle.go) because the
// `pathRefs` key needs whole-value overwrite on every settle —
// re-enriching must replace, not append. Deep merge would append.
func mergePathRefsIntoMeta(meta string, refs []pathlinks.PathRef) (string, error) {
	// Fast path: empty / whitespace / `{}` meta. The hot case at
	// settle time — pre-pathlinks items rarely carry sibling meta —
	// skips both the unmarshal and the map allocation.
	trimmed := strings.TrimSpace(meta)
	if trimmed == "" || trimmed == "{}" {
		return marshalPathRefsOnly(refs)
	}
	obj := map[string]json.RawMessage{}
	// json.Unmarshal into the map preserves the raw bytes of
	// non-conflicting siblings so they round-trip untouched.
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		// Not a JSON object — overwrite. Log so corruption is visible.
		log.Printf("triage: pathlinks merge skipped corrupt meta: %v", err)
		return marshalPathRefsOnly(refs)
	}
	encoded, err := json.Marshal(refs)
	if err != nil {
		return "", err
	}
	obj["pathRefs"] = encoded
	out, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func marshalPathRefsOnly(refs []pathlinks.PathRef) (string, error) {
	out, err := json.Marshal(struct {
		PathRefs []pathlinks.PathRef `json:"pathRefs"`
	}{PathRefs: refs})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (r *Router) settleStreamingThinking(threadID string, turnIndex int, scope string, status string) error {
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	active := r.activeThinkingBlocks[key]
	index := r.blockIndexByScope[key]
	if active {
		delete(r.activeThinkingBlocks, key)
		if count := r.streamingItemCounts[threadID]; count > 0 {
			r.streamingItemCounts[threadID] = count - 1
		}
	}
	r.mu.Unlock()
	if !active {
		return nil
	}

	// Same invariant as settleStreamingText: once we've decremented the
	// counter, the drain must fire regardless of what we find in the
	// store. A lookup miss or an already-non-streaming row would
	// otherwise leak whatever sat behind the streaming lock.
	defer r.drainInterruptQueueIfIdle(threadID)

	itemID := thinkingItemID(turnIndex, scope, index)
	if err := r.flushStreamingItem(threadID, itemID); err != nil {
		return err
	}
	item, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil || !found {
		return err
	}
	if item.Status != statusStreaming {
		return nil
	}
	item.Status = status
	item.UpdatedAt = time.Now().UnixMilli()
	// `item.Summary` already reflects the running tail. AppendItemSummaryTail
	// keeps the last `thinkingPreviewRunes` characters in place on every
	// streaming flush, and the live pane summary survives settle for
	// thinking rows (`applyLiveStateForUpsert`) so the on-screen view
	// doesn't shrink. No payload re-read on the hot path — that would
	// block the next provider event (a tool_use or text_delta following
	// thinking) and produce the perceived end-of-thinking freeze.
	if status == statusErrored {
		item.Summary = interruptedSummary(item.Summary)
	}
	return r.persistItem(item, nil)
}

func nextErrorID(turnIndex int, scope string, seq int) string {
	if scope == "" {
		return fmt.Sprintf("error:%d:%d", turnIndex, seq)
	}
	return fmt.Sprintf("error:%d:%s:%d", turnIndex, scope, seq)
}

func (r *Router) nextErrorSequence(threadID string, turnIndex int, scope string) int {
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	seq := r.errorSeqByScope[key]
	r.errorSeqByScope[key] = seq + 1
	return seq
}

func nextCompactionID(turnIndex int) string {
	return fmt.Sprintf("compact:%d", turnIndex)
}

func nextToolCompletionID(launchID string) string {
	return "complete:" + launchID
}

// backgroundCompletionID returns the stable id for a backgrounded
// task's tool_completion sibling. Steady-state callers must resolve a
// launch tool_use_id before writing a sibling. The task_id fallback is
// defensive only, and keeps any accidental no-launch id distinct from a
// future real tool_use id.
func backgroundCompletionID(launchID, taskID string) string {
	if launchID != "" {
		return "complete:" + launchID
	}
	return "complete:by-task:" + taskID
}
