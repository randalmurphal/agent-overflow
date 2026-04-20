package claude

import (
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/stringsx"
)

// Parser holds per-session parse state shared across NDJSON lines.
//
// The Claude SDK splits a single tool invocation across two messages: an
// assistant message carrying the `tool_use` block, and a later user message
// echoing the `tool_result` block keyed by the same `tool_use_id`. We need
// to remember per-tool flags (currently `run_in_background`) from the first
// message so the complete-side event carries the same `is_background` hint
// without re-parsing the original input.
//
// Parser is not safe for concurrent use — each Session owns one, and the
// readLoop serializes line parsing. A zero-value Parser is valid; the
// internal map lazily initialises on first write.
type Parser struct {
	// backgroundToolUses flags tool_use IDs that were started with
	// `run_in_background: true` so the matching tool_result event can be
	// tagged the same way.
	backgroundToolUses map[string]bool
	// taskToolUses correlates Claude background task lifecycle messages
	// (task_started/task_updated/TaskOutput) back to the originating
	// tool_use id so we can complete the right timeline row.
	taskToolUses map[string]string
	// completedTasks suppresses duplicate terminal task_updated events.
	completedTasks map[string]struct{}
	// taskOutputTasks suppresses duplicate TaskOutput terminal results and
	// prevents a later task_updated from clobbering richer TaskOutput data.
	taskOutputTasks map[string]struct{}
	// completedToolUseIDs dedups parallel completion signals for the same
	// background tool. Both `system/task_updated` (terminal status) and
	// `system/task_notification` can carry a completion for the same task;
	// whichever fires first emits EventToolComplete and records the
	// tool_use_id here so the other is suppressed. The set is keyed by
	// tool_use_id (not task_id) because task_notification carries the
	// tool_use_id inline and so is self-sufficient even on a fresh adapter
	// session with no task_id→tool_use_id mapping.
	completedToolUseIDs map[string]struct{}
	// streamBlockTypes tracks partial-message content block types by
	// (parent_tool_use_id,index) so a later content_block_stop can identify
	// which streaming block closed.
	streamBlockTypes map[string]string
}

// NewParser returns an initialised Parser. Callers that only need one-shot
// parsing can use the package-level ParseLine helper instead.
func NewParser() *Parser {
	return &Parser{}
}

// Close releases parser-owned maps. Called by Session.Close so the
// `completedToolUseIDs` dedup set (and sibling background-task caches)
// don't linger after the session ends. Calling Close on a zero-value
// parser or twice is safe.
func (p *Parser) Close() {
	if p == nil {
		return
	}
	p.backgroundToolUses = nil
	p.taskToolUses = nil
	p.completedTasks = nil
	p.taskOutputTasks = nil
	p.completedToolUseIDs = nil
	p.streamBlockTypes = nil
}

// markToolUseCompleted records that an EventToolComplete has already been
// emitted for the given tool_use_id. Returns true only on the first call
// for a given id — the caller emits the completion, adds to the set, and
// subsequent calls (from the parallel task_notification signal) are
// suppressed. Empty ids are ignored.
func (p *Parser) markToolUseCompleted(toolUseID string) bool {
	if toolUseID == "" {
		return false
	}
	if p.completedToolUseIDs == nil {
		p.completedToolUseIDs = make(map[string]struct{})
	}
	if _, ok := p.completedToolUseIDs[toolUseID]; ok {
		return false
	}
	p.completedToolUseIDs[toolUseID] = struct{}{}
	return true
}

// ParseLine parses a single NDJSON line from Claude CLI stdout and returns
// zero or more ProviderEvents. This is the stateless entry point — cross-line
// correlation (e.g. background tool_use → tool_result tagging) is not
// available. Use (*Parser).ParseLine for that.
func ParseLine(threadID string, line []byte) ([]provider.ProviderEvent, error) {
	return (&Parser{}).ParseLine(threadID, line)
}

// ParseLine on a Parser preserves state across calls so tool-use / tool-result
// pairs can share metadata (e.g. the `is_background` flag).
func (p *Parser) ParseLine(threadID string, line []byte) ([]provider.ProviderEvent, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	var msgType string
	if err := json.Unmarshal(raw["type"], &msgType); err != nil {
		return nil, fmt.Errorf("missing or invalid type field: %w", err)
	}

	now := time.Now()

	switch msgType {
	case "system":
		return p.parseSystem(threadID, raw, now, line)
	case "assistant":
		return p.parseAssistant(threadID, raw, now, line)
	case "user":
		// Claude echoes tool results via the user role. Pick them up so
		// the triage layer can persist tool-call completions instead of
		// relying on the implicit signal at `result` (turn end).
		return p.parseUser(threadID, raw, now, line)
	case "result":
		return parseResult(threadID, raw, now, line)
	case "stream_event":
		return p.parseStreamEvent(threadID, raw, now)
	case "control_request":
		return parseControlRequest(threadID, raw, now, line)
	case "rate_limit_event":
		return parseRateLimitEvent(threadID, raw, now)
	default:
		// Unknown type — skip gracefully.
		return nil, nil
	}
}

// markBackground records that the given tool_use ID was launched with
// `run_in_background: true`. The matching tool_result event will copy
// the flag onto its Meta.
func (p *Parser) markBackground(toolUseID string) {
	if toolUseID == "" {
		return
	}
	if p.backgroundToolUses == nil {
		p.backgroundToolUses = make(map[string]bool)
	}
	p.backgroundToolUses[toolUseID] = true
}

// isBackground reports whether the given tool_use ID was started with
// `run_in_background: true`. The Claude tool_use ↔ tool_result correlation
// is one-shot; callers that consume a correlated event should also call
// clearBackground to release the entry.
func (p *Parser) isBackground(toolUseID string) bool {
	if toolUseID == "" || p.backgroundToolUses == nil {
		return false
	}
	return p.backgroundToolUses[toolUseID]
}

func (p *Parser) clearBackground(toolUseID string) {
	if toolUseID == "" || p.backgroundToolUses == nil {
		return
	}
	delete(p.backgroundToolUses, toolUseID)
}

func (p *Parser) rememberTaskToolUse(taskID, toolUseID string) {
	if taskID == "" || toolUseID == "" {
		return
	}
	if p.taskToolUses == nil {
		p.taskToolUses = make(map[string]string)
	}
	p.taskToolUses[taskID] = toolUseID
}

func (p *Parser) taskToolUse(taskID string) string {
	if taskID == "" || p.taskToolUses == nil {
		return ""
	}
	return p.taskToolUses[taskID]
}

func (p *Parser) clearTask(taskID string) {
	if taskID == "" {
		return
	}
	if p.taskToolUses != nil {
		delete(p.taskToolUses, taskID)
	}
	if p.completedTasks != nil {
		delete(p.completedTasks, taskID)
	}
	if p.taskOutputTasks != nil {
		delete(p.taskOutputTasks, taskID)
	}
}

func (p *Parser) markTaskCompleted(taskID string) bool {
	if taskID == "" {
		return false
	}
	if p.completedTasks == nil {
		p.completedTasks = make(map[string]struct{})
	}
	if _, ok := p.completedTasks[taskID]; ok {
		return false
	}
	p.completedTasks[taskID] = struct{}{}
	return true
}

func (p *Parser) hasTaskOutput(taskID string) bool {
	if taskID == "" || p.taskOutputTasks == nil {
		return false
	}
	_, ok := p.taskOutputTasks[taskID]
	return ok
}

func (p *Parser) markTaskOutput(taskID string) bool {
	if taskID == "" {
		return false
	}
	if p.taskOutputTasks == nil {
		p.taskOutputTasks = make(map[string]struct{})
	}
	if _, ok := p.taskOutputTasks[taskID]; ok {
		return false
	}
	p.taskOutputTasks[taskID] = struct{}{}
	return true
}

func (p *Parser) rememberStreamBlock(parentToolUseID string, index int, blockType string) {
	if blockType == "" {
		return
	}
	if p.streamBlockTypes == nil {
		p.streamBlockTypes = make(map[string]string)
	}
	p.streamBlockTypes[streamBlockKey(parentToolUseID, index)] = blockType
}

func (p *Parser) takeStreamBlock(parentToolUseID string, index int) string {
	if p.streamBlockTypes == nil {
		return ""
	}
	key := streamBlockKey(parentToolUseID, index)
	blockType := p.streamBlockTypes[key]
	delete(p.streamBlockTypes, key)
	return blockType
}

func streamBlockKey(parentToolUseID string, index int) string {
	return fmt.Sprintf("%s:%d", parentToolUseID, index)
}

// firstNonEmpty is a tiny alias for stringsx.FirstNonEmpty. Kept package-
// local so the dense parser expressions (firstNonEmpty(a, b, c)) stay
// readable; the behavior matches exactly — return the first v != "".
func firstNonEmpty(values ...string) string {
	return stringsx.FirstNonEmpty(values...)
}
