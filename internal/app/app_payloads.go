package app

import (
	"fmt"
	"unicode/utf8"

	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
)

type PayloadPreview struct {
	Data       string `json:"data"`
	NextOffset int    `json:"nextOffset"`
	TotalSize  int    `json:"totalSize"`
	IsComplete bool   `json:"isComplete"`
	// PatchSpans carries the persisted highlight spans of a diff-kind
	// payload (computed once at persist time, stored beside the row).
	// Keys are content-addressed per file, so spans for a file the
	// served text truncates simply never match and that file falls back
	// to the RPC path. See app_highlight_diff_seed.go.
	PatchSpans []PatchSpanSeed `json:"patchSpans,omitempty"`
}

type PayloadChunk struct {
	Data       string `json:"data"`
	Offset     int    `json:"offset"`
	NextOffset int    `json:"nextOffset"`
	TotalSize  int    `json:"totalSize"`
	IsComplete bool   `json:"isComplete"`
}

// PayloadContent returns a payload's raw bytes. Rendering is a frontend
// projection based on the payload kind.
type PayloadContent struct {
	Data string `json:"data"`
	// PatchSpans: see PayloadPreview.PatchSpans.
	PatchSpans []PatchSpanSeed `json:"patchSpans,omitempty"`
}

// payloadRead is a read of a payload's text: data starts at byte offset,
// total is the payload's size, and complete reports that data reaches its
// end.
type payloadRead struct {
	data     []byte
	offset   int
	total    int
	complete bool
}

// payloadText reads up to maxBytes of a payload's text from byte offset,
// which is 0 or the end of an earlier read; a negative offset reads from
// 0, and the read reports the offset it read from. A read that stops
// before the end stops between characters: a character the window would
// split is left for the next read, so the text a JSON client receives for
// consecutive reads, where each invalid byte is one U+FFFD, is the
// payload's. A window of 1 to utf8.UTFMax-1 bytes is read as utf8.UTFMax,
// so every read of a window short of the end returns at least one byte. A
// window of 0 reads nothing and reports the size.
func (a *App) payloadText(threadID, payloadID string, offset, maxBytes int) (payloadRead, error) {
	offset = max(offset, 0)
	if maxBytes > 0 {
		maxBytes = max(maxBytes, utf8.UTFMax)
	}
	data, total, complete, err := a.store.GetPayloadChunk(threadID, payloadID, offset, maxBytes)
	if err != nil {
		return payloadRead{}, err
	}
	if !complete {
		data = data[:stringsx.CompleteRunePrefix(data)]
	}
	return payloadRead{data: data, offset: offset, total: total, complete: complete}, nil
}

// GetPayloadPreview serves a payload's leading text (payloadText), with
// the payload's persisted highlight spans attached for diff kinds.
//
//ao:scope threads:read
func (a *App) GetPayloadPreview(threadID string, payloadID string, maxBytes int) (PayloadPreview, error) {
	if err := a.readyThreadHistory(threadID); err != nil {
		return PayloadPreview{}, err
	}
	meta, err := a.getThreadPayloadMeta(threadID, payloadID)
	if err != nil {
		return PayloadPreview{}, err
	}
	read, err := a.payloadText(threadID, payloadID, 0, maxBytes)
	if err != nil {
		return PayloadPreview{}, err
	}
	return PayloadPreview{
		Data:       string(read.data),
		NextOffset: len(read.data),
		TotalSize:  read.total,
		IsComplete: read.complete,
		PatchSpans: a.persistedPayloadPatchSpans(threadID, meta.Kind, payloadID),
	}, nil
}

// GetPayloadChunk serves a payload's text from byte offset (payloadText):
// 0, or the NextOffset of the preview or of an earlier chunk.
//
//ao:scope threads:read
func (a *App) GetPayloadChunk(threadID string, payloadID string, offset int, maxBytes int) (PayloadChunk, error) {
	if err := a.readyThreadHistory(threadID); err != nil {
		return PayloadChunk{}, err
	}
	if _, err := a.getThreadPayloadMeta(threadID, payloadID); err != nil {
		return PayloadChunk{}, err
	}
	read, err := a.payloadText(threadID, payloadID, offset, maxBytes)
	if err != nil {
		return PayloadChunk{}, err
	}
	return PayloadChunk{
		Data:       string(read.data),
		Offset:     read.offset,
		NextOffset: read.offset + len(read.data),
		TotalSize:  read.total,
		IsComplete: read.complete,
	}, nil
}

// GetPayloadData returns a payload body, with the payload's persisted
// highlight spans attached for diff kinds. The caller must supply the
// owning thread so payload ids cannot be read outside the thread
// timeline that references them.
//
//ao:scope threads:read
func (a *App) GetPayloadData(threadID string, payloadID string) (PayloadContent, error) {
	if err := a.readyThreadHistory(threadID); err != nil {
		return PayloadContent{}, err
	}
	meta, err := a.getThreadPayloadMeta(threadID, payloadID)
	if err != nil {
		return PayloadContent{}, err
	}
	data, err := a.store.GetPayloadData(threadID, payloadID)
	if err != nil {
		return PayloadContent{}, err
	}
	return PayloadContent{
		Data:       string(data),
		PatchSpans: a.persistedPayloadPatchSpans(threadID, meta.Kind, payloadID),
	}, nil
}

// readyThreadHistory prepares a read of a thread's history or payloads:
// the thread's fork copy is ready and its stream buffers are written. A
// live client places the read against the deltas it received by stream
// offset (store.Item.StreamEnd), so the read must hold the text of every
// delta emitted before it; a delta still buffered would be in neither.
func (a *App) readyThreadHistory(threadID string) error {
	if err := a.store.CheckForkReady(threadID); err != nil {
		return err
	}
	if a.triage == nil {
		return nil
	}
	if err := a.triage.FlushThread(threadID); err != nil {
		return fmt.Errorf("flush live stream buffers for thread %s: %w", threadID, err)
	}
	return nil
}

func (a *App) getThreadPayloadMeta(threadID string, payloadID string) (store.PayloadMeta, error) {
	if threadID == "" {
		return store.PayloadMeta{}, fmt.Errorf("thread id is required")
	}
	if payloadID == "" {
		return store.PayloadMeta{}, fmt.Errorf("payload id is required")
	}
	item, err := a.findThreadItemByPayloadID(threadID, payloadID)
	if err != nil {
		return store.PayloadMeta{}, err
	}
	// GetThreadItemByPayloadID already joins payloads for item.PayloadID,
	// so primary payloads carry Kind and Meta without a second SQLite read.
	if item.PayloadID == payloadID && item.PayloadKind != "" {
		return store.PayloadMeta{
			ID:   item.PayloadID,
			Kind: item.PayloadKind,
			Meta: item.PayloadMeta,
		}, nil
	}
	meta, err := a.store.GetPayloadMeta(threadID, payloadID)
	if err != nil {
		return store.PayloadMeta{}, err
	}
	return meta, nil
}

func (a *App) findThreadItemByPayloadID(threadID string, payloadID string) (store.Item, error) {
	item, found, err := a.store.GetThreadItemByPayloadID(threadID, payloadID)
	if err != nil {
		return store.Item{}, fmt.Errorf("find item by payload id %s on thread %s: %w", payloadID, threadID, err)
	}
	if !found {
		return store.Item{}, fmt.Errorf("payload %s not linked to thread %s", payloadID, threadID)
	}
	return item, nil
}
