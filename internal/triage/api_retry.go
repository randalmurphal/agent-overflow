// Package triage — handler for transient-retry envelopes from both
// providers. Renders as an inline timeline row (kind `api_retry`) that
// updates in place across re-attempts and flips to completed on the
// next forward-progress event for the thread. Hides the first three
// attempts to mirror Claude Code's `SystemAPIErrorMessage` behavior:
// most retries succeed silently, so the noise is wasted on the user.
//
// There is no resolution-counterpart wire event from either Claude or
// Codex — we don't wait for one. The row is a historical record of
// the retry attempt itself, not a banner needing later clearing.

package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// apiRetryHideAttemptsBelow is the visibility threshold. Mirrors
// Claude Code's `SystemAPIErrorMessage.tsx:27,38-40` `hidden =
// retryAttempt < 4` — most retries succeed within three attempts and
// surfacing them as a sticky row pollutes the chat with noise the
// user can't act on. When unknown (Codex doesn't always emit a
// structured count), we treat the retry as visible: a one-off
// reconnect is rare enough that "show on first observation" is a
// safer default than "always hide."
const apiRetryHideAttemptsBelow = 4

// retryEventMeta is the typed view of an EventAPIRetry's Meta. Both
// providers normalize their wire shapes to this form upstream — see
// claude/parse_system.go:buildClaudeAPIRetryMeta and codex/protocol.go
// for the producers. Missing fields stay zero so the handler can fall
// back to a generic "Retrying..." label without inventing detail.
type retryEventMeta struct {
	Attempt    int    `json:"attempt"`
	MaxRetries int    `json:"max_retries"`
	Error      string `json:"error"`
}

// handleAPIRetry routes EventAPIRetry to a per-turn upserted timeline
// row. The deterministic id `retry:<turnIndex>` collapses re-attempts
// onto a single row that updates in place; a forward-progress event
// (text delta, tool start, turn complete, etc) flips the row's status
// from running to completed so it reads as historical.
func (r *Router) handleAPIRetry(evt provider.ProviderEvent) error {
	meta := decodeRetryEventMeta(evt.Meta)

	// Visibility gate — drop attempts below the threshold.
	if meta.Attempt > 0 && meta.Attempt < apiRetryHideAttemptsBelow {
		return nil
	}

	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		// Without a turn index we can't allocate a deterministic id;
		// drop rather than fabricate. Retries before the first turn
		// is open are an unsupported wire ordering — log via the
		// existing turn-index error path elsewhere.
		return nil
	}

	now := eventTimestampMillis(evt)
	itemID := apiRetryItemID(turnIndex)

	rowMeta, _ := json.Marshal(map[string]any{
		"kind":        itemKindAPIRetry,
		"attempt":     meta.Attempt,
		"max_retries": meta.MaxRetries,
		"error":       meta.Error,
	})

	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      itemKindAPIRetry,
		Role:      "system",
		Status:    statusRunning,
		Summary:   apiRetrySummary(meta),
		Meta:      string(rowMeta),
		CreatedAt: now,
		UpdatedAt: now,
	}

	if existing, found, err := r.store.GetThreadItem(evt.ThreadID, item.ID); err == nil && found {
		// Re-attempt — preserve item_index/payload but refresh the
		// summary, meta and updatedAt. Once the row has flipped to
		// completed (via maybeMarkAPIRetryCompleted), keep it
		// completed; an out-of-order retry observation late in the
		// turn shouldn't reopen the row.
		item.CreatedAt = existing.CreatedAt
		item.ItemIndex = existing.ItemIndex
		item.PayloadID = existing.PayloadID
		if existing.Status == statusCompleted {
			item.Status = statusCompleted
		}
	} else if err != nil {
		return fmt.Errorf("api_retry existing lookup %s: %w", item.ID, err)
	}

	if err := r.persistItem(item, nil); err != nil {
		return err
	}

	// Flag the thread so the streaming-hot maybeMarkAPIRetryCompleted
	// path knows there's a row to flip. Late-attempt observations on
	// an already-completed row leave the flag unset, so we don't
	// re-arm the per-event DB lookup for a row that doesn't need
	// flipping.
	if item.Status == statusRunning {
		r.markOpenAPIRetryRow(evt.ThreadID)
	}
	return nil
}

func (r *Router) markOpenAPIRetryRow(threadID string) {
	r.mu.Lock()
	r.state(threadID).openAPIRetryRow = true
	r.mu.Unlock()
}

func (r *Router) clearOpenAPIRetryRow(threadID string) {
	r.mu.Lock()
	if st := r.threadStateIfPresent(threadID); st != nil {
		st.openAPIRetryRow = false
	}
	r.mu.Unlock()
}

func (r *Router) hasOpenAPIRetryRow(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	return st != nil && st.openAPIRetryRow
}

// isAPIRetryForwardProgress reports whether an event kind counts as
// forward-progress for an open api_retry row — i.e. the wire proved
// the provider's next API call succeeded enough to keep going. Most
// content-bearing events qualify; EventAPIRetry itself, EventError,
// and pure status/lifecycle signals do not (a retry that opens then
// closes itself, or an error that cuts the turn, leave the running
// retry row as the historical context for what came next).
//
// Lives next to maybeMarkAPIRetryCompleted so a future change to
// "what counts as forward progress" lands in one file.
func isAPIRetryForwardProgress(kind provider.EventKind) bool {
	switch kind {
	case provider.EventTextDelta,
		provider.EventThinking,
		provider.EventContentBlockStart,
		provider.EventToolStart,
		provider.EventToolComplete,
		provider.EventTurnComplete,
		provider.EventBackgroundTaskTerminal,
		provider.EventBackgroundTaskNotification,
		provider.EventTodoUpdate,
		provider.EventProposedPlan,
		provider.EventTokenUsage:
		return true
	}
	return false
}

// maybeMarkAPIRetryCompleted flips an open `api_retry` row to
// completed when a forward-progress event arrives for the thread. The
// row stays in the timeline as a historical "we paused, then kept
// going" marker — the row's status discriminator is what the frontend
// uses to switch from a pulsing indicator to a static row. Idempotent:
// no row, already-completed row, or no open turn all return cleanly.
//
// Hot-path optimization: the per-event call site (router.go:Handle for
// every text delta / thinking / tool / token-usage / etc) checks the
// in-memory openAPIRetryRows flag first. The vast majority of streams
// have no open retry row, and the early return avoids a SQLite
// GetThreadItem per event during streaming. The flag is set in
// handleAPIRetry and cleared on flip-to-completed / clearOpenTurn /
// CleanupThread.
//
// Forward-progress callers: stream text/thinking, tool start/complete,
// turn complete. EventAPIRetry itself is NOT a forward-progress event
// (it would close the row it just opened); EventError is also not
// forward progress (the error closes the turn — the api_retry row
// stays as-is, the turn's overall outcome is captured separately).
func (r *Router) maybeMarkAPIRetryCompleted(threadID string) {
	if !r.hasOpenAPIRetryRow(threadID) {
		return
	}
	turnIndex, ok := r.openTurnIndex(threadID)
	if !ok {
		return
	}
	itemID := apiRetryItemID(turnIndex)
	existing, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil || !found {
		// Stale flag — clear so the next streaming event short-circuits.
		r.clearOpenAPIRetryRow(threadID)
		return
	}
	if existing.Status == statusCompleted {
		r.clearOpenAPIRetryRow(threadID)
		return
	}
	existing.Status = statusCompleted
	existing.UpdatedAt = time.Now().UnixMilli()
	if err := r.persistItem(existing, nil); err != nil {
		// Non-fatal — the row stays as running, the next forward-
		// progress event will retry. Log so a persistent failure is
		// visible in operator logs. Leave the flag set so we try
		// again on the next event.
		log.Printf("triage: mark api_retry completed for thread %s: %v", threadID, err)
		return
	}
	r.clearOpenAPIRetryRow(threadID)
}

func apiRetryItemID(turnIndex int) string {
	return fmt.Sprintf("retry:%d", turnIndex)
}

// apiRetrySummary builds the row's one-line summary. The provider's
// error string is provider-controlled and can be arbitrary length;
// cap it before interpolation so a malformed retry meta can't push a
// multi-line summary onto the timeline.
func apiRetrySummary(meta retryEventMeta) string {
	errStr := truncateRunes(strings.TrimSpace(meta.Error), maxAPIRetryErrorChars)
	switch {
	case meta.Attempt > 0 && meta.MaxRetries > 0 && errStr != "":
		return fmt.Sprintf("Retrying (%d/%d, %s)", meta.Attempt, meta.MaxRetries, errStr)
	case meta.Attempt > 0 && meta.MaxRetries > 0:
		return fmt.Sprintf("Retrying (%d/%d)", meta.Attempt, meta.MaxRetries)
	case errStr != "":
		return "Retrying — " + errStr
	default:
		return "Retrying provider request..."
	}
}

const maxAPIRetryErrorChars = 120

func decodeRetryEventMeta(raw json.RawMessage) retryEventMeta {
	var meta retryEventMeta
	if len(raw) == 0 {
		return meta
	}
	_ = json.Unmarshal(raw, &meta)
	return meta
}
