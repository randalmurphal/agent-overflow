package sessionimport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// SidechainProjector incrementally projects one live transcript-mirror
// sidechain into the same event vocabulary as ConvertSubagentTranscript.
// It owns converter state across batches, so assistant block ordinals,
// timestamps, and compact-boundary pairing do not reset each time Claude
// flushes a mirror envelope.
//
// A projector is single-use and not safe for concurrent calls. Append and
// Close return only events and warnings not returned by an earlier call.
type SidechainProjector struct {
	converter *converter
	rowIndex  int
	pending   *Row
	closed    bool
}

// NewSidechainProjector creates an incremental projector scoped to one
// launch tool_use id. An empty scope is rejected because unscoped events
// would leak a child transcript into the parent conversation.
func NewSidechainProjector(launchToolUseID string) (*SidechainProjector, error) {
	scope := strings.TrimSpace(launchToolUseID)
	if scope == "" {
		return nil, fmt.Errorf("sessionimport: new sidechain projector: empty launch tool_use id")
	}
	return &SidechainProjector{converter: newSubagentConverter(scope)}, nil
}

// Append decodes and projects one transcript_mirror entries batch. Claude
// sends complete transcript rows here, not arbitrary byte fragments.
func (p *SidechainProjector) Append(entries []json.RawMessage) (ConvertResult, error) {
	rows, err := DecodeSidechainRows(entries, time.Time{})
	if err != nil {
		return ConvertResult{}, err
	}
	return p.AppendRows(rows)
}

// DecodeSidechainRows performs the one full JSON decode a mirrored entry
// needs. The live parser inspects and deduplicates these Rows before handing
// them to AppendRows, avoiding separate map decodes for classification,
// projection, and child-agent binding.
//
// Claude normally timestamps transcript rows. When it omits one, receivedAt
// is the honest live timestamp: unlike an offline import, a mirror row has an
// observed arrival boundary and must not wait indefinitely for a later row to
// lend it a clock.
func DecodeSidechainRows(entries []json.RawMessage, receivedAt time.Time) ([]Row, error) {
	rows := make([]Row, 0, len(entries))
	fallbackMillis := receivedAt.UnixMilli()
	if receivedAt.IsZero() {
		fallbackMillis = 0
	}
	for i, entry := range entries {
		var raw map[string]any
		if err := json.Unmarshal(entry, &raw); err != nil {
			return nil, fmt.Errorf("sessionimport: decode sidechain mirror entry %d: %w", i, err)
		}
		row := newRow(raw, i)
		if row.Timestamp == 0 {
			row.Timestamp = fallbackMillis
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// AppendRows projects an already-decoded batch atomically. Validation and
// decoding happen before this call, so a rejected batch cannot partially
// advance converter state or consume source UUIDs in the caller.
func (p *SidechainProjector) AppendRows(rows []Row) (ConvertResult, error) {
	if p == nil || p.converter == nil {
		return ConvertResult{}, fmt.Errorf("sessionimport: append sidechain mirror: nil projector")
	}
	if p.closed {
		return ConvertResult{}, fmt.Errorf("sessionimport: append sidechain mirror: projector is closed")
	}
	for i := range rows {
		rows[i].Index = p.rowIndex + i
	}

	p.rowIndex += len(rows)
	for _, row := range rows {
		if row.UUID == "" || !isConvertibleTranscriptType(row.Type) {
			continue
		}
		p.push(row)
	}
	return p.drain(), nil
}

// Close flushes the final look-behind row and returns deferred conversion
// warnings. Calling Close more than once is safe and returns an empty result.
func (p *SidechainProjector) Close() ConvertResult {
	if p == nil || p.converter == nil || p.closed {
		return ConvertResult{}
	}
	p.closed = true
	if p.pending != nil {
		p.converter.convertRow(*p.pending)
		p.pending = nil
	}
	p.converter.appendDeferredWarnings()
	return p.drain()
}

func (p *SidechainProjector) push(row Row) {
	if p.pending != nil {
		previous := *p.pending
		// The compact summary is written as the boundary's child. Holding
		// only a boundary lets a split mirror batch fold both into one
		// divider without delaying ordinary live rows.
		if row.IsCompactSummary && row.ParentUUID == previous.UUID {
			text, isString := contentString(messageOf(row))
			if !isString {
				text = blockText(contentBlocks(messageOf(row)))
			}
			p.converter.compactSummaries[previous.UUID] = text
			p.converter.consumedSummary[row.UUID] = true
			p.converter.convertRow(previous)
			p.pending = nil
			return
		}
		p.converter.convertRow(previous)
		p.pending = nil
	}

	if row.Type == "system" && row.Subtype == "compact_boundary" {
		p.pending = &row
		return
	}
	p.converter.convertRow(row)
}

func (p *SidechainProjector) drain() ConvertResult {
	result := ConvertResult{
		Events:   p.converter.events,
		Warnings: p.converter.warnings,
	}
	p.converter.events = nil
	p.converter.warnings = nil
	return result
}

func newSubagentConverter(scope string) *converter {
	return &converter{
		usageByModel:         map[string]*provider.TokenUsage{},
		unknownSystem:        map[string]int{},
		emittedAgents:        map[string]bool{},
		openingPromptByScope: map[string]bool{},
		subagentScope:        scope,
		compactSummaries:     map[string]string{},
		consumedSummary:      map[string]bool{},
	}
}

func isConvertibleTranscriptType(rowType string) bool {
	switch rowType {
	case "user", "assistant", "system", "attachment":
		return true
	default:
		return false
	}
}

// One subagent transcript, on its own.
//
// LoadSubagents is the whole-session join: it discovers which agent
// belongs to which Task by walking a parent transcript's tool_result
// rows, because a subagent row carries no `parentToolUseID` and the
// file name is the only link. That is the right shape for import, which
// starts from a session file and has to find everything in it.
//
// The live path arrives from the other direction. Claude's
// `system/task_notification` hands out the agent's sidechain JSONL as
// `output_file` and names the launching `tool_use_id` in the same
// envelope, so the pair the join exists to discover is already known —
// and there is no parent transcript in play at all, only a live thread.
// These two entry points serve that caller: one file, one known launch,
// nothing else read.

// ConvertSubagentTranscript reads ONE subagent sidechain transcript
// (`<sessionDir>/subagents/agent-<agentId>.jsonl`, which is what a
// `task_notification` `output_file` resolves to for a `local_agent`
// task) and projects it into the import event vocabulary scoped to the
// tool_use that launched it.
//
// Every event comes back with `ParentToolUseID = launchToolUseID` and
// `TurnIndex` 0: a subagent has no turns of its own — its rows live in
// the launching turn (invariant 10) — so pinning the turn is the
// caller's job.
//
// The caller is responsible for bounding the file before calling: this
// reads it whole, the same way import reads a joined subagent.
func ConvertSubagentTranscript(path, launchToolUseID string) (ConvertResult, error) {
	if strings.TrimSpace(launchToolUseID) == "" {
		return ConvertResult{}, fmt.Errorf("sessionimport: convert subagent transcript: empty launch tool_use id")
	}
	rows, err := readSubagentRows(path)
	if err != nil {
		return ConvertResult{}, fmt.Errorf("sessionimport: read subagent transcript %q: %w", path, err)
	}
	return ConvertSubagentRows(rows, launchToolUseID), nil
}

// ConvertSubagentTranscriptData is the in-memory counterpart used when the
// caller already read and bounded Claude's output_file for payload storage.
// It avoids reading every terminal transcript twice while preserving the same
// parser and projection as file-backed import.
func ConvertSubagentTranscriptData(data []byte, launchToolUseID string) (ConvertResult, error) {
	if strings.TrimSpace(launchToolUseID) == "" {
		return ConvertResult{}, fmt.Errorf("sessionimport: convert subagent transcript data: empty launch tool_use id")
	}
	rows, err := readSubagentRowsFrom(bytes.NewReader(data), "")
	if err != nil {
		return ConvertResult{}, fmt.Errorf("sessionimport: read subagent transcript data: %w", err)
	}
	return ConvertSubagentRows(rows, launchToolUseID), nil
}

// ConvertSubagentRows is ConvertSubagentTranscript's in-memory half: the
// same projection over rows the caller already holds.
//
// It runs the ordinary converter with `subagentScope` pinned from the
// start, which is exactly the state `emitSubagent` puts it in while
// converting a joined agent — so a sidechain converted on its own here
// produces the same events, in the same order, with the same ids, as the
// same rows converted as part of a full session import. Turn management
// stays suppressed for the same reason it is suppressed there (a
// subagent's rows belong to the launching turn), and there is therefore
// no open turn to close and no synthesised `EventTurnComplete`: the
// agent's usage belongs on its parent's turn, which this caller is not
// building.
func ConvertSubagentRows(rows []Row, launchToolUseID string) ConvertResult {
	scope := strings.TrimSpace(launchToolUseID)
	if scope == "" || len(rows) == 0 {
		return ConvertResult{}
	}
	c := newSubagentConverter(scope)
	c.indexCompactSummaries(rows)
	c.seedClock(rows)
	for _, row := range rows {
		c.convertRow(row)
	}
	c.appendDeferredWarnings()
	return ConvertResult{Events: c.events, Warnings: c.warnings}
}
