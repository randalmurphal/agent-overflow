package rollout

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/userquestion"
)

func isAsyncQuestionTool(name string) bool {
	return strings.TrimPrefix(name, "functions.") == "request_user_input_async"
}

// The legacy mirror carries questions but no item ID. The accepted raw tool
// call owns its stable identity, just as the typed item does in paginated files.
func (c *converter) isAsyncQuestionMirror(raw json.RawMessage) bool {
	_, found, err := userquestion.Decode(raw)
	if err != nil {
		c.corrupt++
	}
	return found
}

func (c *converter) emitAsyncQuestions(id string, meta userquestion.Metadata) {
	if _, done := c.itemRows[id]; done {
		return
	}
	c.ensureTurn()
	meta.BlockType = "text"
	c.emit(provider.ProviderEvent{Kind: provider.EventContentBlockStop, ItemID: id, TurnID: c.turn.id, TurnIndex: c.turn.index, Meta: metaJSON(map[string]any{"blockType": meta.BlockType, "delivery": meta.Delivery, "questions": meta.Questions}), Timestamp: c.lastTimestamp})
	c.itemRows[id] = struct{}{}
	c.releaseTool(id)
}

func (c *converter) applyAsyncQuestionItem(raw json.RawMessage) bool {
	meta, found, err := userquestion.Decode(raw)
	if !found {
		return false
	}
	var head struct {
		ID string `json:"id"`
	}
	if err != nil || json.Unmarshal(raw, &head) != nil || strings.TrimSpace(head.ID) == "" {
		c.corrupt++
		return true
	}
	c.emitAsyncQuestions(head.ID, meta)
	return true
}

// Legacy rollouts retain the accepted tool call instead of the typed item.
// Failed calls stay tools; only an accepted request produces a question card.
func (c *converter) completeAsyncQuestionTool(tool *openTool, output string, isError bool) bool {
	if !isAsyncQuestionTool(tool.rawToolName) {
		return false
	}
	var result struct {
		Accepted bool `json:"accepted"`
	}
	var args struct {
		Questions []userquestion.Question `json:"questions"`
	}
	if !isError && json.Unmarshal([]byte(output), &result) == nil && result.Accepted {
		if json.Unmarshal(tool.input, &args) == nil && userquestion.Validate(args.Questions) == nil {
			c.emitAsyncQuestions(tool.callID, userquestion.Metadata{Delivery: "async", Questions: args.Questions})
			return true
		}
		c.corrupt++
	}
	return false
}

func (c *converter) exposeAsyncQuestionTool(tool *openTool) {
	c.emit(provider.ProviderEvent{Kind: provider.EventToolStart, TurnID: tool.turnID, TurnIndex: tool.turnIndex, ItemID: tool.itemID, ItemType: tool.itemType, Role: "assistant", Meta: c.toolStartMeta(tool), Timestamp: tool.startedAt})
}
