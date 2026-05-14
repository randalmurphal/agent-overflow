package codex

import (
	"encoding/json"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

func (s *Session) dispatchNotification(method string, params json.RawMessage) {
	params = s.observeRawResponseItem(method, params)
	providerThreadID := providerThreadIDFromParams(params)
	parentToolUseID := s.parentToolUseForProviderThread(providerThreadID)
	if parentToolUseID != "" {
		s.rememberAgentPathForProviderThread(providerThreadID, parentToolUseID, params)
	}
	if method == "thread/started" {
		s.rememberAgentMetaForProviderThread(providerThreadID, params)
	}
	if parentToolUseID != "" && isChildTurnLifecycleNotification(method) {
		if evt := s.childLifecycleEvent(method, params, parentToolUseID); evt != nil {
			s.onEvent(*evt)
		}
		return
	}

	if method == "item/plan/delta" {
		s.appendPlanDelta(params)
	}
	events := s.classifyNotificationWithBufferedPlan(method, params)
	suppressSubagentNotificationCarrier := s.emitSubagentNotificationsFromUserCarrier(method, params, events)

	for i := range events {
		evt := &events[i]
		if suppressSubagentNotificationCarrier && evt.Kind == provider.EventUserText {
			continue
		}
		s.prepareNotificationEvent(evt, method, params, parentToolUseID)
		if evt.Kind == provider.EventTurnStart && evt.TurnID != "" && !s.claimTurnStart(evt.TurnID) {
			// The app-server occasionally re-sends turn/started (recovery,
			// retries). Suppress the duplicate so downstream persistence sees
			// exactly one turn per user send (Bug B6).
			continue
		}
		s.updateNotificationState(evt)
		// No heuristic background classification here (invariant 25). The
		// wire-typed signals are exposed in evt.Meta; triage owns projection.
		s.onEvent(*evt)
	}
}

func (s *Session) emitSubagentNotificationsFromUserCarrier(method string, params json.RawMessage, events []provider.ProviderEvent) bool {
	// Detect <subagent_notification> tags injected by Codex core into the next
	// user-message item after a detached child agent reaches a terminal state
	// with no parent `wait` outstanding.
	if method != "item/completed" || !hasProviderEventKind(events, provider.EventUserText) {
		return false
	}
	notifications, remainder := extractSubagentNotificationsAndRemainderFromUserMessage(params)
	if len(notifications) == 0 {
		return false
	}
	// Codex-injected subagent notifications are standalone contextual user
	// fragments. If a tag appears inside ordinary user prose, treat it as
	// literal text rather than a forgeable control message.
	if strings.TrimSpace(remainder) != "" || !s.allSubagentNotificationsResolveToParent(notifications) {
		return false
	}
	for _, n := range notifications {
		parentItemID := s.parentToolUseForAgentPath(n.AgentPath)
		if parentItemID == "" {
			parentItemID = s.parentToolUseForProviderThread(n.AgentPath)
		}
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventSubagentNotification,
			ThreadID:  s.threadID,
			ItemID:    parentItemID,
			Meta:      buildSubagentNotificationMeta(n),
			Timestamp: time.Now(),
		})
	}
	return true
}

func (s *Session) prepareNotificationEvent(evt *provider.ProviderEvent, method string, params json.RawMessage, parentToolUseID string) {
	if parentToolUseID != "" {
		evt.ParentToolUseID = parentToolUseID
	}
	s.maybeRewriteCollabControlItemID(evt, params)
	s.maybeRememberCollabReceiverThreads(method, params)
	s.enrichRawToolCallMetadata(evt)
	s.preserveWaitAgentReceiverTargets(evt)
	s.enrichCollabReceiverMetadata(evt)
}

func (s *Session) updateNotificationState(evt *provider.ProviderEvent) {
	switch evt.Kind {
	case provider.EventTurnStart:
		if evt.TurnID == "" {
			return
		}
		s.mu.Lock()
		s.activeTurnID = evt.TurnID
		s.mu.Unlock()
	case provider.EventTurnComplete:
		s.mu.Lock()
		s.activeTurnID = ""
		s.rawToolCallsByID = make(map[string]rawToolCall)
		s.waitReceiverIDsByCall = make(map[string][]string)
		s.mu.Unlock()
		s.clearPlanBufferForTurn(evt.TurnID)
		s.clearTurnStart(evt.TurnID)
	}
}

func hasProviderEventKind(events []provider.ProviderEvent, kind provider.EventKind) bool {
	for i := range events {
		if events[i].Kind == kind {
			return true
		}
	}
	return false
}
