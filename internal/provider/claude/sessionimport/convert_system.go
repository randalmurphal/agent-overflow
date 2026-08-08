package sessionimport

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/provider"
)

// System-row conversion: compact boundaries (including the summary row
// folded into them), local command results, API errors, and model-refusal
// notifications.

func (c *converter) convertSystem(row Row) {
	switch subtype := row.Subtype; {
	case subtype == "compact_boundary":
		c.ensureTurn(row)
		content := rawString(row.Raw, "content")
		if content == "" {
			content = "Conversation compacted"
		}
		c.emitCompaction(row, content, c.compactSummaries[row.UUID])
	case subtype == "local_command":
		text := systemText(row)
		if text == "" {
			return
		}
		c.ensureTurn(row)
		c.emit(provider.ProviderEvent{
			Kind:           provider.EventCommandResult,
			ItemID:         row.UUID,
			Content:        text,
			ContentPresent: true,
		}, row)
	case subtype == "api_error":
		c.ensureTurn(row)
		summary := systemText(row)
		if summary == "" {
			summary = "API error"
		}
		enum := strings.TrimSpace(firstNonEmpty(
			rawString(row.Raw, "apiErrorStatus"),
			formatNumber(row.Raw["apiErrorStatus"]),
			"unknown",
		))
		meta, _ := json.Marshal(map[string]any{
			"api_error_enum": enum,
			"error":          enum,
		})
		c.emit(provider.ProviderEvent{
			Kind:    provider.EventError,
			Content: summary,
			Meta:    meta,
		}, row)
	case subtype == "turn_duration":
		// No item. The row's timestamp is already the newest one seen in
		// the turn, which is what the synthesised completion carries.
	case strings.HasPrefix(subtype, "model_refusal"):
		text := systemText(row)
		if text == "" {
			text = "Model refused the request"
		}
		c.ensureTurn(row)
		meta, _ := json.Marshal(map[string]any{"kind": subtype})
		c.emit(provider.ProviderEvent{
			Kind:    provider.EventNotification,
			ItemID:  row.UUID,
			Content: text,
			Meta:    meta,
		}, row)
	case subtype == "":
		// A system row with no subtype carries nothing routable.
	default:
		c.unknownSystem[subtype]++
	}
}

// compactMetaKeys are the compaction fields worth persisting. The full
// `compactMetadata` object also carries `preservedSegment`, which is a
// chunk of the pre-compaction conversation — heavy content belongs in a
// payload, never in the always-shipped items.meta.
var compactMetaKeys = []string{"trigger", "preTokens", "durationMs"}

func (c *converter) emitCompaction(row Row, content, summary string) {
	fields := map[string]any{}
	if meta := rawMap(row.Raw, "compactMetadata"); meta != nil {
		for _, key := range compactMetaKeys {
			if value, ok := meta[key]; ok {
				fields[key] = value
			}
		}
	}
	if summary != "" {
		fields["summary"] = summary
	}
	var encoded json.RawMessage
	if len(fields) > 0 {
		encoded = rawJSON(fields)
	}
	c.emit(provider.ProviderEvent{
		Kind:    provider.EventCompactBoundary,
		ItemID:  row.UUID,
		Content: content,
		Meta:    encoded,
	}, row)
}

// systemText reads a system row's `content`, which is a string on every
// observed subtype but tolerates a block list.
func systemText(row Row) string {
	if text := rawString(row.Raw, "content"); text != "" {
		return text
	}
	list, _ := row.Raw["content"].([]any)
	blocks := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		if block, ok := entry.(map[string]any); ok {
			blocks = append(blocks, block)
		}
	}
	return blockText(blocks)
}
