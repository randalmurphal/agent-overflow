package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// handleUserText routes the wire-confirmation envelope for a user message.
// Four branches (pending-send matching is by IDENTITY when the entry was
// registered with an expected wire id — Claude-family sends mint the uuid
// the CLI echoes back — and FIFO otherwise; see consumeMatchingPendingSend):
//
//  1. AO-initiated direct send: a matching pending-send entry exists
//     for an already-persisted `user:<turnIndex>` row. Stamp
//     `provider_item_id` onto that row, merging the new key into the
//     existing meta so attachments / source-plan refs survive.
//
//  2. AO-initiated queued send: a matching pending-send entry carries
//     a deferred row. Persist the `user:<turnIndex>:flush:<n>` row only
//     after the provider echo supplies a stable `provider_item_id`, so chat
//     history does not get ahead of provider context.
//
//  3. Wire-only, PARENTED: a subagent's task prompt echoed as user-role
//     content. No pending-send match (it isn't an AO send). Persist a
//     nested `user:wire:<provider_item_id>` user_text row under its
//     launching tool_call — genuine conversation content.
//
//  4. Wire-only, TOP-LEVEL: no pending-send match and no parent tool.
//     The pending-send registry is the ground truth for "did WE send
//     this", so an unmatched top-level echo is provider-injected context
//     (an unrecognized CLI wrapper, a cascade injection), NOT
//     user-authored. Persist it as a non-user `notification` row
//     (`injected:wire:<provider_item_id>`), never a user bubble. This
//     closes the class where an unrecognized wrapper (the 2.1.x
//     `<agent-message>` subagent report) surfaced as a top-level user
//     message — incident 2026-07.
//
// Both wire-only branches dedup via wireOnlyUserTextSeen so a
// session-resume replay doesn't double-write. An empty
// `provider_item_id` is a non-stable envelope — skip rather than mint a
// non-stable id we can't dedup on.
func (r *Router) handleUserText(evt provider.ProviderEvent) error {
	if evt.ThreadID == "" {
		return nil
	}
	providerItemID := readProviderItemIDFromMeta(evt.Meta)

	if eventParentID(evt) == "" {
		if pending, ok := r.consumeMatchingPendingSend(evt.ThreadID, providerItemID); ok {
			if pending.DeferredItem != nil {
				return r.persistDeferredUserText(pending, providerItemID, evt)
			}
			return r.attachProviderItemIDToUserRow(evt.ThreadID, pending, providerItemID, evt)
		}
		if r.HasPendingSendForThread(evt.ThreadID) {
			// Sends are outstanding but this echo matched none of their
			// expected ids — either an injected envelope arriving during
			// the queue-wait window (routed to the injected-context row
			// below, working as intended) or, if it recurs for every
			// send, Claude no longer honoring the supplied uuid
			// (binary-contract drift; re-spike per
			// docs/references/spike-policy.md).
			log.Printf("triage: top-level wire user echo %s on %s matched no pending send while sends await confirmation — treating as provider-injected content", providerItemID, evt.ThreadID)
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
	if eventParentID(evt) != "" {
		// Parented: a subagent's task prompt echoed as user-role content,
		// nested under its launching tool_call. Genuine conversation
		// content — persist as the nested user_text row.
		return r.persistWireOnlySubagentPrompt(evt, providerItemID)
	}
	// Top-level echo that matched no pending send: provider-injected
	// context, NOT user-authored. Persist as a non-user notification so it
	// can never masquerade as a user message.
	return r.persistInjectedContextNotification(evt, providerItemID)
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

	mergedMeta, err := usermessage.MergeProviderItemID(item.Meta, providerItemID)
	if err != nil {
		return fmt.Errorf("triage: merge provider_item_id into deferred %s/%s meta: %w", item.ThreadID, item.ID, err)
	}
	item.Meta = mergedMeta
	// Persist via the standard MAX+1 path so the queued message lands
	// AFTER any rows the model emitted between dispatch and this wire
	// echo. Capturing an item_index at dispatch time was the ordering
	// bug: streaming rows occupied the captured slot, then a shift on
	// insert placed the queued message above content that arrived first.
	// ThreadID / TurnIndex / Kind / Role / Status are guaranteed populated
	// by the dispatcher's row construction in app_flush_queue.go.
	if err := r.persistItem(item, nil); err != nil {
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
// Eager flush rows (`user:<turn>:flush:<n>`) get one extra step before
// the stamp: they were persisted quietly at dispatch time, before the
// rows the model emitted between dispatch and this echo, so they are
// repositioned to the turn tail here (BumpItemToTurnEnd) so the queued
// message lands AFTER that content — where Claude actually consumed it.
// This mirrors the interrupt-promote path (PromoteQuietFlushSends).
// Direct sends (`user:<turn>`) and steers (`user:<turn>:steer:<n>`) are
// already at their intended position and must not move, so the
// reposition is scoped to `:flush:` exactly as the interrupt path is.
//
// EXCEPT when the interrupt handler already placed the row
// (AnchoredAtInterrupt — set by the quiet promote, the deferred eager
// persist, and the Codex re-send registration): the row is then already
// at its user-visible position — the interrupt cut point — and rows
// landing between the interrupt and this echo are the interrupted
// turn's post-interrupt tail, which the user watched stream BELOW the
// message. Re-bumping here would leapfrog the message over that tail
// and persist an order the live view never showed (queued message
// reordering bug, 2026-07-03/07-05). Anchored rows stamp only.
//
// Missing-row is a bounded edge case (the AO send must have errored
// after RegisterPendingSend but before the optimistic persist). Log
// once and return nil rather than panic — the send-failure path has
// already surfaced an error row to the user, and a stranded pending
// entry would only mis-route the next wire user_text.
func (r *Router) attachProviderItemIDToUserRow(threadID string, pending pendingSend, providerItemID string, evt provider.ProviderEvent) error {
	aoItemID := pending.AOItemID
	existing, found, err := r.store.GetThreadItem(threadID, aoItemID)
	if err != nil {
		return fmt.Errorf("triage: load user row %s/%s for provider_item_id stamp: %w", threadID, aoItemID, err)
	}
	if !found {
		log.Printf("triage: handleUserText pending match for %s/%s but row absent — skipping stamp", threadID, aoItemID)
		return nil
	}

	mergedMeta, err := usermessage.MergeProviderItemID(existing.Meta, providerItemID)
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
		// Otherwise: provider_item_id already equals providerItemID. This
		// is the expected path for an AO send that minted the id up front
		// (app_send.go stamps it before persist) and Claude echoed it back
		// verbatim, as well as for a genuine duplicate (session-resume
		// replay). The row already carries the id, so skip the redundant
		// write + emit; UpdateCheckpointProviderIDs above still folds in
		// parent_uuid, which only the echo knows.
		return nil
	}

	if existingID := usermessage.ReadProviderItemID(existing.Meta); existingID != "" && existingID != providerItemID {
		// The row already carried a DIFFERENT provider_item_id. For a
		// direct send this means Claude did not honour the top-level uuid
		// we supplied (claude.Session.Send) — a binary-contract drift. We
		// overwrite to the echoed id (the real transcript uuid, the correct
		// slice anchor) and self-heal the checkpoint below, but log loudly:
		// a silent drift would mean every fast send→escape quietly
		// regressed from the UUID-keyed slice to the ordinal-walk fallback.
		// Re-spike the uuid contract per docs/references/spike-policy.md.
		log.Printf("triage: handleUserText user row %s/%s pre-stamped provider_item_id %q but wire echo carried %q — Claude did not honour the supplied uuid; overwriting to the echoed id and re-checking the uuid contract", threadID, aoItemID, existingID, providerItemID)
	}

	// Eager flush rows were persisted quietly at dispatch, before the rows
	// the model emitted between dispatch and this echo. Reposition to the
	// turn tail so the queued message lands AFTER that content, matching
	// where Claude consumed it — the echo-side mirror of the interrupt
	// promote (PromoteQuietFlushSends). Scoped to :flush: so direct sends
	// and steers, already at their intended slot, never move — and skipped
	// when the interrupt handler already anchored the row (see the doc
	// comment above).
	if strings.Contains(aoItemID, ":flush:") && !pending.AnchoredAtInterrupt {
		bumped, bumpErr := r.store.BumpItemToTurnEnd(threadID, aoItemID)
		if bumpErr != nil {
			return fmt.Errorf("triage: reposition flush row %s/%s to turn tail: %w", threadID, aoItemID, bumpErr)
		}
		existing = bumped
		mergedMeta, err = usermessage.MergeProviderItemID(existing.Meta, providerItemID)
		if err != nil {
			return fmt.Errorf("triage: merge provider_item_id into repositioned flush row %s/%s meta: %w", threadID, aoItemID, err)
		}
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

// persistWireOnlySubagentPrompt creates a fresh nested `user_text` row
// for a subagent's task prompt echoed as user-role content (parented by
// its launching tool_call). The id format `user:wire:<provider_item_id>`
// is deterministic from the wire id so repeated arrivals (session resume
// replay) upsert the same row even if the in-memory dedup set has been
// swept. Turn index resolution prefers the open turn (the wire-only
// envelope arrives mid-turn by definition); LastTurnIndex is the
// defensive fallback for races against clearOpenTurn.
//
// This is the PARENTED wire-only branch only. A top-level wire-only echo
// (no parent) is not a subagent prompt and not an AO send — it is
// provider-injected context, routed to persistInjectedContextNotification.
func (r *Router) persistWireOnlySubagentPrompt(evt provider.ProviderEvent, providerItemID string) error {
	turnIndex, ok := r.openTurnIndex(evt.ThreadID)
	if !ok {
		last, err := r.store.LastTurnIndex(evt.ThreadID)
		if err != nil {
			return fmt.Errorf("triage: resolve turn index for wire-only subagent prompt on %s: %w", evt.ThreadID, err)
		}
		turnIndex = last
	}

	metaBytes, err := json.Marshal(map[string]any{
		"provider_item_id": providerItemID,
		"wire_only":        true,
	})
	if err != nil {
		return fmt.Errorf("triage: encode wire-only subagent prompt meta: %w", err)
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

// persistInjectedContextNotification creates a non-user `notification`
// row for provider-injected context that reached the wire-only branch at
// TOP LEVEL (no parent tool, no pending-send match). Such content was NOT
// authored by the user — the pending-send registry is authoritative for
// AO-initiated sends — so it must never persist as a `user_text` bubble.
// This is the safety net that keeps any unrecognized CLI wrapper (a new
// upstream `<...>` shape we don't yet catalogue) from masquerading as the
// user: recognized noise is suppressed upstream in the provider parser;
// whatever slips through surfaces here as a clearly-non-user row.
//
// Role is `system` and kind is `notification`, so the frontend renders it
// through NotificationRow (a subtle system line), not the user bubble.
// The id prefix `injected:wire:` (not `user:wire:`) keeps it out of the
// user-message id space while staying deterministic from the wire id for
// resume-replay idempotency. Summary is a rune-safe preview; the raw body
// is duplicated in the subagent transcript for the known agent-report
// case, and the provider-events log retains the full wire line for any
// genuinely novel shape, so we do not stash a separate payload.
func (r *Router) persistInjectedContextNotification(evt provider.ProviderEvent, providerItemID string) error {
	turnIndex, ok := r.openTurnIndex(evt.ThreadID)
	if !ok {
		last, err := r.store.LastTurnIndex(evt.ThreadID)
		if err != nil {
			return fmt.Errorf("triage: resolve turn index for injected context on %s: %w", evt.ThreadID, err)
		}
		turnIndex = last
	}

	metaBytes, err := json.Marshal(map[string]any{
		"provider_item_id": providerItemID,
		"wire_only":        true,
		"injected":         true,
	})
	if err != nil {
		return fmt.Errorf("triage: encode injected-context meta: %w", err)
	}

	now := eventTimestampMillis(evt)
	item := store.Item{
		ID:        fmt.Sprintf("injected:wire:%s", providerItemID),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      string(provider.ItemNotification),
		Role:      "system",
		Status:    statusCompleted,
		Summary:   injectedContextSummary(evt.Content),
		Meta:      string(metaBytes),
		CreatedAt: now,
		UpdatedAt: now,
	}
	return r.persistItem(item, nil)
}

// injectedContextSummaryMaxRunes bounds the injected-context notification
// summary. Injected bodies can be large (a full subagent report); the row
// is a subtle single-line marker, so a rune-safe preview is enough.
const injectedContextSummaryMaxRunes = 200

// injectedContextSummary builds the notification summary for injected
// content: a fixed label plus a rune-safe, single-line preview of the
// body. Rune-safe (not byte-clipped) so a multi-byte character at the
// cutoff is never split into an invalid rune.
func injectedContextSummary(content string) string {
	preview := strings.TrimSpace(content)
	if i := strings.IndexByte(preview, '\n'); i >= 0 {
		preview = strings.TrimSpace(preview[:i])
	}
	runes := []rune(preview)
	if len(runes) > injectedContextSummaryMaxRunes {
		preview = string(runes[:injectedContextSummaryMaxRunes]) + "…"
	}
	if preview == "" {
		return "Injected provider context"
	}
	return "Injected provider context: " + preview
}
