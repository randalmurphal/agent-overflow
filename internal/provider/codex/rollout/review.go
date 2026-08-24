package rollout

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

const importedCodexReviewToolName = "codex_review"

// reviewConversion is the root rollout's half of a built-in review. Codex
// persists the reviewer transcript in a child rollout, while the root owns
// the visible command, review boundary, formatted result, and outer turn.
type reviewConversion struct {
	outerTurnID   string
	controlTurnID string
	launchID      string
	target        json.RawMessage
	hint          string
	model         string

	eventBase   int
	launchEvent int
	startOffset int64
	exited      bool
	pendingText string
	formatted   string
	fallback    string
	errorText   string
}

func (r *reviewConversion) scopes(evt provider.ProviderEvent) bool {
	if evt.ParentToolUseID != "" {
		return false
	}
	switch evt.Kind {
	case provider.EventTurnStart, provider.EventTurnComplete, provider.EventUserText,
		provider.EventCommandResult:
		return false
	case provider.EventToolStart:
		return evt.ItemID != r.launchID
	default:
		return true
	}
}

func (c *converter) beginReview(env envelope) {
	var payload reviewModePayload
	if json.Unmarshal(env.Payload, &payload) != nil {
		c.corrupt++
		return
	}
	c.beginReviewWith(payload.TurnID, payload.ItemID, payload.Target, payload.UserFacingHint)
}

func (c *converter) beginReviewItem(payload itemCompletedPayload, item turnItem) {
	c.beginReviewWith(payload.TurnID, item.ID, item.Target, item.UserFacingHint)
}

func (c *converter) beginReviewWith(turnID, itemID string, target json.RawMessage, hint string) {
	if c.review != nil {
		return
	}
	turnID = strings.TrimSpace(turnID)
	if c.turn != nil && c.turn.id != turnID {
		c.closeTurn(nil, time.Time{})
	}
	if c.turn == nil {
		c.openTurnWith(turnID, c.lastTimestamp, false)
		c.emitTurnStart()
	}

	launchID := "codex-review:" + strings.TrimSpace(itemID)
	if strings.TrimSuffix(launchID, ":") == "codex-review" {
		launchID += lineUUID(c.lineStart)
	}
	run := &reviewConversion{
		outerTurnID: turnID,
		launchID:    launchID,
		target:      append(json.RawMessage(nil), target...),
		hint:        strings.TrimSpace(hint),
		eventBase:   len(c.events) - 1,
		launchEvent: -1,
		startOffset: c.lineStart,
	}
	c.review = run

	c.emit(provider.ProviderEvent{
		Kind:           provider.EventUserText,
		Role:           "user",
		Content:        reviewCommand(target),
		ContentPresent: true,
		Meta:           json.RawMessage(`{"command":"review","expandComposerCommands":true}`),
	})
	run.launchEvent = len(c.events)
	c.emit(provider.ProviderEvent{
		Kind:     provider.EventToolStart,
		ItemID:   launchID,
		ItemType: importedCodexReviewToolName,
		Meta:     reviewLaunchMeta(run),
	})
}

func (c *converter) applyReviewTaskStarted(env envelope) bool {
	if c.review == nil {
		return false
	}
	var payload taskStartedPayload
	if json.Unmarshal(env.Payload, &payload) != nil {
		c.corrupt++
		return true
	}
	c.review.controlTurnID = strings.TrimSpace(payload.TurnID)
	if c.turn != nil && payload.ModelContextWindow > 0 {
		c.turn.contextWindow = payload.ModelContextWindow
		c.refreshTurnStartMeta()
	}
	c.refreshReviewLaunchMeta()
	return true
}

func (c *converter) exitReview(env envelope) {
	if c.review == nil {
		return
	}
	var payload reviewModePayload
	if json.Unmarshal(env.Payload, &payload) != nil {
		c.corrupt++
		return
	}
	c.review.exited = true
	c.review.pendingText = ""
	c.review.fallback = reviewOutputFallback(payload.ReviewOutputRaw)
}

func (c *converter) exitReviewItem(item turnItem) {
	if c.review == nil {
		return
	}
	c.review.exited = true
	c.review.pendingText = ""
	if item.ReviewOutput != nil {
		c.review.fallback = firstNonEmpty(
			item.ReviewOutput.OverallExplanation,
			item.ReviewOutput.OverallCorrectness,
		)
	}
}

func (c *converter) emitReviewAwareAssistantText(text string) {
	if c.review == nil {
		c.emitAssistantText(text)
		return
	}
	if c.review.exited {
		c.review.formatted = text
		return
	}
	previous := c.review.pendingText
	c.review.pendingText = text
	if strings.TrimSpace(previous) != "" {
		c.emitAssistantText(previous)
	}
}

func (c *converter) completeReviewTurn(env envelope, aborted bool) bool {
	if c.review == nil {
		return false
	}
	completedAt := c.lastTimestamp
	var completion provider.TurnCompleteMeta
	if aborted {
		var payload turnAbortedPayload
		if json.Unmarshal(env.Payload, &payload) != nil {
			c.corrupt++
			return true
		}
		if at := secondsToTime(payload.CompletedAt); !at.IsZero() {
			completedAt = at
		}
		completion = &provider.WireTurnCompleteMeta{
			StopReason:   "aborted",
			Aborted:      true,
			ErrorMessage: "Turn " + firstNonEmpty(strings.TrimSpace(payload.Reason), "aborted"),
		}
	} else {
		var payload taskCompletePayload
		if json.Unmarshal(env.Payload, &payload) != nil {
			c.corrupt++
			return true
		}
		if at := secondsToTime(payload.CompletedAt); !at.IsZero() {
			completedAt = at
		}
		completion = &provider.WireTurnCompleteMeta{StopReason: "end_turn"}
	}
	failed := c.finishReview(aborted)
	if failed {
		completion = &provider.WireTurnCompleteMeta{
			StopReason:   "error",
			ErrorMessage: firstNonEmpty(c.review.errorText, "Code review failed"),
		}
	}
	c.closeTurn(completion, completedAt)
	c.review = nil
	return true
}

func (c *converter) finishReview(aborted bool) bool {
	run := c.review
	if run == nil {
		return false
	}
	result := strings.TrimSpace(run.formatted)
	if result == "" {
		result = strings.TrimSpace(run.fallback)
	}
	hasReviewResult := result != ""
	if result == "" && aborted {
		result = "Review was interrupted. Run /review again to restart it."
	}
	failed := !aborted && !hasReviewResult && strings.TrimSpace(run.errorText) != ""
	if failed {
		result = "Code review failed: " + strings.TrimSpace(run.errorText)
	}
	if result == "" {
		result = "Code review finished without returning a result."
	}

	resultID := "codex-review-result:" + firstNonEmpty(run.outerTurnID, lineUUID(c.lineStart))
	c.emit(provider.ProviderEvent{
		Kind:           provider.EventContentBlockStop,
		Role:           "assistant",
		ItemID:         resultID + ":agent",
		ItemType:       "agentMessage",
		Content:        result,
		ContentPresent: true,
		Meta: metaJSON(map[string]any{
			"blockType":                        "text",
			provider.MetaTranscriptSnapshotKey: true,
		}),
		ParentToolUseID: run.launchID,
	})
	status := "completed"
	isError := false
	if aborted {
		status = "killed"
		isError = true
	} else if failed {
		status = "failed"
		isError = true
	}
	c.emit(provider.ProviderEvent{
		Kind:     provider.EventToolComplete,
		ItemID:   run.launchID,
		ItemType: importedCodexReviewToolName,
		Meta: metaJSON(map[string]any{
			"toolName":            importedCodexReviewToolName,
			"input":               map[string]any{"tool": "review", "model": run.model},
			"directCommandResult": true,
			"item_status":         status,
			"is_error":            isError,
		}),
	})
	resultMeta, _ := json.Marshal(provider.CommandResultMeta{
		AgentResult: &provider.CommandAgentResultMeta{
			LaunchID:   run.launchID,
			SourceKind: "review",
			SourceName: "Code review",
		},
	})
	c.emit(provider.ProviderEvent{
		Kind:           provider.EventCommandResult,
		ItemID:         resultID,
		Content:        result,
		ContentPresent: true,
		Meta:           resultMeta,
	})
	return failed
}

func (c *converter) refreshReviewLaunchMeta() {
	if c.review == nil || c.review.launchEvent < 0 || c.review.launchEvent >= len(c.events) {
		return
	}
	c.events[c.review.launchEvent].Meta = reviewLaunchMeta(c.review)
}

func reviewLaunchMeta(run *reviewConversion) json.RawMessage {
	return metaJSON(map[string]any{
		"toolName": importedCodexReviewToolName,
		"input": map[string]any{
			"tool":                "review",
			"prompt":              firstNonEmpty(run.hint, reviewTargetLabel(run.target)),
			"model":               run.model,
			"newAgentNickname":    "Code review",
			"newAgentRole":        "review",
			"reviewControlTurnId": run.controlTurnID,
		},
		"directCommandResult": true,
	})
}

func reviewCommand(target json.RawMessage) string {
	var wire struct {
		Type         string `json:"type"`
		Branch       string `json:"branch"`
		SHA          string `json:"sha"`
		Title        string `json:"title"`
		Instructions string `json:"instructions"`
	}
	if json.Unmarshal(target, &wire) != nil {
		return "/review"
	}
	switch wire.Type {
	case "baseBranch":
		return strings.TrimSpace("/review branch " + wire.Branch)
	case "commit":
		return strings.TrimSpace("/review commit " + wire.SHA + " " + wire.Title)
	case "custom":
		return strings.TrimSpace("/review custom " + wire.Instructions)
	case "uncommittedChanges", "uncommitted_changes":
		return "/review"
	default:
		return "/review"
	}
}

func reviewTargetLabel(target json.RawMessage) string {
	command := reviewCommand(target)
	if command == "/review" {
		return "Review uncommitted changes"
	}
	return strings.TrimSpace(strings.TrimPrefix(command, "/review"))
}

func reviewOutputFallback(raw json.RawMessage) string {
	var output struct {
		OverallCorrectness string `json:"overall_correctness"`
		OverallExplanation string `json:"overall_explanation"`
	}
	if json.Unmarshal(raw, &output) != nil {
		return ""
	}
	return firstNonEmpty(output.OverallExplanation, output.OverallCorrectness)
}

func reviewChildSourceUUID(childID, sourceUUID string) string {
	return fmt.Sprintf("review:%s:%s", childID, sourceUUID)
}
