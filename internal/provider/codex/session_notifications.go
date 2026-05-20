package codex

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

func (s *Session) dispatchNotification(method string, params json.RawMessage) {
	// mcpServer/oauthLogin/completed is a side-channel signal directed
	// at the App's probe-cache, not a transcript event. Handle it
	// upstream of the general classifier so it never gets normalised
	// into a ProviderEvent the rest of the pipeline has to ignore.
	if method == "mcpServer/oauthLogin/completed" {
		s.dispatchMCPOAuthCompletion(params)
		return
	}
	if method == "mcpServer/startupStatus/updated" {
		s.dispatchMCPStartupUpdate(params)
		return
	}
	params = s.observeRawResponseItem(method, params)
	providerThreadID := providerThreadIDFromParams(params)
	parentToolUseID := s.parentToolUseForProviderThread(providerThreadID)
	if parentToolUseID != "" {
		s.rememberAgentPathForProviderThread(providerThreadID, parentToolUseID, params)
	}
	if method == "thread/started" {
		s.rememberAgentMetaForProviderThread(providerThreadID, params)
	}
	if parentToolUseID != "" && isChildSuppressedThreadNotification(method) {
		// Drop child-thread state notifications that would otherwise
		// overwrite the parent thread's projection. See
		// isChildSuppressedThreadNotification for the rationale.
		return
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

// dispatchMCPStartupUpdate decodes `mcpServer/startupStatus/updated`
// and forwards to the registered handler. Per-thread-only emission;
// AO routes these into the app-level mcpstatus cache so the popup
// reflects the live provider state without a refetch. Missing
// serverName is logged and dropped (defensive — Codex source
// guarantees the field is set).
func (s *Session) dispatchMCPStartupUpdate(params json.RawMessage) {
	s.mu.Lock()
	handler := s.mcpStartupUpdateHandler
	s.mu.Unlock()
	if handler == nil {
		return
	}
	var parsed struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(params, &parsed); err != nil {
		log.Printf("codex: decode mcpServer/startupStatus/updated: %v", err)
		return
	}
	if strings.TrimSpace(parsed.Name) == "" {
		log.Printf("codex: mcpServer/startupStatus/updated: missing name")
		return
	}
	handler(MCPStartupUpdate{Name: parsed.Name, State: parsed.Status, Error: parsed.Error})
}

func (s *Session) dispatchMCPOAuthCompletion(params json.RawMessage) {
	s.mu.Lock()
	handler := s.mcpOAuthCompletedHandler
	s.mu.Unlock()
	if handler == nil {
		return
	}
	var parsed struct {
		ServerName string `json:"serverName"`
		Success    bool   `json:"success"`
		Error      string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(params, &parsed); err != nil {
		log.Printf("codex: decode mcpServer/oauthLogin/completed: %v", err)
		return
	}
	if parsed.ServerName == "" {
		log.Printf("codex: mcpServer/oauthLogin/completed: missing serverName")
		return
	}
	handler(parsed.ServerName, parsed.Success, parsed.Error)
}
