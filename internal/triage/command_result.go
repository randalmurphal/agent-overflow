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
// attribute it to the parent agent. A forked command can stamp its own agent
// source so the renderer presents that answer as sourced Markdown without
// changing the durable role.
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
	commandResultInlineRunes      = 4000
	commandAgentResultLaunchRunes = 512
	commandAgentResultKindRunes   = 32
	commandAgentResultNameRunes   = 128
)

// commandResultMeta is the items.meta shape for a command_result row. It is a
// contract with the frontend row that lands in a later phase.
//
// Command is deliberately absent: the CLI reports the command's NAME only
// afterwards, on the `<command-name>` metadata echo that arrives after this
// envelope — which handleUserText consumes as the send's echo (stamping the
// optimistic user row) or drops when unmatched, never showing the XML.
// Correlating the two would mean holding parser state across envelopes for a
// cosmetic label; the row is legible without it.
type commandResultMeta struct {
	Kind string `json:"kind"`
	// Preview is the head of the output, always present. When Truncated is
	// false it IS the whole output and the row needs no payload fetch.
	Preview   string `json:"preview"`
	Truncated bool   `json:"truncated,omitempty"`
	// TotalBytes is the full output length, present only when truncated, so a
	// collapsed row can say how much is behind the fetch.
	TotalBytes int `json:"totalBytes,omitempty"`
	// AgentResult is present only for a command that ran as a forked agent.
	// It changes presentation and attribution. The durable row remains
	// system-authored command output.
	AgentResult *provider.CommandAgentResultMeta `json:"agentResult,omitempty"`
}

// CommandResultRow is the shaped `command_result` row content: what goes
// in items.summary, what goes in items.meta, and whether the output was
// too large to live inline (in which case the caller owns a
// `command_result` payload holding the full text).
type CommandResultRow struct {
	Summary   string
	Meta      string
	Oversized bool
}

// BuildCommandResultRow shapes one provider-executed command's output
// into the row fields both writers persist. Pure: text in, field values
// out. The caller decides where the full bytes live when Oversized.
func BuildCommandResultRow(text string) (CommandResultRow, error) {
	return buildCommandResultRow(text, nil)
}

// BuildCommandResultRowWithAgentResult shapes provider command output whose
// visible answer came from a child agent. Imports use this to preserve the
// same durable attribution as the live router.
func BuildCommandResultRowWithAgentResult(
	text string,
	agentResult *provider.CommandAgentResultMeta,
) (CommandResultRow, error) {
	return buildCommandResultRow(text, sanitizeCommandAgentResult(agentResult))
}

func buildCommandResultRow(text string, agentResult *provider.CommandAgentResultMeta) (CommandResultRow, error) {
	preview := truncateRunes(text, commandResultInlineRunes)
	truncated := preview != text
	meta := commandResultMeta{
		Kind:        itemKindCommandResult,
		Preview:     preview,
		Truncated:   truncated,
		AgentResult: agentResult,
	}
	if truncated {
		meta.TotalBytes = len(text)
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return CommandResultRow{}, err
	}
	return CommandResultRow{Summary: preview, Meta: string(encoded), Oversized: truncated}, nil
}

// CommandResultItemID is the id of a `command_result` row. A
// provider-supplied message id keys the row (making a replayed envelope
// idempotent); otherwise it falls back to the caller's per-turn sequence.
func CommandResultItemID(turnIndex int, providerItemID string, seq int) string {
	if id := strings.TrimSpace(providerItemID); id != "" {
		return "command-result:" + id
	}
	return fmt.Sprintf("command-result:%d:%d", turnIndex, seq)
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
	resultMeta := decodeCommandResultEventMeta(evt.Meta)
	if resultMeta.Suppressed {
		return nil
	}

	turnIndex := r.timelineNotificationTurnIndex(evt)
	itemID := commandResultItemID(evt, turnIndex, r)
	now := eventTimestampMillis(evt)

	shaped, err := buildCommandResultRow(text, resultMeta.AgentResult)
	if err != nil {
		return fmt.Errorf("command result marshal meta %s: %w", itemID, err)
	}
	preview, truncated := shaped.Summary, shaped.Oversized

	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      itemKindCommandResult,
		Role:      "system",
		Status:    statusCompleted,
		Summary:   preview,
		ParentID:  eventParentID(evt),
		Meta:      shaped.Meta,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if resultMeta.AgentResult != nil {
		r.enrichPathRefsFromTexts(evt.ThreadID, &item, text)
		r.enrichCodeSpans(&item)
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

// decodeCommandResultEventMeta reads the provider's row policy and optional
// forked-agent source. Suppressed output belongs to a command Agent Overflow
// issued for its own bookkeeping (`/rename`, the live-config `/effort` and
// `/fast` writes), or a user-typed `/effort` / `/fast` / `/model` whose reply
// the CLI confirmed and AO already renders in its own UI.
//
// Triage does NOT decide this and deliberately cannot: the decision needs to
// know who typed the command AND what was asked of it, and both are facts only
// the send path holds. The provider package stamps the flag, correlating its
// send-time record with the reply through the command's own lifecycle uuid, so
// nothing here parses output text or holds state across envelopes. An unmarked
// event — every other command, every REFUSED state echo, every provider that
// emits no lifecycle bracket, every imported row — persists exactly as before.
//
// Suppression is scoped to the ROW. The event still reaches every other
// consumer, which is what lets the live-config reconciler settle an /effort
// apply from output the transcript never shows.
func decodeCommandResultEventMeta(raw json.RawMessage) provider.CommandResultMeta {
	var meta provider.CommandResultMeta
	if len(raw) == 0 {
		return meta
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		// Undecodable meta is not a suppression signal: the safe direction is
		// keeping a row the user might want, not silently dropping history.
		return provider.CommandResultMeta{}
	}
	meta.AgentResult = sanitizeCommandAgentResult(meta.AgentResult)
	return meta
}

func sanitizeCommandAgentResult(source *provider.CommandAgentResultMeta) *provider.CommandAgentResultMeta {
	if source == nil {
		return nil
	}
	clean := &provider.CommandAgentResultMeta{
		LaunchID:   truncateRunes(strings.TrimSpace(source.LaunchID), commandAgentResultLaunchRunes),
		SourceKind: truncateRunes(strings.TrimSpace(source.SourceKind), commandAgentResultKindRunes),
		SourceName: truncateRunes(strings.TrimSpace(source.SourceName), commandAgentResultNameRunes),
	}
	if clean.LaunchID == "" || clean.SourceKind == "" || clean.SourceName == "" {
		return nil
	}
	return clean
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
		return CommandResultItemID(turnIndex, id, 0)
	}
	seq := r.nextScopeSequence(evt.ThreadID, turnIndex, itemKindCommandResult)
	id := CommandResultItemID(turnIndex, "", seq)
	log.Printf("triage: command_result on %s carried no provider id; allocated %s", evt.ThreadID, id)
	return id
}
