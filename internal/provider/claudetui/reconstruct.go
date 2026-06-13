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
}

type assistantEnvelope struct {
	Type    string           `json:"type"` // "assistant"
	Message assistantMessage `json:"message"`
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
// NDJSON line.
func (a *messageAssembler) assistantLine() json.RawMessage {
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
	}
	return mustMarshal(env)
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
// delta passthrough.
func streamEventLine(sse json.RawMessage) json.RawMessage {
	return mustMarshal(streamEventEnvelope{Type: "stream_event", Event: sse})
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
