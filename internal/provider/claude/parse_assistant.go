// Package claude — parser for `assistant`-type NDJSON lines. The top-level
// parseAssistant dispatches each content block to a per-type helper
// (appendTextEvent / appendToolUseEvent / appendThinkingEvent /
// appendExitPlanModeEvent) so new block types can be added without growing
// the main function.

package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// assistantContentBlock is the subset of fields every block type on an
// `assistant.message.content` entry carries. The decoded blocks are fed
// into the appendX helpers below; each helper reads the fields relevant
// to its block.Type and ignores the rest.
type assistantContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
}

// assistantUsage mirrors the usage object Claude attaches to
// `assistant.message` lines. Split out so the usage branch of
// parseAssistant can turn it into a context-window snapshot without
// the rest of the message struct leaking into the helper.
type assistantUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type assistantMessage struct {
	ID      string                  `json:"id"`
	Model   string                  `json:"model"`
	Content []assistantContentBlock `json:"content"`
	Role    string                  `json:"role"`
	Usage   *assistantUsage         `json:"usage,omitempty"`
}

func (p *Parser) parseAssistant(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var msg assistantMessage

	// The message payload is under "message" key for assistant type.
	if rawMsg, ok := raw["message"]; ok {
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			return nil, fmt.Errorf("parse assistant message: %w", err)
		}
	} else {
		// Might be flat — try parsing raw directly.
		data, _ := json.Marshal(raw)
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, nil
		}
	}

	// Top-level parent_tool_use_id links subagent (Task-tool) child messages
	// to their parent Task tool use. It's not always present, and only a
	// string when it is.
	var parentToolUseID string
	if v, ok := raw["parent_tool_use_id"]; ok {
		_ = json.Unmarshal(v, &parentToolUseID)
	}
	// Silence the unused-line warning. `line` is kept on the signature to
	// match the other parse* helpers so the top-level switch in ParseLine
	// stays uniform.
	_ = line

	// Track the final assistant message id at the session level so the
	// eventual `result` envelope (which does NOT carry this id) can
	// emit it on EventTurnComplete.Meta.assistant_message_id. Only
	// top-level assistant messages qualify — subagent Task messages
	// carry `parent_tool_use_id`, and the final-text label we want
	// here is the parent thread's. See
	// docs/references/claude-wire.md §assistant.
	if parentToolUseID == "" {
		p.setLastAssistantMessageID(msg.ID)
	}

	var events []provider.ProviderEvent

	// Subagent assistant messages carry the model id the spawned agent
	// is actually running (which can differ from the parent's model
	// when the spawn requested a specific tier — e.g. opus / haiku).
	// Emit a meta-only EventToolStart targeting the parent tool_use_id
	// so triage merges `subagent_model` onto the parent's items.meta
	// without clobbering its summary or tool_name. Dedupe per parent;
	// the model never changes mid-subagent.
	model := strings.TrimSpace(msg.Model)
	if parentToolUseID != "" && model != "" && !p.hasStampedSubagentModel(parentToolUseID) {
		modelMeta, _ := json.Marshal(map[string]any{
			"subagent_model": model,
		})
		events = append(events, provider.ProviderEvent{
			Kind:      provider.EventToolStart,
			ThreadID:  threadID,
			ItemID:    parentToolUseID,
			Meta:      modelMeta,
			Timestamp: now,
		})
		p.markSubagentModelStamped(parentToolUseID)
	}

	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			// Text content is already emitted delta-by-delta via the
			// stream_event path (`parse_stream.go`) — `--include-partial-
			// messages` is always on for our session. Emitting again
			// from the coalesced assistant envelope would append the
			// full block on top of the streamed partials, doubling the
			// text in the cumulative summary. This manifested as a
			// user-visible rendering bug (mermaid diagrams appearing
			// twice) after the closure gate started actually rendering
			// their content.
		case "tool_use":
			events = p.appendToolUseEvent(events, threadID, parentToolUseID, now, block)
		case "thinking":
			// Same as text: streamed via stream_event thinking_delta.
		}
	}

	events = p.appendAssistantUsageEvent(events, threadID, parentToolUseID, now, msg.Usage)
	return events, nil
}

// appendTextEvent emits a streaming text delta for a `text` block. The
// message-level id (msg.ID) is used as the item id so every text chunk in
// the same assistant message collapses onto one timeline row.
func appendTextEvent(
	events []provider.ProviderEvent,
	threadID, itemID, parentToolUseID string,
	now time.Time,
	block assistantContentBlock,
) []provider.ProviderEvent {
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventTextDelta,
		ThreadID:        threadID,
		ItemID:          itemID,
		Content:         block.Text,
		Role:            "assistant",
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	})
}

// appendToolUseEvent handles `tool_use` blocks. ExitPlanMode takes a
// dedicated path (proposed-plan event); TodoWrite takes a dedicated
// path (live-plan event, with NO timeline tool-call row); every other
// tool call — including AskUserQuestion — emits a generic
// EventToolStart row. AskUserQuestion is additionally surfaced via the
// parallel can_use_tool control_request path as an
// EventUserInputRequest that drives the in-composer answer panel; the
// timeline tool-call row is the persisted historical record.
func (p *Parser) appendToolUseEvent(
	events []provider.ProviderEvent,
	threadID, parentToolUseID string,
	now time.Time,
	block assistantContentBlock,
) []provider.ProviderEvent {
	if block.Name == "ExitPlanMode" {
		return appendExitPlanModeEvent(events, threadID, parentToolUseID, now, block)
	}
	if block.Name == "TodoWrite" {
		return p.appendTodoWriteEvent(events, threadID, parentToolUseID, now, block)
	}

	isBackground := hasRunInBackground(block.Input)
	if isBackground {
		p.markBackground(block.ID)
	}

	meta := marshalToolMeta(block.Name, block.Input, isBackground)
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventToolStart,
		ThreadID:        threadID,
		ItemID:          block.ID,
		ItemType:        block.Name,
		Meta:            meta,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	})
}

// appendTodoWriteEvent converts a TodoWrite tool call into a single
// EventTodoUpdate carrying the normalized todo snapshot. The tool_use
// is also marked so its eventual tool_result can be dropped in
// parse_user.go — TodoWrite never produces a timeline tool-call row.
//
// Wire shape (per claude-code-source-code/src/utils/todo/types.ts):
//
//	{ todos: [ { content: string, status: "pending" | "in_progress" | "completed", activeForm: string } ] }
//
// Status is normalized to camelCase (`inProgress`) at emit time so the
// frontend sees the same enum regardless of provider — Codex already
// emits camelCase per its Rust serde.
//
// An empty todos array drops the event rather than emit an empty list
// the frontend would render as "no todos".
func (p *Parser) appendTodoWriteEvent(
	events []provider.ProviderEvent,
	threadID, parentToolUseID string,
	now time.Time,
	block assistantContentBlock,
) []provider.ProviderEvent {
	steps := extractTodoWriteSteps(block.Input)
	if len(steps) == 0 {
		return events
	}
	meta, err := json.Marshal(map[string]any{
		"kind":  "todo_update",
		"title": "Updated Todos",
		"plan":  steps,
	})
	if err != nil {
		return events
	}
	// Mark only after the marshal succeeds. Marking before would leak a
	// stale entry if marshal ever failed and silently drop the matching
	// tool_result via parse_user.go's TodoWrite carve-out. Marshal of a
	// typed primitives map can't fail in practice; this ordering is the
	// defensive belt.
	p.markTodoWrite(block.ID)
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventTodoUpdate,
		ThreadID:        threadID,
		ItemID:          block.ID,
		ItemType:        block.Name,
		Content:         "Updated Todos",
		Meta:            meta,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	})
}

// todoWriteStep is the typed wire shape we marshal into Meta.plan.
// Keeping it a named struct (instead of map[string]string) preserves
// type safety across the parse → marshal → triage decode round trip
// and avoids per-step map allocation.
type todoWriteStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// extractTodoWriteSteps decodes a TodoWrite tool_use input into the
// shared plan-step shape used by both providers ({step, status}). The
// status enum is normalized from Claude's snake_case to the camelCase
// shape Codex already emits, so triage and the frontend see one
// vocabulary.
func extractTodoWriteSteps(input json.RawMessage) []todoWriteStep {
	var payload struct {
		Todos []struct {
			Content    string `json:"content"`
			Status     string `json:"status"`
			ActiveForm string `json:"activeForm"`
		} `json:"todos"`
	}
	if err := json.Unmarshal(input, &payload); err != nil {
		return nil
	}
	steps := make([]todoWriteStep, 0, len(payload.Todos))
	for _, todo := range payload.Todos {
		content := strings.TrimSpace(todo.Content)
		if content == "" {
			continue
		}
		steps = append(steps, todoWriteStep{
			Step:   content,
			Status: normalizeTodoWriteStatus(todo.Status),
		})
	}
	return steps
}

// normalizeTodoWriteStatus converts Claude TodoWrite's snake_case
// status enum into the camelCase form Codex's update_plan already
// emits. Unknown values pass through as `pending` so the frontend can
// render a sensible default if Claude ever ships a new status the
// parser doesn't recognise.
func normalizeTodoWriteStatus(raw string) string {
	switch strings.TrimSpace(raw) {
	case "in_progress":
		return "inProgress"
	case "completed":
		return "completed"
	case "pending", "":
		return "pending"
	default:
		return "pending"
	}
}

// appendExitPlanModeEvent converts an ExitPlanMode tool call into an
// EventProposedPlan. A missing plan body drops the event rather than emit
// an empty plan the frontend would render as "no content".
func appendExitPlanModeEvent(
	events []provider.ProviderEvent,
	threadID, parentToolUseID string,
	now time.Time,
	block assistantContentBlock,
) []provider.ProviderEvent {
	planMarkdown := extractExitPlanModePlan(block.Input)
	if planMarkdown == "" {
		return events
	}
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventProposedPlan,
		ThreadID:        threadID,
		ItemID:          block.ID,
		ItemType:        block.Name,
		Content:         planMarkdown,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	})
}

// appendThinkingEvent emits a thinking block, optionally carrying the
// cryptographic signature Claude attaches when it's enabled. The
// signature is marshaled into meta rather than stamped onto the Content
// so the frontend can rely on Content being plain text.
func appendThinkingEvent(
	events []provider.ProviderEvent,
	threadID, itemID, parentToolUseID string,
	now time.Time,
	block assistantContentBlock,
) []provider.ProviderEvent {
	var meta json.RawMessage
	if block.Signature != "" {
		meta, _ = json.Marshal(map[string]any{
			"signature": block.Signature,
		})
	}
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventThinking,
		ThreadID:        threadID,
		ItemID:          itemID,
		Content:         block.Thinking,
		Meta:            meta,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	})
}

// appendAssistantUsageEvent emits a context-window snapshot from a top-level
// assistant usage object. Subagent assistant envelopes carry
// parent_tool_use_id and belong to the subagent's private accounting, not the
// parent chat meter.
func (p *Parser) appendAssistantUsageEvent(
	events []provider.ProviderEvent,
	threadID, parentToolUseID string,
	now time.Time,
	usage *assistantUsage,
) []provider.ProviderEvent {
	if usage == nil {
		return events
	}
	return appendContextUsageEvent(events, threadID, parentToolUseID, now, *usage)
}

func appendContextUsageEvent(
	events []provider.ProviderEvent,
	threadID, parentToolUseID string,
	now time.Time,
	usage assistantUsage,
) []provider.ProviderEvent {
	if parentToolUseID != "" {
		return events
	}
	window, ok := contextWindowFromClaudeUsage(usage)
	if !ok {
		return events
	}
	usageMeta, _ := json.Marshal(window)
	return append(events, provider.ProviderEvent{
		Kind:      provider.EventTokenUsage,
		ThreadID:  threadID,
		Meta:      usageMeta,
		Timestamp: now,
	})
}

func contextWindowFromClaudeUsage(usage assistantUsage) (provider.ContextWindow, bool) {
	used := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
	if used <= 0 {
		return provider.ContextWindow{}, false
	}
	return provider.ContextWindow{UsedTokens: used}, true
}

// hasRunInBackground returns true when the tool input JSON contains
// `"run_in_background": true`. Malformed JSON is treated as absent —
// this is a best-effort hint, not a correctness-critical value.
func hasRunInBackground(input json.RawMessage) bool {
	if len(input) == 0 {
		return false
	}
	var parsed struct {
		RunInBackground bool `json:"run_in_background"`
	}
	if err := json.Unmarshal(input, &parsed); err != nil {
		return false
	}
	return parsed.RunInBackground
}

// marshalToolMeta builds the EventToolStart Meta payload. We omit
// `is_background` when false so pipelines downstream don't have to
// distinguish "explicitly foreground" from "unknown" — absence is the
// default.
func marshalToolMeta(toolName string, input json.RawMessage, isBackground bool) json.RawMessage {
	fields := map[string]any{
		"toolName": toolName,
		"input":    input,
	}
	if isBackground {
		fields["is_background"] = true
	}
	out, _ := json.Marshal(fields)
	return out
}

func extractExitPlanModePlan(input json.RawMessage) string {
	var payload struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal(input, &payload); err != nil {
		return ""
	}
	return payload.Plan
}
