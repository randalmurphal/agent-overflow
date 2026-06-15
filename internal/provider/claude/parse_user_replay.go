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
	"html"
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
// Synthetic `<task-notification>` bodies are a special case inside
// the suppressed branch: when a background subagent completes WHILE
// a concurrent foreground tool_result is in flight, Claude's CLI
// skips the structured `system/task_notification` envelope and
// delivers the completion only as inline XML on this isReplay user
// envelope (LocalShellTask.tsx:160-165 wraps the queued attachment).
// We still suppress the EventUserText, but extract the inner fields
// and emit `EventBackgroundTaskNotification` so triage's stash-drain
// path can write the `tool_completion` sibling. Idempotent with the
// structured path — if both arrive the second drain returns no-op.
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
		// Replay every `<task-notification>` in the body, not just the
		// first. The coalesced multi-notification flush is confirmed on
		// the claude-tui /v1/messages wire (sibling completions during a
		// TaskOutput(block=true) wait, spike/claude-mitm/
		// probe_taskoutput_siblings.py); this headless echo is fed from
		// the same enqueuePendingNotification source, so we extract all
		// defensively even though a headless multi-block bundle has not
		// been captured directly. A non-routable block (empty <task-id>)
		// has no idempotency key, so it is skipped rather than fabricating
		// a malformed completion.
		var events []provider.ProviderEvent
		for _, fields := range ExtractAllTaskNotificationFields(content) {
			if !fields.Routable() {
				continue
			}
			events = append(events, p.replayTaskNotificationEvents(threadID, fields, now)...)
		}
		return events, nil
	}

	providerItemID := firstNonEmpty(msg.ID, readRawString(raw["uuid"]))
	parentUUID := readRawString(raw["parentUuid"])

	var meta json.RawMessage
	if providerItemID != "" || parentUUID != "" {
		marshaled, err := json.Marshal(map[string]string{
			"provider_item_id": providerItemID,
			"parent_uuid":      parentUUID,
		})
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

// TaskNotificationFields holds the inner-tag values lifted from a
// `<task-notification>...</task-notification>` body. Mirrors the field
// set the structured `system/task_notification` envelope carries — only
// TaskID is required; everything else is best-effort enrichment.
//
// Exported because the claude-tui provider reconstructs the structured
// task_updated + task_notification envelopes from a `<task-notification>`
// it finds inline in a `/v1/messages` request body — its only wire
// signal that a backgrounded task finished, since the stream-json system
// envelopes headless emits never cross that wire. Reusing this one
// extractor keeps both call sites drift-free against upstream tag changes.
type TaskNotificationFields struct {
	TaskID     string
	ToolUseID  string
	Status     string
	OutputFile string
	Summary    string
}

// Routable reports whether the notification carries the `<task-id>` that
// triage uses as the idempotency key — to resolve the launch and drain
// the pending-terminal stash. A notification with an empty task_id can't
// be keyed, so every caller skips it rather than synthesizing a
// completion that resolves to no launch and can't be deduped. This is the
// one definition of that rule; the scanner and both call sites
// (parseUserReplay here, claudetui's eachTaskNotification) ask it here so
// a routing decision can't drift from an emission decision.
func (f TaskNotificationFields) Routable() bool { return f.TaskID != "" }

// ExtractAllTaskNotificationFields lifts the fields out of EVERY
// `<task-notification>` block in content, in wire order. When a
// backgrounded command finishes while the agent is blocked on a
// TaskOutput(block=true) poll, the CLI coalesces one
// "[SYSTEM NOTIFICATION - NOT USER INPUT]" + `<task-notification>` per
// completed task into a SINGLE message — the TaskOutput-waited task AND
// any sibling that finished during the wait (confirmed on 2.1.170:
// spike/claude-mitm/probe_taskoutput_siblings.py). Extracting only the
// first would silently drop the rest, stranding those launches as
// "running" forever. Each returned element carries the parsed fields;
// callers skip the non-routable ones (fields.Routable() == false, i.e. an
// empty <task-id>) rather than the extractor swallowing them.
func ExtractAllTaskNotificationFields(content string) []TaskNotificationFields {
	var out []TaskNotificationFields
	for from := 0; from < len(content); {
		fields, end := scanTaskNotification(content, from)
		if end < 0 {
			break
		}
		out = append(out, fields)
		from = end
	}
	return out
}

// scanTaskNotification parses the first `<task-notification>...
// </task-notification>` block at or after `from`. It returns the parsed
// fields and the index in content just past the closing tag — or end<0
// when no balanced block remains, so a caller loop can terminate.
//
// The closing tag is sought AFTER the opening tag, so a stray
// `</task-notification>` before the next open is ignored instead of
// truncating the scan. That matters for ExtractAllTaskNotificationFields:
// a malformed or echoed close in one block must not strand the valid
// blocks that follow it (the multi-extract loop would otherwise stop on
// the first close-before-open it hit). A missing close (an unterminated
// `<task-notification`) yields end<0 so a malformed echo can't fabricate
// a block.
//
// Tags are extracted by shallow substring scan; the upstream wire shape
// (LocalShellTask.tsx / LocalAgentTask.tsx) keeps the children we read
// non-nested. Sibling sections an agent notification adds after
// `<summary>` (`<result>`, `<usage>`, `<worktree>`) are ignored — each
// child is matched by its own tag.
func scanTaskNotification(content string, from int) (TaskNotificationFields, int) {
	const openPrefix = "<task-notification"
	const closeTag = "</task-notification>"
	rel := content[from:]
	openIdx := strings.Index(rel, openPrefix)
	if openIdx < 0 {
		return TaskNotificationFields{}, -1
	}
	closeRel := strings.Index(rel[openIdx:], closeTag)
	if closeRel < 0 {
		return TaskNotificationFields{}, -1
	}
	closeIdx := openIdx + closeRel
	// Advance past the opening tag's `>` so `body` represents inner
	// content only. Tolerant of an attribute-bearing variant
	// `<task-notification foo="bar">` even though we don't expect one
	// from current Claude releases.
	bodyStart := strings.Index(rel[openIdx:closeIdx], ">")
	if bodyStart < 0 {
		return TaskNotificationFields{}, -1
	}
	body := rel[openIdx+bodyStart+1 : closeIdx]
	fields := TaskNotificationFields{
		TaskID:     extractXMLChild(body, "task-id"),
		ToolUseID:  extractXMLChild(body, "tool-use-id"),
		Status:     extractXMLChild(body, "status"),
		OutputFile: extractXMLChild(body, "output-file"),
		Summary:    extractXMLChild(body, "summary"),
	}
	return fields, from + closeIdx + len(closeTag)
}

// extractXMLChild returns the trimmed, entity-decoded inner text of
// `<tag>...</tag>` inside body, or "" when the tag is missing or
// unbalanced. The upstream wrapper emits children without attributes,
// so the literal open `<tag>` and close `</tag>` match exactly. We
// decode `&amp;` / `&lt;` / `&gt;` / `&quot;` / `&apos;` because the
// summary text in particular can carry user-visible characters that
// the CLI escapes on the wire.
func extractXMLChild(body, tag string) string {
	openTag := "<" + tag + ">"
	closeTag := "</" + tag + ">"
	o := strings.Index(body, openTag)
	if o < 0 {
		return ""
	}
	o += len(openTag)
	c := strings.Index(body[o:], closeTag)
	if c < 0 {
		return ""
	}
	return html.UnescapeString(strings.TrimSpace(body[o : o+c]))
}

// replayTaskNotificationEvents routes the synthetic-XML
// `<task-notification>` payload through the shared
// EventBackgroundTaskNotification builder so triage receives identical
// inputs whichever wire path Claude chose. The XML wrapper doesn't
// expose `parent_tool_use_id`; the shared builder falls back to the
// parser's task_id ↔ tool_use_id map for it.
func (p *Parser) replayTaskNotificationEvents(threadID string, fields TaskNotificationFields, now time.Time) []provider.ProviderEvent {
	return []provider.ProviderEvent{p.buildBackgroundTaskNotificationEvent(threadID, backgroundTaskNotificationFields{
		TaskID:     fields.TaskID,
		ToolUseID:  fields.ToolUseID,
		Status:     fields.Status,
		OutputFile: fields.OutputFile,
		Summary:    fields.Summary,
	}, now)}
}
