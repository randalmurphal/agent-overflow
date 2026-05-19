package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/pathlinks"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

type queuedPersistence struct {
	item    store.Item
	payload *store.Payload
}

type activeStreamBlock struct {
	threadID  string
	turnIndex int
	scope     string
	itemID    string
}

func activeStreamKey(threadID string, turnIndex int, scope, providerItemID string) string {
	if providerItemID == "" {
		return scopeCounterKey(threadID, turnIndex, scope)
	}
	return fmt.Sprintf("%s|%d|%s|provider:%s", threadID, turnIndex, scope, providerItemID)
}

func (r *Router) ensureTextBlockStarted(threadID string, turnIndex int, scope, providerItemID string) (bool, string) {
	key := activeStreamKey(threadID, turnIndex, scope, providerItemID)
	counterKey := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeTextBlocks[key] {
		return false, r.activeTextBlockRefs[key].itemID
	}
	r.segmentIndexByScope[counterKey] = r.segmentIndexByScope[counterKey] + 1
	itemID := textItemID(turnIndex, scope, r.segmentIndexByScope[counterKey])
	r.activeTextBlocks[key] = true
	r.activeTextBlockRefs[key] = activeStreamBlock{
		threadID:  threadID,
		turnIndex: turnIndex,
		scope:     scope,
		itemID:    itemID,
	}
	r.streamingItemCounts[threadID] = r.streamingItemCounts[threadID] + 1
	return true, itemID
}

func (r *Router) ensureThinkingBlockStarted(threadID string, turnIndex int, scope, providerItemID string) (bool, string) {
	key := activeStreamKey(threadID, turnIndex, scope, providerItemID)
	counterKey := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeThinkingBlocks[key] {
		return false, r.activeThinkingBlockRefs[key].itemID
	}
	r.blockIndexByScope[counterKey] = r.blockIndexByScope[counterKey] + 1
	itemID := thinkingItemID(turnIndex, scope, r.blockIndexByScope[counterKey])
	r.activeThinkingBlocks[key] = true
	r.activeThinkingBlockRefs[key] = activeStreamBlock{
		threadID:  threadID,
		turnIndex: turnIndex,
		scope:     scope,
		itemID:    itemID,
	}
	r.streamingItemCounts[threadID] = r.streamingItemCounts[threadID] + 1
	return true, itemID
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

	var firstErrr error
	for _, queued := range queue {
		item := queued.item
		if forceErrored {
			item.Status = statusErrored
			item.Summary = interruptedSummary(item.Summary)
			item.UpdatedAt = time.Now().UnixMilli()
		}
		if err := r.persistItem(item, queued.payload); err != nil {
			log.Printf("triage: drain persist failed for item %s on thread %s: %v", item.ID, threadID, err)
			if firstErrr == nil {
				firstErrr = err
			}
		}
	}
	return firstErrr
}

// settleTurnStreaming collects every active streaming scope in
// (threadID, turnIndex) and settles them in parallel before returning.
// Used by handleTurnComplete; the synchronous wait barrier ensures the
// turns row UPDATE (which follows this call) sequences after every
// streaming-item commit on the same logical turn.
//
// The scan runs against activeTextBlockRefs / activeThinkingBlockRefs so
// provider-keyed streams and legacy scope-keyed streams share the same
// settle path. Those maps are pruned aggressively — entries clear on
// content_block_stop (typical provider path), clearOpenTurn (turn
// completion), and CleanupThread (session teardown). In steady state they
// hold ~1-5 entries for the current in-flight turn on active threads, so a
// flat scan beats the bookkeeping overhead of a nested
// map[threadIDTurn]map[scope] structure.
//
// Per-scope goroutines parallelize the SQLite write work — on
// multi-scope turns (e.g. an interrupted turn with two text blocks
// and a thinking block all in flight) the total settle latency drops
// from O(N × per-block SQLite time) to ~O(per-block SQLite time).
// Each goroutine is tracked by BOTH the per-turn local WaitGroup
// (sequencing barrier for the caller) and r.settleWG (shutdown drain).
func (r *Router) settleTurnStreaming(threadID string, turnIndex int, status string) error {
	r.mu.Lock()
	textKeys := make([]string, 0)
	for key, ref := range r.activeTextBlockRefs {
		if ref.threadID == threadID && ref.turnIndex == turnIndex && r.activeTextBlocks[key] {
			textKeys = append(textKeys, key)
		}
	}
	thinkingKeys := make([]string, 0)
	for key, ref := range r.activeThinkingBlockRefs {
		if ref.threadID == threadID && ref.turnIndex == turnIndex && r.activeThinkingBlocks[key] {
			thinkingKeys = append(thinkingKeys, key)
		}
	}
	r.mu.Unlock()

	var (
		turnWG   sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	captureErr := func(err error) {
		if err != nil {
			errOnce.Do(func() { firstErr = err })
		}
	}

	for _, key := range textKeys {
		ref, active := r.takeActiveTextBlockByKey(key)
		if !active {
			continue
		}
		turnWG.Add(1)
		r.settleWG.Add(1)
		go func(itemID string) {
			defer turnWG.Done()
			defer r.settleWG.Done()
			captureErr(r.doSettleStreamingText(threadID, itemID, status, "", false))
		}(ref.itemID)
	}
	for _, key := range thinkingKeys {
		ref, active := r.takeActiveThinkingBlockByKey(key)
		if !active {
			continue
		}
		turnWG.Add(1)
		r.settleWG.Add(1)
		go func(itemID string) {
			defer turnWG.Done()
			defer r.settleWG.Done()
			captureErr(r.doSettleStreamingThinking(threadID, itemID, status, "", false))
		}(ref.itemID)
	}
	turnWG.Wait()
	return firstErr
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

// takeActiveTextBlock is the synchronous prelude shared by the sync
// and async settleStreamingText variants. It checks and clears the
// activeTextBlocks slot atomically so a duplicate settle attempt
// no-ops correctly. The streamingItemCounts decrement is DEFERRED to
// finishSettle (which runs after persistItem completes in the heavy
// body) so hasActiveStreamingItem stays true across the settle —
// maybeDeferOrPersist therefore continues to queue incoming
// non-streaming rows while the settle is mid-flight.
//
// Returns (itemID, true) when the caller should proceed to the heavy
// body; (_, false) when another caller already settled this block.
func (r *Router) takeActiveTextBlock(threadID string, turnIndex int, scope, providerItemID string) (itemID string, active bool) {
	key := activeStreamKey(threadID, turnIndex, scope, providerItemID)
	ref, active := r.takeActiveTextBlockByKey(key)
	if active {
		return ref.itemID, true
	}
	if providerItemID != "" {
		return "", false
	}
	return r.takeFirstActiveTextBlock(threadID, turnIndex, scope)
}

func (r *Router) takeActiveTextBlockByKey(key string) (activeStreamBlock, bool) {
	r.mu.Lock()
	active := r.activeTextBlocks[key]
	ref := r.activeTextBlockRefs[key]
	if active {
		delete(r.activeTextBlocks, key)
		delete(r.activeTextBlockRefs, key)
	}
	r.mu.Unlock()
	if !active {
		return activeStreamBlock{}, false
	}
	return ref, true
}

func (r *Router) takeFirstActiveTextBlock(threadID string, turnIndex int, scope string) (itemID string, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, ref := range r.activeTextBlockRefs {
		if ref.threadID == threadID && ref.turnIndex == turnIndex && ref.scope == scope && r.activeTextBlocks[key] {
			delete(r.activeTextBlocks, key)
			delete(r.activeTextBlockRefs, key)
			return ref.itemID, true
		}
	}
	return "", false
}

// finishSettle decrements streamingItemCounts and drains the interrupt
// queue if the thread has no remaining streaming rows. Runs after the
// SQLite write completes (or after a lookup miss / non-streaming row
// short-circuit) so the count's "0 → drain" transition is durable.
// Safe to call from any goroutine.
func (r *Router) finishSettle(threadID string) {
	r.mu.Lock()
	if count := r.streamingItemCounts[threadID]; count > 0 {
		r.streamingItemCounts[threadID] = count - 1
	}
	r.mu.Unlock()
	r.drainInterruptQueueIfIdle(threadID)
}

// doSettleStreamingText is the heavy body of the text-block settle:
// flush the stream-persist buffer, re-read the item from SQLite,
// stamp final status + pathRefs, persist, then run finishSettle.
// Called by both the sync wrapper (settleStreamingText, used inside
// settleTurnStreaming) and the async wrapper
// (settleStreamingTextAsync, used at content-block-stop on the
// provider read-loop). Safe to run in any goroutine.
func (r *Router) doSettleStreamingText(threadID, itemID, status, finalContent string, finalContentPresent bool) error {
	// finishSettle MUST fire whether we persist successfully, find no
	// row, or find a row that already settled. Each of those still
	// represents a streaming slot that just closed; without the drain,
	// queued non-streaming rows behind the lock would leak.
	defer r.finishSettle(threadID)

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
	if finalContentPresent {
		item.Summary = finalContent
	}
	if status == statusErrored {
		item.Summary = interruptedSummary(item.Summary)
	}
	item.UpdatedAt = time.Now().UnixMilli()
	// Stamp the Go-validated path allowlist onto item.Meta before
	// persisting (see enrichPathRefs).
	r.enrichPathRefs(threadID, &item)
	return r.persistItem(item, nil)
}

// settleStreamingText is the synchronous text-block settle. Used by
// settleTurnStreaming's per-scope goroutines (where the per-turn
// WaitGroup guarantees turn-row commit sequencing after all streaming
// commits) and by tests that need a deterministic post-settle barrier
// without an extra WaitForPendingSettles call.
func (r *Router) settleStreamingText(threadID string, turnIndex int, scope string, status string) error {
	itemID, active := r.takeActiveTextBlock(threadID, turnIndex, scope, "")
	if !active {
		return nil
	}
	return r.doSettleStreamingText(threadID, itemID, status, "", false)
}

// settleStreamingTextAsync is the fire-and-forget text-block settle.
// Used by content-block-stop and stream-items lifecycle on the
// provider read-loop, where blocking on SQLite would stall the next
// provider event (the freeze hot path). The sync prelude runs in the
// calling goroutine so the activeTextBlocks slot is cleared
// immediately (duplicate settle calls no-op without spawning a second
// goroutine). The heavy body runs on a goroutine tracked by
// r.settleWG so app shutdown can drain.
func (r *Router) settleStreamingTextAsync(threadID string, turnIndex int, scope, providerItemID, status, finalContent string, finalContentPresent bool) {
	itemID, active := r.takeActiveTextBlock(threadID, turnIndex, scope, providerItemID)
	if !active {
		if finalContentPresent && finalContent != "" {
			if err := r.persistCompletedTextItem(threadID, turnIndex, scope, finalContent); err != nil {
				log.Printf("triage: persist completed text %s/%s: %v", threadID, providerItemID, err)
			}
		}
		return
	}
	r.settleWG.Add(1)
	go func() {
		defer r.settleWG.Done()
		if err := r.doSettleStreamingText(threadID, itemID, status, finalContent, finalContentPresent); err != nil {
			log.Printf("triage: async settle text %s/%s: %v", threadID, itemID, err)
		}
	}()
}

func (r *Router) settleStreamingTextScopeAsync(threadID string, turnIndex int, scope string, status string) {
	keys := r.activeTextKeysForScope(threadID, turnIndex, scope)
	for _, key := range keys {
		ref, active := r.takeActiveTextBlockByKey(key)
		if !active {
			continue
		}
		r.settleWG.Add(1)
		go func(itemID string) {
			defer r.settleWG.Done()
			if err := r.doSettleStreamingText(threadID, itemID, status, "", false); err != nil {
				log.Printf("triage: async settle text %s/%s: %v", threadID, itemID, err)
			}
		}(ref.itemID)
	}
}

func (r *Router) activeTextKeysForScope(threadID string, turnIndex int, scope string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]string, 0)
	for key, ref := range r.activeTextBlockRefs {
		if ref.threadID == threadID && ref.turnIndex == turnIndex && ref.scope == scope && r.activeTextBlocks[key] {
			keys = append(keys, key)
		}
	}
	return keys
}

func (r *Router) persistCompletedTextItem(threadID string, turnIndex int, scope, content string) error {
	itemID := r.nextTextItemID(threadID, turnIndex, scope)
	now := time.Now().UnixMilli()
	item := store.Item{
		ID:        itemID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      itemKindAssistantText,
		Role:      "assistant",
		Status:    statusCompleted,
		Summary:   content,
		CreatedAt: now,
		UpdatedAt: now,
	}
	r.enrichPathRefs(threadID, &item)
	return r.persistItem(item, nil)
}

func (r *Router) nextTextItemID(threadID string, turnIndex int, scope string) string {
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.segmentIndexByScope[key] = r.segmentIndexByScope[key] + 1
	return textItemID(turnIndex, scope, r.segmentIndexByScope[key])
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

// takeActiveThinkingBlock mirrors takeActiveTextBlock for the thinking
// block lifecycle. Same lock discipline: clear the slot synchronously,
// defer the count decrement to finishSettle so the streaming-active
// signal survives the async heavy body.
func (r *Router) takeActiveThinkingBlock(threadID string, turnIndex int, scope, providerItemID string) (itemID string, active bool) {
	key := activeStreamKey(threadID, turnIndex, scope, providerItemID)
	ref, active := r.takeActiveThinkingBlockByKey(key)
	if active {
		return ref.itemID, true
	}
	if providerItemID != "" {
		return "", false
	}
	return r.takeFirstActiveThinkingBlock(threadID, turnIndex, scope)
}

func (r *Router) takeActiveThinkingBlockByKey(key string) (activeStreamBlock, bool) {
	r.mu.Lock()
	active := r.activeThinkingBlocks[key]
	ref := r.activeThinkingBlockRefs[key]
	if active {
		delete(r.activeThinkingBlocks, key)
		delete(r.activeThinkingBlockRefs, key)
	}
	r.mu.Unlock()
	if !active {
		return activeStreamBlock{}, false
	}
	return ref, true
}

func (r *Router) takeFirstActiveThinkingBlock(threadID string, turnIndex int, scope string) (itemID string, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, ref := range r.activeThinkingBlockRefs {
		if ref.threadID == threadID && ref.turnIndex == turnIndex && ref.scope == scope && r.activeThinkingBlocks[key] {
			delete(r.activeThinkingBlocks, key)
			delete(r.activeThinkingBlockRefs, key)
			return ref.itemID, true
		}
	}
	return "", false
}

func (r *Router) activeThinkingItemID(threadID string, turnIndex int, scope, providerItemID string) (string, bool) {
	key := activeStreamKey(threadID, turnIndex, scope, providerItemID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if ref, ok := r.activeThinkingBlockRefs[key]; ok && r.activeThinkingBlocks[key] {
		return ref.itemID, true
	}
	if providerItemID != "" {
		return "", false
	}
	for key, ref := range r.activeThinkingBlockRefs {
		if ref.threadID == threadID && ref.turnIndex == turnIndex && ref.scope == scope && r.activeThinkingBlocks[key] {
			return ref.itemID, true
		}
	}
	return "", false
}

// doSettleStreamingThinking is the heavy body of the thinking-block
// settle: flush the stream-persist buffer, re-read, flip status,
// persist, finishSettle. Mirrors doSettleStreamingText shape so the
// sync wrapper and async wrapper share one body.
//
// `item.Summary` already reflects the running tail. AppendItemSummaryTail
// keeps the last `thinkingPreviewRunes` characters in place on every
// streaming flush; the frontend mirrors that cap in
// applyItemDelta so the settle is a visual no-op. No payload re-read
// on the hot path — that would block the next provider event (a
// tool_use or text_delta following thinking) and produce the
// perceived end-of-thinking freeze.
func (r *Router) doSettleStreamingThinking(threadID, itemID, status, finalContent string, finalContentPresent bool) error {
	defer r.finishSettle(threadID)

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
	if finalContentPresent {
		item.Summary = thinkingSummaryPreview(finalContent)
		if item.PayloadID != "" {
			if err := r.store.ReplacePayloadData(item.PayloadID, []byte(finalContent), item.PayloadMeta, item.UpdatedAt); err != nil {
				return fmt.Errorf("thinking final replace payload %s: %w", item.PayloadID, err)
			}
		}
	}
	if status == statusErrored {
		item.Summary = interruptedSummary(item.Summary)
	}
	return r.persistItem(item, nil)
}

func (r *Router) settleStreamingThinking(threadID string, turnIndex int, scope string, status string) error {
	itemID, active := r.takeActiveThinkingBlock(threadID, turnIndex, scope, "")
	if !active {
		return nil
	}
	return r.doSettleStreamingThinking(threadID, itemID, status, "", false)
}

func (r *Router) settleStreamingThinkingAsync(threadID string, turnIndex int, scope, providerItemID, status, finalContent string, finalContentPresent bool) {
	itemID, active := r.takeActiveThinkingBlock(threadID, turnIndex, scope, providerItemID)
	if !active {
		if finalContentPresent && finalContent != "" {
			if err := r.persistCompletedThinkingItem(threadID, turnIndex, scope, finalContent); err != nil {
				log.Printf("triage: persist completed thinking %s/%s: %v", threadID, providerItemID, err)
			}
		}
		return
	}
	r.settleWG.Add(1)
	go func() {
		defer r.settleWG.Done()
		if err := r.doSettleStreamingThinking(threadID, itemID, status, finalContent, finalContentPresent); err != nil {
			log.Printf("triage: async settle thinking %s/%s: %v", threadID, itemID, err)
		}
	}()
}

func (r *Router) settleStreamingThinkingScopeAsync(threadID string, turnIndex int, scope string, status string) {
	keys := r.activeThinkingKeysForScope(threadID, turnIndex, scope)
	for _, key := range keys {
		ref, active := r.takeActiveThinkingBlockByKey(key)
		if !active {
			continue
		}
		r.settleWG.Add(1)
		go func(itemID string) {
			defer r.settleWG.Done()
			if err := r.doSettleStreamingThinking(threadID, itemID, status, "", false); err != nil {
				log.Printf("triage: async settle thinking %s/%s: %v", threadID, itemID, err)
			}
		}(ref.itemID)
	}
}

func (r *Router) activeThinkingKeysForScope(threadID string, turnIndex int, scope string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]string, 0)
	for key, ref := range r.activeThinkingBlockRefs {
		if ref.threadID == threadID && ref.turnIndex == turnIndex && ref.scope == scope && r.activeThinkingBlocks[key] {
			keys = append(keys, key)
		}
	}
	return keys
}

func (r *Router) persistCompletedThinkingItem(threadID string, turnIndex int, scope, content string) error {
	itemID := r.nextThinkingItemID(threadID, turnIndex, scope)
	payloadID := "thinking:" + itemID
	now := time.Now().UnixMilli()
	item := store.Item{
		ID:        itemID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      itemKindThinking,
		Role:      "assistant",
		Status:    statusCompleted,
		Summary:   thinkingSummaryPreview(content),
		PayloadID: payloadID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	payload := store.Payload{
		ID:        payloadID,
		Kind:      itemKindThinking,
		Meta:      buildPayloadMeta(itemKindThinking, provider.ProviderEvent{ThreadID: threadID, Content: content, Timestamp: time.Now()}),
		Data:      []byte(content),
		CreatedAt: now,
	}
	return r.persistItem(item, &payload)
}

func (r *Router) nextThinkingItemID(threadID string, turnIndex int, scope string) string {
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blockIndexByScope[key] = r.blockIndexByScope[key] + 1
	return thinkingItemID(turnIndex, scope, r.blockIndexByScope[key])
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

func (r *Router) nextCompactionSequence(threadID string, turnIndex int) int {
	key := scopeCounterKey(threadID, turnIndex, "compaction")
	r.mu.Lock()
	defer r.mu.Unlock()
	seq := r.compactionSeqByScope[key]
	r.compactionSeqByScope[key] = seq + 1
	return seq
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
