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
//
// ToolUseID + Content carry the advisor_tool_result shape — the
// server-side advisor result envelope arrives with `role:"assistant"`
// (not user-role like the standard tool_result), so the result body
// rides on a content block here rather than going through parse_user.go.
type assistantContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
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
	// Error is the SDK's `assistant.error` enum. When set, the API
	// rejected the prompt and the CLI emits a follow-up `result`
	// envelope with `is_error:true`. We surface this as a fatal
	// EventError tagged `expect_turn_complete:true` so the router
	// closes the turn via the real `result`, not a synthesized one.
	// Enum values per the agent SDK: `authentication_failed`,
	// `billing_error`, `rate_limit`, `invalid_request`, `server_error`,
	// `unknown`, `max_output_tokens`.
	Error string `json:"error,omitempty"`
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
	// emit it on provider.WireTurnCompleteMeta. Only
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

	// Advisor envelopes carry their own degenerate `usage` block (the
	// advisor is a separate model run with its own context window — see
	// docs/references/claude-wire.md §server_tool_use). Routing that
	// usage through the context meter would clobber the parent's meter
	// for the duration of the advisor call. Detect "advisor-only"
	// envelopes here and suppress the usage emit below.
	advisorOnly := len(msg.Content) > 0
	for _, block := range msg.Content {
		if block.Type != "server_tool_use" && block.Type != "advisor_tool_result" {
			advisorOnly = false
			break
		}
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
			events = p.appendToolUseEvent(events, threadID, parentToolUseID, msg.ID, now, block)
		case "thinking":
			// Same as text: streamed via stream_event thinking_delta.
		case "server_tool_use":
			// Claude's server-side tool call (today: `advisor`). The
			// matching result arrives on a SECOND assistant envelope
			// carrying an `advisor_tool_result` content block — see
			// docs/references/claude-wire.md §server_tool_use.
			events = p.appendServerToolUseEvent(events, threadID, parentToolUseID, msg.ID, msg.Model, now, block)
		case "advisor_tool_result":
			// Result of a prior `server_tool_use` advisor call. Closes
			// the tool lifecycle for the matching `srvtoolu_*` id.
			events = p.appendAdvisorResultEvent(events, threadID, parentToolUseID, now, line, block)
		}
	}

	if !advisorOnly {
		events = p.appendAssistantUsageEvent(events, threadID, parentToolUseID, now, msg.Usage)
	}

	// `assistant.error` (e.g. `rate_limit`, `authentication_failed`) is
	// surfaced as a fatal EventError. Claude has emitted this enum in two
	// places across versions: under `message.error`, and as a top-level
	// envelope field next to `message`. Per the agent SDK, the CLI follows
	// this with a real `result{is_error:true}` envelope which closes the
	// turn through the wire path; the `expect_turn_complete:true` flag
	// tells the triage router not to synthesize a duplicate TurnComplete.
	// Subagent assistant errors (parent_tool_use_id != "") use the parent
	// thread's open turn; the failure still closes the parent turn.
	if errorEnum := assistantErrorEnum(raw, msg); errorEnum != "" {
		errMeta, _ := json.Marshal(map[string]any{
			"api_error_enum":       errorEnum,
			"error":                errorEnum,
			"fatal":                true,
			"expect_turn_complete": true,
		})
		events = append(events, provider.ProviderEvent{
			Kind:            provider.EventError,
			ThreadID:        threadID,
			Content:         assistantErrorSummary(msg, errorEnum),
			Meta:            errMeta,
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		})
	}

	return events, nil
}

func assistantErrorEnum(raw map[string]json.RawMessage, msg assistantMessage) string {
	if enum := strings.TrimSpace(msg.Error); enum != "" {
		return enum
	}
	return strings.TrimSpace(readRawString(raw["error"]))
}

func assistantErrorSummary(msg assistantMessage, enum string) string {
	for _, block := range msg.Content {
		if block.Type != "text" {
			continue
		}
		if text := boundedProviderErrorMessage(block.Text); text != "" {
			return text
		}
	}
	return errorEnumToHumanCopy(enum)
}

// errorEnumToHumanCopy maps an `assistant.error` enum value to a
// human-readable summary the frontend renders verbatim on the
// timeline error row. The strings mirror Claude Code's
// `SystemAPIErrorMessage` copy where it carries one; for enum values
// the TUI doesn't render explicitly we fall back to a short
// description so the row never goes blank. The frontend can branch
// on `meta.error` (the raw enum) for actionable affordances like
// "Add credits" / "Run /login".
//
// The default branch concatenates the enum into the summary so
// novel SDK values surface readable text. Cap the enum at
// maxAssistantErrorEnumChars so a malformed/hostile envelope can't
// push an arbitrary-length string onto the timeline row — Svelte
// autoescapes content so this isn't an XSS path, but an unbounded
// summary can still distort layout.
func errorEnumToHumanCopy(enum string) string {
	switch enum {
	case "authentication_failed":
		return "Authentication failed"
	case "billing_error":
		return "Billing error"
	case "rate_limit":
		return "Rate limit reached"
	case "invalid_request":
		return "Invalid request"
	case "server_error":
		return "Anthropic API server error"
	case "max_output_tokens":
		return "Reached max output tokens"
	case "unknown":
		return "API error"
	default:
		return "API error: " + truncate(enum, maxAssistantErrorEnumChars)
	}
}

const maxAssistantErrorEnumChars = 64

func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
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
	threadID, parentToolUseID, assistantMessageID string,
	now time.Time,
	block assistantContentBlock,
) []provider.ProviderEvent {
	p.rememberToolUseParent(block.ID, parentToolUseID)

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

	meta := marshalToolMeta(block.Name, block.Input, isBackground, assistantMessageID)
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
//
// MCP tool names arrive on the wire as `mcp__<server>__<tool>`. We
// normalize them into the unified `MCP/<tool>` shape that the Codex
// `mcpToolCall` envelope already produces, and stamp the raw
// `{server, tool}` pair onto `meta.mcp` so the renderer can synthesize
// the body as `server.tool(args)` from a single source on both
// providers.
func marshalToolMeta(toolName string, input json.RawMessage, isBackground bool, assistantMessageID string) json.RawMessage {
	normalizedToolName := toolName
	var mcp map[string]string
	if server, tool, ok := parseClaudeMCPToolName(toolName); ok {
		normalizedToolName = "MCP/" + tool
		mcp = map[string]string{"server": server, "tool": tool}
	}

	fields := map[string]any{
		"toolName": normalizedToolName,
		"input":    input,
	}
	if mcp != nil {
		fields["mcp"] = mcp
	}
	if isBackground {
		fields["is_background"] = true
	}
	if assistantMessageID != "" {
		fields["assistant_message_id"] = assistantMessageID
	}
	if !isBackground && (toolName == "Agent" || toolName == "Task") {
		fields["is_inline_subagent"] = true
		if assistantMessageID != "" {
			fields["inline_subagent_group_id"] = assistantMessageID
		}
	}
	out, _ := json.Marshal(fields)
	return out
}

// parseClaudeMCPToolName splits a Claude `mcp__<server>__<tool>` block
// name into its server and tool halves. The first `__` after the
// `mcp__` prefix is the separator — server names can contain single
// underscores, tool names can contain anything. Both halves must be
// non-empty for a valid MCP tool name.
func parseClaudeMCPToolName(toolName string) (server, tool string, ok bool) {
	const prefix = "mcp__"
	if !strings.HasPrefix(toolName, prefix) {
		return "", "", false
	}
	rest := toolName[len(prefix):]
	sep := strings.Index(rest, "__")
	if sep <= 0 {
		return "", "", false
	}
	server = rest[:sep]
	tool = rest[sep+len("__"):]
	if tool == "" {
		return "", "", false
	}
	return server, tool, true
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

// appendServerToolUseEvent handles a `server_tool_use` content block —
// the call side of a Claude server-side tool. Today this is exclusively
// `advisor`; if Anthropic adds web_search/web_fetch under the same
// envelope shape, route by `block.Name` here.
//
// The advisor model is read from the parent envelope's `message.model`
// (passed in as advisorModel). The wire does not carry a separate
// `advisor_model` field on the server_tool_use block; the
// advisor uses the same model family as the parent in practice, so
// stamping `message.model` is correct and matches what the
// usage.iterations[type=advisor_message].model field reports.
//
// `markAdvisor` remembers the id so the matching `advisor_tool_result`
// block can identify which completion is an advisor result vs a
// regular tool_result (the latter never reaches this path — those are
// user-role envelopes handled in parse_user.go).
func (p *Parser) appendServerToolUseEvent(
	events []provider.ProviderEvent,
	threadID, parentToolUseID, assistantMessageID, advisorModel string,
	now time.Time,
	block assistantContentBlock,
) []provider.ProviderEvent {
	if block.ID == "" || block.Name == "" {
		return events
	}
	// Hard-gate to the advisor name. The helper's comment promises name
	// routing for future server-side tools (web_search / web_fetch under
	// the same envelope shape), but the body — `markAdvisor`,
	// `marshalAdvisorToolMeta`, the advisor-only result correlation —
	// only knows how to render advisor. Forwarding an unknown server
	// tool would stamp `advisor_model` on its meta and route it through
	// AdvisorRow. Drop unknown names rather than silently misclassify
	// them; a parser refresh is the right place to recognise the new
	// shape when it lands.
	if block.Name != "advisor" {
		return events
	}
	p.markAdvisor(block.ID)
	meta := marshalAdvisorToolMeta(block.Name, advisorModel, assistantMessageID)
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

// appendAdvisorResultEvent emits the EventToolComplete for an
// `advisor_tool_result` content block. The block arrives on a
// `role:"assistant"` envelope (not user-role like standard tool_result)
// so it cannot share the parse_user.go plumbing.
//
// The result body is `block.content.text` (nested) where the outer
// `content` is the assistant message's content array element and the
// inner `content` is `{type:"advisor_result", text:"..."}`. Meta is
// minimal — no exit_code, no is_background path; the advisor runs
// inline (not backgrounded) and Anthropic does not surface an error
// channel on this block today.
func (p *Parser) appendAdvisorResultEvent(
	events []provider.ProviderEvent,
	threadID, parentToolUseID string,
	now time.Time,
	line []byte,
	block assistantContentBlock,
) []provider.ProviderEvent {
	if block.ToolUseID == "" {
		// Orphan result with no correlation id — drop rather than
		// emit a completion that can't be matched to a launch row.
		return events
	}
	// Drop a stray `advisor_tool_result` that wasn't preceded by a
	// `server_tool_use` we recognised. Defensive — keeps the parser
	// from synthesising a completion against a non-existent launch
	// when the wire shape drifts.
	if !p.isAdvisor(block.ToolUseID) {
		return events
	}
	p.clearAdvisor(block.ToolUseID)

	text := extractAdvisorResultText(block.Content)
	meta, _ := json.Marshal(map[string]any{
		"is_error": false,
	})
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventToolComplete,
		ThreadID:        threadID,
		ItemID:          block.ToolUseID,
		Content:         text,
		Meta:            meta,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
		Raw:             line,
	})
}

// extractAdvisorResultText reads the text field out of the nested
// `content` object on an `advisor_tool_result` block. The wire shape
// is `{type:"advisor_result", text:"..."}` — distinct from the
// user-side tool_result `content` (which is string-or-array). Returns
// "" on malformed input rather than failing; the empty content still
// produces a completion event so the running row settles.
func extractAdvisorResultText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var payload struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &payload) != nil {
		return ""
	}
	// Pin the inner discriminator so a wire-drift shape (e.g. a future
	// `advisor_error`) doesn't silently slip through as a normal
	// response body. The completion event still fires with empty
	// Content so the running row settles; the parser-refresh that
	// recognises the new shape is the right place to handle it.
	if payload.Type != "advisor_result" {
		return ""
	}
	return payload.Text
}

// marshalAdvisorToolMeta builds the EventToolStart Meta for an advisor
// invocation. Mirrors marshalToolMeta's shape for triage compatibility
// (same toolName/input/assistant_message_id keys) but adds
// `advisor_model` — the frontend's AdvisorRow renders it via
// displayModelLabel, the same way subagent rows render their
// `subagent_model`.
func marshalAdvisorToolMeta(toolName, advisorModel, assistantMessageID string) json.RawMessage {
	fields := map[string]any{
		"toolName": toolName,
		"input":    json.RawMessage("{}"),
	}
	if advisorModel != "" {
		fields["advisor_model"] = advisorModel
	}
	if assistantMessageID != "" {
		fields["assistant_message_id"] = assistantMessageID
	}
	out, _ := json.Marshal(fields)
	return out
}
