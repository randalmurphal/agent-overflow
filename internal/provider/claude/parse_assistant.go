// Package claude — parser for `assistant`-type NDJSON lines. The top-level
// parseAssistant dispatches each content block to a per-type helper so new
// block types can be added without growing the main function.

package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionimport"
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
	// Iterations is the per-API-call breakdown `message_delta` carries.
	// Only the advisor model is read from it (advisorIterationModels).
	Iterations []usageIteration `json:"iterations,omitempty"`
}

type usageIteration struct {
	Type  string `json:"type"`
	Model string `json:"model,omitempty"`
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

	// `assistant.error` (e.g. `rate_limit`, `authentication_failed`) is
	// surfaced as a fatal EventError below. Claude has emitted this enum in
	// two places across versions: under `message.error`, and as a top-level
	// envelope field next to `message`. Computed here because BOTH the
	// synthetic-command-result branch and the text/thinking recovery branches
	// below gate on it: an error envelope's text is the error copy, owned by
	// the EventError path, and re-emitting it as content duplicates the
	// api_error row (see TestAssistantErrorEnvelopeDoesNotRecoverErrorTextAsContent).
	errorEnum := assistantErrorEnum(raw, msg)

	// CLI-generated local command output. The CLI runs `/usage`, `/context`,
	// a skill, a plugin command etc. itself — no API call, num_turns 0 — and
	// delivers the output as an `assistant` envelope stamped with its own
	// `<synthetic>` model sentinel (upstream `SYNTHETIC_MODEL`, emitted by
	// `localCommandOutputToSDKAssistantMessage`). Routed to its own event so
	// it can never land in an assistant bubble, and returned early so it
	// neither becomes the turn's `assistant_message_id` (it is not model
	// output) nor gets recovered a second time by the text branch below.
	//
	// The errorEnum precedence is load-bearing: `<synthetic>` is ALSO the
	// model on the CLI's synthesized API-error message, and that one belongs
	// to the EventError path.
	if errorEnum == "" && isSyntheticCLIModel(msg.Model) {
		return p.commandResultEvents(threadID, msg, now, line), nil
	}

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

	for i, block := range msg.Content {
		switch block.Type {
		case "text":
			// Drop already-streamed text — re-emitting the coalesced
			// snapshot would double it (the bug pinned by
			// TestAssistantEnvelopeDoesNotDuplicateStreamedText). A
			// never-streamed message is a CLI-internal retry whose reply
			// arrived as a snapshot with no stream lifecycle; recover it so
			// it isn't silently lost. See the streamedMessageIDs field doc.
			// Skip error envelopes entirely (errorEnum != ""): their text is
			// the error copy, owned by the EventError path below — see the
			// errorEnum comment above.
			if errorEnum == "" && !p.hasStreamedMessageID(msg.ID) {
				events = p.appendRecoveredBlockEvent(events, threadID, parentToolUseID, msg.ID, "text", block.Text, now)
			}
		case "tool_use":
			events = p.appendAdvisorCallsNotRun(events, threadID, parentToolUseID, msg.ID, now)
			events = p.appendToolUseEvent(events, threadID, parentToolUseID, msg.ID, now, block)
		case "thinking":
			// Same contract as text, including the error-envelope skip. The
			// thinking signature is not recovered: on the wire it rides
			// `signature_delta`, which we deliberately don't handle
			// (claude-wire.md §Delta types) — the CLI's session jsonl is
			// authoritative for resume/fork replay, so AO never needs the
			// signature. See the streamedMessageIDs field doc.
			if errorEnum == "" && !p.hasStreamedMessageID(msg.ID) {
				events = p.appendRecoveredBlockEvent(events, threadID, parentToolUseID, msg.ID, "thinking", block.Thinking, now)
			}
		case "server_tool_use":
			// Claude's server-side tool call (today: `advisor`). The
			// matching result arrives on a SECOND assistant envelope
			// carrying an `advisor_tool_result` content block — see
			// docs/references/claude-wire.md §server_tool_use.
			events = p.appendServerToolUseEvent(events, threadID, parentToolUseID, msg.ID, advisorIterationModels(msg.Usage), msg.Content[i+1:], now, block)
		case "advisor_tool_result":
			// Result of a prior `server_tool_use` advisor call. Closes
			// the tool lifecycle for the matching `srvtoolu_*` id.
			events = p.appendAdvisorResultEvent(events, threadID, parentToolUseID, now, line, block)
		}
	}

	if !advisorOnly {
		if parentToolUseID == "" && errorEnum == "" {
			events = append(events, p.reportMessageUsage(threadID, msg.ID, msg.Model, msg.Usage, now)...)
		}
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
	// errorEnum is computed once above (it also gates content recovery).
	if errorEnum != "" {
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
			Failure:         claudeAssistantFailure(errorEnum, parentToolUseID != ""),
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		})
	}

	return events, nil
}

func claudeAssistantFailure(errorEnum string, closesParentTurn bool) *provider.FailureMeta {
	class := provider.FailureFatal
	var reason provider.FailureReason
	switch errorEnum {
	case "rate_limit":
		class = provider.FailureTransient
		reason = provider.FailureReasonUsageLimit
	case "server_error":
		class = provider.FailureTransientAfterRetry
	}
	failure := &provider.FailureMeta{
		Class: class, Boundary: provider.FailureBoundaryTurn, Reason: reason, Code: errorEnum,
	}
	if closesParentTurn {
		failure.Scope = provider.FailureScopeParentTurn
	}
	return failure
}

// appendRecoveredBlockEvent emits a completed-block event for a
// text/thinking block that arrived on a coalesced `assistant` snapshot
// without ever streaming: a top-level CLI-internal retry, or any
// forwarded subagent block, since subagents never stream (see the `text`
// branch in parseAssistant and the streamedMessageIDs field doc). It
// rides the EventContentBlockStop channel so triage's late-completion
// handler (settleStreaming*Async's !active+ContentPresent branch)
// persists it as a completed row directly — no streaming state, no
// dependence on the turn-lifecycle late-fold settle.
//
// The item id is `message.id#ordinal` (recoveredBlockItemID), the ordinal
// coming from a parser-tracked per-message counter rather than the
// envelope-local content index, because Claude delivers each block as its
// own single-block snapshot envelope (so the content index is always 0).
// That keeps two same-kind blocks of one message on distinct rows instead
// of the second overwriting the first via FindStreamItemByProviderItemID.
// Empty blocks are skipped (nothing to recover) and do not consume an
// ordinal.
//
// Note: because this event carries content with ContentPresent=true,
// triage persists it inline on the thread's event worker (the !active
// branch of settleStreaming*Async runs persistOrUpdateCompleted*), as it
// does any completed row, after waiting for the thread's in-flight
// stream settles. A normal empty content_block_stop settles async.
func (p *Parser) appendRecoveredBlockEvent(
	events []provider.ProviderEvent,
	threadID, parentToolUseID, messageID string,
	blockType, content string,
	now time.Time,
) []provider.ProviderEvent {
	if content == "" {
		return events
	}
	index := p.nextRecoveredBlockIndex(parentToolUseID, messageID)
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventContentBlockStop,
		ThreadID:        threadID,
		ItemID:          recoveredBlockItemID(messageID, index),
		Content:         content,
		ContentPresent:  true,
		Meta:            blockMeta(index, blockType),
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
	})
}

// syntheticCLIModel is the sentinel the Claude CLI stamps on every assistant
// message it authored itself rather than received from the API (upstream
// `SYNTHETIC_MODEL`, claude-code-source-code/src/utils/messages.ts). On AO's
// stream-json wire the only producer that reaches us with content and no error
// enum is local slash-command output; the error producer is discriminated by
// `assistant.error` before this is consulted. See claude-wire.md §"Slash
// commands".
const syntheticCLIModel = "<synthetic>"

func isSyntheticCLIModel(model string) bool {
	return strings.TrimSpace(model) == syntheticCLIModel
}

// commandResultEvents projects a `<synthetic>`-model assistant envelope into a
// single EventCommandResult carrying the command's output text.
//
// The envelope is a complete snapshot — the CLI never streams command output
// (no `stream_event` deltas were observed for it on 2.1.219) — so this is one
// completed event, not a stream lifecycle. Multiple text blocks are joined
// with a blank line, the same way the CLI's own wrapper concatenates stdout and
// stderr sections; non-text blocks cannot occur on this envelope and are
// ignored rather than guessed at.
//
// Empty output emits nothing: a command that printed nothing has no row to
// show, and a blank one would read as a failed command.
//
// The event is emitted even when its transcript row is suppressed
// (`CommandResultMeta.Suppressed`, decided from the send-time candidate AND
// this text — see command_result_suppression.go). Suppression removes a ROW,
// not a signal:
// the live-config settle path and the peer-rename read-back both consume
// this event, and dropping it here would break the very confirmations the
// suppressed commands exist to deliver.
func (p *Parser) commandResultEvents(threadID string, msg assistantMessage, now time.Time, line []byte) []provider.ProviderEvent {
	var parts []string
	for _, block := range msg.Content {
		if block.Type != "text" {
			continue
		}
		if text := strings.TrimRight(block.Text, "\n"); strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	text := strings.Join(parts, "\n\n")
	if text == "" {
		return nil
	}
	var meta json.RawMessage
	if p.activeCommandUUID != "" {
		resultMeta := provider.CommandResultMeta{CommandUUID: p.activeCommandUUID}
		if p.peerTurns != nil {
			command, known := p.peerTurns.directSlashCommand(p.activeCommandUUID)
			if known && !command.Internal {
				resultMeta.UserCommand = command.Name
			}
			// Inside this command's own started -> completed window, so the
			// uuid is what says which command this text answers. Both calls
			// are no-ops for a uuid the session has nothing registered for.
			p.peerTurns.notePeerRenameOutput(p.activeCommandUUID, text)
			// The TEXT is half the answer: a user-typed state echo is
			// suppressed only when this is a recognised confirmation of it,
			// so a rejected /model slug keeps the row that says so
			// (command_result_suppression.go).
			resultMeta.Suppressed = p.peerTurns.commandResultRowSuppressed(p.activeCommandUUID, text)
		}
		meta, _ = json.Marshal(resultMeta)
	}
	events := []provider.ProviderEvent{{
		Kind:           provider.EventCommandResult,
		ThreadID:       threadID,
		ItemID:         strings.TrimSpace(msg.ID),
		Content:        text,
		ContentPresent: true,
		Meta:           meta,
		Timestamp:      now,
		Raw:            line,
	}}
	if p.activeCommandUUID != "" && p.holdMirroredCommandOutput(p.activeCommandUUID, events) {
		return nil
	}
	return events
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
	// Claude Code 2.1.150's TaskCreate / TaskUpdate family replaces
	// TodoWrite with per-task CRUD. We mirror TodoWrite's reroute:
	// stage the input here and emit the EventTodoUpdate snapshot only
	// after the matching tool_result confirms success (TaskCreate's
	// authoritative id, TaskUpdate's success flag). TaskList / TaskGet
	// are read-only and render as regular tool rows.
	if block.Name == "TaskCreate" || block.Name == "TaskUpdate" {
		p.recordPendingTaskMutation(block.ID, taskMutationOp(block.Name), block.Input, parentToolUseID)
		return events
	}

	// EnterWorktree / ExitWorktree render as ordinary tool rows AND move
	// the process's working directory; the result side emits the
	// workspace change (parse_worktree.go).
	if isWorktreeToolName(block.Name) {
		p.markWorktreeTool(block.ID, block.Name, parentToolUseID)
	}

	isBackground := hasRunInBackground(block.Input)
	if isBackground {
		p.markBackground(block.ID, backgroundHintInput)
	}
	if isAgentLaunchToolName(block.Name) {
		p.markAgentLaunchTool(block.ID)
	}
	if block.Name == sessionimport.BashToolName || block.Name == sessionimport.MonitorToolName {
		p.markAckTool(block.ID, block.Name)
	}

	meta := marshalToolMeta(block.Name, block.Input, isBackground, isAgentLaunchToolName(block.Name), assistantMessageID)
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
//
// `id` and `owner` are populated by the Claude Code 2.1.150+ Task*
// family path (triage projects per-task events into this shape via
// taskStepsLocked); legacy TodoWrite and Codex
// update_plan leave them empty so omitempty drops them from the
// marshaled JSON. Keep the two producers writing this same shape so
// triage's decodeTodoSteps stays the only decoder.
type todoWriteStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
	ID     string `json:"id,omitempty"`
	Owner  string `json:"owner,omitempty"`
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

// taskMutationOp returns the `op` tag used to discriminate pending
// TaskCreate / TaskUpdate mutations in parse_user.go's apply path.
// Centralised so the assistant-side recorder and the result-side
// applier reference one source of truth.
func taskMutationOp(toolName string) string {
	switch toolName {
	case "TaskCreate":
		return "create"
	case "TaskUpdate":
		return "update"
	}
	return ""
}

// taskCreateInput is the decoded TaskCreate tool_use input. Only the
// fields the snapshot currently renders are typed; `description`,
// `activeForm`, and `metadata` round-trip through Claude untouched
// because json.Unmarshal already ignores unknown fields silently.
type taskCreateInput struct {
	Subject string `json:"subject"`
}

func decodeTaskCreateInput(input json.RawMessage) (taskCreateInput, bool) {
	if len(input) == 0 {
		return taskCreateInput{}, false
	}
	var decoded taskCreateInput
	if err := json.Unmarshal(input, &decoded); err != nil {
		return taskCreateInput{}, false
	}
	decoded.Subject = strings.TrimSpace(decoded.Subject)
	if decoded.Subject == "" {
		// Subject is required per schema and is what the activity rail
		// renders. A create with no subject would land in the widget
		// as a blank row.
		return taskCreateInput{}, false
	}
	return decoded, true
}

// taskUpdateInput is the decoded TaskUpdate tool_use input. Only the
// fields the snapshot currently surfaces are explicitly typed; the
// rest (addBlocks, addBlockedBy, description, activeForm, metadata)
// are forward-compat additions left to round-trip through Claude on
// its own.
type taskUpdateInput struct {
	TaskID  string `json:"taskId"`
	Status  string `json:"status"`
	Subject string `json:"subject"`
	Owner   string `json:"owner"`
}

func decodeTaskUpdateInput(input json.RawMessage) (taskUpdateInput, bool) {
	if len(input) == 0 {
		return taskUpdateInput{}, false
	}
	var decoded taskUpdateInput
	if err := json.Unmarshal(input, &decoded); err != nil {
		return taskUpdateInput{}, false
	}
	decoded.TaskID = strings.TrimSpace(decoded.TaskID)
	if decoded.TaskID == "" {
		return taskUpdateInput{}, false
	}
	decoded.Status = strings.TrimSpace(decoded.Status)
	decoded.Subject = strings.TrimSpace(decoded.Subject)
	decoded.Owner = strings.TrimSpace(decoded.Owner)
	return decoded, true
}

// normalizeTaskStatus reuses the TodoWrite enum mapping with two
// adjustments:
//
//  1. Empty input returns `("", false)` so a TaskUpdate that does
//     not include a `status` field leaves the existing task status
//     alone. Callers gate mutation on the non-empty case; without
//     this, a `TaskUpdate({owner: "..."})` would clobber the
//     status to the normaliser's default `pending` bucket.
//  2. `deleted` is a terminal status TaskUpdate alone can set,
//     signalling permanent removal from the list. Callers branch on
//     this rather than passing it through to the snapshot (deleted
//     tasks should disappear, not render as a fourth bucket).
func normalizeTaskStatus(raw string) (status string, deleted bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	if trimmed == "deleted" {
		return "", true
	}
	return normalizeTodoWriteStatus(trimmed), false
}

// taskCreateResultID extracts the authoritative task id from a
// TaskCreate tool_use_result. The Claude wire shape is
// `{task: {id: "...", subject: "..."}}` — the inner id is what
// subsequent TaskUpdate calls reference, and what we key the parser's
// local mirror on. Empty result means the create failed; the caller
// drops the mutation without touching state.
func taskCreateResultID(toolUseResult json.RawMessage) string {
	if len(toolUseResult) == 0 {
		return ""
	}
	var payload struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if json.Unmarshal(toolUseResult, &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.Task.ID)
}

// taskUpdateResult captures the success flag a TaskUpdate result
// echoes. Failed updates (`success:false`) skip the apply step so a
// TaskUpdate that Claude rejected does not silently mutate our local
// mirror.
type taskUpdateResult struct {
	Success bool
}

func decodeTaskUpdateResult(toolUseResult json.RawMessage) (taskUpdateResult, bool) {
	if len(toolUseResult) == 0 {
		return taskUpdateResult{}, false
	}
	var payload struct {
		Success bool `json:"success"`
	}
	if json.Unmarshal(toolUseResult, &payload) != nil {
		return taskUpdateResult{}, false
	}
	return taskUpdateResult{Success: payload.Success}, true
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

// isAgentLaunchToolName reports whether toolName is Claude's
// subagent-launching tool: "Agent" on current CLIs (2.1.170+, every
// captured local_agent fixture) and "Task" on older builds — the same
// two-name set claudetui/reconstruct.go (agentLaunches) and triage's
// tool_meta_rules match, kept in sync so a CLI version change degrades
// gracefully. Matching only "Agent" would leave a "Task"-named launch
// unmarked, and the resume-detection reconnect fallback
// (parse_system.go task_started case 2) would then misclassify that
// ordinary launch as a resume and wrongly background it.
// task_type:"local_agent" on system/task_started is a conceptual
// classification, not a tool name (see claude-wire.md
// §system/task_started).
func isAgentLaunchToolName(name string) bool {
	return name == "Agent" || name == "Task"
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
func marshalToolMeta(toolName string, input json.RawMessage, isBackground, isAgentLaunch bool, assistantMessageID string) json.RawMessage {
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
	if isAgentLaunch {
		fields[provider.MetaSubagentLaunchKey] = true
	}
	if assistantMessageID != "" {
		fields["assistant_message_id"] = assistantMessageID
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
// The advisor can run on a different model than the parent, and the
// only per-call record of it is usage.iterations[type=advisor_message].model
// (see advisorMessageState). advisorModels is that list from this
// envelope's usage, empty when the envelope carries no iterations. The
// launch carries `advisor_model` only when the model is already known;
// otherwise stampAdvisorModels adds it when the message's usage arrives.
//
// `markAdvisor` remembers the id so the matching `advisor_tool_result`
// block can identify which completion is an advisor result vs a
// regular tool_result (the latter never reaches this path — those are
// user-role envelopes handled in parse_user.go).
//
// A call the API will never run emits nothing (advisorCallDropped).
// later is the rest of this envelope's content after the block.
func (p *Parser) appendServerToolUseEvent(
	events []provider.ProviderEvent,
	threadID, parentToolUseID, assistantMessageID string,
	advisorModels []string,
	later []assistantContentBlock,
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
	if p.advisorCallDropped(parentToolUseID, assistantMessageID, block.ID, later) {
		return events
	}
	p.markAdvisor(block.ID)
	model := p.recordAdvisorCall(parentToolUseID, assistantMessageID, block.ID, advisorModels)
	meta := marshalAdvisorToolMeta(block.Name, model, assistantMessageID)
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
// inner `content` is `{type:"advisor_result", text:"..."}`. A failed
// call answers `{type:"advisor_tool_result_error", error_code:"..."}`
// (observed codes: `overloaded`, `too_many_requests`); the completion
// is an error carrying `advisor_error_code`. Meta is otherwise minimal:
// no exit_code, no is_background path; the advisor runs inline.
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

	result := decodeAdvisorResult(block.Content)
	fields := map[string]any{"is_error": result.isError}
	if result.errorCode != "" {
		fields["advisor_error_code"] = result.errorCode
	}
	meta, _ := json.Marshal(fields)
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventToolComplete,
		ThreadID:        threadID,
		ItemID:          block.ToolUseID,
		Content:         result.text,
		Meta:            meta,
		ParentToolUseID: parentToolUseID,
		Timestamp:       now,
		Raw:             line,
	})
}

// advisorResult is the decoded inner `content` of an
// `advisor_tool_result` block.
type advisorResult struct {
	text      string
	isError   bool
	errorCode string
}

// decodeAdvisorResult reads the nested `content` object on an
// `advisor_tool_result` block. The wire shape is
// `{type:"advisor_result", text:"..."}` on success (distinct from the
// user-side tool_result `content`, which is string-or-array) and
// `{type:"advisor_tool_result_error", error_code:"..."}` on failure.
// Any other shape (including `advisor_redacted_result`, whose body is
// encrypted) yields an empty success: the completion still settles the
// running row, and a parser refresh that recognises the new shape is
// the right place to handle it.
func decodeAdvisorResult(content json.RawMessage) advisorResult {
	if len(content) == 0 {
		return advisorResult{}
	}
	var payload struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		ErrorCode string `json:"error_code"`
	}
	if json.Unmarshal(content, &payload) != nil {
		return advisorResult{}
	}
	switch payload.Type {
	case "advisor_result":
		return advisorResult{text: payload.Text}
	case "advisor_tool_result_error":
		return advisorResult{isError: true, errorCode: strings.TrimSpace(payload.ErrorCode)}
	}
	return advisorResult{}
}

// advisorMessageState pairs the advisor calls of one API message with the
// models that ran them. The API reports an advisor's model only as
// usage.iterations[type=advisor_message].model, one entry per advisor
// call in call order, and all of a message's advisor calls share that
// message's usage. Headless stream-json delivers the iterations on the
// closing `message_delta`, after the `server_tool_use` envelopes; the
// TUI reconstructor puts them on the assembled assistant envelope. The
// parent's `message.model` is never the advisor's model source: the
// advisor is configured separately and routinely differs.
//
// It also records whether the message holds a client tool_use. The API
// runs a server tool only in a response with no client tool_use, so an
// advisor call sharing its message with one never runs and never gets a
// result (claude-wire.md §Orphaned server-side tool calls). A real
// call's result is always the block right after the call, before any
// client tool_use of its message.
type advisorMessageState struct {
	messageID string
	// clientToolUse is set once the message carried a client tool_use.
	clientToolUse bool
	// calls are the message's advisor tool ids in call order.
	calls []string
	// models are the advisor iteration models in call order.
	models []string
	// stamped counts the leading calls whose model has been emitted.
	stamped int
}

// advisorIterationModels returns the advisor models from a usage
// breakdown in call order, or nil when it lists none.
func advisorIterationModels(u *assistantUsage) []string {
	if u == nil {
		return nil
	}
	var models []string
	for _, it := range u.Iterations {
		if it.Type == "advisor_message" {
			models = append(models, strings.TrimSpace(it.Model))
		}
	}
	return models
}

// advisorState returns the scope's state for messageID, starting fresh
// when the scope has moved on to another message.
func (p *Parser) advisorState(scope, messageID string) *advisorMessageState {
	st := p.advisorMessages[scope]
	if st != nil && st.messageID == messageID {
		return st
	}
	if p.advisorMessages == nil || (st == nil && len(p.advisorMessages) >= parserTaskMapCap) {
		p.advisorMessages = make(map[string]*advisorMessageState)
	}
	st = &advisorMessageState{messageID: messageID}
	p.advisorMessages[scope] = st
	return st
}

// recordAdvisorCall appends an advisor call to its message and returns
// its model when the message's usage already reported it, "" otherwise.
func (p *Parser) recordAdvisorCall(scope, messageID, toolUseID string, models []string) string {
	st := p.advisorState(scope, messageID)
	if len(models) > len(st.models) {
		st.models = models
	}
	st.calls = append(st.calls, toolUseID)
	idx := len(st.calls) - 1
	if st.stamped != idx || idx >= len(st.models) || st.models[idx] == "" {
		return ""
	}
	st.stamped++
	return st.models[idx]
}

// advisorCallDropped reports whether an advisor call is one the API
// will not run: its message already carried a client tool_use, or a
// client tool_use follows it in this envelope before its result
// (claudetui assembles a whole message into one envelope). Such a call
// gets no row. Across envelopes only a message with an id is judged
// (appendAdvisorCallsNotRun).
func (p *Parser) advisorCallDropped(scope, messageID, toolUseID string, later []assistantContentBlock) bool {
	if st := p.advisorMessages[scope]; st != nil && st.messageID == messageID && st.clientToolUse {
		return true
	}
	for _, block := range later {
		if block.Type == "advisor_tool_result" && block.ToolUseID == toolUseID {
			return false
		}
		if block.Type == "tool_use" {
			return true
		}
	}
	return false
}

// appendAdvisorCallsNotRun handles a client tool_use: it records that
// the scope's current message holds one and settles each advisor call
// of that message still waiting for its result as declined, since the
// API will not run it. A settled call leaves the model pairing so a
// late `message_delta` model can only land on a call that ran. A
// message without an id is never judged.
func (p *Parser) appendAdvisorCallsNotRun(
	events []provider.ProviderEvent,
	threadID, scope, messageID string,
	now time.Time,
) []provider.ProviderEvent {
	if messageID == "" {
		return events
	}
	st := p.advisorState(scope, messageID)
	st.clientToolUse = true
	kept := st.calls[:0]
	for _, id := range st.calls {
		if !p.isAdvisor(id) {
			kept = append(kept, id)
			continue
		}
		p.clearAdvisor(id)
		events = append(events, provider.ProviderEvent{
			Kind:            provider.EventToolComplete,
			ThreadID:        threadID,
			ItemID:          id,
			Meta:            advisorNotRunMeta,
			ParentToolUseID: scope,
			Timestamp:       now,
		})
	}
	st.calls = kept
	return events
}

// advisorNotRunMeta settles an advisor call the API did not run.
var advisorNotRunMeta = json.RawMessage(`{"is_error":false,"item_status":"declined"}`)

// startAdvisorMessage drops the scope's advisor state when a new API
// message begins; calls of an interrupted message never get a model.
func (p *Parser) startAdvisorMessage(scope string) {
	if p == nil {
		return
	}
	delete(p.advisorMessages, scope)
}

// stampAdvisorModels handles the closing `message_delta` usage of the
// scope's current message: each advisor call launched without a model
// gets a meta-only EventToolStart carrying `advisor_model`. The message
// is over, so the scope's state is released.
func (p *Parser) stampAdvisorModels(events []provider.ProviderEvent, threadID, scope string, u *assistantUsage, now time.Time) []provider.ProviderEvent {
	if p == nil {
		return events
	}
	st := p.advisorMessages[scope]
	if st == nil {
		return events
	}
	models := advisorIterationModels(u)
	if len(models) == 0 {
		return events
	}
	delete(p.advisorMessages, scope)
	for i := st.stamped; i < len(st.calls) && i < len(models); i++ {
		if models[i] == "" {
			continue
		}
		meta, _ := json.Marshal(map[string]any{
			"advisor_model":    models[i],
			"meta_update_only": true,
		})
		events = append(events, provider.ProviderEvent{
			Kind:      provider.EventToolStart,
			ThreadID:  threadID,
			ItemID:    st.calls[i],
			ItemType:  "advisor",
			Meta:      meta,
			Timestamp: now,
		})
	}
	return events
}

// marshalAdvisorToolMeta builds the EventToolStart Meta for an advisor
// invocation. Mirrors marshalToolMeta's shape for triage compatibility
// (same toolName/input/assistant_message_id keys) but adds
// `advisor_model` when known — the frontend's AdvisorRow renders it via
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
