package triage

import (
	"database/sql"
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
	updatedAt    int64
	timer        *time.Timer
}

type pendingStreamFlush struct {
	threadID     string
	itemID       string
	kind         string
	payloadID    string
	summaryDelta string
	payloadDelta string
	updatedAt    int64
}

func streamPersistKey(threadID, itemID string) string {
	return threadID + "|" + itemID
}

func (r *Router) emitItemDelta(evt ItemDeltaEvent) {
	r.emit("provider:item_delta", evt)
}

func (r *Router) bufferTextPersistence(threadID, itemID, delta string, updatedAt int64) error {
	return r.bufferStreamPersistence(pendingStreamFlush{
		threadID:     threadID,
		itemID:       itemID,
		kind:         itemKindAssistantText,
		summaryDelta: delta,
		updatedAt:    updatedAt,
	})
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

func (r *Router) bufferStreamPersistence(delta pendingStreamFlush) error {
	if delta.summaryDelta == "" && delta.payloadDelta == "" {
		return nil
	}

	key := streamPersistKey(delta.threadID, delta.itemID)
	var pending *pendingStreamFlush

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

	if len(buffer.summaryDelta)+len(buffer.payloadDelta) >= streamPersistByteThreshold {
		pending = r.takeStreamPersistenceLocked(key)
	} else {
		r.scheduleStreamPersistenceLocked(key, buffer)
	}
	r.mu.Unlock()

	if pending == nil {
		return nil
	}
	return r.flushStreamPersistence(*pending)
}

func (r *Router) scheduleStreamPersistenceLocked(key string, buffer *streamPersistBuffer) {
	if buffer.timer != nil {
		return
	}
	buffer.timer = time.AfterFunc(streamPersistInterval, func() {
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
		updatedAt:    buffer.updatedAt,
	}
}

func (r *Router) flushStreamPersistenceKey(key string) {
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
		_, err := r.store.AppendItemSummary(flush.threadID, flush.itemID, flush.summaryDelta, flush.updatedAt)
		return ignoreLateStreamPersistence(err)
	case itemKindThinking:
		updated, err := r.store.AppendItemSummaryPreview(
			flush.threadID,
			flush.itemID,
			flush.summaryDelta,
			thinkingPreviewRunes,
			"...",
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
