package codex

import (
	"crypto/sha256"
	"encoding/json"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

const maxSubagentNotificationDedupEntries = 1024

type subagentNotificationDedupKey struct {
	ParentItemID string
	MetaHash     [sha256.Size]byte
}

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
	if s.emitSubagentNotificationsFromRawMailboxCarrier(method, params, providerThreadID, parentToolUseID) {
		return
	}
	if parentToolUseID != "" {
		s.rememberAgentPathForProviderThread(providerThreadID, parentToolUseID, params)
	}
	if method == "thread/started" {
		s.rememberAgentMetaForProviderThread(providerThreadID, params)
	}
	if parentToolUseID != "" {
		childStateNotification := isChildSuppressedThreadNotification(method) ||
			isChildSuppressedItemNotification(method, params)
		if childStateNotification {
			// Drop child-thread state notifications that would otherwise
			// overwrite the parent thread's projection. See the suppression
			// helpers in collab_agents.go for the rationale.
			return
		}
		if isChildTurnLifecycleNotification(method) {
			if evt := s.childLifecycleEvent(method, params, parentToolUseID); evt != nil {
				s.onEvent(*evt)
			}
			return
		}
	}

	if method == "item/plan/delta" {
		s.appendPlanDelta(params)
	}
	if method == "thread/tokenUsage/updated" {
		// Parent-thread only (child-thread notifications returned above).
		// Folds the cumulative total into the per-turn usage accounting;
		// the context-meter EventTokenUsage classification below is
		// untouched.
		s.usageAcct.observe(params)
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
	// Codex-injected subagent notifications are standalone contextual user
	// fragments. If a tag appears inside ordinary user prose, treat it as
	// literal text rather than a forgeable control message.
	return s.emitResolvedSubagentNotifications(notifications, remainder, true)
}

func (s *Session) emitSubagentNotificationsFromRawMailboxCarrier(
	method string,
	params json.RawMessage,
	providerThreadID string,
	parentToolUseID string,
) bool {
	// Codex's MultiAgentV2 path delivers detached child completions to
	// the parent mailbox as model-visible context. App-server has no typed
	// timeline event for that boundary; with experimental raw events
	// enabled, the accepted mailbox item is visible as a raw message
	// immediately before the next sampling request is built. That is the
	// "parent has seen this context" signal the completion row should
	// follow.
	if method != "rawResponseItem/completed" {
		return false
	}
	if strings.TrimSpace(parentToolUseID) != "" {
		return false
	}
	if s.codexThreadID != "" && strings.TrimSpace(providerThreadID) != s.codexThreadID {
		return false
	}

	item := readNestedObject(params, "item")
	return s.emitResolvedSubagentNotificationsFromRawMessageItem(item)
}

func (s *Session) emitResolvedSubagentNotificationsFromRawMessageItem(item map[string]json.RawMessage) bool {
	if item == nil || readRawString(item, "type") != "message" {
		return false
	}

	var notifications []subagentNotification
	var remainder string
	switch strings.TrimSpace(readRawString(item, "role")) {
	case "user":
		notifications, remainder = extractSubagentNotificationsAndRemainderFromRawUserMessageItem(item)
	case "assistant":
		notifications, remainder = extractSubagentNotificationsAndRemainderFromRawInterAgentMessageItem(item)
	default:
		return false
	}
	return s.emitResolvedSubagentNotifications(notifications, remainder, false)
}

func (s *Session) emitResolvedSubagentNotifications(notifications []subagentNotification, remainder string, requireKnownParent bool) bool {
	if len(notifications) == 0 {
		return false
	}
	if strings.TrimSpace(remainder) != "" {
		return false
	}
	if requireKnownParent && !s.allSubagentNotificationsResolveToParent(notifications) {
		return false
	}
	emitted := false
	for _, n := range notifications {
		if s.emitSubagentNotification(n, requireKnownParent) {
			emitted = true
		}
	}
	return emitted
}

func (s *Session) emitSubagentNotification(n subagentNotification, requireKnownParent bool) bool {
	if s.onEvent == nil {
		return false
	}
	parentItemID := s.parentToolUseForAgentPath(n.AgentPath)
	if parentItemID == "" {
		parentItemID = s.parentToolUseForProviderThread(n.AgentPath)
	}
	if parentItemID == "" && requireKnownParent {
		return false
	}
	meta := buildSubagentNotificationMeta(n)
	if !s.claimSubagentNotification(parentItemID, meta) {
		return false
	}
	s.onEvent(provider.ProviderEvent{
		Kind:      provider.EventSubagentNotification,
		ThreadID:  s.threadID,
		ItemID:    parentItemID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
	return true
}

func (s *Session) claimSubagentNotification(parentItemID string, meta json.RawMessage) bool {
	key := subagentNotificationDedupKey{
		ParentItemID: parentItemID,
		MetaHash:     sha256.Sum256(meta),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subagentNotificationDedup == nil {
		s.subagentNotificationDedup = make(map[subagentNotificationDedupKey]struct{})
	}
	if _, ok := s.subagentNotificationDedup[key]; ok {
		return false
	}
	if len(s.subagentNotificationDedup) >= maxSubagentNotificationDedupEntries {
		s.subagentNotificationDedup = make(map[subagentNotificationDedupKey]struct{})
	}
	s.subagentNotificationDedup[key] = struct{}{}
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
		s.usageAcct.onTurnStart()
		if evt.TurnID == "" {
			return
		}
		s.mu.Lock()
		s.activeTurnID = evt.TurnID
		s.mu.Unlock()
	case provider.EventTurnComplete:
		if meta, ok := evt.TurnComplete.(*provider.WireTurnCompleteMeta); ok && meta != nil {
			s.attachTurnUsage(meta)
		}
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
