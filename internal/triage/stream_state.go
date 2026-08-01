package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"slices"
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

func providerItemMeta(providerItemID string) string {
	providerItemID = strings.TrimSpace(providerItemID)
	if providerItemID == "" {
		return ""
	}
	out, err := json.Marshal(map[string]string{"provider_item_id": providerItemID})
	if err != nil {
		return ""
	}
	return string(out)
}

func withProviderItemMeta(existing string, providerItemID string) string {
	meta := providerItemMeta(providerItemID)
	if meta == "" {
		return existing
	}
	return mergeItemMetaJSON(existing, json.RawMessage(meta))
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
	r.incStreamingCounts(threadID, scope)
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
	r.incStreamingCounts(threadID, scope)
	return true, itemID
}

func (r *Router) hasActiveStreamingItem(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streamingItemCounts[threadID] > 0
}

// streamingScopeKey keys streamingScopeCounts by thread + scope. Scope is
// the item's ParentID: "" for the main loop, the Agent tool_use_id for a
// subagent. No turnIndex — only the open turn streams at any moment, and
// setOpenTurn clears the thread's scope counts at each turn boundary.
// threadID is a server-generated UUID, so the "|" separator is
// unambiguous (no scope/thread value contains it).
func streamingScopeKey(threadID, scope string) string {
	return threadID + "|" + scope
}

// incStreamingCounts bumps the thread-wide and per-scope streaming
// counters together; decStreamingCounts is the inverse. They MUST move in
// lockstep — the thread-wide counter gates the interrupt-queue DRAIN, the
// scoped counter gates the QUEUE decision (invariant 11) — so the two
// can't desync if a new streaming-block kind is added later. Both callers
// already hold r.mu.
func (r *Router) incStreamingCounts(threadID, scope string) {
	r.streamingItemCounts[threadID] = r.streamingItemCounts[threadID] + 1
	scopeKey := streamingScopeKey(threadID, scope)
	r.streamingScopeCounts[scopeKey] = r.streamingScopeCounts[scopeKey] + 1
}

func (r *Router) decStreamingCounts(threadID, scope string) {
	if count := r.streamingItemCounts[threadID]; count > 0 {
		r.streamingItemCounts[threadID] = count - 1
	}
	// Delete the scoped key at zero instead of leaving a 0 entry: scope
	// keys are per-subagent (one per Agent tool_use_id) and unbounded over
	// a thread's life, whereas the thread-wide counter has one key per
	// thread and can sit at 0 until teardown.
	scopeKey := streamingScopeKey(threadID, scope)
	if count := r.streamingScopeCounts[scopeKey]; count > 0 {
		if count == 1 {
			delete(r.streamingScopeCounts, scopeKey)
		} else {
			r.streamingScopeCounts[scopeKey] = count - 1
		}
	}
}

// hasActiveStreamingItemForScope reports whether a streaming text or
// thinking block is open (or mid-settle) in the given scope on this
// thread. It mirrors hasActiveStreamingItem's decrement-at-finishSettle
// timing at scope granularity, so a same-scope completion still queues
// across an async settle (preserving FIFO), while a different-scope
// completion does not wait.
func (r *Router) hasActiveStreamingItemForScope(threadID, scope string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streamingScopeCounts[streamingScopeKey(threadID, scope)] > 0
}

// maybeDeferOrPersist enforces invariant 11: a NEW row created mid-stream
// must defer its item_index until the stream it interrupts settles, so it
// can't render above the still-streaming tail (which took its lower index
// at segment start). The defer is SAME-scope only — keyed on the item's
// ParentID. A main-scope completion deferring behind a concurrent
// subagent-scope stream drained past later main text (thread 4d82b192
// turn 18: "Report CPU model -> done" landed after "First back").
func (r *Router) maybeDeferOrPersist(threadID string, item store.Item, payload *store.Payload) error {
	if !r.hasActiveStreamingItemForScope(threadID, item.ParentID) {
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

// hasQueuedInterruptItems reports whether any deferred persists are
// queued for threadID. The promoted-echo boundary path uses it to
// decide whether a drain (and a re-bump of the promoted row) is needed
// before sampling the turn's max item_index (round-6, R6-2).
func (r *Router) hasQueuedInterruptItems(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.interruptQueue[threadID]) > 0
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

// drainLock returns threadID's drain mutex, creating it on first use.
// Entries are never deleted (see the drainLocks field doc).
func (r *Router) drainLock(threadID string) *sync.Mutex {
	r.drainLocksMu.Lock()
	defer r.drainLocksMu.Unlock()
	mu, ok := r.drainLocks[threadID]
	if !ok {
		mu = &sync.Mutex{}
		r.drainLocks[threadID] = mu
	}
	return mu
}

// drainInterruptQueue persists every queued item for the thread, under
// the thread's drain lock so the pop-to-persisted span is atomic
// against the promoted-echo boundary sample (round-7, R7-3).
func (r *Router) drainInterruptQueue(threadID string, forceErrored bool) error {
	lock := r.drainLock(threadID)
	lock.Lock()
	defer lock.Unlock()
	return r.drainInterruptQueueLocked(threadID, forceErrored)
}

// drainInterruptQueueLocked is the drain body; the caller must hold the
// thread's drain lock. The queue is handed off before iteration
// (cleared from the map under r.mu), so an early return on persist
// failure would silently strand the remaining items. We log each
// failure and return the first error once the full queue has been
// attempted.
func (r *Router) drainInterruptQueueLocked(threadID string, forceErrored bool) error {
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
		go func(scope, itemID string) {
			defer turnWG.Done()
			defer r.settleWG.Done()
			captureErr(r.doSettleStreamingText(threadID, scope, itemID, status, "", false))
		}(ref.scope, ref.itemID)
	}
	for _, key := range thinkingKeys {
		ref, active := r.takeActiveThinkingBlockByKey(key)
		if !active {
			continue
		}
		turnWG.Add(1)
		r.settleWG.Add(1)
		go func(scope, itemID string) {
			defer turnWG.Done()
			defer r.settleWG.Done()
			captureErr(r.doSettleStreamingThinking(threadID, scope, itemID, status, "", false))
		}(ref.scope, ref.itemID)
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
// no-ops correctly. The streamingItemCounts AND streamingScopeCounts
// decrements are DEFERRED to finishSettle (which runs after persistItem
// completes in the heavy body) so both counters stay non-zero across
// the settle — maybeDeferOrPersist therefore continues to queue an
// incoming same-scope row while the settle is mid-flight (preserving
// FIFO), and drainInterruptQueueIfIdle holds off until the thread is
// fully idle.
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
func (r *Router) finishSettle(threadID, scope string) {
	r.mu.Lock()
	r.decStreamingCounts(threadID, scope)
	r.mu.Unlock()
	r.drainInterruptQueueIfIdle(threadID)
}

// doSettleStreamingText is the heavy body of the text-block settle:
// flush the stream-persist buffer, re-read the item from SQLite,
// stamp final status + pathRefs, persist only the changed fields, and
// emit a lightweight patch instead of the full Item. Called by both the
// sync wrapper (settleStreamingText, used inside settleTurnStreaming)
// and the async wrapper (settleStreamingTextAsync, used at
// content-block-stop on the provider read-loop). Safe to run in any
// goroutine.
func (r *Router) doSettleStreamingText(threadID, scope, itemID, status, finalContent string, finalContentPresent bool) error {
	// finishSettle MUST fire whether we persist successfully, find no
	// row, or find a row that already settled. Each of those still
	// represents a streaming slot that just closed; without the drain,
	// queued non-streaming rows behind the lock would leak.
	defer r.finishSettle(threadID, scope)
	// The live-stream path-refs cache only exists for streaming rows.
	// Clear it AFTER flushStreamingItem so the final-flush emit still
	// sees the prior hash and short-circuits in the common case where
	// the last 250ms window added no new paths. Clearing before the
	// flush would force a redundant action:meta emit and UpdateItemMeta
	// for every settled text row with paths.
	defer r.clearStreamingPathRefs(threadID, itemID)

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
	pathRefSource := item.Summary
	if finalContentPresent {
		pathRefSource = finalContent
		item.Summary = finalContent
	}
	// Final observer tick with the row's final MODEL text
	// (pathRefSource — before the interrupted-summary decoration; the
	// decorated text differs from what streamed, and for a stream cut
	// mid-fence the suffix would land inside the fence, so seeds for
	// either version can only prefix-match. The undecorated text at
	// least matches every fence the model actually closed). The
	// observer pushes final highlight seeds and drops its per-row
	// state.
	if r.assistantTextStream != nil {
		r.assistantTextStream(threadID, itemID, pathRefSource, true)
	}
	if status == statusErrored {
		item.Summary = interruptedSummary(item.Summary)
	}
	now := time.Now().UnixMilli()
	r.enrichPathRefsFromTexts(threadID, &item, pathRefSource)
	// Persist highlight spans keyed to the FINAL summary (after the
	// interrupted decoration above — the spans must match exactly what
	// the frontend renders; for a stream cut mid-fence the decorated
	// open fence is the rendered content).
	r.enrichCodeSpans(&item)
	if finalContentPresent && item.PayloadID != "" {
		metaJSON := buildPayloadMeta(itemKindAssistantText, provider.ProviderEvent{
			ThreadID:  threadID,
			Content:   finalContent,
			Timestamp: time.UnixMilli(now),
		})
		if err := r.store.ReplacePayloadData(item.PayloadID, []byte(finalContent), metaJSON, now); err != nil {
			return fmt.Errorf("assistant text replace payload %s: %w", item.PayloadID, err)
		}
	}

	update := store.ItemPartialUpdate{
		Status:    &status,
		Meta:      &item.Meta,
		UpdatedAt: &now,
	}
	summaryChanged := finalContentPresent || status == statusErrored
	if summaryChanged {
		update.Summary = &item.Summary
	}
	return r.persistItemFieldsAndPatch(threadID, itemID, item.Kind, update)
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
	return r.doSettleStreamingText(threadID, scope, itemID, status, "", false)
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
			if err := r.persistOrUpdateCompletedTextItem(threadID, turnIndex, scope, providerItemID, finalContent); err != nil {
				log.Printf("triage: persist completed text %s/%s: %v", threadID, providerItemID, err)
			}
		}
		return
	}
	r.settleWG.Add(1)
	go func() {
		defer r.settleWG.Done()
		if err := r.doSettleStreamingText(threadID, scope, itemID, status, finalContent, finalContentPresent); err != nil {
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
			if err := r.doSettleStreamingText(threadID, scope, itemID, status, "", false); err != nil {
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

func (r *Router) persistOrUpdateCompletedTextItem(threadID string, turnIndex int, scope, providerItemID, content string) error {
	if providerItemID != "" {
		r.WaitForPendingSettles()
		if item, found, err := r.store.FindStreamItemByProviderItemID(threadID, turnIndex, itemKindAssistantText, scope, providerItemID); err != nil {
			return err
		} else if found {
			if item.Status != statusStreaming && item.Status != statusCompleted {
				return nil
			}
			if item.Status == statusCompleted && item.Summary == content {
				// Idempotent re-assert (duplicate content-present stop):
				// the row already holds exactly this settled content.
				// Re-emitting the completed upsert would dispose a
				// frontend smoother mid-drain (terminal upserts dispose
				// without snap), turning the rest of a still-revealing
				// row into a wholesale jump.
				return nil
			}
			item.Summary = content
			item.Status = statusCompleted
			item.UpdatedAt = time.Now().UnixMilli()
			r.enrichPathRefsFromTexts(threadID, &item, content)
			r.enrichCodeSpans(&item)
			payload := assistantTextPayload(threadID, item.ID, content, item.UpdatedAt)
			return r.persistItem(item, &payload)
		}
	}
	return r.persistCompletedTextItem(threadID, turnIndex, scope, providerItemID, content)
}

func (r *Router) persistCompletedTextItem(threadID string, turnIndex int, scope, providerItemID, content string) error {
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
		PayloadID: assistantTextPayloadID(threadID, itemID),
		ParentID:  scope,
		Meta:      providerItemMeta(providerItemID),
		CreatedAt: now,
		UpdatedAt: now,
	}
	r.enrichPathRefsFromTexts(threadID, &item, content)
	r.enrichCodeSpans(&item)
	payload := assistantTextPayload(threadID, item.ID, content, now)
	if scope == "" {
		// Top-level recovery lands in the live transcript mid-view;
		// stream the wire projection so it reveals instead of mounting
		// wholesale, and leave a breadcrumb (rare: a CLI-internal API
		// retry delivered the reply snapshot-only). Subagent-scoped
		// blocks keep the single completed upsert: recovery is their
		// NORMAL delivery path (the CLI emits no partial stream events
		// for subagent messages), they render inside cards, and the
		// settle patch would race the fold eviction in the frontend's
		// applyItemPatch before the reveal wrote any text.
		log.Printf("triage: recovered never-streamed text block %s on thread %s (%d bytes)", itemID, threadID, len(content))
		return r.persistCompletedBlockEmitStreaming(item, &payload, content)
	}
	return r.persistItem(item, &payload)
}

func (r *Router) nextTextItemID(threadID string, turnIndex int, scope string) string {
	key := scopeCounterKey(threadID, turnIndex, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.segmentIndexByScope[key] = r.segmentIndexByScope[key] + 1
	return textItemID(turnIndex, scope, r.segmentIndexByScope[key])
}

// enrichPathRefs is the settle-time hook for assistant_text rows.
// Delegates to enrichPathRefsFromTexts using item.Summary as the only
// validation source. Kept as a wrapper so the streaming-text settle
// path stays unchanged.
//
// Other persist sites (ProposedPlan, AskUserQuestion, advisor result,
// ChannelMessage) live in different fields than item.Summary and call
// enrichPathRefsFromTexts directly with the correct source.
func (r *Router) enrichPathRefs(threadID string, item *store.Item) {
	if item.Kind != itemKindAssistantText {
		return
	}
	r.enrichPathRefsFromTexts(threadID, item, item.Summary)
}

// enrichPathRefsFromTexts is the explicit-source variant. It validates
// path-shaped tokens across one or more text sources against the
// workspace filesystem and stores the resulting allowlist on item.Meta.
// Per-item dedupe keeps the slice size bounded when several sources
// reference the same path.
//
// Failures are non-fatal: a missing thread record or a malformed
// existing meta JSON falls back to skipping enrichment so the
// persist path stays robust under partial state. The cost (Go regex
// + os.Stat × unique paths) is sub-frame for realistic messages —
// see internal/pathlinks/AGENTS.md for measured ranges.
func (r *Router) enrichPathRefsFromTexts(threadID string, item *store.Item, sources ...string) {
	workspacePath := r.workspacePathFor(threadID)
	if workspacePath == "" {
		return
	}
	refs := extractPathRefsFromTexts(workspacePath, sources)
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

// extractPathRefsFromTexts runs the per-source pathlinks validator
// and concatenates the results. Duplicates across sources are kept
// because PathRefs carry occurrence-level info — the frontend wraps
// per-occurrence, and the per-source ExtractAndValidate already dedupes
// its own stat calls so cost stays bounded.
func extractPathRefsFromTexts(workspacePath string, sources []string) []pathlinks.PathRef {
	var all []pathlinks.PathRef
	for _, text := range sources {
		if text == "" {
			continue
		}
		refs := pathlinks.ExtractAndValidate(workspacePath, text)
		if len(refs) == 0 {
			continue
		}
		all = append(all, refs...)
	}
	return all
}

// clearStreamingPathRefs drops the live-stream pathRefs dedupe cache
// for a single (threadID, itemID). Called when a streaming row
// transitions out of streaming state (doSettleStreamingText) so the
// cache doesn't outlive the row. Per-thread sweeps in
// CleanupThread / clearActiveStreamBlocksForTurnLocked
// cover the broader teardown paths.
func (r *Router) clearStreamingPathRefs(threadID, itemID string) {
	if threadID == "" || itemID == "" {
		return
	}
	key := streamPersistKey(threadID, itemID)
	r.mu.Lock()
	delete(r.streamingPathRefsLast, key)
	r.mu.Unlock()
}

// streamingPathRefsState is the per-streaming-row live pathRefs state.
// scanner does the incremental regex extraction (stat re-validation
// still runs over the full candidate set every tick — that is what
// keeps mid-stream link appearance/disappearance identical to a full
// rescan). lastMerged / lastRefs / lastMetaBase snapshot the last
// SUCCESSFULLY PERSISTED enrichment so an unchanged tick can skip the
// meta JSON round-trip entirely: mergePathRefsIntoMeta is
// deterministic, so identical refs over an identical meta base
// reproduce lastMerged byte for byte.
//
// Mutated only under streamFlushMu (all flush paths serialize there);
// the map itself is guarded by r.mu.
type streamingPathRefsState struct {
	scanner      *pathlinks.StreamScanner
	lastMerged   string
	lastRefs     []pathlinks.PathRef
	lastMetaBase string
}

// enrichStreamingPathRefsAndEmit re-validates path-shaped tokens
// against the live Summary of an in-flight assistant_text row,
// persists the resulting allowlist via UpdateItemMeta (which does NOT
// bump updated_at), and emits a `provider:item_event` with
// action:"meta" so the frontend can re-render path links mid-stream.
//
// Why this exists: settle-time enrichment (enrichPathRefs) only fires
// when the stream ends, so a user watching a long assistant_text
// stream sees raw paths until the model finishes. This helper runs on
// every flush-persistence tick for an assistant_text row, gated by a
// per-row last-merged cache so unchanged validator output
// short-circuits before the SQLite UPDATE and the event emit.
//
// `item` is the row the flush's summary append just returned to the
// caller — caller-side invariants (only fires from
// flushStreamPersistence's itemKindAssistantText case after a
// successful summary append) make the kind/status fields trustworthy
// without a re-fetch.
//
// An empty refs slice is a no-op: triage's settle path leaves meta
// untouched when nothing validates, so the streaming path mirrors
// that — no need to push `{"pathRefs":[]}` rows just to confirm
// "still no paths." Once a path appears the merged value differs
// from the cached one, emission fires.
//
// Best-effort: a thread without a workspace or a merge failure
// returns silently. The settle path will re-run enrichPathRefs
// against the final summary either way (it stays the authoritative
// full-text scan).
func (r *Router) enrichStreamingPathRefsAndEmit(item store.Item, updatedAt int64) {
	workspacePath := r.workspacePathFor(item.ThreadID)
	if workspacePath == "" {
		return
	}
	key := streamPersistKey(item.ThreadID, item.ID)
	r.mu.Lock()
	state := r.streamingPathRefsLast[key]
	if state == nil {
		state = &streamingPathRefsState{scanner: pathlinks.NewStreamScanner(workspacePath)}
		r.streamingPathRefsLast[key] = state
	}
	r.mu.Unlock()
	refs := state.scanner.Update(item.Summary)
	if len(refs) == 0 {
		return
	}
	// Same validated set over the same meta base reproduces the merged
	// JSON byte for byte — skip the marshal round-trip, the UPDATE, and
	// the emit without computing any of them.
	if state.lastMerged != "" && item.Meta == state.lastMetaBase && slices.Equal(refs, state.lastRefs) {
		return
	}
	merged, err := mergePathRefsIntoMeta(item.Meta, refs)
	if err != nil {
		log.Printf("triage: streaming pathlinks merge meta for %s: %v", item.ID, err)
		return
	}
	if state.lastMerged == merged {
		return
	}
	if err := r.store.UpdateItemMeta(item.ThreadID, item.ID, merged); err != nil {
		log.Printf("triage: streaming pathlinks UpdateItemMeta %s: %v", item.ID, err)
		return
	}
	state.lastMerged = merged
	state.lastRefs = refs
	state.lastMetaBase = item.Meta
	r.emit("provider:item_event", newItemStreamMeta(item.ThreadID, item.ID, item.Kind, merged, updatedAt))
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
	_, workspacePath, err := r.store.GetThreadProviderWorkspace(threadID)
	if err != nil {
		log.Printf("triage: pathlinks lookup thread %s: %v", threadID, err)
		return ""
	}
	r.mu.Lock()
	r.workspacePathByThread[threadID] = workspacePath
	r.mu.Unlock()
	return workspacePath
}

// mergePathRefsIntoMeta returns the item.Meta JSON string with a
// `pathRefs` key set to the supplied slice, preserving any other
// existing top-level keys (e.g. `task_id` — its partial index
// idx_items_meta_task_id must keep working). An empty / whitespace-only input
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
		return pathlinks.MarshalRefsJSON(refs)
	}
	obj := map[string]json.RawMessage{}
	// json.Unmarshal into the map preserves the raw bytes of
	// non-conflicting siblings so they round-trip untouched.
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		// Not a JSON object — overwrite. Log so corruption is visible.
		log.Printf("triage: pathlinks merge skipped corrupt meta: %v", err)
		return pathlinks.MarshalRefsJSON(refs)
	}
	encoded, err := json.Marshal(refs)
	if err != nil {
		return "", err
	}
	obj[pathlinks.MetaKey] = encoded
	out, err := json.Marshal(obj)
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
func (r *Router) doSettleStreamingThinking(threadID, scope, itemID, status, finalContent string, finalContentPresent bool) error {
	defer r.finishSettle(threadID, scope)

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
	now := time.Now().UnixMilli()
	update := store.ItemPartialUpdate{
		Status:    &status,
		UpdatedAt: &now,
	}
	if finalContentPresent {
		summary := thinkingSummaryPreview(finalContent)
		update.Summary = &summary
		if item.PayloadID != "" {
			if err := r.store.ReplacePayloadData(item.PayloadID, []byte(finalContent), item.PayloadMeta, now); err != nil {
				return fmt.Errorf("thinking final replace payload %s: %w", item.PayloadID, err)
			}
		}
	}
	if status == statusErrored {
		summary := interruptedSummary(item.Summary)
		update.Summary = &summary
	}
	return r.persistItemFieldsAndPatch(threadID, itemID, item.Kind, update)
}

func (r *Router) settleStreamingThinking(threadID string, turnIndex int, scope string, status string) error {
	itemID, active := r.takeActiveThinkingBlock(threadID, turnIndex, scope, "")
	if !active {
		return nil
	}
	return r.doSettleStreamingThinking(threadID, scope, itemID, status, "", false)
}

func (r *Router) settleStreamingThinkingAsync(threadID string, turnIndex int, scope, providerItemID, status, finalContent string, finalContentPresent bool) {
	itemID, active := r.takeActiveThinkingBlock(threadID, turnIndex, scope, providerItemID)
	if !active {
		if finalContentPresent && finalContent != "" {
			if err := r.persistOrUpdateCompletedThinkingItem(threadID, turnIndex, scope, providerItemID, finalContent); err != nil {
				log.Printf("triage: persist completed thinking %s/%s: %v", threadID, providerItemID, err)
			}
		}
		return
	}
	r.settleWG.Add(1)
	go func() {
		defer r.settleWG.Done()
		if err := r.doSettleStreamingThinking(threadID, scope, itemID, status, finalContent, finalContentPresent); err != nil {
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
			if err := r.doSettleStreamingThinking(threadID, scope, itemID, status, "", false); err != nil {
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

func (r *Router) persistOrUpdateCompletedThinkingItem(threadID string, turnIndex int, scope, providerItemID, content string) error {
	if providerItemID != "" {
		r.WaitForPendingSettles()
		if item, found, err := r.store.FindStreamItemByProviderItemID(threadID, turnIndex, itemKindThinking, scope, providerItemID); err != nil {
			return err
		} else if found {
			if item.Status != statusStreaming && item.Status != statusCompleted {
				return nil
			}
			if item.Status == statusCompleted && item.Summary == thinkingSummaryPreview(content) {
				// Idempotent re-assert (duplicate content-present stop):
				// same rationale as the text branch above. The preview is
				// the trailing 400 runes, so for a same-provider-item-id
				// re-assert a matching tail means the same content — skip
				// the payload rewrite along with the upsert.
				return nil
			}
			item.Summary = thinkingSummaryPreview(content)
			item.Status = statusCompleted
			item.UpdatedAt = time.Now().UnixMilli()
			if item.PayloadID != "" {
				if err := r.store.ReplacePayloadData(item.PayloadID, []byte(content), item.PayloadMeta, item.UpdatedAt); err != nil {
					return fmt.Errorf("thinking final replace payload %s: %w", item.PayloadID, err)
				}
			}
			return r.persistItem(item, nil)
		}
	}
	return r.persistCompletedThinkingItem(threadID, turnIndex, scope, providerItemID, content)
}

func (r *Router) persistCompletedThinkingItem(threadID string, turnIndex int, scope, providerItemID, content string) error {
	itemID := r.nextThinkingItemID(threadID, turnIndex, scope)
	payloadID := thinkingPayloadID(itemID)
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
		ParentID:  scope,
		Meta:      providerItemMeta(providerItemID),
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
	if scope == "" {
		// Same top-level-only streaming projection as the text branch —
		// see persistCompletedTextItem for the rationale and the
		// subagent carve-out.
		log.Printf("triage: recovered never-streamed thinking block %s on thread %s (%d bytes)", itemID, threadID, len(content))
		return r.persistCompletedBlockEmitStreaming(item, &payload, content)
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
