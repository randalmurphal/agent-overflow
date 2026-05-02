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
	// todoWriteToolUses flags tool_use IDs for the `TodoWrite` tool. The
	// tool_use itself emits an EventTodoUpdate (not a generic tool start)
	// so the matching tool_result must be dropped — there is no tool-call
	// row to complete. See parse_assistant.go appendTodoWriteEvent and
	// parse_user.go appendToolResultCompletion.
	todoWriteToolUses map[string]bool
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
	p.todoWriteToolUses = nil
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
		// Two distinct shapes share the `user` envelope:
		//   1. CLI replay echoes (only present with --replay-user-messages):
		//      `isReplay:true`, `message.content` is a plain string. We
		//      promote these to EventUserText so triage's pending-send
		//      correlation can match the AO-initiated prompt to its wire
		//      echo. The replay branch is checked BEFORE parseUser so a
		//      pathological future shape (replay flag AND tool_result
		//      content) cannot double-emit; replay wins.
		//   2. tool_result echoes (the long-standing path): the model has
		//      finished a tool call; parseUser emits one EventToolComplete
		//      per result block.
		// Non-replay string-content user messages still drop silently
		// inside parseUser today — the replay flag is what gives us a
		// confirmation point.
		if isReplayEnvelope(raw) {
			return p.parseUserReplay(threadID, raw, now, line)
		}
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

// parseUserReplay promotes a replay-echo user envelope to a single
// EventUserText. The replay shape (per @anthropic-ai/claude-agent-sdk
// `SDKUserMessageReplaySchema`) carries `message.content` as a plain
// string, but we accept the array shape `[{type:"text",text:"..."}]`
// defensively too — extractToolResultText already covers both.
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

// markTodoWrite records that the given tool_use ID was a `TodoWrite`
// invocation. Bounded by parserTaskMapCap on the same wholesale-reset
// principle as taskToolUses / subagentModelStamped: a TodoWrite tool_use
// without a matching tool_result is pathological (interrupt mid-call,
// hostile provider) but should not let the per-session map grow without
// bound. Wholesale reset is benign because the cap is well above any
// realistic TodoWrite fan-out and the consequence of dropping a stale
// entry is just one orphan tool_result rendered as a generic completion
// row instead of being dropped silently — strictly less surprising than
// an unbounded leak.
func (p *Parser) markTodoWrite(toolUseID string) {
	if toolUseID == "" {
		return
	}
	if p.todoWriteToolUses == nil {
		p.todoWriteToolUses = make(map[string]bool)
	}
	if len(p.todoWriteToolUses) >= parserTaskMapCap {
		p.todoWriteToolUses = make(map[string]bool)
	}
	p.todoWriteToolUses[toolUseID] = true
}

func (p *Parser) isTodoWrite(toolUseID string) bool {
	if toolUseID == "" || p.todoWriteToolUses == nil {
		return false
	}
	return p.todoWriteToolUses[toolUseID]
}

func (p *Parser) clearTodoWrite(toolUseID string) {
	if toolUseID == "" || p.todoWriteToolUses == nil {
		return
	}
	delete(p.todoWriteToolUses, toolUseID)
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
