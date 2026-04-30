package claude

import (
	"encoding/json"
	"fmt"
	"strings"
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
// Correlation state is intentionally minimal. Two maps are enough:
// `backgroundToolUses` tags tool_use ids started with
// `run_in_background:true` so the immediate placeholder `tool_result`
// can propagate `is_background:true` on its completion meta.
// `taskToolUses` correlates `system/task_*` envelopes back to the
// originating `tool_use_id`. Any dedup for
// `EventBackgroundTaskTerminal` — both `task_updated` and TaskOutput
// can arrive for the same task_id — is NOT the parser's job; triage's
// `AppendCompletionItem` is idempotent. See
// docs/architecture/turn-lifecycle.md §Task lifecycle.
//
// Parser is not safe for concurrent use — each Session owns one, and the
// readLoop serializes line parsing. A zero-value Parser is valid; the
// internal maps lazily initialise on first write.
type Parser struct {
	// backgroundToolUses flags tool_use IDs that were started with
	// `run_in_background: true` so the matching tool_result event can be
	// tagged the same way.
	backgroundToolUses map[string]bool
	// taskToolUses correlates Claude background task lifecycle messages
	// (task_started/task_updated/TaskOutput) back to the originating
	// tool_use id so we can target the right timeline row.
	taskToolUses map[string]string
	// subagentModelStamped dedupes per-parent_tool_use_id meta-update
	// emissions of `subagent_model`. Subagent assistant messages all
	// carry the same `message.model`, so we only emit the meta merge
	// once per parent rather than on every assistant envelope inside
	// the subagent's lifetime.
	subagentModelStamped map[string]bool
	// streamBlockTypes tracks partial-message content block types by
	// (parent_tool_use_id,index) so a later content_block_stop can identify
	// which streaming block closed.
	streamBlockTypes map[string]string
	// model is the latest model id observed on this session. Seeded from
	// the system/init line and used to price result usage so triage
	// doesn't have to reach back into the store for pricing. When
	// the CLI rewrites the model mid-session (e.g. Sonnet → Opus auto-
	// upgrade), the next init echo updates this field.
	model string
	// lastAssistantMessageID is the id of the most-recent `assistant`
	// envelope's `message.id`. The `result` envelope does not carry this
	// id, so we track it in-stream and stamp it onto
	// `EventTurnComplete.Meta.assistant_message_id` so triage can write
	// the `turns.assistant_message_id` column. Reset to "" inside
	// `parseResult` after emission so it doesn't leak into the next turn.
	// See docs/references/claude-wire.md §result and
	// docs/architecture/turn-lifecycle.md §Turn lifecycle.
	lastAssistantMessageID string
}

// SetModel primes the parser with the model id the session was started
// with. Init messages still overwrite the field when they arrive, but
// seeding from Session.Start lets the first result usage snapshot carry
// priced usage metadata even if the init line is late or absent.
func (p *Parser) SetModel(model string) {
	if p == nil {
		return
	}
	p.model = strings.TrimSpace(model)
}

// currentModel returns the parser's tracked model id, or "" when the
// parser is nil. Used by the pricing paths so ParseLine can be called
// on a nil receiver without panicking — the package-level ParseLine
// helper and a handful of tests that construct a *Session without a
// parser rely on this.
func (p *Parser) currentModel() string {
	if p == nil {
		return ""
	}
	return p.model
}

// NewParser returns an initialised Parser. Callers that only need one-shot
// parsing can use the package-level ParseLine helper instead.
func NewParser() *Parser {
	return &Parser{}
}

// Close releases parser-owned maps. Called by Session.Close so the
// correlation maps don't linger after the session ends. Calling Close
// on a zero-value parser or twice is safe.
func (p *Parser) Close() {
	if p == nil {
		return
	}
	p.backgroundToolUses = nil
	p.taskToolUses = nil
	p.subagentModelStamped = nil
	p.streamBlockTypes = nil
	p.lastAssistantMessageID = ""
}

// markSubagentModelStamped records that we've already emitted a
// `subagent_model` meta-update for the given parent tool_use_id, so the
// parser can drop duplicates on every subsequent subagent assistant
// envelope. The map is bounded by parserTaskMapCap; on overflow we
// reset wholesale (benign — only costs at most one duplicate emit per
// stale parent).
func (p *Parser) markSubagentModelStamped(parentToolUseID string) {
	if p == nil || parentToolUseID == "" {
		return
	}
	if p.subagentModelStamped == nil {
		p.subagentModelStamped = make(map[string]bool)
	}
	if len(p.subagentModelStamped) >= parserTaskMapCap {
		p.subagentModelStamped = make(map[string]bool)
	}
	p.subagentModelStamped[parentToolUseID] = true
}

func (p *Parser) hasStampedSubagentModel(parentToolUseID string) bool {
	if p == nil || parentToolUseID == "" || p.subagentModelStamped == nil {
		return false
	}
	return p.subagentModelStamped[parentToolUseID]
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
		return p.parseResult(threadID, raw, now, line)
	case "stream_event":
		return p.parseStreamEvent(threadID, raw, now)
	case "control_request":
		return parseControlRequest(threadID, raw, now, line)
	case "control_response":
		// The CLI emits control_response envelopes only as replies to
		// outbound client control_requests (today: stop_task). The
		// Session-level readLoop intercepts these before ParseLine via
		// the controlResponsePrefix gate and routes them to the
		// awaiting StopTask caller. If a control_response somehow
		// reaches ParseLine (stateless test harness, tool that calls
		// ParseLine directly), it is a no-op — no triage or event
		// consumer has a view on this envelope.
		return nil, nil
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

// parserTaskMapCap bounds the taskToolUses map so an abandoned task —
// one that never clears from the correlation table — cannot grow the
// parser's per-session state without bound. When the cap is hit the
// map is replaced wholesale, which may lose a late-arriving lookup
// for an ancient task; that is benign because the corresponding store
// row is already terminal, and the cap is well above any realistic
// in-flight task fan-out.
const parserTaskMapCap = 1024

// parserStreamBlockCap bounds streamBlockTypes for the same reason —
// Claude normally pairs every content_block_start with a block_stop
// that calls takeStreamBlock, but an interrupt (or a malformed
// abandoned block) can leak the entry. The cap covers only the
// pathological case.
const parserStreamBlockCap = 1024

func (p *Parser) rememberTaskToolUse(taskID, toolUseID string) {
	if taskID == "" || toolUseID == "" {
		return
	}
	if p.taskToolUses == nil {
		p.taskToolUses = make(map[string]string)
	}
	if len(p.taskToolUses) >= parserTaskMapCap {
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

// setLastAssistantMessageID remembers the id of the most recent
// `assistant` envelope's `message.id`. Called from parse_assistant.go
// on every assistant envelope so `parseResult` can include it on
// `EventTurnComplete.Meta.assistant_message_id`. Empty input is
// ignored so mid-stream content-only messages (no id) don't clobber
// the stored id.
func (p *Parser) setLastAssistantMessageID(id string) {
	if p == nil || id == "" {
		return
	}
	p.lastAssistantMessageID = id
}

// takeLastAssistantMessageID returns the last id tracked and clears
// it. Called by `parseResult` at turn boundary so the id does not
// leak from one turn to the next within the same session.
func (p *Parser) takeLastAssistantMessageID() string {
	if p == nil {
		return ""
	}
	id := p.lastAssistantMessageID
	p.lastAssistantMessageID = ""
	return id
}

func (p *Parser) rememberStreamBlock(parentToolUseID string, index int, blockType string) {
	if blockType == "" {
		return
	}
	if p.streamBlockTypes == nil {
		p.streamBlockTypes = make(map[string]string)
	}
	if len(p.streamBlockTypes) >= parserStreamBlockCap {
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
