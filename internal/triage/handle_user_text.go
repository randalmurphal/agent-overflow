package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// handleUserText routes the wire-confirmation envelope for a user message.
// Three branches:
//
//  1. AO-initiated direct send: a matching pending-send FIFO entry exists
//     for an already-persisted `user:<turnIndex>` row. Stamp
//     `provider_item_id` onto that row, merging the new key into the
//     existing meta so attachments / source-plan refs survive.
//
//  2. AO-initiated queued send: a matching pending-send FIFO entry carries
//     a deferred row. Persist the `user:<turnIndex>:flush:<n>` row only
//     after the provider echo supplies a stable `provider_item_id`, so chat
//     history does not get ahead of provider context.
//
//  3. Wire-only cascade injection: no pending-send match. The provider
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

	if eventParentID(evt) == "" {
		if pending, ok := r.consumePendingSendHead(evt.ThreadID); ok {
			if pending.DeferredItem != nil {
				return r.persistDeferredUserText(pending, providerItemID, evt)
			}
			return r.attachProviderItemIDToUserRow(evt.ThreadID, pending.AOItemID, providerItemID, evt)
		}
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

func (r *Router) persistDeferredUserText(pending pendingSend, providerItemID string, evt provider.ProviderEvent) error {
	if pending.DeferredItem == nil {
		return nil
	}
	item := *pending.DeferredItem
	if providerItemID == "" {
		log.Printf("triage: handleUserText popped deferred pending entry %s/%s but wire echo carried no provider_item_id — leaving Zone 2 unconfirmed; check parser coverage for the wire shape", item.ThreadID, item.ID)
		return nil
	}
	now := eventTimestampMillis(evt)
	item.CreatedAt = now
	item.UpdatedAt = now

	mergedMeta, err := mergeProviderItemIDIntoMeta(item.Meta, providerItemID)
	if err != nil {
		return fmt.Errorf("triage: merge provider_item_id into deferred %s/%s meta: %w", item.ThreadID, item.ID, err)
	}
	item.Meta = mergedMeta
	if strings.TrimSpace(item.ThreadID) == "" {
		item.ThreadID = evt.ThreadID
	}
	if item.TurnIndex == 0 && pending.TurnIndex != 0 {
		item.TurnIndex = pending.TurnIndex
	}
	if item.Kind == "" {
		item.Kind = string(provider.ItemUserText)
	}
	if item.Role == "" {
		item.Role = "user"
	}
	if item.Status == "" {
		item.Status = statusCompleted
	}
	if pending.InsertItemIndex != nil {
		if err := r.persistItemAtIndex(item, *pending.InsertItemIndex); err != nil {
			return fmt.Errorf("triage: persist deferred user_text %s/%s at index %d: %w", item.ThreadID, item.ID, *pending.InsertItemIndex, err)
		}
	} else if err := r.persistItem(item, nil); err != nil {
		return fmt.Errorf("triage: persist deferred user_text %s/%s: %w", item.ThreadID, item.ID, err)
	}
	persisted, found, err := r.store.GetThreadItem(item.ThreadID, item.ID)
	if err != nil {
		return fmt.Errorf("triage: reload deferred user_text %s/%s: %w", item.ThreadID, item.ID, err)
	}
	if !found {
		persisted = item
	}
	parentUUID := readParentUUIDFromMeta(evt.Meta)
	r.mu.Lock()
	confirmedHook := r.deferredUserTextConfirmed
	r.mu.Unlock()
	if confirmedHook != nil {
		confirmedHook(item.ThreadID, persisted)
	}
	if err := r.store.UpdateCheckpointProviderIDs(item.ThreadID, item.ID, providerItemID, parentUUID); err != nil {
		return fmt.Errorf("triage: update message checkpoint provider ids: %w", err)
	}
	return nil
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

func readParentUUIDFromMeta(meta json.RawMessage) string {
	if len(meta) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(meta, &m) != nil {
		return ""
	}
	id, _ := m["parent_uuid"].(string)
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
	parentUUID := readParentUUIDFromMeta(evt.Meta)
	if mergedMeta == existing.Meta {
		if err := r.store.UpdateCheckpointProviderIDs(threadID, aoItemID, providerItemID, parentUUID); err != nil {
			return fmt.Errorf("triage: update message checkpoint provider ids: %w", err)
		}
		if providerItemID == "" {
			// We popped a pending-send marker but the wire echo carried
			// no stable id, so meta isn't updated and no upsert emits.
			// The frontend's queue-confirm gate keys on the meta-stamp,
			// so this leaves the queue overlay stuck until the thread
			// is reloaded. Loud log because the most likely cause is a
			// parser gap for a new wire shape — a silent stuck-UI is
			// the worst presentation.
			log.Printf("triage: handleUserText popped pending entry %s/%s but wire echo carried no provider_item_id — queue-confirm path will not fire; check parser coverage for the wire shape", threadID, aoItemID)
		}
		// Otherwise: provider_item_id was already set to the same value
		// (genuine duplicate, e.g. session-resume replay). Skip the
		// redundant write + emit.
		return nil
	}

	existing.Meta = mergedMeta
	existing.UpdatedAt = eventTimestampMillis(evt)
	persisted, err := r.store.UpsertItem(existing, nil)
	if err != nil {
		return fmt.Errorf("triage: upsert user row %s/%s with provider_item_id: %w", threadID, aoItemID, err)
	}
	if err := r.store.UpdateCheckpointProviderIDs(threadID, aoItemID, providerItemID, parentUUID); err != nil {
		return fmt.Errorf("triage: update message checkpoint provider ids: %w", err)
	}
	r.emitItemUpsertWithActivity(persisted, false)
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
		ParentID:  eventParentID(evt),
		Meta:      string(metaBytes),
		CreatedAt: now,
		UpdatedAt: now,
	}
	return r.persistItem(item, nil)
}
