package triage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/store"
)

const (
	streamPersistInterval      = 250 * time.Millisecond
	streamPersistByteThreshold = 4096

	// Command output buffers both the SQLite append AND the wire-visible
	// item upsert (text/thinking emit live deltas per chunk; command rows
	// have no delta channel — the row upsert is what bumps updatedAt and
	// refreshes an expanded output view). A shorter interval keeps the
	// expanded live tail at ~10Hz. The byte threshold is larger than the
	// text one because a fast producer (build logs, file dumps) ships
	// PTY-sized chunks; a 4KB cap would flush per chunk and buy nothing.
	commandOutputPersistInterval      = 100 * time.Millisecond
	commandOutputPersistByteThreshold = 64 * 1024
)

type ItemDeltaEvent struct {
	ThreadID  string `json:"threadId"`
	ItemID    string `json:"itemId"`
	Kind      string `json:"kind"`
	Delta     string `json:"delta"`
	UpdatedAt int64  `json:"updatedAt"`
}

type streamPersistBuffer struct {
	threadID     string
	itemID       string
	kind         string
	payloadID    string
	summaryDelta string
	payloadDelta string
	// meta carries the most recent chunk's provider meta. Only
	// command_output flushes consume it (buildPayloadMeta reads
	// command/exit fields from it); text/thinking leave it nil.
	meta      json.RawMessage
	updatedAt int64
	timer     *time.Timer
}

type pendingStreamFlush struct {
	threadID     string
	itemID       string
	kind         string
	payloadID    string
	summaryDelta string
	payloadDelta string
	meta         json.RawMessage
	updatedAt    int64
}

func streamPersistKey(threadID, itemID string) string {
	return threadID + "|" + itemID
}

func (r *Router) emitItemDelta(evt ItemDeltaEvent) {
	r.emit("provider:item_event", newItemStreamDelta(evt))
}

func (r *Router) stageTextPersistenceForEmit(threadID, itemID, payloadID, delta string, updatedAt int64) bool {
	return r.stageStreamPersistence(pendingStreamFlush{
		threadID:     threadID,
		itemID:       itemID,
		kind:         itemKindAssistantText,
		payloadID:    payloadID,
		summaryDelta: delta,
		payloadDelta: delta,
		updatedAt:    updatedAt,
	}, false)
}

func (r *Router) bufferThinkingPersistence(threadID, itemID, payloadID, delta string, updatedAt int64) error {
	return r.bufferStreamPersistence(pendingStreamFlush{
		threadID:     threadID,
		itemID:       itemID,
		kind:         itemKindThinking,
		payloadID:    payloadID,
		summaryDelta: delta,
		payloadDelta: delta,
		updatedAt:    updatedAt,
	})
}

func (r *Router) stageThinkingPersistenceForEmit(threadID, itemID, payloadID, delta string, updatedAt int64) bool {
	return r.stageStreamPersistence(pendingStreamFlush{
		threadID:     threadID,
		itemID:       itemID,
		kind:         itemKindThinking,
		payloadID:    payloadID,
		summaryDelta: delta,
		payloadDelta: delta,
		updatedAt:    updatedAt,
	}, false)
}

func (r *Router) bufferStreamPersistence(delta pendingStreamFlush) error {
	flushNow := r.stageStreamPersistence(delta, true)
	if !flushNow {
		return nil
	}
	return r.flushStreamingItem(delta.threadID, delta.itemID)
}

// bufferCommandOutputPersistence stages a Codex command-output delta.
// Unlike text/thinking, command output has no live wire-delta channel —
// the flush is what writes SQLite AND emits the row upsert, so until a
// flush fires the chunk is invisible to both. handleCommandOutput
// guarantees the item row exists before the first stage for an item.
func (r *Router) bufferCommandOutputPersistence(threadID, itemID, content string, meta json.RawMessage, updatedAt int64) error {
	return r.bufferStreamPersistence(pendingStreamFlush{
		threadID:     threadID,
		itemID:       itemID,
		kind:         payloadKindCommandOutput,
		payloadDelta: content,
		meta:         meta,
		updatedAt:    updatedAt,
	})
}

// hasCommandOutputBuffer reports whether a command-output persistence
// buffer is live for the item. handleCommandOutput uses it to skip the
// per-chunk item read on the streaming hot path: a live buffer implies
// the row's existence was already verified at window start.
func (r *Router) hasCommandOutputBuffer(threadID, itemID string) bool {
	key := streamPersistKey(threadID, itemID)
	r.mu.Lock()
	buffer := r.streamPersistBuffers[key]
	live := buffer != nil && buffer.kind == payloadKindCommandOutput
	r.mu.Unlock()
	return live
}

// discardCommandOutputBufferLocked drops a pending command-output buffer
// without flushing. Used by the Replace path: the authoritative
// aggregated-output snapshot subsumes every buffered delta, so flushing
// first would only burn a write that the rewrite immediately overwrites.
// Caller must hold r.mu (and streamFlushMu — see handleCommandOutput).
func (r *Router) discardCommandOutputBufferLocked(threadID, itemID string) {
	key := streamPersistKey(threadID, itemID)
	buffer := r.streamPersistBuffers[key]
	if buffer == nil || buffer.kind != payloadKindCommandOutput {
		return
	}
	if buffer.timer != nil {
		buffer.timer.Stop()
	}
	delete(r.streamPersistBuffers, key)
}

// stageStreamPersistence records a live delta in the in-memory flush buffer.
// When flushOnThreshold is false, the caller owns the threshold flush after
// its wire-visible side effect has run. That preserves the invariant that
// GetPayloadData can flush any delta already visible to the frontend without
// putting threshold SQLite writes before live event emission.
func (r *Router) stageStreamPersistence(delta pendingStreamFlush, flushOnThreshold bool) bool {
	if delta.summaryDelta == "" && delta.payloadDelta == "" {
		return false
	}

	key := streamPersistKey(delta.threadID, delta.itemID)
	flushNow := false

	r.mu.Lock()
	buffer := r.streamPersistBuffers[key]
	if buffer == nil {
		buffer = &streamPersistBuffer{
			threadID:  delta.threadID,
			itemID:    delta.itemID,
			kind:      delta.kind,
			payloadID: delta.payloadID,
		}
		r.streamPersistBuffers[key] = buffer
	}
	buffer.summaryDelta += delta.summaryDelta
	buffer.payloadDelta += delta.payloadDelta
	buffer.updatedAt = delta.updatedAt
	if buffer.payloadID == "" {
		buffer.payloadID = delta.payloadID
	}
	if len(delta.meta) > 0 {
		buffer.meta = delta.meta
	}

	if len(buffer.summaryDelta)+len(buffer.payloadDelta) >= persistByteThresholdForKind(buffer.kind) {
		flushNow = true
		if !flushOnThreshold {
			r.scheduleStreamPersistenceLocked(key, buffer)
		}
	} else {
		r.scheduleStreamPersistenceLocked(key, buffer)
	}
	r.mu.Unlock()
	return flushNow
}

func persistByteThresholdForKind(kind string) int {
	if kind == payloadKindCommandOutput {
		return commandOutputPersistByteThreshold
	}
	return streamPersistByteThreshold
}

func persistIntervalForKind(kind string) time.Duration {
	if kind == payloadKindCommandOutput {
		return commandOutputPersistInterval
	}
	return streamPersistInterval
}

func (r *Router) scheduleStreamPersistenceLocked(key string, buffer *streamPersistBuffer) {
	if buffer.timer != nil {
		return
	}
	buffer.timer = time.AfterFunc(persistIntervalForKind(buffer.kind), func() {
		r.flushStreamPersistenceKey(key)
	})
}

func (r *Router) takeStreamPersistenceLocked(key string) *pendingStreamFlush {
	buffer := r.streamPersistBuffers[key]
	if buffer == nil {
		return nil
	}
	if buffer.timer != nil {
		buffer.timer.Stop()
		buffer.timer = nil
	}
	delete(r.streamPersistBuffers, key)
	if buffer.summaryDelta == "" && buffer.payloadDelta == "" {
		return nil
	}
	return &pendingStreamFlush{
		threadID:     buffer.threadID,
		itemID:       buffer.itemID,
		kind:         buffer.kind,
		payloadID:    buffer.payloadID,
		summaryDelta: buffer.summaryDelta,
		payloadDelta: buffer.payloadDelta,
		meta:         buffer.meta,
		updatedAt:    buffer.updatedAt,
	}
}

// flushStreamPersistenceKey is the timer callback. It holds streamFlushMu
// across extract+write so a concurrent replace/settle on the read loop
// cannot interleave with the SQLite write — without it a timer flush
// extracted-but-not-committed when a command's authoritative Replace
// snapshot lands would append a duplicate output tail after the rewrite.
func (r *Router) flushStreamPersistenceKey(key string) {
	r.streamFlushMu.Lock()
	defer r.streamFlushMu.Unlock()
	r.mu.Lock()
	pending := r.takeStreamPersistenceLocked(key)
	r.mu.Unlock()
	if pending == nil {
		return
	}
	if err := r.flushStreamPersistence(*pending); err != nil {
		log.Printf("triage: stream persistence flush %s/%s: %v", pending.threadID, pending.itemID, err)
	}
}

func (r *Router) flushStreamingItem(threadID, itemID string) error {
	r.streamFlushMu.Lock()
	defer r.streamFlushMu.Unlock()
	key := streamPersistKey(threadID, itemID)
	r.mu.Lock()
	pending := r.takeStreamPersistenceLocked(key)
	r.mu.Unlock()
	if pending == nil {
		return nil
	}
	return r.flushStreamPersistence(*pending)
}

func (r *Router) flushStreamingThread(threadID string) error {
	r.streamFlushMu.Lock()
	defer r.streamFlushMu.Unlock()
	prefix := threadID + "|"
	r.mu.Lock()
	pending := make([]pendingStreamFlush, 0)
	for key := range r.streamPersistBuffers {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if flush := r.takeStreamPersistenceLocked(key); flush != nil {
			pending = append(pending, *flush)
		}
	}
	r.mu.Unlock()

	var firstErr error
	for _, flush := range pending {
		if err := r.flushStreamPersistence(flush); err != nil {
			log.Printf("triage: stream persistence flush %s/%s: %v", flush.threadID, flush.itemID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (r *Router) FlushThread(threadID string) error {
	if r == nil {
		return nil
	}
	return r.flushStreamingThread(threadID)
}

func (r *Router) flushAllStreamPersistence() error {
	r.streamFlushMu.Lock()
	defer r.streamFlushMu.Unlock()
	r.mu.Lock()
	pending := make([]pendingStreamFlush, 0, len(r.streamPersistBuffers))
	for key := range r.streamPersistBuffers {
		if flush := r.takeStreamPersistenceLocked(key); flush != nil {
			pending = append(pending, *flush)
		}
	}
	r.mu.Unlock()

	var firstErr error
	for _, flush := range pending {
		if err := r.flushStreamPersistence(flush); err != nil {
			log.Printf("triage: stream persistence flush %s/%s: %v", flush.threadID, flush.itemID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (r *Router) flushStreamPersistence(flush pendingStreamFlush) error {
	switch flush.kind {
	case itemKindAssistantText:
		updated, err := r.store.AppendItemSummary(
			flush.threadID,
			flush.itemID,
			flush.summaryDelta,
			flush.updatedAt,
		)
		if isLateStreamPersistence(err) {
			return nil
		}
		if err != nil {
			return err
		}
		// Live path-link validation: re-run the workspace allowlist
		// against the row's running summary so path tokens in this
		// flush become clickable mid-stream. Best-effort, dedupes
		// against the previous merged meta. The fresh Item returned by
		// AppendItemSummary is the same row enrich needs — pass it
		// through to skip a redundant SQLite read on the hot path. See
		// enrichStreamingPathRefsAndEmit.
		r.enrichStreamingPathRefsAndEmit(updated, flush.updatedAt)
		if flush.payloadID == "" || flush.payloadDelta == "" {
			return nil
		}
		if err := r.store.AppendPayloadData(flush.payloadID, []byte(flush.payloadDelta), updated.PayloadMeta, flush.updatedAt); err != nil {
			return fmt.Errorf("assistant text stream append payload %s: %w", flush.payloadID, err)
		}
		return nil
	case itemKindThinking:
		updated, err := r.store.AppendItemSummaryTail(
			flush.threadID,
			flush.itemID,
			flush.summaryDelta,
			thinkingPreviewRunes,
			flush.updatedAt,
		)
		if isLateStreamPersistence(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if flush.payloadID == "" || flush.payloadDelta == "" {
			return nil
		}
		if err := r.store.AppendPayloadData(flush.payloadID, []byte(flush.payloadDelta), updated.PayloadMeta, flush.updatedAt); err != nil {
			return fmt.Errorf("thinking stream append payload %s: %w", flush.payloadID, err)
		}
		return nil
	case payloadKindCommandOutput:
		return r.flushCommandOutputPersistence(flush)
	default:
		return fmt.Errorf("unknown stream persistence kind %q for %s", flush.kind, flush.itemID)
	}
}

func ignoreLateStreamPersistence(err error) error {
	if err == nil {
		return nil
	}
	if isLateStreamPersistence(err) {
		return nil
	}
	return err
}

func isLateStreamPersistence(err error) bool {
	return errors.Is(err, store.ErrItemSettled) || errors.Is(err, sql.ErrNoRows)
}
