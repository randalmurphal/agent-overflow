package triage

import (
	"encoding/json"
	"fmt"
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// handleUserText routes the wire-confirmation envelope for a user message.
// Two branches:
//
//  1. AO-initiated send: a matching pending-send FIFO entry exists. Stamp
//     `provider_item_id` onto the existing `user:<turnIndex>` row that
//     `app_send.go` already persisted optimistically, merging the new key
//     into the row's existing meta so attachments / source-plan refs
//     survive. The wire content is for traceability; AO's persisted
//     summary stays authoritative because the send path already trims and
//     normalises it.
//
//  2. Wire-only cascade injection: no pending-send match. The provider
//     injected a user message into the agent's context (Claude
//     `task_notification` echo today; future Codex MCP-injected user
//     input). Persist a fresh row with id `user:wire:<provider_item_id>`
//     so the timeline mirrors the agent's actual context at wire-arrival
//     timing. Dedup via wireOnlyUserTextSeen so a session-resume replay
//     doesn't double-write.
//
// An empty `provider_item_id` in the wire-only branch is a non-stable
// envelope — skip rather than mint a non-stable id we can't dedup on.
func (r *Router) handleUserText(evt provider.ProviderEvent) error {
	if evt.ThreadID == "" {
		return nil
	}
	providerItemID := readProviderItemIDFromMeta(evt.Meta)

	if pending, ok := r.consumePendingSendHead(evt.ThreadID); ok {
		return r.attachProviderItemIDToUserRow(evt.ThreadID, pending.AOItemID, providerItemID, evt)
	}

	if providerItemID == "" {
		// No id to dedup on — skip rather than mint a non-stable id.
		// A wire-only envelope without a provider_item_id is a
		// non-cascading replay we can't correlate.
		return nil
	}
	if !r.markWireOnlyUserTextSeen(evt.ThreadID, providerItemID) {
		return nil // duplicate — already persisted on prior arrival
	}
	return r.persistWireOnlyUserText(evt, providerItemID)
}

// readProviderItemIDFromMeta extracts `provider_item_id` from the event
// meta. Both Phase B (Claude replay envelope) and Phase C (Codex
// userMessage item) set the key as a top-level string. Defensive parse:
// returns empty on absent / parse failure / non-string value so callers
// don't have to repeat the same defensive shape.
func readProviderItemIDFromMeta(meta json.RawMessage) string {
	if len(meta) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(meta, &m) != nil {
		return ""
	}
	id, _ := m["provider_item_id"].(string)
	return id
}

// attachProviderItemIDToUserRow merges `provider_item_id` onto the
// existing AO-persisted user row's meta and re-emits the upsert. The
// row's Summary stays untouched: AO already trimmed/cleaned content on
// enqueue, and overwriting from the wire would clobber attachments
// rendering and revision provenance the user-facing summary already
// reflects.
//
// Missing-row is a bounded edge case (the AO send must have errored
// after RegisterPendingSend but before the optimistic persist). Log
// once and return nil rather than panic — the send-failure path has
// already surfaced an error row to the user, and a stranded pending
// entry would only mis-route the next wire user_text.
func (r *Router) attachProviderItemIDToUserRow(threadID, aoItemID, providerItemID string, evt provider.ProviderEvent) error {
	existing, found, err := r.store.GetThreadItem(threadID, aoItemID)
	if err != nil {
		return fmt.Errorf("triage: load user row %s/%s for provider_item_id stamp: %w", threadID, aoItemID, err)
	}
	if !found {
		log.Printf("triage: handleUserText pending match for %s/%s but row absent — skipping stamp", threadID, aoItemID)
		return nil
	}

	mergedMeta, err := mergeProviderItemIDIntoMeta(existing.Meta, providerItemID)
	if err != nil {
		return fmt.Errorf("triage: merge provider_item_id into %s/%s meta: %w", threadID, aoItemID, err)
	}
	if mergedMeta == existing.Meta {
		// Nothing to change — provider_item_id was empty or already set
		// to the same value. Skip the redundant write + emit.
		return nil
	}

	existing.Meta = mergedMeta
	existing.UpdatedAt = eventTimestampMillis(evt)
	persisted, err := r.store.UpsertItem(existing, nil)
	if err != nil {
		return fmt.Errorf("triage: upsert user row %s/%s with provider_item_id: %w", threadID, aoItemID, err)
	}
	r.emitItemUpsert(persisted)
	return nil
}

// mergeProviderItemIDIntoMeta returns a JSON string that carries the
// existing meta keys plus `provider_item_id`. An empty providerItemID
// returns the original meta unchanged (no-op for callers that don't
// have a wire id). An empty/whitespace existing meta still produces a
// well-formed `{"provider_item_id":"..."}` so the row's meta stays
// valid JSON.
func mergeProviderItemIDIntoMeta(existing, providerItemID string) (string, error) {
	if providerItemID == "" {
		return existing, nil
	}
	merged := map[string]any{}
	if existing != "" {
		if err := json.Unmarshal([]byte(existing), &merged); err != nil {
			return "", fmt.Errorf("decode existing meta: %w", err)
		}
		if merged == nil {
			merged = map[string]any{}
		}
	}
	if cur, ok := merged["provider_item_id"].(string); ok && cur == providerItemID {
		return existing, nil
	}
	merged["provider_item_id"] = providerItemID
	encoded, err := json.Marshal(merged)
	if err != nil {
		return "", fmt.Errorf("encode merged meta: %w", err)
	}
	return string(encoded), nil
}

// persistWireOnlyUserText creates a fresh `user_text` row representing
// a cascade-injected user message — Claude's `task_notification` echo
// or future Codex MCP-injected user input. The id format
// `user:wire:<provider_item_id>` is deterministic from the wire id so
// repeated arrivals (session resume replay) upsert the same row even
// if the in-memory dedup set has been swept. Turn index resolution
// prefers the open turn (the wire-only envelope arrives mid-turn by
// definition); LastTurnIndex is the defensive fallback for races
// against clearOpenTurn.
func (r *Router) persistWireOnlyUserText(evt provider.ProviderEvent, providerItemID string) error {
	turnIndex, ok := r.openTurnIndex(evt.ThreadID)
	if !ok {
		last, err := r.store.LastTurnIndex(evt.ThreadID)
		if err != nil {
			return fmt.Errorf("triage: resolve turn index for wire-only user_text on %s: %w", evt.ThreadID, err)
		}
		turnIndex = last
	}

	metaBytes, err := json.Marshal(map[string]any{
		"provider_item_id": providerItemID,
		"wire_only":        true,
	})
	if err != nil {
		return fmt.Errorf("triage: encode wire-only user_text meta: %w", err)
	}

	now := eventTimestampMillis(evt)
	item := store.Item{
		ID:        fmt.Sprintf("user:wire:%s", providerItemID),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      string(provider.ItemUserText),
		Role:      "user",
		Status:    statusCompleted,
		Summary:   evt.Content,
		Meta:      string(metaBytes),
		CreatedAt: now,
		UpdatedAt: now,
	}
	return r.persistItem(item, nil)
}
