package sessionimport

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
)

// MetaImportUnavailableKey is the ProviderEvent.Meta key this package
// stamps when an item's heavy payload existed once but cannot be
// recovered from disk. The writer lifts it onto items.meta through
// itemmeta.MarkImportUnavailable, and the frontend renders the reason as
// "Not available from import" instead of an empty expander.
//
// It is itemmeta's key, not a matching spelling of it: the event meta and
// the persisted meta are the same key by definition — the writer strips it
// from one and re-stamps it on the other — so two literals could only ever
// be a way for them to disagree. itemmeta is stdlib-only, which is what
// lets a provider package name it without acquiring a path to store.
const MetaImportUnavailableKey = itemmeta.ImportUnavailableKey

// MetaImportUnavailableToolOutputGC is the only reason this package
// produces: Claude externalised an oversized tool result to
// `<sessionDir>/tool-results/<toolUseId>.txt` (or cleared it outright)
// and its housekeeping later deleted the file. The transcript keeps a
// 2 KB preview at best; the full output is gone.
const MetaImportUnavailableToolOutputGC = "tool-output-gc"

// Warning codes emitted by Convert.
const (
	WarnUnknownSystemSubtype = "unknown-system-subtype"
	WarnUncorrelatedResult   = "uncorrelated-tool-result"
	WarnToolOutputGC         = "tool-output-gc"
)

// ConvertOptions carries what a chain cannot answer about itself.
type ConvertOptions struct {
	// SessionDir is `<projectDir>/<sessionID>` — the per-session sidecar
	// directory holding `tool-results/` and `subagents/`. Empty disables
	// the externalised-output existence check (everything oversized then
	// reads as still-present, which is the non-destructive default).
	SessionDir string
	// Subagents maps a Task/Agent tool_use id to the rows of the subagent
	// transcript it spawned. See LoadSubagents.
	Subagents map[string][]Row
}

// ConvertResult is one decoded branch projected into import events together
// with branch-level metadata that is not itself a timeline event.
type ConvertResult struct {
	Events   []importir.Event
	Warnings []importir.Warning
	Profile  importir.ModelProfile
}

// Convert projects one branch chain into the provider-event vocabulary a
// live session speaks, so the neutral writer builds the same rows for an
// imported thread that triage builds for a live one.
//
// Deliberate departures from the live wire, all because a transcript is
// history rather than a stream:
//
//   - Text and thinking blocks arrive complete. One event carries the
//     whole block with ContentPresent set; import never emits a partial
//     delta.
//   - Turn boundaries are synthesised. A user prompt row closes the open
//     turn and opens the next; the transcript has no `result` envelope,
//     so per-turn usage is accumulated from each assistant message's
//     `usage` and rides the synthesised EventTurnComplete exactly where
//     the wire's per-turn delta would.
//   - `attachment` rows produce nothing, matching the live parser: the
//     stream-json wire has no attachment envelope at all, and their
//     bodies (file contents, tool-discovery deltas) are unbounded — the
//     wrong thing to fold into a cheap, always-shipped items.meta.
//   - `progress`, mode/permission-mode/queue-operation/agent-name/
//     file-history/tag records never reach here: they are not transcript
//     types, or the DAG dropped them.
func Convert(chain []Row, opts ConvertOptions) ConvertResult {
	c := &converter{
		opts:                 opts,
		usageByModel:         map[string]*provider.TokenUsage{},
		unknownSystem:        map[string]int{},
		emittedAgents:        map[string]bool{},
		openingPromptByScope: map[string]bool{},
	}
	c.indexCompactSummaries(chain)
	c.seedClock(chain)
	for _, row := range chain {
		c.convertRow(row)
	}
	c.closeTurn()
	c.appendDeferredWarnings()
	return ConvertResult{
		Events:   c.events,
		Warnings: c.warnings,
		Profile:  c.profile,
	}
}

type converter struct {
	opts     ConvertOptions
	events   []importir.Event
	warnings []importir.Warning

	turnIndex int
	turnOpen  bool
	lastRow   Row
	// lastTimestamp is the newest row time seen so far. Not every row
	// carries a `timestamp` (older writers omit it on metadata-ish rows),
	// and an imported item must never fall back to now() — the last known
	// source time is the honest answer.
	lastTimestamp int64

	stopReason         string
	assistantMessageID string
	usageByModel       map[string]*provider.TokenUsage
	usageOrder         []string
	// profile is the latest model named by a top-level assistant message.
	// Subagent models are usage contributors, not the model selected for the
	// parent conversation.
	profile importir.ModelProfile

	// subagentScope is the Task/Agent tool_use id whose transcript is
	// being converted right now. Non-empty suppresses turn management and
	// user-prompt rows (a subagent's prompt is the Task tool's input, not
	// a message from the user) and stamps ParentToolUseID on every event.
	subagentScope string
	emittedAgents map[string]bool
	// openingPromptByScope gives the first user-role row in each subagent a
	// launch-scoped identity. Live Claude can render that prompt from the
	// launch input before async stdout would ever echo it; the transcript uuid
	// then enriches the same row instead of appending it at terminal.
	openingPromptByScope map[string]bool

	// blockOrdinal counts emitted content blocks per assistant message id.
	// Claude writes ONE transcript row per content block while streaming,
	// so every row reports block index 0 — the per-message counter is what
	// keeps two text blocks of one message on two rows instead of one
	// overwriting the other. Same rule the live parser's
	// nextRecoveredBlockIndex applies.
	blockOrdinal map[string]int

	compactSummaries map[string]string // boundary uuid -> committed summary
	consumedSummary  map[string]bool   // summary row uuids folded above

	unknownSystem  map[string]int
	uncorrelated   int
	unavailableGCs int
}

// indexCompactSummaries pairs each `compact_boundary` row with the
// `isCompactSummary` user row that follows it. The CLI writes the summary
// as the boundary's child, so one compaction is one divider row plus one
// summary body — not two timeline rows.
func (c *converter) indexCompactSummaries(chain []Row) {
	c.compactSummaries = map[string]string{}
	c.consumedSummary = map[string]bool{}

	summaryByParent := make(map[string]Row, 1)
	for _, row := range chain {
		if row.IsCompactSummary && row.ParentUUID != "" {
			summaryByParent[row.ParentUUID] = row
		}
	}
	for _, row := range chain {
		if row.Type != "system" || row.Subtype != "compact_boundary" {
			continue
		}
		summary, ok := summaryByParent[row.UUID]
		if !ok {
			continue
		}
		text, isString := contentString(messageOf(summary))
		if !isString {
			text = blockText(contentBlocks(messageOf(summary)))
		}
		c.compactSummaries[row.UUID] = text
		c.consumedSummary[summary.UUID] = true
	}
}

func (c *converter) convertRow(row Row) {
	c.lastRow = row
	switch row.Type {
	case "user":
		c.convertUser(row)
	case "assistant":
		c.convertAssistant(row)
	case "system":
		c.convertSystem(row)
	case "attachment":
		// Deliberately nothing — see the Convert doc comment.
	}
}

// ---------------------------------------------------------------------
// user rows
// ---------------------------------------------------------------------

func (c *converter) convertUser(row Row) {
	if row.IsCompactSummary {
		if c.consumedSummary[row.UUID] {
			return
		}
		// A summary with no boundary row above it (older writers) is
		// still the compaction divider for this branch.
		c.ensureTurn(row)
		text, ok := contentString(messageOf(row))
		if !ok {
			text = blockText(contentBlocks(messageOf(row)))
		}
		c.emitCompaction(row, "Conversation compacted", text)
		return
	}

	blocks := contentBlocks(messageOf(row))
	results := filterBlocks(blocks, "tool_result")
	if len(results) > 0 {
		c.ensureTurn(row)
		for _, block := range results {
			c.convertToolResult(row, block)
		}
		return
	}

	text, ok := userPromptText(row)
	if !ok {
		return
	}
	// Inside a subagent this is the agent's own conversation — the task
	// prompt the CLI handed it, and anything delivered into it later —
	// so it nests under the launch (c.emit stamps the scope) rather than
	// opening a turn. startTurn is already inert in scope; the call is
	// kept so both cases read the same.
	//
	// A LIVE launch creates the prompt row from tool input before the child
	// emits output. Marking the first scoped user row makes the transcript
	// uuid enrich that launch-scoped row rather than append another one. A
	// backgrounded agent never echoes on ordinary stdout, which is why this
	// transcript path still owns the provider identity.
	c.startTurn(row)
	evt := provider.ProviderEvent{
		Kind:           provider.EventUserText,
		Role:           "user",
		ItemID:         row.UUID,
		Content:        text,
		ContentPresent: true,
	}
	if scope := strings.TrimSpace(c.subagentScope); scope != "" && !c.openingPromptByScope[scope] {
		c.openingPromptByScope[scope] = true
		meta, _ := json.Marshal(map[string]any{provider.MetaSubagentOpeningPromptKey: true})
		evt.Meta = meta
	}
	c.emit(evt, row)
}

// userPromptText reports whether a user row is something the person
// typed, and returns its text. Everything else on a `user` row is
// machinery: tool-result echoes, `isMeta` caveats, the compaction
// summary, and `isVisibleInTranscriptOnly` context injections.
func userPromptText(row Row) (string, bool) {
	if row.Type != "user" || row.IsMeta || row.IsCompactSummary {
		return "", false
	}
	if rawBool(row.Raw, "isVisibleInTranscriptOnly") {
		return "", false
	}
	msg := messageOf(row)
	if text, ok := contentString(msg); ok {
		if strings.TrimSpace(text) == "" {
			return "", false
		}
		return text, true
	}
	blocks := contentBlocks(msg)
	if len(blocks) == 0 {
		return "", false
	}
	// A block list carrying a tool_result is a tool echo, never a prompt,
	// even when a text block rides alongside it.
	if len(filterBlocks(blocks, "tool_result")) > 0 {
		return "", false
	}
	text := blockText(blocks)
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

// convertToolResult upserts a tool_result onto its launch row. Subagent
// work runs BETWEEN the launch and the completion, so the joined
// subagent transcript is converted first — same order a live session
// streams it in.
func (c *converter) convertToolResult(row Row, block map[string]any) {
	toolUseID := rawString(block, "tool_use_id")
	if toolUseID == "" {
		c.uncorrelated++
		return
	}
	c.emitSubagent(toolUseID, row)

	content := toolResultText(block["content"])
	toolUseResult := row.Raw["toolUseResult"]

	fields := map[string]any{"is_error": rawBool(block, "is_error")}
	if encoded := rawJSON(block); encoded != nil {
		fields["tool_result"] = encoded
	}
	if encoded := rawJSON(toolUseResult); encoded != nil {
		// Transcripts spell this camelCase; the wire (and therefore every
		// downstream shape helper) spells it snake_case. Translate here so
		// the writer never has to know which side produced the event.
		fields["tool_use_result"] = encoded
	}
	if code, ok := toolResultExitCode(block, toolUseResult); ok {
		fields["exit_code"] = code
	}
	if backgroundToolResult(toolUseResult) {
		fields["is_background"] = true
	}
	if agentID, commandName, ok := skillForkResult(toolUseResult); ok {
		fields["skillFork"] = map[string]string{
			"agentId":     agentID,
			"commandName": commandName,
		}
	}
	if c.toolOutputUnavailable(content) {
		fields[MetaImportUnavailableKey] = MetaImportUnavailableToolOutputGC
		c.unavailableGCs++
	}

	c.emit(provider.ProviderEvent{
		Kind:    provider.EventToolComplete,
		ItemID:  toolUseID,
		Content: content,
		Meta:    rawJSON(fields),
	}, row)
}

// ---------------------------------------------------------------------
// subagents
// ---------------------------------------------------------------------

// emitSubagent converts the transcript of the subagent a Task/Agent tool
// spawned, stamping ParentToolUseID so the writer nests the rows under
// the launch row and the existing store subagent grouping works unchanged.
func (c *converter) emitSubagent(toolUseID string, parentRow Row) {
	rows := c.opts.Subagents[toolUseID]
	if len(rows) == 0 || c.emittedAgents[toolUseID] || c.subagentScope != "" {
		return
	}
	c.emittedAgents[toolUseID] = true

	previous := c.subagentScope
	c.subagentScope = toolUseID
	for _, row := range rows {
		c.convertRow(row)
	}
	c.subagentScope = previous
	c.lastRow = parentRow
}

// ---------------------------------------------------------------------
// turns
// ---------------------------------------------------------------------

func (c *converter) ensureTurn(row Row) {
	if c.turnOpen || c.subagentScope != "" {
		return
	}
	c.openTurn(row)
}

// startTurn closes the open turn and opens the next. A user prompt is the
// only turn boundary a transcript records.
func (c *converter) startTurn(row Row) {
	if c.subagentScope != "" {
		return
	}
	c.closeTurn()
	c.openTurn(row)
}

func (c *converter) openTurn(row Row) {
	c.turnIndex++
	c.turnOpen = true
	c.stopReason = ""
	c.assistantMessageID = ""
	c.usageByModel = map[string]*provider.TokenUsage{}
	c.usageOrder = nil
	c.emit(provider.ProviderEvent{Kind: provider.EventTurnStart}, row)
}

func (c *converter) closeTurn() {
	if !c.turnOpen {
		return
	}
	c.turnOpen = false

	meta := &provider.WireTurnCompleteMeta{
		StopReason:         c.stopReason,
		AssistantMessageID: c.assistantMessageID,
	}
	var total provider.TokenUsage
	for _, model := range c.usageOrder {
		usage := *c.usageByModel[model]
		meta.ModelUsage = append(meta.ModelUsage, provider.ModelTokenUsage{Model: model, TokenUsage: usage})
		total.Add(usage)
	}
	if !total.IsZero() {
		meta.Usage = &total
	}
	c.emit(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		TurnComplete: meta,
	}, c.lastRow)
}

// ---------------------------------------------------------------------
// emit + shared decoding
// ---------------------------------------------------------------------

// seedClock primes the row clock from the chain's earliest timestamped row.
//
// Rows without a `timestamp` inherit the last one seen, but the first rows
// of a chain have nothing behind them — and an event with no timestamp
// costs the WHOLE branch, because the writer refuses an import rather than
// restamp history with now(). The chain is in causal order, so its first
// recorded time is the honest floor for anything that precedes it.
func (c *converter) seedClock(chain []Row) {
	for _, row := range chain {
		if row.Timestamp > 0 {
			c.lastTimestamp = row.Timestamp
			return
		}
	}
}

func (c *converter) emit(evt provider.ProviderEvent, row Row) {
	if row.Timestamp > 0 {
		c.lastTimestamp = row.Timestamp
		evt.Timestamp = time.UnixMilli(row.Timestamp)
	} else if c.lastTimestamp > 0 {
		evt.Timestamp = time.UnixMilli(c.lastTimestamp)
	}
	if evt.ParentToolUseID == "" {
		evt.ParentToolUseID = c.subagentScope
	}
	evt.TurnIndex = c.turnIndex
	c.events = append(c.events, importir.Event{ProviderEvent: evt, SourceUUID: row.UUID})
}

func (c *converter) appendDeferredWarnings() {
	if c.uncorrelated > 0 {
		c.warnings = append(c.warnings, importir.Warning{
			Code:    WarnUncorrelatedResult,
			Message: fmt.Sprintf("Dropped %d tool result(s) that named no tool call.", c.uncorrelated),
		})
	}
	if c.unavailableGCs > 0 {
		c.warnings = append(c.warnings, importir.Warning{
			Code:    WarnToolOutputGC,
			Message: fmt.Sprintf("%d tool output(s) were cleaned up by Claude and could not be imported.", c.unavailableGCs),
		})
	}
	if len(c.unknownSystem) > 0 {
		names := make([]string, 0, len(c.unknownSystem))
		for name := range c.unknownSystem {
			names = append(names, name)
		}
		sort.Strings(names)
		c.warnings = append(c.warnings, importir.Warning{
			Code:    WarnUnknownSystemSubtype,
			Message: "Skipped unrecognised system records: " + strings.Join(names, ", ") + ".",
		})
	}
}
