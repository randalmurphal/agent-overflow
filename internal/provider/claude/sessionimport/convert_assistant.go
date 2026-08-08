package sessionimport

import (
	"encoding/json"
	"strconv"
	"strings"

	"agent-overflow/internal/provider"
)

// Assistant-row conversion: API-error rows, CLI-executed command output,
// the text / thinking / tool_use content blocks, and the per-model usage
// accumulation that lands on the turn's completion event.

func (c *converter) convertAssistant(row Row) {
	msg := messageOf(row)
	if msg == nil {
		return
	}
	c.ensureTurn(row)

	if enum, summary, ok := assistantAPIError(row, msg); ok {
		meta, _ := json.Marshal(map[string]any{
			"api_error_enum": enum,
			"error":          enum,
			"fatal":          true,
		})
		c.emit(provider.ProviderEvent{
			Kind:    provider.EventError,
			Content: summary,
			Meta:    meta,
		}, row)
		return
	}

	blocks := contentBlocks(msg)

	// The CLI stamps its own `<synthetic>` sentinel on output it produced
	// without an API call (a provider-executed slash command). It is not
	// model output and must never render as an assistant bubble — same
	// routing the live parser applies.
	if strings.TrimSpace(rawString(msg, "model")) == syntheticCLIModel {
		if text := joinTextBlocks(blocks); text != "" {
			c.emit(provider.ProviderEvent{
				Kind:           provider.EventCommandResult,
				ItemID:         rawString(msg, "id"),
				Content:        text,
				ContentPresent: true,
			}, row)
		}
		return
	}

	messageID := rawString(msg, "id")
	if messageID != "" {
		c.assistantMessageID = messageID
	}
	if reason := rawString(msg, "stop_reason"); reason != "" {
		c.stopReason = reason
	}
	c.accumulateUsage(msg)

	for _, block := range blocks {
		switch rawString(block, "type") {
		case "text":
			text := rawString(block, "text")
			if text == "" {
				continue
			}
			c.emit(provider.ProviderEvent{
				Kind:           provider.EventTextDelta,
				ItemID:         c.nextBlockItemID(messageID, row),
				Role:           "assistant",
				Content:        text,
				ContentPresent: true,
			}, row)
		case "thinking":
			text := rawString(block, "thinking")
			if text == "" {
				continue
			}
			var meta json.RawMessage
			if sig := rawString(block, "signature"); sig != "" {
				meta, _ = json.Marshal(map[string]any{"signature": sig})
			}
			c.emit(provider.ProviderEvent{
				Kind:           provider.EventThinking,
				ItemID:         c.nextBlockItemID(messageID, row),
				Content:        text,
				ContentPresent: true,
				Meta:           meta,
			}, row)
		case "tool_use":
			id := rawString(block, "id")
			if id == "" {
				continue
			}
			c.emit(provider.ProviderEvent{
				Kind:     provider.EventToolStart,
				ItemID:   id,
				ItemType: rawString(block, "name"),
				Meta:     toolStartMeta(block, messageID),
			}, row)
		}
	}
}

// syntheticCLIModel mirrors the CLI's SYNTHETIC_MODEL sentinel.
const syntheticCLIModel = "<synthetic>"

// nextBlockItemID allocates the provider item id for a completed content
// block. Falls back to the row uuid when the message carried no id.
func (c *converter) nextBlockItemID(messageID string, row Row) string {
	if messageID == "" {
		return row.UUID
	}
	if c.blockOrdinal == nil {
		c.blockOrdinal = map[string]int{}
	}
	index := c.blockOrdinal[messageID]
	c.blockOrdinal[messageID] = index + 1
	return messageID + "#" + strconv.Itoa(index)
}

// assistantAPIError detects the two shapes a failed request leaves in a
// transcript: the SDK `error` enum on (or next to) the message, and the
// CLI's own `isApiErrorMessage` row carrying an HTTP status.
func assistantAPIError(row Row, msg map[string]any) (enum, summary string, ok bool) {
	enum = strings.TrimSpace(firstNonEmpty(rawString(msg, "error"), rawString(row.Raw, "error")))
	if enum == "" && rawBool(row.Raw, "isApiErrorMessage") {
		enum = strings.TrimSpace(firstNonEmpty(
			rawString(row.Raw, "apiErrorStatus"),
			formatNumber(row.Raw["apiErrorStatus"]),
		))
		if enum == "" {
			enum = "unknown"
		}
	}
	if enum == "" {
		return "", "", false
	}
	summary = joinTextBlocks(contentBlocks(msg))
	if summary == "" {
		if text, isStr := contentString(msg); isStr {
			summary = text
		}
	}
	if strings.TrimSpace(summary) == "" {
		summary = "API error: " + enum
	}
	return enum, summary, true
}

// toolStartMeta mirrors the live parser's marshalToolMeta so an imported
// launch row shapes identically: normalised MCP tool name, the raw input
// for the summary preview, the background flag, and the owning assistant
// message id.
func toolStartMeta(block map[string]any, assistantMessageID string) json.RawMessage {
	name := rawString(block, "name")
	fields := map[string]any{"toolName": name}
	if server, tool, ok := splitMCPToolName(name); ok {
		fields["toolName"] = "MCP/" + tool
		fields["mcp"] = map[string]string{"server": server, "tool": tool}
	}
	if input, ok := block["input"]; ok {
		if encoded := rawJSON(input); encoded != nil {
			fields["input"] = encoded
		}
		if rawBool(rawMapValue(input), "run_in_background") {
			fields["is_background"] = true
		}
	}
	if assistantMessageID != "" {
		fields["assistant_message_id"] = assistantMessageID
	}
	return rawJSON(fields)
}

// splitMCPToolName splits `mcp__<server>__<tool>`.
func splitMCPToolName(name string) (server, tool string, ok bool) {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return "", "", false
	}
	rest := name[len(prefix):]
	sep := strings.Index(rest, "__")
	if sep <= 0 {
		return "", "", false
	}
	if tool = rest[sep+2:]; tool == "" {
		return "", "", false
	}
	return rest[:sep], tool, true
}

func (c *converter) accumulateUsage(msg map[string]any) {
	usage := rawMap(msg, "usage")
	if usage == nil {
		return
	}
	delta := provider.TokenUsage{
		InputTokens:              rawInt(usage, "input_tokens"),
		OutputTokens:             rawInt(usage, "output_tokens"),
		CacheReadInputTokens:     rawInt(usage, "cache_read_input_tokens"),
		CacheCreationInputTokens: rawInt(usage, "cache_creation_input_tokens"),
	}
	if delta.IsZero() {
		return
	}
	// Cost is deliberately absent: a transcript carries none. The wire's
	// `total_cost_usd` rides the stream-json `result` envelope, which is
	// not written to the session file, so imported usage is priced at
	// query time from the rate table (CostSource "none").
	model := strings.TrimSpace(rawString(msg, "model"))
	if model == "" {
		model = "unknown"
	}
	existing, ok := c.usageByModel[model]
	if !ok {
		existing = &provider.TokenUsage{}
		c.usageByModel[model] = existing
		c.usageOrder = append(c.usageOrder, model)
	}
	existing.Add(delta)
}
