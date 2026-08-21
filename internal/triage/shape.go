// Package triage — the exported row-shape surface.
//
// Everything in this file is a PURE projection: provider wire meta (or
// an already-persisted row) in, `store.Item` / `store.Payload` field
// values out. No Router, no store access, no clock, no side effects.
//
// It exists as its own file because triage is no longer the only writer
// of AO's timeline rows. `internal/sessionimport` replays historical
// provider sessions straight into SQLite — deliberately NOT through
// `Router` (see docs/specs and the import plan: the Router has live-only
// side effects and would persist imported prompts as notifications), so
// the only way an imported thread can look identical to a live one is
// for both to call the same shaping code. A parity test in
// `internal/sessionimport` pins that: the same synthetic events routed
// through `Router` and through the import writer must produce identical
// rows modulo ids and timestamps.
//
// Consequences for editing:
//
//   - Changing an id FORMAT here changes ids of future rows on both
//     paths at once. That is the point; keep it that way.
//   - Do not reach for Router state to answer a shaping question. If a
//     helper needs correlation state, it belongs in the lifecycle file
//     that owns that state, and only its pure core belongs here.
//   - Errors are returned, never logged. `ShapeToolItemMeta` is the one
//     helper that can fail; triage's `shapeToolItemMeta` wrapper
//     (tool_meta_rules.go) owns the "log and keep going" policy, and
//     import owns its own.
//
// Shape helpers already exported elsewhere and reused as-is by import:
// `ExtractDiffMeta`, `ExtractCommandOutputMeta(WithError)`,
// `ExtractThinkingMeta`, `ExtractCompactionMeta`,
// `ExtractProposedPlanMeta`, `ToolResultMeta`, `ToolInlineDiff(File)`,
// `ShapeToolItemMeta` (tool_meta_rules.go), and the Claude file-change
// extractors in tool_result_claude_file_change.go.

package triage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
)

// ToolStartMeta is the decoded slice of an EventToolStart's Meta that
// row shaping reads. Producers: the Claude parser's `marshalToolMeta`
// and the Codex adapter's per-tool meta extras.
type ToolStartMeta struct {
	ToolName        string          `json:"toolName"`
	Input           json.RawMessage `json:"input"`
	MetaUpdateOnly  bool            `json:"meta_update_only"`
	IsBackground    bool            `json:"is_background"`
	TaskID          string          `json:"task_id"`
	SubagentModel   string          `json:"subagent_model"`
	ParentToolUseID string          `json:"parent_tool_use_id"`
	// ResumesToolUseID and Description carry Claude's resume-rebind
	// linkage: system/task_started rebinding an idle async agent's
	// task_id onto a NEW tool_use (e.g. the harness's SendMessage call)
	// — see claude-wire.md §E6. ResumesToolUseID is the tool_use_id of
	// the ORIGINAL launch this tool_use is resuming; Description is the
	// original agent's description straight off the rebind
	// task_started envelope. Both are only populated by the parser's
	// resume path (parse_system.go); a normal launch's meta-only
	// task_started update never sets them.
	ResumesToolUseID string `json:"resumes_tool_use_id"`
	Description      string `json:"description"`
}

// ToolCompleteMeta is the decoded slice of an EventToolComplete's Meta
// that row shaping reads.
type ToolCompleteMeta struct {
	IsBackground bool `json:"is_background"`
	// WatchTask marks a background launch that OBSERVES rather than
	// works (Claude's Monitor, claude-wire.md §E7): it never produces a
	// result a queued user send could be waiting on, so the flush-queue
	// drain ignores it while the reaper/revert/context-repair consumers
	// still count it as live background work. Copied onto the launch
	// row's meta by the keep-running flip below so the store predicate
	// can filter on it.
	WatchTask  bool            `json:"watch_task"`
	IsError    bool            `json:"is_error"`
	ExitCode   *int            `json:"exit_code,omitempty"`
	ItemStatus string          `json:"item_status,omitempty"`
	TaskID     string          `json:"task_id,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
}

// DecodeToolStartMeta decodes raw into a ToolStartMeta. Undecodable meta
// yields the zero value: a launch with unreadable meta still gets a row,
// just an unadorned one.
func DecodeToolStartMeta(raw json.RawMessage) ToolStartMeta {
	if len(raw) == 0 {
		return ToolStartMeta{}
	}
	var m ToolStartMeta
	if json.Unmarshal(raw, &m) != nil {
		return ToolStartMeta{}
	}
	return m
}

// DecodeToolStartMetaObject is DecodeToolStartMeta for an already-decoded
// top-level event meta object.
func DecodeToolStartMetaObject(obj map[string]json.RawMessage) ToolStartMeta {
	if obj == nil {
		return ToolStartMeta{}
	}
	var m ToolStartMeta
	decodeRawField(obj, "toolName", &m.ToolName)
	if input, ok := obj["input"]; ok {
		m.Input = input
	}
	decodeRawField(obj, "meta_update_only", &m.MetaUpdateOnly)
	decodeRawField(obj, "is_background", &m.IsBackground)
	decodeRawField(obj, "task_id", &m.TaskID)
	decodeRawField(obj, "subagent_model", &m.SubagentModel)
	decodeRawField(obj, "parent_tool_use_id", &m.ParentToolUseID)
	decodeRawField(obj, "resumes_tool_use_id", &m.ResumesToolUseID)
	decodeRawField(obj, "description", &m.Description)
	return m
}

// DecodeToolCompleteMeta decodes raw into a ToolCompleteMeta, with the
// same zero-value-on-garbage rule as DecodeToolStartMeta.
func DecodeToolCompleteMeta(raw json.RawMessage) ToolCompleteMeta {
	if len(raw) == 0 {
		return ToolCompleteMeta{}
	}
	var m ToolCompleteMeta
	if json.Unmarshal(raw, &m) != nil {
		return ToolCompleteMeta{}
	}
	return m
}

// DecodeToolCompleteMetaObject is DecodeToolCompleteMeta for callers that
// already decoded the completion's top-level JSON object. Decoding the small
// typed fields individually avoids re-scanning large tool_result echoes.
func DecodeToolCompleteMetaObject(obj map[string]json.RawMessage) ToolCompleteMeta {
	if obj == nil {
		return ToolCompleteMeta{}
	}
	var m ToolCompleteMeta
	decodeRawField(obj, "is_background", &m.IsBackground)
	decodeRawField(obj, "watch_task", &m.WatchTask)
	decodeRawField(obj, "is_error", &m.IsError)
	decodeRawField(obj, "exit_code", &m.ExitCode)
	decodeRawField(obj, "item_status", &m.ItemStatus)
	decodeRawField(obj, "task_id", &m.TaskID)
	decodeRawField(obj, "toolName", &m.ToolName)
	if input, ok := obj["input"]; ok {
		m.Input = input
	}
	return m
}

func decodeRawField(obj map[string]json.RawMessage, key string, dst any) {
	if raw, ok := obj[key]; ok {
		_ = json.Unmarshal(raw, dst)
	}
}

// StoredToolCallMeta returns the `items.meta` string for a tool_call
// row: the event meta re-encoded as a canonical JSON object, minus the
// Codex `item` echo (a file_change row's changes already live in the
// diff payload). Non-object or undecodable meta stores as "".
func StoredToolCallMeta(itemType string, toolName string, raw json.RawMessage) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return ""
	}
	return StoredToolCallMetaObject(itemType, toolName, obj)
}

// BuildToolCallSummary is the launch-time summary of a tool_call row:
// `<toolName>: <input preview>`, falling back to the item type and then
// the literal "tool" when the wire named neither.
func BuildToolCallSummary(meta ToolStartMeta, itemType string) string {
	name := strings.TrimSpace(meta.ToolName)
	if name == "" {
		name = strings.TrimSpace(itemType)
	}
	if name == "" {
		name = "tool"
	}
	preview := toolInputPreview(meta.Input)
	if preview == "" {
		return name
	}
	return name + ": " + preview
}

// BuildCompletionSummary appends the terminal-status suffix (if any) to
// the summary the launch row already carries.
func BuildCompletionSummary(launchSummary string, meta ToolCompleteMeta) string {
	suffix := CompletionSuffix(meta)
	if suffix == "" {
		return launchSummary
	}
	return launchSummary + " " + suffix
}

// CompletionBaseSummary picks the summary a completion should build on.
// The launch row's own summary wins whenever it already carries a
// preview; a bare tool name (or an empty summary) is rebuilt from the
// completion's input, which is the only place a completion-without-
// launch or a late-input tool can get one.
func CompletionBaseSummary(launch store.Item, meta ToolCompleteMeta, itemType string) string {
	preview := toolInputPreview(meta.Input)
	if preview == "" {
		return launch.Summary
	}
	toolName := stringsx.FirstNonEmptyTrimmed(meta.ToolName, launch.ToolName, itemType, "tool")
	current := strings.TrimSpace(launch.Summary)
	if current == "" || current == strings.TrimSpace(launch.ToolName) || current == strings.TrimSpace(itemType) || !strings.Contains(current, ":") {
		return toolName + ": " + preview
	}
	return launch.Summary
}

// CompletionSuffix is the parenthesised terminal-state marker a
// completed tool_call summary ends with, or "" for a clean success.
func CompletionSuffix(meta ToolCompleteMeta) string {
	switch {
	case meta.IsError:
		return "(error)"
	case meta.ExitCode != nil && *meta.ExitCode != 0:
		return fmt.Sprintf("(exit %d)", *meta.ExitCode)
	case meta.ItemStatus == "failed":
		return "(failed)"
	case meta.ItemStatus == "errored":
		return "(errored)"
	case meta.ItemStatus == "killed":
		return "(killed)"
	case meta.ItemStatus == statusDeclined:
		return "(declined)"
	default:
		return ""
	}
}

// CompletionStatus maps a completion's meta onto the `items.status`
// value. `declined` stays distinct from `errored` because the UI renders
// a user refusal differently from a failure.
func CompletionStatus(meta ToolCompleteMeta) string {
	if meta.IsError || meta.ItemStatus == "failed" || meta.ItemStatus == "errored" || meta.ItemStatus == "killed" {
		return statusErrored
	}
	if meta.ItemStatus == statusDeclined {
		return statusDeclined
	}
	return statusCompleted
}

// CompletionPayloadForTool builds the on-demand result payload for a
// completed tool call: command-output shaped for shell-style tools,
// generic tool_call_result otherwise. Returns nil when the completion
// carried no content worth storing.
func CompletionPayloadForTool(itemID string, toolName string, command string, evt provider.ProviderEvent, meta ToolCompleteMeta, now int64) *store.Payload {
	var obj map[string]json.RawMessage
	if len(evt.Meta) > 0 {
		_ = json.Unmarshal(evt.Meta, &obj)
	}
	return CompletionPayloadForToolObject(itemID, toolName, command, evt.Content, obj, meta, now)
}

// CompletionPayloadForToolObject is CompletionPayloadForTool for a caller
// that already decoded the completion envelope.
func CompletionPayloadForToolObject(itemID string, toolName string, command string, content string, obj map[string]json.RawMessage, meta ToolCompleteMeta, now int64) *store.Payload {
	if isCommandOutputToolName(toolName) {
		return CommandCompletionPayloadObject(itemID, command, content, obj, meta, now)
	}
	return completionPayload(itemID, provider.ProviderEvent{Content: content}, meta, now)
}

// CommandCompletionPayload builds the `command_output` payload for a
// shell-style tool completion, with the command line, exit code and
// error flags folded into the payload meta header (meta is cheap, data
// is heavy). Returns nil for empty output.
func CommandCompletionPayload(itemID string, command string, evt provider.ProviderEvent, meta ToolCompleteMeta, now int64) *store.Payload {
	var obj map[string]json.RawMessage
	if len(evt.Meta) > 0 {
		_ = json.Unmarshal(evt.Meta, &obj)
	}
	return CommandCompletionPayloadObject(itemID, command, evt.Content, obj, meta, now)
}

// CommandCompletionPayloadObject is CommandCompletionPayload for an
// already-decoded completion envelope.
func CommandCompletionPayloadObject(itemID string, command string, content string, obj map[string]json.RawMessage, meta ToolCompleteMeta, now int64) *store.Payload {
	if content == "" {
		return nil
	}
	fields := cloneRawMessageMap(obj)
	if fields == nil {
		fields = make(map[string]json.RawMessage)
	}
	if strings.TrimSpace(command) != "" {
		setRawStringDefault(fields, "command", strings.TrimSpace(command))
	}
	if meta.ExitCode != nil {
		setRawIntDefault(fields, "exitCode", *meta.ExitCode)
		setRawIntDefault(fields, "exit_code", *meta.ExitCode)
	}
	if meta.IsError {
		if _, ok := fields["is_error"]; !ok {
			fields["is_error"] = json.RawMessage("true")
		}
	}
	if meta.ItemStatus != "" {
		setRawStringDefault(fields, "itemStatus", meta.ItemStatus)
	}
	return &store.Payload{
		ID:        "command-output:" + itemID,
		Kind:      "command_output",
		Meta:      buildCommandOutputPayloadMeta(content, fields),
		Data:      []byte(content),
		CreatedAt: now,
	}
}

func setRawStringDefault(fields map[string]json.RawMessage, key, value string) {
	if _, exists := fields[key]; !exists {
		fields[key] = json.RawMessage(strconv.Quote(value))
	}
}

func setRawIntDefault(fields map[string]json.RawMessage, key string, value int) {
	if _, exists := fields[key]; !exists {
		fields[key] = json.RawMessage(strconv.Itoa(value))
	}
}

// Deterministic row ids.
//
// Every id below is a pure function of turn/scope/sequence coordinates,
// which is what makes an upsert idempotent across a reconnect and what
// lets the importer mint ids that match what a live session would have
// produced. Sequence allocation itself is Router state (stream_state.go);
// only the formatting lives here.

// TextItemID is the id of an assistant_text row: the Nth text segment of
// a turn, optionally scoped to a subagent's tool_use id.
func TextItemID(turnIndex int, scope string, segmentIndex int) string {
	if scope == "" {
		return fmt.Sprintf("text:%d:%d", turnIndex, segmentIndex)
	}
	return fmt.Sprintf("text:%d:%s:%d", turnIndex, scope, segmentIndex)
}

// ThinkingItemID is the id of a thinking row, scoped like TextItemID.
func ThinkingItemID(turnIndex int, scope string, blockIndex int) string {
	if scope == "" {
		return fmt.Sprintf("think:%d:%d", turnIndex, blockIndex)
	}
	return fmt.Sprintf("think:%d:%s:%d", turnIndex, scope, blockIndex)
}

// ErrorItemID is the id of an error row: the Nth error of a turn within
// a scope. The sequence comes from the Router's per-scope counter.
func ErrorItemID(turnIndex int, scope string, seq int) string {
	if scope == "" {
		return fmt.Sprintf("error:%d:%d", turnIndex, seq)
	}
	return fmt.Sprintf("error:%d:%s:%d", turnIndex, scope, seq)
}

// maxProviderCompactionIDLength bounds the provider-supplied fragment
// of a compaction row id. Longer values are hashed rather than
// truncated so two distinct long ids can't collide on the same row.
const maxProviderCompactionIDLength = 420

// NormalizeProviderCompactionID reduces a provider-supplied compaction
// item id to the fragment a row id may embed: "" when the wire carried
// nothing usable (empty, or control characters that would make the id
// unreadable), the trimmed value when it fits, and a sha256 digest when
// it does not.
func NormalizeProviderCompactionID(providerID string) string {
	trimmed := strings.TrimSpace(providerID)
	if trimmed == "" || strings.ContainsFunc(trimmed, unicode.IsControl) {
		return ""
	}
	if len(trimmed) <= maxProviderCompactionIDLength {
		return trimmed
	}
	hash := sha256.Sum256([]byte(trimmed))
	return fmt.Sprintf("sha256:%x", hash)
}

// CompactionItemID is the id of a `compaction` divider row. A usable
// providerItemID keys the row on the provider's own identity (so a
// re-delivered boundary upserts in place); otherwise the row falls back
// to the caller's per-turn sequence. seq is ignored in the first case.
func CompactionItemID(turnIndex int, providerItemID string, seq int) string {
	if providerID := NormalizeProviderCompactionID(providerItemID); providerID != "" {
		return fmt.Sprintf("compact:%d:provider:%s", turnIndex, providerID)
	}
	return fmt.Sprintf("compact:%d:seq:%d", turnIndex, seq)
}

// ToolCompletionID is the id of the `tool_completion` sibling row a
// backgrounded (or split) tool call settles into, derived from the
// launch row's id so the sibling upserts in place.
func ToolCompletionID(launchID string) string {
	return "complete:" + launchID
}

// AssistantTextPayloadID is the deterministic payload id for an
// assistant_text row's full text blob. Payload identity is thread-scoped by
// the store schema, so including the thread again here would make identical
// imported branch prefixes hash differently and defeat physical sharing.
func AssistantTextPayloadID(itemID string) string {
	return "assistant-text:" + itemID
}

// ThinkingPayloadID is the deterministic payload id for a streaming
// thinking-style row, keyed off the item id so later deltas address the same
// blob without a Store round-trip. Shared by handleThinking, the
// persistCompletedThinkingItem fallback, and handleCompactionReasoning — the
// last reuses the thinking streaming machinery under its reserved scope, so its
// payload follows the same convention.
func ThinkingPayloadID(itemID string) string {
	return "thinking:" + itemID
}

// ShouldSplitCodexToolCompletion reports whether a Codex tool's completion
// gets its own `tool_completion` sibling row instead of settling the launch
// row in place.
//
// `wait_agent` / `resume_agent` are the two: the launch records that the
// parent WAITED, and the sibling records what it was told — two facts at two
// times, which one upserted row cannot hold. Every other tool settles in
// place.
//
// The caller supplies the provider test; this answers only the tool-name half,
// so a Claude thread that happens to run a tool by one of those names is
// unaffected.
func ShouldSplitCodexToolCompletion(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "wait_agent", "resume_agent":
		return true
	default:
		return false
	}
}
