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
	threadID  string
	itemID    string
	kind      string
	payloadID string
	// content accumulates the window's delta text once. For text and
	// thinking the staged summaryDelta and payloadDelta are always the
	// same string, so a single builder replaces what used to be two
	// `string +=` copies of identical content per chunk (each of which
	// reallocated the whole accumulated window); command_output stages
	// payloadDelta only. take-time materializes the pendingStreamFlush
	// fields from this one buffer.
	content strings.Builder
	// meta carries the most recent chunk's provider meta. Only
	// command_output flushes consume it (buildPayloadMeta reads
	// command/exit fields from it); text/thinking leave it nil.
	meta      json.RawMessage
	updatedAt int64
	timer     *time.Timer
}

// contentWeight is the value compared against persistByteThresholdForKind.
// The historical threshold check was len(summaryDelta)+len(payloadDelta),
// which double-counts text/thinking content (both fields carried the same
// string) — preserved exactly so flush timing does not change.
func (b *streamPersistBuffer) contentWeight() int {
	if b.kind == payloadKindCommandOutput {
		return b.content.Len()
	}
	return 2 * b.content.Len()
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
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return false
	}
	buffer := st.streamPersistBuffers[itemID]
	return buffer != nil && buffer.kind == payloadKindCommandOutput
}

// discardCommandOutputBufferLocked drops a pending command-output buffer
// without flushing. Used by the Replace path: the authoritative
// aggregated-output snapshot subsumes every buffered delta, so flushing
// first would only burn a write that the rewrite immediately overwrites.
// Caller must hold r.mu (and streamFlushMu — see handleCommandOutput).
func (r *Router) discardCommandOutputBufferLocked(threadID, itemID string) {
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return
	}
	buffer := st.streamPersistBuffers[itemID]
	if buffer == nil || buffer.kind != payloadKindCommandOutput {
		return
	}
	if buffer.timer != nil {
		buffer.timer.Stop()
	}
	delete(st.streamPersistBuffers, itemID)
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

	flushNow := false

	r.mu.Lock()
	st := r.state(delta.threadID)
	buffer := st.streamPersistBuffers[delta.itemID]
	if buffer == nil {
		buffer = &streamPersistBuffer{
			threadID:  delta.threadID,
			itemID:    delta.itemID,
			kind:      delta.kind,
			payloadID: delta.payloadID,
		}
		if st.streamPersistBuffers == nil {
			st.streamPersistBuffers = make(map[string]*streamPersistBuffer)
		}
		st.streamPersistBuffers[delta.itemID] = buffer
	}
	// Text/thinking stagers pass the identical string as summaryDelta and
	// payloadDelta; command_output stages payloadDelta only. Either way
	// the window's content accumulates exactly once.
	if delta.kind == payloadKindCommandOutput {
		buffer.content.WriteString(delta.payloadDelta)
	} else {
		buffer.content.WriteString(delta.summaryDelta)
	}
	buffer.updatedAt = delta.updatedAt
	if buffer.payloadID == "" {
		buffer.payloadID = delta.payloadID
	}
	if len(delta.meta) > 0 {
		buffer.meta = delta.meta
	}

	if buffer.contentWeight() >= persistByteThresholdForKind(buffer.kind) {
		flushNow = true
		if !flushOnThreshold {
			r.scheduleStreamPersistenceLocked(buffer)
		}
	} else {
		r.scheduleStreamPersistenceLocked(buffer)
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

func (r *Router) scheduleStreamPersistenceLocked(buffer *streamPersistBuffer) {
	if buffer.timer != nil {
		return
	}
	threadID, itemID := buffer.threadID, buffer.itemID
	buffer.timer = time.AfterFunc(persistIntervalForKind(buffer.kind), func() {
		r.flushStreamPersistenceKey(threadID, itemID)
	})
}

// takeStreamPersistenceLocked extracts and removes one thread's buffer
// for itemID. Caller holds r.mu.
func (r *Router) takeStreamPersistenceLocked(st *threadState, itemID string) *pendingStreamFlush {
	if st == nil {
		return nil
	}
	buffer := st.streamPersistBuffers[itemID]
	if buffer == nil {
		return nil
	}
	if buffer.timer != nil {
		buffer.timer.Stop()
		buffer.timer = nil
	}
	delete(st.streamPersistBuffers, itemID)
	if buffer.content.Len() == 0 {
		return nil
	}
	// Materialize once; for text/thinking both fields reference the SAME
	// string, matching what the stagers fed in without holding the
	// window's content twice.
	content := buffer.content.String()
	pending := &pendingStreamFlush{
		threadID:     buffer.threadID,
		itemID:       buffer.itemID,
		kind:         buffer.kind,
		payloadID:    buffer.payloadID,
		payloadDelta: content,
		meta:         buffer.meta,
		updatedAt:    buffer.updatedAt,
	}
	if buffer.kind != payloadKindCommandOutput {
		pending.summaryDelta = content
	}
	return pending
}

// flushStreamPersistenceKey is the timer callback. It holds streamFlushMu
// across extract+write so a concurrent replace/settle on the read loop
// cannot interleave with the SQLite write — without it a timer flush
// extracted-but-not-committed when a command's authoritative Replace
// snapshot lands would append a duplicate output tail after the rewrite.
func (r *Router) flushStreamPersistenceKey(threadID, itemID string) {
	r.streamFlushMu.Lock()
	defer r.streamFlushMu.Unlock()
	r.mu.Lock()
	pending := r.takeStreamPersistenceLocked(r.threadStateIfPresent(threadID), itemID)
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
	r.mu.Lock()
	pending := r.takeStreamPersistenceLocked(r.threadStateIfPresent(threadID), itemID)
	r.mu.Unlock()
	if pending == nil {
		return nil
	}
	return r.flushStreamPersistence(*pending)
}

func (r *Router) flushStreamingThread(threadID string) error {
	r.streamFlushMu.Lock()
	defer r.streamFlushMu.Unlock()
	r.mu.Lock()
	pending := make([]pendingStreamFlush, 0)
	if st := r.threadStateIfPresent(threadID); st != nil {
		for itemID := range st.streamPersistBuffers {
			if flush := r.takeStreamPersistenceLocked(st, itemID); flush != nil {
				pending = append(pending, *flush)
			}
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
	pending := make([]pendingStreamFlush, 0)
	for _, st := range r.threads {
		for itemID := range st.streamPersistBuffers {
			if flush := r.takeStreamPersistenceLocked(st, itemID); flush != nil {
				pending = append(pending, *flush)
			}
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
	// The summary append and the payload chunk append for one window run
	// as ONE store transaction (the payload half is skipped when the item
	// has no linked payload yet or the window carried no payload bytes) —
	// this is the hottest recurring write path, and the former separate
	// AppendPayloadData call doubled writer-lock acquisitions per window.
	payloadID := flush.payloadID
	if flush.payloadDelta == "" {
		payloadID = ""
	}
	switch flush.kind {
	case itemKindAssistantText:
		updated, err := r.store.AppendItemSummaryAndPayloadData(
			flush.threadID,
			flush.itemID,
			flush.summaryDelta,
			payloadID,
			[]byte(flush.payloadDelta),
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
		// the append is the same row enrich needs — pass it through to
		// skip a redundant SQLite read on the hot path. See
		// enrichStreamingPathRefsAndEmit.
		r.enrichStreamingPathRefsAndEmit(updated, flush.updatedAt)
		// Same full-running-summary cadence feeds the highlight seed
		// push (app-wired; nil when unwired).
		if r.assistantTextStream != nil {
			r.assistantTextStream(updated.ThreadID, updated.ID, updated.Summary, false)
		}
		return nil
	case itemKindThinking:
		_, err := r.store.AppendItemSummaryTailAndPayloadData(
			flush.threadID,
			flush.itemID,
			flush.summaryDelta,
			thinkingPreviewRunes,
			payloadID,
			[]byte(flush.payloadDelta),
			flush.updatedAt,
		)
		if isLateStreamPersistence(err) {
			return nil
		}
		return err
	case payloadKindCommandOutput:
		return r.flushCommandOutputPersistence(flush)
	default:
		return fmt.Errorf("unknown stream persistence kind %q for %s", flush.kind, flush.itemID)
	}
}

func isLateStreamPersistence(err error) bool {
	return errors.Is(err, store.ErrItemSettled) || errors.Is(err, sql.ErrNoRows)
}
