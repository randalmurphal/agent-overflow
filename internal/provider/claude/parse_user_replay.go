// Package claude — parser for the `user{isReplay:true}` envelope
// variant. Claude emits these when the session was spawned with
// `--replay-user-messages` (see session.go) — every queued user-side
// message gets echoed back on stdout for stream-json acknowledgment.
//
// Two distinct content classes ride this envelope:
//
//  1. Real user input — typed in our composer, queued via stdin while
//     a turn was running, echoed back. Triage's pending-send FIFO
//     matches on `provider_item_id` (top-level `uuid`) and stamps the
//     existing AO-persisted optimistic row.
//  2. Claude-injected queued attachments — task notifications, stall
//     pings, system reminders, slash-command echoes, etc. These ride
//     the same envelope shape as real user input but are model-context
//     payloads, never typed by the user. They must NOT persist as
//     user-bubble rows.
//
// The split is owned at this parser layer (see
// isClaudeInjectedReplayContent's doc for why detection cannot move
// downstream into triage).

package claude

import (
	"encoding/json"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// claudeInjectedXMLWrappers names the balanced XML wrappers Claude
// injects into user-role message content. When a `user{isReplay:true}`
// envelope's content contains both the open and close tag of any
// pair, the envelope is treated as a Claude-injected attachment and
// suppressed.
//
// The first entry's open is `<task-notification` (no closing `>`) so
// it matches both the bare `<task-notification>` shape and any
// future attribute-bearing variant. Every other entry uses the full
// open tag.
//
// Drift surface — keep synced with upstream:
//   - <task-notification>      claude-code-source-code/src/tasks/LocalShellTask/LocalShellTask.tsx:160-165
//   - <system-reminder>        claude-code-source-code/src/utils/messages.ts (multiple sites; pervasive system-reminder wrapper)
//   - <bash-input/-stdout/-stderr>  claude-code-source-code/src/services/processBashCommand.tsx
//   - <local-command-stdout>   claude-code-source-code/src/services/processSlashCommand.tsx
var claudeInjectedXMLWrappers = []struct{ open, close string }{
	{"<task-notification", "</task-notification>"},
	{"<system-reminder>", "</system-reminder>"},
	{"<bash-input>", "</bash-input>"},
	{"<bash-stdout>", "</bash-stdout>"},
	{"<bash-stderr>", "</bash-stderr>"},
	{"<local-command-stdout>", "</local-command-stdout>"},
}

// isReplayEnvelope reports whether a `user` envelope is the CLI's
// replay echo of an AO-initiated user message — the wire signal we get
// when the session was spawned with `--replay-user-messages`. Only
// `isReplay==true` qualifies; absence of the field, false, or any
// non-bool shape returns false so the existing tool_result path stays
// untouched.
func isReplayEnvelope(raw map[string]json.RawMessage) bool {
	flagRaw, ok := raw["isReplay"]
	if !ok {
		return false
	}
	var isReplay bool
	if err := json.Unmarshal(flagRaw, &isReplay); err != nil {
		return false
	}
	return isReplay
}

// isClaudeInjectedReplayContent reports whether a `user{isReplay:true}`
// envelope's content body is a Claude-injected queued attachment
// (task notification, stall ping, system reminder, slash command
// echo, etc.) rather than real user input. Used by parseUserReplay
// to suppress the `EventUserText` emission so triage never persists
// these as user-bubble rows.
//
// Detection MUST run at the parser layer because triage's
// `consumePendingSendHead` is a destructive FIFO pop — late
// detection would corrupt the queue-confirm path for any real user
// message racing the suppression check.
//
// Two detection classes, both conservative (false positives would
// silently drop real user content):
//
//  1. Balanced XML wrappers Claude injects into user-role content
//     (see claudeInjectedXMLWrappers). Open AND close must both be
//     present, so a single mention in real user text doesn't trigger.
//     This is the load-bearing path: queued task-notification
//     attachments wrap their body with `<task-notification>` XML and
//     this catches every variant — wrapped, bare, attribute-bearing,
//     statusless stall pings.
//  2. Origin-prefix wraps from `wrapCommandText` for modes we don't
//     support (coordinator, channel) — defensive only.
//
// Intentionally NOT suppressed:
//   - `human` origin prefix `"The user sent a new message while you
//     were working:\n"` — real user input. Pending-send FIFO matches
//     by uuid; AO-persisted summary is authoritative.
//   - `task-notification` origin prefix
//     `"A background agent completed a task:\n"` — that wrap always
//     contains a `<task-notification>` XML body, caught by class 1.
//     Suppressing on this prefix alone would false-positive a real
//     user typing the literal prefix in chat.
//   - Content with no prefix, no XML wrappers — Codex-style raw
//     echoes; Claude reattach edge cases. Continue emitting so the
//     existing wire-only persist path can run.
//
// Trimming uses TrimSpace so leading/trailing whitespace doesn't
// defeat the prefix check on either class.
func isClaudeInjectedReplayContent(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}

	if strings.HasPrefix(trimmed, "The coordinator sent a message while you were working:\n") {
		return true
	}
	if strings.HasPrefix(trimmed, "A message arrived from ") &&
		strings.Contains(trimmed, " while you were working:\n") {
		return true
	}

	for _, pair := range claudeInjectedXMLWrappers {
		if strings.Contains(trimmed, pair.open) && strings.Contains(trimmed, pair.close) {
			return true
		}
	}

	return false
}

// parseUserReplay promotes a replay-echo user envelope to a single
// EventUserText. The replay shape (per @anthropic-ai/claude-agent-sdk
// `SDKUserMessageReplaySchema`) carries `message.content` as a plain
// string, but we accept the array shape `[{type:"text",text:"..."}]`
// defensively too — extractToolResultText already covers both.
//
// Claude-injected queued attachments (task notifications, stall
// pings, system reminders, slash-command echoes, ...) ride this
// envelope shape too. Drop them at the parser so triage never sees
// an EventUserText for non-user content — see
// isClaudeInjectedReplayContent's doc.
//
// `provider_item_id` resolution: top-level `uuid` is the source for
// every replay shape current Claude releases emit (queued_command at
// claude-code-source-code/src/QueryEngine.ts:880-892, initial-user ack
// at QueryEngine.ts:738-749 yielding `message: msgToAck.message` whose
// inner shape is `{role, content}` only — see
// utils/messages.ts:502-507 `createUserMessage`, which never sets
// `message.id`). The `message.id` preference is a defensive carve-out
// for a hypothetical future SDK shape that exposes the API-assigned id
// alongside the SDK uuid; if such a shape lands we want the more
// specific identifier. Until then, the uuid fallback is the
// load-bearing path. Without it, queued messages flushed via stdin
// arrive with no stable handle — triage's pending-send correlator
// (handle_user_text.go) pops the FIFO entry but the merge no-ops on
// empty id, no upsert emits, and the frontend's queue-confirm path
// stays stuck.
func (p *Parser) parseUserReplay(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	msgRaw, ok := raw["message"]
	if !ok {
		return nil, nil
	}
	var msg struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return nil, nil
	}

	content := extractToolResultText(msg.Content)

	if isClaudeInjectedReplayContent(content) {
		return nil, nil
	}

	providerItemID := firstNonEmpty(msg.ID, readRawString(raw["uuid"]))

	var meta json.RawMessage
	if providerItemID != "" {
		marshaled, err := json.Marshal(map[string]string{"provider_item_id": providerItemID})
		if err == nil {
			meta = marshaled
		}
	}

	return []provider.ProviderEvent{{
		Kind:      provider.EventUserText,
		ThreadID:  threadID,
		Content:   content,
		Meta:      meta,
		Timestamp: now,
		Raw:       line,
	}}, nil
}
