package claudetui

import (
	"encoding/json"
	"strings"
)

// reconstruct.go turns the raw Anthropic /v1/messages SSE stream of one agent
// turn into the Claude Code stream-json envelopes the shared claude.Parser
// consumes. Nothing here emits provider.ProviderEvents directly — it produces
// envelope bytes that session.go feeds through claude.Parser, so the TUI path
// and the headless path converge on identical event output.
//
// This is the real-time port of ao_transform.py's assemble_assistant() +
// envelope synthesis. See docs/architecture/claude-tui-provider.md §The core
// idea.

// --- raw SSE shapes -------------------------------------------------------

type sseEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        json.RawMessage `json:"delta"`
	Message      json.RawMessage `json:"message"`
	Usage        json.RawMessage `json:"usage"`
}

type sseDelta struct {
	Type        string          `json:"type"`
	Text        string          `json:"text"`
	Thinking    string          `json:"thinking"`
	Signature   string          `json:"signature"`
	PartialJSON string          `json:"partial_json"`
	StopReason  string          `json:"stop_reason"`
	Citation    json.RawMessage `json:"citation"`
}

type sseMessageStart struct {
	ID    string          `json:"id"`
	Model string          `json:"model"`
	Usage json.RawMessage `json:"usage"`
}

// --- stream-json envelope shapes (what claude.Parser reads) --------------

type streamEventEnvelope struct {
	Type  string          `json:"type"`  // "stream_event"
	Event json.RawMessage `json:"event"` // the raw SSE event, verbatim
	// ParentToolUseID nests a subagent's streaming deltas under the Agent
	// tool_call that launched it (top-level, exactly as headless emits it). Empty
	// (omitted) for the main agent loop. See turndriver.go §subagent path.
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`
}

type assistantEnvelope struct {
	Type    string           `json:"type"` // "assistant"
	Message assistantMessage `json:"message"`
	// ParentToolUseID nests a subagent assistant message (and the tool_use
	// starts inside it) under its parent Agent tool_call. Empty for the main loop.
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`
}

type assistantMessage struct {
	ID      string            `json:"id,omitempty"`
	Model   string            `json:"model,omitempty"`
	Role    string            `json:"role"` // "assistant"
	Content []json.RawMessage `json:"content"`
	Usage   json.RawMessage   `json:"usage,omitempty"`
}

type resultEnvelope struct {
	Type       string          `json:"type"`    // "result"
	Subtype    string          `json:"subtype"` // "success" | "error_during_execution"
	StopReason string          `json:"stop_reason,omitempty"`
	Usage      json.RawMessage `json:"usage,omitempty"`
	Errors     []string        `json:"errors,omitempty"`
}

type initEnvelope struct {
	Type      string   `json:"type"`    // "system"
	Subtype   string   `json:"subtype"` // "init"
	SessionID string   `json:"session_id,omitempty"`
	Model     string   `json:"model,omitempty"`
	CWD       string   `json:"cwd,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	Version   string   `json:"claude_code_version,omitempty"`
}

// replayUserEnvelope is the `user{isReplay:true}` echo headless emits to confirm
// a user message entered the agent's context. It is the sole wire source of the
// parser's EventUserText, which consumes triage's pending-send FIFO and stamps
// provider_item_id onto the optimistic user row. The reconstructor synthesizes
// one per AO Send so the TUI path confirms sends exactly as headless does. The
// top-level uuid becomes provider_item_id (parse_user_replay.go reads
// firstNonEmpty(message.id, raw["uuid"])).
type replayUserEnvelope struct {
	Type     string            `json:"type"`     // "user"
	IsReplay bool              `json:"isReplay"` // true — routes to parseUserReplay
	UUID     string            `json:"uuid,omitempty"`
	Message  replayUserMessage `json:"message"`
}

type replayUserMessage struct {
	Role    string `json:"role"`    // "user"
	Content string `json:"content"` // plain string; extractToolResultText handles it
}

// taskUpdatedEnvelope is the system/task_updated stream-json envelope headless
// emits when a backgrounded task reaches a terminal state. claude-tui synthesizes
// it from a request-body <task-notification> (turndriver.emitBackgroundCompletions);
// the parser turns it into EventBackgroundTaskTerminal, which triage stashes as the
// host-side process exit. That stash is what the paired task_notification drains to
// write the tool_completion sibling — without it the notification alone never
// completes the launch (triage invariant 21: task_notification is not a completion
// source). See docs/architecture/claude-tui-provider.md §Background completions.
type taskUpdatedEnvelope struct {
	Type      string           `json:"type"`    // "system"
	Subtype   string           `json:"subtype"` // "task_updated"
	TaskID    string           `json:"task_id"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
	Patch     taskUpdatedPatch `json:"patch"`
}

type taskUpdatedPatch struct {
	Status string `json:"status"` // completed | failed | killed
}

// taskNotificationEnvelope is the system/task_notification stream-json envelope.
// Paired with (and fed right after) taskUpdatedEnvelope, it carries the human
// summary and output_file; the parser emits EventBackgroundTaskNotification, which
// drains the stash the task_updated left and writes the tool_completion sibling at
// the current write head.
type taskNotificationEnvelope struct {
	Type       string `json:"type"`    // "system"
	Subtype    string `json:"subtype"` // "task_notification"
	TaskID     string `json:"task_id"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	Status     string `json:"status,omitempty"`
	OutputFile string `json:"output_file,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// --- per-turn assembler ---------------------------------------------------

// messageAssembler replays one agent request's SSE content-block deltas into a
// complete assistant message (mirrors ao_transform.assemble_assistant). It is
// single-use: one assembler per /v1/messages agent response.
type messageAssembler struct {
	order    []int
	blocks   map[int]map[string]json.RawMessage
	text     map[int]*strings.Builder
	thinking map[int]*strings.Builder
	sig      map[int]*strings.Builder
	inputBuf map[int]*strings.Builder

	msgID string
	model string
	usage json.RawMessage
	stop  string
}

func newMessageAssembler() *messageAssembler {
	return &messageAssembler{
		blocks:   map[int]map[string]json.RawMessage{},
		text:     map[int]*strings.Builder{},
		thinking: map[int]*strings.Builder{},
		sig:      map[int]*strings.Builder{},
		inputBuf: map[int]*strings.Builder{},
	}
}

// consume folds one raw SSE event into the assembler. Unparseable events are
// skipped — the read path can't recover a broken chunk, and a single dropped
// delta is strictly less harmful than aborting the turn.
func (a *messageAssembler) consume(raw json.RawMessage) {
	var ev sseEvent
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	switch ev.Type {
	case "content_block_start":
		var block map[string]json.RawMessage
		if json.Unmarshal(ev.ContentBlock, &block) != nil {
			return
		}
		if _, seen := a.blocks[ev.Index]; !seen {
			a.order = append(a.order, ev.Index)
		}
		a.blocks[ev.Index] = block

	case "content_block_delta":
		var d sseDelta
		if json.Unmarshal(ev.Delta, &d) != nil {
			return
		}
		a.ensureBlock(ev.Index)
		switch d.Type {
		case "text_delta":
			a.text[ev.Index].WriteString(d.Text)
		case "thinking_delta":
			a.thinking[ev.Index].WriteString(d.Thinking)
		case "signature_delta":
			a.sig[ev.Index].WriteString(d.Signature)
		case "input_json_delta":
			a.inputBuf[ev.Index].WriteString(d.PartialJSON)
		}

	case "message_start":
		var m sseMessageStart
		if json.Unmarshal(ev.Message, &m) == nil {
			a.msgID = m.ID
			a.model = m.Model
			if len(m.Usage) > 0 {
				a.usage = m.Usage
			}
		}

	case "message_delta":
		if len(ev.Usage) > 0 {
			a.usage = ev.Usage
		}
		var d sseDelta
		if json.Unmarshal(ev.Delta, &d) == nil && d.StopReason != "" {
			a.stop = d.StopReason
		}
	}
}

// ensureBlock guarantees a block slot exists for an index whose
// content_block_start we never saw (an orphan delta) so the delta isn't lost.
func (a *messageAssembler) ensureBlock(index int) {
	if _, ok := a.blocks[index]; !ok {
		a.blocks[index] = map[string]json.RawMessage{"type": json.RawMessage(`"unknown"`)}
		a.order = append(a.order, index)
	}
	if a.text[index] == nil {
		a.text[index] = &strings.Builder{}
	}
	if a.thinking[index] == nil {
		a.thinking[index] = &strings.Builder{}
	}
	if a.sig[index] == nil {
		a.sig[index] = &strings.Builder{}
	}
	if a.inputBuf[index] == nil {
		a.inputBuf[index] = &strings.Builder{}
	}
}

// assistantLine renders the assembled assistant envelope as a stream-json
// NDJSON line. parentToolUseID is the launching Agent tool_call for a subagent
// response (empty for the main loop); it threads through as top-level
// parent_tool_use_id so the shared parser nests the message and its tool_use
// starts under that Agent card.
func (a *messageAssembler) assistantLine(parentToolUseID string) json.RawMessage {
	content := make([]json.RawMessage, 0, len(a.order))
	for _, idx := range a.order {
		content = append(content, a.renderBlock(idx))
	}
	env := assistantEnvelope{
		Type: "assistant",
		Message: assistantMessage{
			ID:      a.msgID,
			Model:   a.model,
			Role:    "assistant",
			Content: content,
			Usage:   a.usage,
		},
		ParentToolUseID: parentToolUseID,
	}
	return mustMarshal(env)
}

// agentLaunches harvests the Agent/Task tool_use blocks from an assembled main
// assistant message: their tool_use id and the `prompt` from their input. The
// prompt is what a following subagent request content-matches on to resolve its
// parent (see reconstructor.resolveSubagentParent). The wire tool name is
// "Agent" on 2.1.170 and "Task" on older builds; both are matched so a version
// bump degrades to "subagent not nested", never a panic. Nesting itself is by
// parent_tool_use_id, not the name, so this filter only scopes the registry.
func (a *messageAssembler) agentLaunches() []agentLaunch {
	var out []agentLaunch
	for _, idx := range a.order {
		block := a.blocks[idx]
		if rawString(block["type"]) != "tool_use" {
			continue
		}
		switch rawString(block["name"]) {
		case "Agent", "Task":
		default:
			continue
		}
		id := rawString(block["id"])
		if id == "" {
			continue
		}
		prompt := ""
		if b := a.inputBuf[idx]; b != nil && b.Len() > 0 {
			var in struct {
				Prompt string `json:"prompt"`
			}
			if json.Unmarshal([]byte(b.String()), &in) == nil {
				prompt = in.Prompt
			}
		}
		out = append(out, agentLaunch{toolUseID: id, prompt: prompt})
	}
	return out
}

// renderBlock merges a block's content_block fields with its accumulated
// streaming text/thinking/signature/input.
func (a *messageAssembler) renderBlock(index int) json.RawMessage {
	block := map[string]json.RawMessage{}
	for k, v := range a.blocks[index] {
		block[k] = v
	}
	if b := a.text[index]; b != nil && b.Len() > 0 {
		block["text"] = mustMarshal(b.String())
	}
	if b := a.thinking[index]; b != nil && b.Len() > 0 {
		block["thinking"] = mustMarshal(b.String())
	}
	if b := a.sig[index]; b != nil && b.Len() > 0 {
		block["signature"] = mustMarshal(b.String())
	}
	if b := a.inputBuf[index]; b != nil && b.Len() > 0 {
		block["input"] = parseToolInput(b.String())
	}
	return mustMarshal(block)
}

// parseToolInput parses an accumulated input_json_delta string. A malformed
// accumulation is preserved under a sentinel key rather than dropped, matching
// ao_transform.py — the model's intent is still inspectable downstream.
func parseToolInput(s string) json.RawMessage {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return mustMarshal(map[string]string{"__unparsed_partial_json__": s})
}

// --- envelope synthesizers ------------------------------------------------

// streamEventLine wraps a raw SSE event as a stream_event envelope for live
// delta passthrough. parentToolUseID is empty for the main loop and the
// launching Agent tool_call for a subagent's deltas.
func streamEventLine(sse json.RawMessage, parentToolUseID string) json.RawMessage {
	return mustMarshal(streamEventEnvelope{Type: "stream_event", Event: sse, ParentToolUseID: parentToolUseID})
}

// resultLine synthesizes a turn-complete result envelope. usage is the
// accumulated turn usage (flat snake_case). aborted produces the shape
// parse_result.detectInterrupted classifies as an interrupt.
func resultLine(stopReason string, usage json.RawMessage, aborted bool) json.RawMessage {
	if aborted {
		return mustMarshal(resultEnvelope{
			Type:    "result",
			Subtype: "error_during_execution",
			Errors:  []string{"Request interrupted by user"},
		})
	}
	return mustMarshal(resultEnvelope{
		Type:       "result",
		Subtype:    "success",
		StopReason: stopReason,
		Usage:      usage,
	})
}

// initLine synthesizes the system:init envelope. session_id and version come
// from the SessionStart hook (or "" before it arrives); model and tools come
// from the first agent request body.
func initLine(sessionID, model, cwd, version string, tools []string) json.RawMessage {
	return mustMarshal(initEnvelope{
		Type:      "system",
		Subtype:   "init",
		SessionID: sessionID,
		Model:     model,
		CWD:       cwd,
		Tools:     tools,
		Version:   version,
	})
}

// replayUserLine synthesizes the user{isReplay:true} echo for one AO Send. uuid
// is the app-minted UserMessageUUID (direct sends) or a Send-minted id (queued
// sends, whose flush path supplies none) — either way a stable handle triage
// stamps as provider_item_id. content is the user's text, used by the parser
// only to tell genuine input from Claude-injected replay content; the
// AO-persisted row summary stays authoritative once the pending send matches.
func replayUserLine(uuid, content string) json.RawMessage {
	return mustMarshal(replayUserEnvelope{
		Type:     "user",
		IsReplay: true,
		UUID:     uuid,
		Message:  replayUserMessage{Role: "user", Content: content},
	})
}

// taskUpdatedLine synthesizes the system/task_updated envelope for a reconstructed
// background-task terminal. status is the raw <status> from the <task-notification>;
// the parser re-normalizes it (claude.NormalizeTaskTerminalStatus) and emits no
// terminal for a non-terminal value, so a caller that already gated on a terminal
// status gets a stash and one that didn't gets a harmless no-op.
func taskUpdatedLine(taskID, toolUseID, status string) json.RawMessage {
	return mustMarshal(taskUpdatedEnvelope{
		Type:      "system",
		Subtype:   "task_updated",
		TaskID:    taskID,
		ToolUseID: toolUseID,
		Patch:     taskUpdatedPatch{Status: status},
	})
}

// taskNotificationLine synthesizes the system/task_notification envelope fed right
// after taskUpdatedLine, carrying the summary and output_file triage reads onto the
// tool_completion sibling it writes when it drains the task_updated stash.
func taskNotificationLine(taskID, toolUseID, status, outputFile, summary string) json.RawMessage {
	return mustMarshal(taskNotificationEnvelope{
		Type:       "system",
		Subtype:    "task_notification",
		TaskID:     taskID,
		ToolUseID:  toolUseID,
		Status:     status,
		OutputFile: outputFile,
		Summary:    summary,
	})
}

// rawString decodes a JSON string value, returning "" for absent, null, or a
// non-string value. Used to read assembled tool_use block fields (type/name/id).
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// mustMarshal marshals a value that is statically known to be marshalable
// (plain structs / maps of json.RawMessage). A failure is a programming error,
// not a runtime condition, so we panic rather than silently emit a broken line
// — consistent with the "errors must never silently fail" rule.
func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic("claudetui: envelope marshal failed (unreachable): " + err.Error())
	}
	return data
}
