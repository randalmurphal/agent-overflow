package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// command_result.go — the persisted row for a slash command the provider CLI
// executed itself (Claude's `<synthetic>`-model assistant envelope; see
// docs/references/claude-wire.md §"Slash commands").
//
// It is history, not live state: the user asked for `/usage` and the answer
// belongs in the transcript. It is NOT model output, which is why it gets its
// own kind rather than riding `assistant_text` — the renderer must not
// attribute it to the agent, and nothing that walks a turn's reply may pick it
// up.
//
// The envelope is a complete snapshot (no `stream_event` deltas are emitted for
// command output on 2.1.219), so the row is written completed in one shot. The
// trailing `result` envelope repeats the same text in `result.result`;
// parse_result.go reads that field only to build an error message, so no second
// row can arise from it — TestCommandResultSequenceProducesOneRow pins that.

const (
	itemKindCommandResult = "command_result"
	// payloadKindCommandResult holds the full output when it exceeds the
	// inline bound. Same split as thinking: preview in the row summary, bytes
	// on demand.
	payloadKindCommandResult = "command_result"
	// commandResultInlineRunes is the bound below which the whole output stays
	// in items.summary. Local command output is small in practice (`/usage`,
	// `/cost`, `/status` are tens of lines), so most rows never allocate a
	// payload; `/context` and a verbose skill can exceed it.
	commandResultInlineRunes = 4000
)

// commandResultMeta is the items.meta shape for a command_result row. It is a
// contract with the frontend row that lands in a later phase.
//
// Command is deliberately absent: the CLI reports the command's NAME only
// afterwards, on the `<command-name>` metadata echo that arrives after this
// envelope and is suppressed as injected content. Correlating the two would
// mean holding parser state across envelopes for a cosmetic label; the row is
// legible without it.
type commandResultMeta struct {
	Kind string `json:"kind"`
	// Preview is the head of the output, always present. When Truncated is
	// false it IS the whole output and the row needs no payload fetch.
	Preview   string `json:"preview"`
	Truncated bool   `json:"truncated,omitempty"`
	// TotalBytes is the full output length, present only when truncated, so a
	// collapsed row can say how much is behind the fetch.
	TotalBytes int `json:"totalBytes,omitempty"`
}

// handleCommandResult persists one command_result row.
//
// The row is attributed to the thread's current turn — a local command runs
// inside a wire round of its own (`system/init` → synthetic assistant →
// `result`), so there is always a turn index to hang it on, and the
// currentTurnIndex fallback to 0 matches every other timeline-notification-
// style writer.
func (r *Router) handleCommandResult(evt provider.ProviderEvent) error {
	text := strings.TrimSpace(evt.Content)
	if text == "" {
		// A command that printed nothing has no row worth showing. The parser
		// already drops these; this is the belt for a synthesized event.
		return nil
	}

	turnIndex := r.timelineNotificationTurnIndex(evt.ThreadID)
	itemID := commandResultItemID(evt, turnIndex, r)
	now := eventTimestampMillis(evt)

	preview := truncateRunes(text, commandResultInlineRunes)
	truncated := preview != text
	meta := commandResultMeta{
		Kind:      itemKindCommandResult,
		Preview:   preview,
		Truncated: truncated,
	}
	if truncated {
		meta.TotalBytes = len(text)
	}
	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("command result marshal meta %s: %w", itemID, err)
	}

	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      itemKindCommandResult,
		Role:      "system",
		Status:    statusCompleted,
		Summary:   preview,
		ParentID:  eventParentID(evt),
		Meta:      string(encodedMeta),
		CreatedAt: now,
		UpdatedAt: now,
	}
	existing, found, err := r.store.GetThreadItem(evt.ThreadID, item.ID)
	if err != nil {
		return fmt.Errorf("command result existing lookup %s: %w", item.ID, err)
	}
	if found {
		item.CreatedAt = existing.CreatedAt
		item.ItemIndex = existing.ItemIndex
		item.PayloadID = existing.PayloadID
	}

	if !truncated {
		return r.persistItem(item, nil)
	}
	// Oversized output: the summary keeps the preview and the full bytes go to
	// a payload the frontend fetches on demand — triage's standing rule that
	// meta is cheap and data is heavy.
	return r.attachPayloadToItem(item, evt, payloadKindCommandResult, preview, true)
}

// commandResultItemID resolves a stable row id.
//
// The synthetic envelope carries the CLI's own `message.id`, which the parser
// forwards; it is unique per command run and makes the write idempotent if the
// same envelope is ever replayed. The fallback exists only for an envelope that
// carried none: it allocates from the same per-(thread, turn) counter the
// timeline-notification rows use, under its own scope label, so ids stay
// distinct across kinds and get swept by the same CleanupThread pass.
func commandResultItemID(evt provider.ProviderEvent, turnIndex int, r *Router) string {
	if id := strings.TrimSpace(eventItemID(evt)); id != "" {
		return "command-result:" + id
	}
	seq := r.nextScopeSequence(evt.ThreadID, turnIndex, itemKindCommandResult)
	id := fmt.Sprintf("command-result:%d:%d", turnIndex, seq)
	log.Printf("triage: command_result on %s carried no provider id; allocated %s", evt.ThreadID, id)
	return id
}
