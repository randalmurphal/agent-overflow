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
	store                 *store.Store
	emit                  func(eventName string, data any) // wraps app.Event.Emit
	mu                    sync.Mutex
	textAccumulators      map[string]*strings.Builder // threadID → accumulated assistant text
	reasoningAccumulators map[string]*strings.Builder // threadID → accumulated Codex reasoning
}

// NewRouter creates a triage router.
func NewRouter(st *store.Store, emit func(eventName string, data any)) *Router {
	return &Router{
		store:                 st,
		emit:                  emit,
		textAccumulators:      make(map[string]*strings.Builder),
		reasoningAccumulators: make(map[string]*strings.Builder),
	}
}

// Handle processes a provider event: persists heavy payloads, forwards inline events.
func (r *Router) Handle(evt provider.ProviderEvent) error {
	switch evt.Kind {
	case provider.EventTextDelta:
		return r.handleTextDelta(evt)
	case provider.EventToolStart,
		provider.EventToolComplete,
		provider.EventTurnStart,
		provider.EventApprovalRequest,
		provider.EventApprovalResolved,
		provider.EventSessionStatus,
		provider.EventToolProgress,
		provider.EventCompactBoundary,
		provider.EventRateLimits,
		provider.EventError,
		provider.EventBackgroundStart:
		return r.emitInline(evt)
	case provider.EventTokenUsage:
		return r.handleTokenUsage(evt)
	case provider.EventInit:
		return r.handleInit(evt)
	case provider.EventModelRerouted:
		return r.handleThreadModelUpdate(evt)
	case provider.EventThreadRenamed:
		return r.handleThreadRename(evt)
	case provider.EventTurnComplete:
		return r.handleTurnComplete(evt)
	case provider.EventBackgroundDelta:
		return nil
	case provider.EventBackgroundComplete:
		return r.persistHeavy(evt, "full_text", string(provider.ItemBackgroundDone))
	case provider.EventDiff:
		return r.persistHeavy(evt, "diff", string(provider.ItemDiff))
	case provider.EventCommandOutput:
		return r.persistHeavy(evt, "command_output", string(provider.ItemCommandExecution))
	case provider.EventThinking:
		return r.handleThinking(evt)
	default:
		log.Printf("triage: unhandled event kind: %s", evt.Kind)
		r.emit("provider:event", evt)
		return nil
	}
}

func (r *Router) handleTextDelta(evt provider.ProviderEvent) error {
	r.accumulate(r.textAccumulators, evt.ThreadID, evt.Content)
	return r.emitInline(evt)
}

func (r *Router) handleInit(evt provider.ProviderEvent) error {
	r.emit("provider:event", evt)
	if evt.Meta == nil {
		return nil
	}

	var info provider.SessionInfo
	if json.Unmarshal(evt.Meta, &info) == nil && info.SessionID != "" {
		if err := r.store.UpdateSessionRef(evt.ThreadID, info.SessionID); err != nil {
			log.Printf("triage: update session ref: %v", err)
		}
	}
	return nil
}

func (r *Router) handleThreadModelUpdate(evt provider.ProviderEvent) error {
	r.emit("provider:event", evt)
	if evt.Content == "" {
		return nil
	}
	if err := r.store.UpdateModel(evt.ThreadID, evt.Content); err != nil {
		return fmt.Errorf("update thread model: %w", err)
	}
	return nil
}

func (r *Router) handleThreadRename(evt provider.ProviderEvent) error {
	r.emit("provider:event", evt)
	if evt.Content == "" {
		return nil
	}
	if err := r.store.UpdateTitle(evt.ThreadID, evt.Content); err != nil {
		return fmt.Errorf("update thread title: %w", err)
	}
	return nil
}

func (r *Router) handleTurnComplete(evt provider.ProviderEvent) error {
	text, reasoning := r.drainBoth(evt.ThreadID)

	var persistErr error
	if text != "" {
		persistErr = r.persistTurnText(evt.ThreadID, text)
	}
	if reasoning != "" {
		err := r.persistHeavy(provider.ProviderEvent{
			Kind:      provider.EventThinking,
			ThreadID:  evt.ThreadID,
			Content:   reasoning,
			Timestamp: evt.Timestamp,
		}, "thinking", string(provider.ItemThinking))
		if persistErr == nil && err != nil {
			persistErr = fmt.Errorf("persist reasoning: %w", err)
		}
	}
	if err := r.maybeGenerateClaudeTitle(evt.ThreadID, evt.Timestamp); persistErr == nil && err != nil {
		persistErr = fmt.Errorf("generate thread title: %w", err)
	}

	r.emit("provider:event", evt)
	return persistErr
}

func (r *Router) handleThinking(evt provider.ProviderEvent) error {
	if evt.ItemID != "" {
		return r.persistHeavy(evt, "thinking", string(provider.ItemThinking))
	}
	r.accumulate(r.reasoningAccumulators, evt.ThreadID, evt.Content)
	return nil
}

func (r *Router) maybeGenerateClaudeTitle(threadID string, timestamp time.Time) error {
	thread, err := r.store.GetThread(threadID)
	if err != nil {
		return err
	}
	if thread.Provider != string(provider.Claude) || thread.Title != "New Thread" {
		return nil
	}

	title, err := r.generatedThreadTitle(threadID)
	if err != nil {
		return err
	}
	if title == "" {
		return nil
	}

	meta, err := json.Marshal(map[string]string{"newTitle": title})
	if err != nil {
		return fmt.Errorf("marshal thread title meta: %w", err)
	}
	return r.handleThreadRename(provider.ProviderEvent{
		Kind:      provider.EventThreadRenamed,
		ThreadID:  threadID,
		Content:   title,
		Meta:      meta,
		Timestamp: timestamp,
	})
}

func (r *Router) handleTokenUsage(evt provider.ProviderEvent) error {
	usage, ok := parseTokenUsage(evt.Meta)
	if !ok {
		return r.emitInline(evt)
	}

	model, err := r.lookupThreadModel(evt.ThreadID)
	if err != nil {
		log.Printf("triage: lookup thread model: %v", err)
		return r.emitInline(evt)
	}
	if model == "" {
		return r.emitInline(evt)
	}

	usage.TotalCostUSD = provider.CalculateCost(model, usage)
	if usage.TotalCostUSD == 0 {
		return r.emitInline(evt)
	}

	meta, err := json.Marshal(usage)
	if err != nil {
		log.Printf("triage: marshal token usage: %v", err)
		return r.emitInline(evt)
	}

	evt.Meta = meta
	return r.emitInline(evt)
}

func (r *Router) emitInline(evt provider.ProviderEvent) error {
	r.emit("provider:event", evt)
	return nil
}

func parseTokenUsage(meta json.RawMessage) (provider.TokenUsage, bool) {
	if len(meta) == 0 {
		return provider.TokenUsage{}, false
	}

	var usage provider.TokenUsage
	if err := json.Unmarshal(meta, &usage); err != nil {
		return provider.TokenUsage{}, false
	}
	return usage, true
}

func (r *Router) generatedThreadTitle(threadID string) (string, error) {
	summary, err := r.store.FirstUserMessage(threadID)
	if err != nil {
		return "", err
	}
	return titleFromUserMessage(summary), nil
}

func titleFromUserMessage(content string) string {
	title := strings.TrimSpace(content)
	if title == "" {
		return ""
	}

	if line, _, ok := strings.Cut(title, "\n"); ok {
		title = line
	}

	title = firstSentence(title)
	title = strings.Join(strings.Fields(title), " ")
	if len(title) <= 50 {
		return title
	}
	return strings.TrimSpace(title[:47]) + "..."
}

func firstSentence(content string) string {
	for i, r := range content {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		return strings.TrimSpace(content[:i+1])
	}
	return content
}

func (r *Router) lookupThreadModel(threadID string) (string, error) {
	thread, err := r.store.GetThread(threadID)
	if err != nil {
		return "", err
	}
	return thread.Model, nil
}

func (r *Router) accumulate(target map[string]*strings.Builder, threadID, content string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	acc, ok := target[threadID]
	if !ok {
		acc = &strings.Builder{}
		target[threadID] = acc
	}
	acc.WriteString(content)
}

// drainBoth atomically drains both text and reasoning accumulators in a single
// critical section. This prevents a concurrent handleThinking from writing
// between two separate drain calls, which would attribute reasoning to the
// wrong turn.
func (r *Router) drainBoth(threadID string) (text, reasoning string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if acc, ok := r.textAccumulators[threadID]; ok && acc.Len() > 0 {
		text = acc.String()
	}
	delete(r.textAccumulators, threadID)

	if acc, ok := r.reasoningAccumulators[threadID]; ok && acc.Len() > 0 {
		reasoning = acc.String()
	}
	delete(r.reasoningAccumulators, threadID)

	return text, reasoning
}

// CleanupThread removes all accumulator state for a thread. Call this when a
// session ends or disconnects to prevent memory leaks.
func (r *Router) CleanupThread(threadID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.textAccumulators, threadID)
	delete(r.reasoningAccumulators, threadID)
}

func (r *Router) persistTurnText(threadID, content string) error {
	now := time.Now().UnixMilli()
	turnIndex, err := r.store.LastTurnIndex(threadID)
	if err != nil {
		log.Printf("triage: last turn index: %v (defaulting to 0)", err)
		turnIndex = 0
	}

	itemIndex, err := r.store.NextItemIndex(threadID, turnIndex)
	if err != nil {
		log.Printf("triage: next item index: %v (defaulting to 0)", err)
		itemIndex = 0
	}

	item := store.Item{
		ID:        uuid.New().String(),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      string(provider.ItemText),
		Role:      "assistant",
		Summary:   content,
		CreatedAt: now,
	}
	if err := r.store.InsertItem(item); err != nil {
		return fmt.Errorf("persist assistant text: %w", err)
	}
	return nil
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
		data, err := json.Marshal(dm)
		if err != nil {
			log.Printf("triage: marshal diff meta: %v", err)
			return "{}"
		}
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
		data, err := json.Marshal(cm)
		if err != nil {
			log.Printf("triage: marshal command output meta: %v", err)
			return "{}"
		}
		return string(data)
	case "thinking":
		tm := ExtractThinkingMeta(evt.Content)
		data, err := json.Marshal(tm)
		if err != nil {
			log.Printf("triage: marshal thinking meta: %v", err)
			return "{}"
		}
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
