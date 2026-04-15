// Package triage classifies provider events and routes them to the frontend
// (small/inline) or SQLite (heavy payloads like diffs and command output).
package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// Router classifies provider events and routes them.
type Router struct {
	store            *store.Store
	emit             func(eventName string, data any) // wraps runtime.EventsEmit
	mu               sync.Mutex
	textAccumulators map[string]*strings.Builder // threadID → accumulated assistant text
}

// NewRouter creates a triage router.
func NewRouter(st *store.Store, emit func(eventName string, data any)) *Router {
	return &Router{
		store:            st,
		emit:             emit,
		textAccumulators: make(map[string]*strings.Builder),
	}
}

// Handle processes a provider event: persists heavy payloads, forwards inline events.
func (r *Router) Handle(evt provider.ProviderEvent) error {
	switch evt.Kind {

	// --- Inline events: forward directly ---

	case provider.EventTextDelta:
		// Accumulate text for persistence and emit to frontend.
		r.mu.Lock()
		acc, ok := r.textAccumulators[evt.ThreadID]
		if !ok {
			acc = &strings.Builder{}
			r.textAccumulators[evt.ThreadID] = acc
		}
		acc.WriteString(evt.Content)
		r.mu.Unlock()
		r.emit("provider:event", evt)
		return nil

	case provider.EventToolStart,
		provider.EventToolComplete,
		provider.EventTurnStart,
		provider.EventApprovalRequest,
		provider.EventApprovalResolved,
		provider.EventSessionStatus,
		provider.EventTokenUsage,
		provider.EventError,
		provider.EventBackgroundStart:
		r.emit("provider:event", evt)
		return nil

	case provider.EventInit:
		r.emit("provider:event", evt)
		// Update thread session_ref from init metadata.
		if evt.Meta != nil {
			var info provider.SessionInfo
			if json.Unmarshal(evt.Meta, &info) == nil && info.SessionID != "" {
				if err := r.store.UpdateSessionRef(evt.ThreadID, info.SessionID); err != nil {
					log.Printf("triage: update session ref: %v", err)
				}
			}
		}
		return nil

	case provider.EventTurnComplete:
		// If text was accumulated during this turn, persist it as an assistant message item.
		r.mu.Lock()
		acc, ok := r.textAccumulators[evt.ThreadID]
		var content string
		if ok && acc.Len() > 0 {
			content = acc.String()
			acc.Reset()
		}
		r.mu.Unlock()

		if content != "" {
			now := time.Now().UnixMilli()
			turnIndex, err := r.store.LastTurnIndex(evt.ThreadID)
			if err != nil {
				log.Printf("triage: last turn index: %v (defaulting to 0)", err)
				turnIndex = 0
			}
			itemIndex, err := r.store.NextItemIndex(evt.ThreadID, turnIndex)
			if err != nil {
				log.Printf("triage: next item index: %v (defaulting to 0)", err)
				itemIndex = 0
			}
			item := store.Item{
				ID:        uuid.New().String(),
				ThreadID:  evt.ThreadID,
				TurnIndex: turnIndex,
				ItemIndex: itemIndex,
				Kind:      string(provider.ItemText),
				Role:      "assistant",
				Summary:   content,
				CreatedAt: now,
			}
			if err := r.store.InsertItem(item); err != nil {
				r.emit("provider:event", evt)
				return fmt.Errorf("persist assistant text: %w", err)
			}
		}
		r.emit("provider:event", evt)
		return nil

	case provider.EventBackgroundDelta:
		// Accumulate in memory — do not emit per-delta.
		return nil

	case provider.EventBackgroundComplete:
		return r.persistHeavy(evt, "full_text", string(provider.ItemBackgroundDone))

	// --- Heavy events: extract meta, persist, emit meta ---

	case provider.EventDiff:
		return r.persistHeavy(evt, "diff", string(provider.ItemDiff))

	case provider.EventCommandOutput:
		return r.persistHeavy(evt, "command_output", string(provider.ItemCommandExecution))

	case provider.EventThinking:
		return r.persistHeavy(evt, "thinking", string(provider.ItemThinking))

	default:
		log.Printf("triage: unhandled event kind: %s", evt.Kind)
		r.emit("provider:event", evt)
		return nil
	}
}

// persistHeavy extracts meta, stores payload + item, emits meta to frontend.
func (r *Router) persistHeavy(evt provider.ProviderEvent, payloadKind string, itemKind string) error {
	now := time.Now().UnixMilli()
	metaJSON := buildPayloadMeta(payloadKind, evt)
	turnIndex, err := r.store.LastTurnIndex(evt.ThreadID)
	if err != nil {
		log.Printf("triage: last turn index: %v (defaulting to 0)", err)
		turnIndex = 0
	}

	payloadID := uuid.New().String()
	itemID := evt.ItemID
	if itemID == "" {
		itemID = uuid.New().String()
	}

	var hasExisting bool
	if evt.Replace {
		existing, found, findErr := r.store.FindTurnItem(evt.ThreadID, turnIndex, itemKind)
		if findErr != nil {
			log.Printf("triage: find turn item: %v", findErr)
		} else if found && existing.PayloadID != "" {
			hasExisting = true
			payloadID = existing.PayloadID
			itemID = existing.ID
		} else if found && existing.ID != "" {
			hasExisting = true
			itemID = existing.ID
		}
	}

	r.emitPayloadMeta(payloadID, evt.ThreadID, payloadKind, metaJSON, now)

	payload := store.Payload{
		ID:        payloadID,
		Kind:      payloadKind,
		Meta:      metaJSON,
		Data:      []byte(evt.Content),
		CreatedAt: now,
	}
	if evt.Replace {
		return r.replaceHeavy(evt, payloadKind, itemKind, payload, itemID, metaJSON, now, turnIndex, hasExisting)
	}
	if err := r.store.InsertPayload(payload); err != nil {
		return fmt.Errorf("persist payload: %w", err)
	}
	return r.insertHeavyItem(evt, payloadKind, itemKind, payloadID, itemID, metaJSON, now, turnIndex)
}

func (r *Router) replaceHeavy(
	evt provider.ProviderEvent,
	payloadKind, itemKind string,
	payload store.Payload,
	itemID, metaJSON string,
	now int64,
	turnIndex int,
	hasExisting bool,
) error {
	if err := r.store.UpsertTurnPayload(evt.ThreadID, turnIndex, payloadKind, payload); err != nil {
		return fmt.Errorf("persist payload: %w", err)
	}

	summary := buildSummary(payloadKind, metaJSON)
	if hasExisting {
		if err := r.store.UpdateItemPayload(itemID, payload.ID, summary, now); err != nil {
			return fmt.Errorf("persist item: %w", err)
		}
		return nil
	}
	return r.insertHeavyItem(evt, payloadKind, itemKind, payload.ID, itemID, metaJSON, now, turnIndex)
}

func (r *Router) insertHeavyItem(
	evt provider.ProviderEvent,
	payloadKind, itemKind, payloadID, itemID, metaJSON string,
	now int64,
	turnIndex int,
) error {
	itemIndex, err := r.store.NextItemIndex(evt.ThreadID, turnIndex)
	if err != nil {
		log.Printf("triage: next item index: %v (defaulting to 0)", err)
		itemIndex = 0
	}

	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      itemKind,
		Role:      "assistant",
		Summary:   buildSummary(payloadKind, metaJSON),
		PayloadID: payloadID,
		CreatedAt: now,
	}
	if err := r.store.InsertItem(item); err != nil {
		return fmt.Errorf("persist item: %w", err)
	}

	return nil
}

func (r *Router) emitPayloadMeta(payloadID, threadID, kind, meta string, createdAt int64) {
	r.emit("provider:meta", struct {
		ID        string `json:"id"`
		ThreadID  string `json:"threadId"`
		Kind      string `json:"kind"`
		Meta      string `json:"meta"`
		CreatedAt int64  `json:"createdAt"`
	}{
		ID:        payloadID,
		ThreadID:  threadID,
		Kind:      kind,
		Meta:      meta,
		CreatedAt: createdAt,
	})
}

func buildPayloadMeta(payloadKind string, evt provider.ProviderEvent) string {
	switch payloadKind {
	case "diff":
		dm := ExtractDiffMeta(evt.Content)
		data, _ := json.Marshal(dm)
		return string(data)
	case "command_output":
		cm := ExtractCommandOutputMeta(evt.Content, "", 0)
		if evt.Meta != nil {
			var parsed struct {
				Command  string `json:"command"`
				ExitCode int    `json:"exitCode"`
			}
			if json.Unmarshal(evt.Meta, &parsed) == nil {
				cm = ExtractCommandOutputMeta(evt.Content, parsed.Command, parsed.ExitCode)
			}
		}
		data, _ := json.Marshal(cm)
		return string(data)
	case "thinking":
		tm := ExtractThinkingMeta(evt.Content)
		data, _ := json.Marshal(tm)
		return string(data)
	default:
		return "{}"
	}
}

// buildSummary creates a short human-readable summary from meta.
func buildSummary(payloadKind, metaJSON string) string {
	switch payloadKind {
	case "diff":
		var dm DiffMeta
		if json.Unmarshal([]byte(metaJSON), &dm) == nil {
			return fmt.Sprintf("%s: +%d/-%d %s", dm.ChangeKind, dm.Insertions, dm.Deletions, dm.FilePath)
		}
	case "command_output":
		var cm CommandOutputMeta
		if json.Unmarshal([]byte(metaJSON), &cm) == nil {
			return fmt.Sprintf("$ %s (exit %d, %d lines)", cm.Command, cm.ExitCode, cm.LineCount)
		}
	case "thinking":
		var tm ThinkingMeta
		if json.Unmarshal([]byte(metaJSON), &tm) == nil {
			return tm.Preview
		}
	}
	return ""
}
