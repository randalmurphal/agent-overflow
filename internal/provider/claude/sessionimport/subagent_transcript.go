package sessionimport

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// SidechainProjector incrementally projects one live transcript-mirror
// sidechain into the events a whole-session import emits for the same rows
// under their launch (emitSubagent).
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
		backgroundHints:      map[string]bool{},
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
