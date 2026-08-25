package sessionimport

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Row is one admitted transcript entry, decoded far enough to walk the
// DAG without re-reaching into Raw for the hot fields. Raw stays the
// authority for everything else — the transcript carries far more per
// row than the importer models, and copying it into a struct would make
// every new wire field a code change here.
type Row struct {
	UUID              string
	ParentUUID        string
	LogicalParentUUID string
	Type              string
	Subtype           string
	// SourceToolAssistantUUID is the assistant row a parallel tool_result
	// answers. It is on the skeleton because the DAG needs it to re-attach
	// an orphaned result — see reattachOrphanedToolResult.
	SourceToolAssistantUUID string

	IsSidechain      bool
	IsMeta           bool
	IsCompactSummary bool
	IsSynthetic      bool

	// Timestamp is epoch ms from the row's ISO `timestamp`, 0 when absent.
	Timestamp int64

	// Raw is the decoded line. It is NIL on a skeleton row — pass 1 of the
	// transcript reader keeps only the fields above, and pass 2 fills Raw
	// for the one branch being converted (see transcript.go).
	Raw map[string]any
	// Index is the row's position in file order. Ordering leaves by it is
	// what makes branch enumeration deterministic.
	Index int
	// Offset / Length locate the row's line in the transcript, so pass 2
	// can re-read exactly this row without re-decoding the file.
	Offset int64
	Length int
}

// newRow projects a parsed transcript entry into a Row.
func newRow(raw map[string]any, index int) Row {
	return Row{
		UUID:                    rawString(raw, "uuid"),
		ParentUUID:              rawString(raw, "parentUuid"),
		LogicalParentUUID:       rawString(raw, "logicalParentUuid"),
		Type:                    rawString(raw, "type"),
		Subtype:                 rawString(raw, "subtype"),
		SourceToolAssistantUUID: rawString(raw, "sourceToolAssistantUUID"),
		IsSidechain:             rawBool(raw, "isSidechain"),
		IsMeta:                  rawBool(raw, "isMeta"),
		IsCompactSummary:        rawBool(raw, "isCompactSummary"),
		IsSynthetic:             rawBool(raw, "isSynthetic"),
		Timestamp:               parseISOMillis(rawString(raw, "timestamp")),
		Raw:                     raw,
		Index:                   index,
	}
}

// newRows projects transcript entries in file order.
func newRows(entries []map[string]any) []Row {
	rows := make([]Row, 0, len(entries))
	for i, e := range entries {
		rows = append(rows, newRow(e, i))
	}
	return rows
}

func rawString(raw map[string]any, key string) string {
	if raw == nil {
		return ""
	}
	s, _ := raw[key].(string)
	return s
}

func rawBool(raw map[string]any, key string) bool {
	if raw == nil {
		return false
	}
	b, _ := raw[key].(bool)
	return b
}

func rawMap(raw map[string]any, key string) map[string]any {
	if raw == nil {
		return nil
	}
	m, _ := raw[key].(map[string]any)
	return m
}

// rawJSON re-encodes a decoded value for a ProviderEvent Meta field.
// Returns nil for absent values and for anything that cannot round-trip,
// which is the same "meta is best-effort" posture the live parser takes.
func rawJSON(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}

// messageOf returns a row's `message` object.
func messageOf(row Row) map[string]any {
	return rawMap(row.Raw, "message")
}

// contentBlocks returns `message.content` as a block list, or nil when
// the content is a plain string (or absent).
func contentBlocks(msg map[string]any) []map[string]any {
	if msg == nil {
		return nil
	}
	list, ok := msg["content"].([]any)
	if !ok {
		return nil
	}
	blocks := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		if block, ok := entry.(map[string]any); ok {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// contentString returns `message.content` when it is a plain string.
func contentString(msg map[string]any) (string, bool) {
	if msg == nil {
		return "", false
	}
	s, ok := msg["content"].(string)
	return s, ok
}

// blockText concatenates the text bodies of a decoded block list.
func blockText(blocks []map[string]any) string {
	var b strings.Builder
	for _, block := range blocks {
		if rawString(block, "type") != "text" {
			continue
		}
		b.WriteString(rawString(block, "text"))
	}
	return b.String()
}

// filterBlocks returns the blocks of a given type.
func filterBlocks(blocks []map[string]any, blockType string) []map[string]any {
	var out []map[string]any
	for _, block := range blocks {
		if rawString(block, "type") == blockType {
			out = append(out, block)
		}
	}
	return out
}

func joinTextBlocks(blocks []map[string]any) string {
	var parts []string
	for _, block := range blocks {
		if rawString(block, "type") != "text" {
			continue
		}
		if text := strings.TrimRight(rawString(block, "text"), "\n"); strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// rawMapValue narrows an already-decoded JSON value to an object.
func rawMapValue(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func rawInt(m map[string]any, key string) int {
	value, ok := intAtAnyKey(m, key)
	if !ok {
		return 0
	}
	return value
}

func intAtAnyKey(m map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		switch typed := m[key].(type) {
		case float64:
			return int(typed), true
		case json.Number:
			if parsed, err := typed.Int64(); err == nil {
				return int(parsed), true
			}
		case int:
			return typed, true
		}
	}
	return 0, false
}

// formatNumber renders a decoded JSON number as a string, for wire fields
// (apiErrorStatus) whose type varies by CLI version.
func formatNumber(value any) string {
	switch typed := value.(type) {
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	case json.Number:
		return typed.String()
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return ""
	}
}
