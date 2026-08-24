package codex

import (
	"encoding/json"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// handleReviewSpecialNotification consumes review-only lifecycle shapes that
// cannot pass through the ordinary classifier unchanged. It runs on the read
// loop before general classification.
func (s *Session) handleReviewSpecialNotification(method string, params json.RawMessage) bool {
	if method == "turn/started" {
		return s.handleReviewControlTurnStarted(params)
	}
	if method == "turn/completed" {
		return s.handleReviewOuterTurnCompleted(params)
	}
	if method != "item/started" && method != "item/completed" {
		return false
	}

	itemType := classifyCodexItemType(params)
	if method == "item/started" {
		switch itemType {
		case "enteredReviewMode":
			return s.handleReviewEntered(params)
		case "agentMessage", "assistantMessage":
			return s.handleReviewAgentMessageStarted(params)
		}
		return false
	}

	switch itemType {
	case "exitedReviewMode":
		return s.handleReviewExited(params)
	case "agentMessage", "assistantMessage":
		return s.handleReviewAgentMessageCompleted(params)
	}
	return false
}

func (s *Session) handleReviewEntered(params json.RawMessage) bool {
	outerTurnID := strings.TrimSpace(readTopLevelString(params, "turnId"))
	item := readNestedObject(params, "item")
	itemID := strings.TrimSpace(readRawString(item, "id"))
	reviewPrompt := strings.TrimSpace(readRawString(item, "review"))

	s.mu.Lock()
	run := s.review
	if run == nil {
		s.mu.Unlock()
		return false
	}
	if run.outerTurnID != "" && outerTurnID != "" && run.outerTurnID != outerTurnID {
		s.mu.Unlock()
		return false
	}
	if outerTurnID == "" {
		outerTurnID = run.outerTurnID
	}
	if outerTurnID == "" {
		s.mu.Unlock()
		return false
	}
	run.outerTurnID = outerTurnID
	if run.entered {
		s.mu.Unlock()
		return true
	}
	run.entered = true
	run.launchID = reviewLaunchID(itemID, outerTurnID)
	launchID := run.launchID
	turnIndex := run.turnIndex
	model := run.model
	targetLabel := reviewTargetLabel(run.target)
	s.mu.Unlock()

	s.claimTurnStart(outerTurnID)
	input := map[string]any{
		"tool":             "review",
		"prompt":           firstNonEmptyString(reviewPrompt, targetLabel),
		"model":            model,
		"newAgentNickname": "Code review",
		"newAgentRole":     "review",
	}
	meta, _ := json.Marshal(map[string]any{
		"toolName":            codexReviewToolName,
		"input":               input,
		"directCommandResult": true,
	})
	now := time.Now()
	s.emitEvents(
		provider.ProviderEvent{
			Kind: provider.EventTurnStart, ThreadID: s.threadID,
			TurnID: outerTurnID, TurnIndex: turnIndex, Timestamp: now,
		},
		provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: s.threadID,
			TurnID: outerTurnID, TurnIndex: turnIndex, ItemID: launchID,
			ItemType: codexReviewToolName, Meta: meta, Timestamp: now,
		},
	)
	return true
}

func (s *Session) handleReviewControlTurnStarted(params json.RawMessage) bool {
	turnID := strings.TrimSpace(readNestedString(params, "turn", "id"))
	if turnID == "" {
		return false
	}
	s.mu.Lock()
	run := s.review
	if run == nil || (run.outerTurnID != "" && run.outerTurnID == turnID) {
		s.mu.Unlock()
		return false
	}
	if run.controlTurnID != "" && run.controlTurnID != turnID {
		s.mu.Unlock()
		return false
	}
	run.controlTurnID = turnID
	s.mu.Unlock()

	evt := provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: s.threadID,
		TurnID: turnID, Timestamp: time.Now(),
	}
	if s.claimTurnStart(turnID) {
		s.updateNotificationState(&evt)
	}
	// The private execution turn is control state only. Emitting it would open
	// a second frontend round and overwrite the visible review's outer id.
	return true
}

func (s *Session) handleReviewAgentMessageStarted(params json.RawMessage) bool {
	turnID := strings.TrimSpace(readTopLevelString(params, "turnId"))
	item := readNestedObject(params, "item")
	snapshot := &reviewAgentSnapshot{
		itemID: strings.TrimSpace(readRawString(item, "id")),
		text:   readRawString(item, "text"),
	}

	s.mu.Lock()
	run := s.review
	if run == nil || run.outerTurnID == "" || run.outerTurnID != turnID {
		s.mu.Unlock()
		return false
	}
	if run.exited {
		// The generated parent-context answer starts after exitedReviewMode.
		// Hold it until item/completed supplies the authoritative snapshot.
		run.pendingAgent = snapshot
		s.mu.Unlock()
		return true
	}
	previous := run.pendingAgent
	run.pendingAgent = snapshot
	launchID := run.launchID
	turnIndex := run.turnIndex
	s.mu.Unlock()

	if previous != nil && strings.TrimSpace(previous.text) != "" {
		s.emitEvent(reviewSnapshotEvent(s.threadID, turnID, turnIndex, launchID, *previous))
	}
	return true
}

func (s *Session) handleReviewAgentMessageCompleted(params json.RawMessage) bool {
	turnID := strings.TrimSpace(readTopLevelString(params, "turnId"))
	item := readNestedObject(params, "item")
	itemID := strings.TrimSpace(readRawString(item, "id"))
	text := readRawString(item, "text")

	s.mu.Lock()
	run := s.review
	if run == nil || run.outerTurnID == "" || run.outerTurnID != turnID {
		s.mu.Unlock()
		return false
	}
	if !run.exited {
		if run.pendingAgent != nil && run.pendingAgent.itemID == itemID {
			run.pendingAgent = nil
		}
		s.mu.Unlock()
		// A completed pre-exit message is a normal review transcript row.
		return false
	}
	run.formattedResult = text
	run.pendingAgent = nil
	s.mu.Unlock()
	return true
}

func (s *Session) handleReviewExited(params json.RawMessage) bool {
	turnID := strings.TrimSpace(readTopLevelString(params, "turnId"))
	item := readNestedObject(params, "item")
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.review
	if run == nil || run.outerTurnID == "" || run.outerTurnID != turnID {
		return false
	}
	run.exited = true
	run.pendingAgent = nil // the last pre-exit message is raw structured output
	run.fallbackResult = readRawString(item, "review")
	return true
}

func (s *Session) handleReviewOuterTurnCompleted(params json.RawMessage) bool {
	wire := decodeTurnCompletedParams(params)
	outerTurnID := strings.TrimSpace(wire.Turn.ID)

	s.mu.Lock()
	run := s.review
	if run == nil || run.outerTurnID == "" || run.outerTurnID != outerTurnID {
		s.mu.Unlock()
		return false
	}
	copy := *run
	s.mu.Unlock()

	result := strings.TrimSpace(copy.formattedResult)
	if result == "" {
		result = strings.TrimSpace(copy.fallbackResult)
	}
	hasReviewResult := result != ""
	if wire.Turn.Status == "failed" && !hasReviewResult {
		message := firstNonEmptyString(wire.Turn.Error.Message, copy.errorMessage)
		if strings.TrimSpace(message) != "" {
			result = "Code review failed: " + strings.TrimSpace(message)
		}
	}
	if result == "" && wire.Turn.Status == "interrupted" {
		result = "Review was interrupted. Run /review again to restart it."
	}
	if result == "" {
		result = "Code review finished without returning a result."
	}

	now := time.Now()
	events := make([]provider.ProviderEvent, 0, 6)
	resultID := "codex-review-result:" + outerTurnID
	events = append(events, reviewSnapshotEvent(
		s.threadID, outerTurnID, copy.turnIndex, copy.launchID,
		reviewAgentSnapshot{itemID: resultID + ":agent", text: result},
	))
	for _, evt := range classifyTurnCompleted(s.threadID, params, now) {
		if evt.Kind != provider.EventError {
			continue
		}
		evt.ParentToolUseID = copy.launchID
		evt.Failure = &provider.FailureMeta{
			Class: provider.FailureFatal, Boundary: provider.FailureBoundaryTurn,
		}
		evt.Meta = mergeMetaKeys(evt.Meta, map[string]any{"fatal": false})
		events = append(events, evt)
	}
	completeMeta := map[string]any{
		"toolName": codexReviewToolName,
		"input": map[string]any{
			"tool":  "review",
			"model": copy.model,
		},
		"directCommandResult": true,
	}
	switch wire.Turn.Status {
	case "failed":
		if hasReviewResult {
			completeMeta["item_status"] = "completed"
		} else {
			completeMeta["is_error"] = true
			completeMeta["item_status"] = "failed"
		}
	case "interrupted":
		completeMeta["is_error"] = true
		completeMeta["item_status"] = "killed"
	default:
		completeMeta["item_status"] = "completed"
	}
	encodedComplete, _ := json.Marshal(completeMeta)
	events = append(events, provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: s.threadID,
		TurnID: outerTurnID, TurnIndex: copy.turnIndex, ItemID: copy.launchID,
		ItemType: codexReviewToolName, Meta: encodedComplete, Timestamp: now,
	})
	resultMeta, _ := json.Marshal(provider.CommandResultMeta{
		AgentResult: &provider.CommandAgentResultMeta{
			LaunchID: copy.launchID, SourceKind: "review", SourceName: "Code review",
		},
	})
	events = append(events, provider.ProviderEvent{
		Kind: provider.EventCommandResult, ThreadID: s.threadID,
		TurnID: outerTurnID, TurnIndex: copy.turnIndex, ItemID: resultID,
		Content: result, ContentPresent: true, Meta: resultMeta, Timestamp: now,
	})
	for _, evt := range classifyTurnCompleted(s.threadID, params, now) {
		if evt.Kind == provider.EventTurnComplete {
			evt.TurnIndex = copy.turnIndex
			if wire.Turn.Status == "failed" && hasReviewResult {
				evt.Failure = nil
				if complete, ok := evt.TurnComplete.(*provider.WireTurnCompleteMeta); ok {
					complete.StopReason = "end_turn"
					complete.Aborted = false
					complete.ErrorMessage = ""
				}
			}
			s.updateNotificationState(&evt)
			events = append(events, evt)
		}
	}

	s.mu.Lock()
	if s.review == run {
		run.completed = true
		if run.responseBound {
			s.review = nil
		}
	}
	s.mu.Unlock()
	s.emitEvents(events...)
	return true
}

func (s *Session) scopeReviewEvents(events []provider.ProviderEvent) []provider.ProviderEvent {
	if len(events) == 0 {
		return events
	}
	s.mu.Lock()
	run := s.review
	if run == nil {
		s.mu.Unlock()
		return events
	}
	outerTurnID := run.outerTurnID
	launchID := run.launchID
	turnIndex := run.turnIndex
	s.mu.Unlock()

	out := events[:0]
	for _, evt := range events {
		if evt.TurnID == "" || evt.TurnID != outerTurnID {
			out = append(out, evt)
			continue
		}
		if evt.Kind == provider.EventTodoUpdate {
			continue
		}
		evt.ParentToolUseID = launchID
		evt.TurnIndex = turnIndex
		if evt.Kind == provider.EventError {
			evt.Meta = mergeMetaKeys(evt.Meta, map[string]any{"fatal": false})
			if evt.Failure != nil {
				failure := *evt.Failure
				failure.Boundary = provider.FailureBoundaryTurn
				evt.Failure = &failure
			}
			s.mu.Lock()
			if s.review != nil && s.review.outerTurnID == outerTurnID {
				s.review.errorMessage = evt.Content
			}
			s.mu.Unlock()
		}
		out = append(out, evt)
	}
	return out
}

func (s *Session) reviewScopeForTurn(turnID string) string {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.review == nil || s.review.outerTurnID != turnID {
		return ""
	}
	return s.review.launchID
}

func reviewSnapshotEvent(threadID, turnID string, turnIndex int, launchID string, snapshot reviewAgentSnapshot) provider.ProviderEvent {
	meta, _ := json.Marshal(map[string]any{
		"blockType":                        "text",
		provider.MetaTranscriptSnapshotKey: true,
	})
	return provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: threadID,
		TurnID: turnID, TurnIndex: turnIndex, ItemID: snapshot.itemID,
		ItemType: "agentMessage", Content: snapshot.text, ContentPresent: true,
		Meta: meta, ParentToolUseID: launchID, Timestamp: time.Now(),
	}
}

func reviewLaunchID(itemID, turnID string) string {
	if itemID = strings.TrimSpace(itemID); itemID != "" {
		return "codex-review:" + itemID
	}
	return "codex-review:" + strings.TrimSpace(turnID)
}

func reviewTargetLabel(target ReviewTarget) string {
	switch target.kind {
	case ReviewTargetBaseBranch:
		return "Review changes against " + target.branch
	case ReviewTargetCommit:
		return "Review commit " + target.sha
	case ReviewTargetCustom:
		return target.instructions
	default:
		return "Review uncommitted changes"
	}
}

func (s *Session) emitEvents(events ...provider.ProviderEvent) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	for _, event := range events {
		s.emitEventLocked(event)
	}
}
