package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/ctxutil"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/stringsx"
)

type collabReceiverMeta struct {
	ThreadID        string `json:"threadId"`
	AgentNickname   string `json:"agentNickname,omitempty"`
	AgentRole       string `json:"agentRole,omitempty"`
	Model           string `json:"-"`
	ReasoningEffort string `json:"-"`
}

type collabLaunchMeta struct {
	Prompt            string
	Model             string
	ReasoningEffort   string
	AgentPath         string
	ReceiverThreadIDs []string
}

func (s *Session) allSubagentNotificationsResolveToParent(notifications []subagentNotification) bool {
	if len(notifications) == 0 {
		return false
	}
	for _, notification := range notifications {
		if s.parentToolUseForAgentPath(notification.AgentPath) != "" {
			continue
		}
		if s.parentToolUseForProviderThread(notification.AgentPath) != "" {
			continue
		}
		return false
	}
	return true
}

// isChildTurnLifecycleNotification lists the methods that drive the
// child-spawn EventSubagentStatus emission (see childLifecycleEvent
// below). The child lifecycle and suppression helpers partition the
// child-thread notifications we care about — anything in those sets is
// intercepted before reaching the parent dispatch in session_notifications.go.
func isChildTurnLifecycleNotification(method string) bool {
	switch method {
	case "turn/started", "turn/completed":
		return true
	default:
		return false
	}
}

// isChildSuppressedThreadNotification lists the notification methods that
// describe child-thread state and must NOT update the parent thread's
// projection when received on a known child wire-thread. Per
// ADR-002, Codex subagents flatten onto the parent thread; these
// notifications would otherwise overwrite the parent's meter / title /
// compact state with the child's. Mirrors the Claude precedent in
// `internal/provider/claude/parse_assistant.go:appendContextUsageEvent`
// (`if parentToolUseID != "" { return events }`).
//
// `turn/started` / `turn/completed` are intentionally NOT here — they
// drive the EventSubagentStatus emission that the spawn card needs.
// Routing for those lives in `isChildTurnLifecycleNotification` above
// (adding child notification handling in one helper but not the matching
// lifecycle / suppression helper is a subtle bug).
//
// `error` and unrecognised `thread/*` methods (e.g. a future
// `thread/error`) are ALSO intentionally not suppressed — subagent
// failures need to surface on the parent thread so the user knows
// something went wrong; silently dropping them would hide real errors.
func isChildSuppressedThreadNotification(method string) bool {
	switch method {
	case "thread/tokenUsage/updated",
		"thread/compacted",
		"thread/name/updated",
		"thread/started",
		"thread/status/changed",
		"thread/archived",
		"thread/unarchived",
		"thread/closed",
		"model/rerouted",
		"model/verification",
		"account/rateLimits/updated",
		"turn/plan/updated":
		return true
	default:
		return false
	}
}

// isChildSuppressedItemNotification lists item lifecycle notifications that
// describe child-thread state rather than child transcript content. Codex now
// emits compaction as `item/*` with `item.type:"contextCompaction"` instead of
// only the older `thread/compacted` notification, so this mirrors
// isChildSuppressedThreadNotification for that item shape.
func isChildSuppressedItemNotification(method string, params json.RawMessage) bool {
	switch method {
	case "item/started", "item/completed":
		return classifyCodexItemType(params) == "contextCompaction"
	default:
		return false
	}
}

func (s *Session) childLifecycleEvents(method string, params json.RawMessage, parentToolUseID string) []provider.ProviderEvent {
	providerThreadID := providerThreadIDFromParams(params)
	if providerThreadID == "" {
		return nil
	}
	if method == "turn/started" {
		event := s.childStatusEvent(providerThreadID, parentToolUseID, "running")
		if event == nil {
			return nil
		}
		return []provider.ProviderEvent{*event}
	}
	if method != "turn/completed" {
		return nil
	}
	completed := decodeTurnCompletedParams(params)
	status := codexSubagentStatusFromTurnCompleted(params)
	if status == "" {
		return nil
	}
	event := s.childStatusEvent(providerThreadID, parentToolUseID, status)
	if event == nil {
		return nil
	}
	events := []provider.ProviderEvent{*event}
	if status == "errored" {
		if message := strings.TrimSpace(completed.Turn.Error.Message); message != "" {
			events = append(events, provider.ProviderEvent{
				Kind:            provider.EventError,
				ThreadID:        s.threadID,
				TurnID:          completed.Turn.ID,
				ParentToolUseID: parentToolUseID,
				Content:         message,
				Meta:            json.RawMessage(`{"fatal":false,"source":"codex_child_turn"}`),
				Timestamp:       time.Now(),
			})
		}
	}
	return events
}

func (s *Session) emitChildLifecycleEvents(method string, params json.RawMessage, parentToolUseID string) {
	providerThreadID := providerThreadIDFromParams(params)
	if providerThreadID == "" {
		return
	}
	s.observeAndEmitChildLifecycle(providerThreadID, s.childLifecycleEvents(method, params, parentToolUseID))
}

// observeAndEmitChildLifecycle invalidates in-flight recovery snapshots for
// every lifecycle wire observation, even if malformed input normalizes to no
// event. Event delivery is reserved before releasing childLifecycleMu so a
// later observation cannot overtake this one, but the external callback never
// runs while the lifecycle lock is held.
func (s *Session) observeAndEmitChildLifecycle(providerThreadID string, events []provider.ProviderEvent) {
	providerThreadID = strings.TrimSpace(providerThreadID)
	if providerThreadID == "" {
		return
	}
	s.childLifecycleMu.Lock()
	if s.childLifecycleRevision == nil {
		s.childLifecycleRevision = make(map[string]uint64)
	}
	s.childLifecycleRevision[providerThreadID]++
	s.eventMu.Lock()
	s.childLifecycleMu.Unlock()
	defer s.eventMu.Unlock()
	for _, event := range events {
		s.emitEventLocked(event)
	}
}

func (s *Session) childLifecycleRevisionForThread(providerThreadID string) uint64 {
	s.childLifecycleMu.Lock()
	defer s.childLifecycleMu.Unlock()
	return s.childLifecycleRevision[strings.TrimSpace(providerThreadID)]
}

// isUnsafeChildProjectionEvent is the second fail-closed layer after wire
// thread ownership. These event kinds mutate thread-wide parent state or
// correlate parent-owned user input, so they must never be projected from a
// child even when the child itself is known. Transcript-bearing child events
// (assistant text, thinking, tools, command output, diffs, warnings) remain
// allowed and receive ParentToolUseID in the normal dispatch path.
func isUnsafeChildProjectionEvent(kind provider.EventKind) bool {
	switch kind {
	case provider.EventInit,
		provider.EventTurnStart,
		provider.EventTurnComplete,
		provider.EventSessionStatus,
		provider.EventTokenUsage,
		provider.EventRateLimits,
		provider.EventModelRerouted,
		provider.EventModelFallback,
		provider.EventThreadRenamed,
		provider.EventCompactBoundary,
		provider.EventTodoUpdate,
		provider.EventTaskCreate,
		provider.EventTaskUpdate,
		provider.EventAPIRetry,
		provider.EventUserText,
		provider.EventProposedPlan:
		return true
	default:
		return false
	}
}

func codexSubagentStatusFromTurnCompleted(params json.RawMessage) string {
	wire := decodeTurnCompletedParams(params)
	return codexSubagentStatusFromTurnStatus(wire.Turn.Status)
}

func codexSubagentStatusFromTurnStatus(turnStatus string) string {
	switch turnStatus {
	case "completed":
		return "completed"
	case "failed":
		return "errored"
	case "interrupted":
		return "interrupted"
	default:
		return ""
	}
}

func (s *Session) childStatusEvent(providerThreadID, parentToolUseID, status string) *provider.ProviderEvent {
	providerThreadID = strings.TrimSpace(providerThreadID)
	parentToolUseID = strings.TrimSpace(parentToolUseID)
	status = strings.TrimSpace(status)
	if providerThreadID == "" || parentToolUseID == "" || status == "" {
		return nil
	}
	meta, err := json.Marshal(map[string]string{
		"agent_path": providerThreadID,
		"status":     status,
	})
	if err != nil {
		log.Printf("codex: marshal child status for %s: %v", providerThreadID, err)
		return nil
	}
	return &provider.ProviderEvent{
		Kind:            provider.EventSubagentStatus,
		ThreadID:        s.threadID,
		ItemID:          parentToolUseID,
		ParentToolUseID: parentToolUseID,
		Meta:            meta,
		Timestamp:       time.Now(),
	}
}

func (s *Session) childErrorStatusEvent(providerThreadID, parentToolUseID string) *provider.ProviderEvent {
	return s.childStatusEvent(providerThreadID, parentToolUseID, "errored")
}

func (s *Session) emitChildErrorStatusEvent(providerThreadID, parentToolUseID string) {
	event := s.childErrorStatusEvent(providerThreadID, parentToolUseID)
	if event == nil {
		s.observeAndEmitChildLifecycle(providerThreadID, nil)
		return
	}
	s.observeAndEmitChildLifecycle(providerThreadID, []provider.ProviderEvent{*event})
}

func (s *Session) emitRecoveredChildStatus(providerThreadID string, expectedRevision uint64, event provider.ProviderEvent) bool {
	s.childLifecycleMu.Lock()
	if s.childLifecycleRevision[providerThreadID] != expectedRevision {
		s.childLifecycleMu.Unlock()
		return false
	}
	s.eventMu.Lock()
	s.childLifecycleMu.Unlock()
	defer s.eventMu.Unlock()
	s.emitEventLocked(event)
	return true
}

func (s *Session) parentToolUseForProviderThread(providerThreadID string) string {
	if providerThreadID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childParentByThread[providerThreadID]
}

func (s *Session) parentToolUseForAgentPath(agentPath string) string {
	if agentPath == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childParentByAgentPath[agentPath]
}

func (s *Session) providerThreadForAgentPath(agentPath, parentToolUseID string) string {
	agentPath = strings.TrimSpace(agentPath)
	parentToolUseID = strings.TrimSpace(parentToolUseID)
	if agentPath == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	providerThreadID := s.childThreadByAgentPath[agentPath]
	if parentToolUseID == "" || s.childParentByThread[providerThreadID] == parentToolUseID {
		return providerThreadID
	}
	return ""
}

func (s *Session) setParentToolUseForProviderThread(providerThreadID, parentToolUseID string) bool {
	return s.registerChildOwnership("", providerThreadID, "", parentToolUseID)
}

func (s *Session) registerHistoricalChildOwnership(sourceThreadID, childThreadID, agentPath, parentToolUseID string) bool {
	return s.registerChildOwnershipWithSource(sourceThreadID, childThreadID, agentPath, parentToolUseID, true)
}

// registerChildOwnership is the single ownership mutation boundary for live
// V1/V2 events, raw enrichment, and resume-history reconstruction. A provider
// thread can never be its own child and an established thread edge cannot be
// remapped to a different launch item by malformed or replayed input. Canonical
// paths are different: Codex may reuse one after an app-server restart, so a
// newly observed child thread replaces the path's historical lookup while the
// old thread keeps its immutable launch ownership for late thread-scoped events.
func (s *Session) registerChildOwnership(sourceThreadID, childThreadID, agentPath, parentToolUseID string) bool {
	return s.registerChildOwnershipWithSource(sourceThreadID, childThreadID, agentPath, parentToolUseID, false)
}

func (s *Session) registerChildOwnershipWithSource(sourceThreadID, childThreadID, agentPath, parentToolUseID string, fromHistory bool) bool {
	sourceThreadID = strings.TrimSpace(sourceThreadID)
	childThreadID = strings.TrimSpace(childThreadID)
	agentPath = strings.TrimSpace(agentPath)
	parentToolUseID = strings.TrimSpace(parentToolUseID)
	if childThreadID == "" || parentToolUseID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if childThreadID == strings.TrimSpace(s.codexThreadID) || (sourceThreadID != "" && childThreadID == sourceThreadID) {
		log.Printf("codex: reject self-referential child ownership source=%q child=%q item=%q", sourceThreadID, childThreadID, parentToolUseID)
		return false
	}
	if s.childParentByThread == nil {
		s.childParentByThread = make(map[string]string)
	}
	if existing := s.childParentByThread[childThreadID]; existing != "" && existing != parentToolUseID {
		log.Printf("codex: reject conflicting child thread ownership %q: existing=%s incoming=%s", childThreadID, existing, parentToolUseID)
		return false
	}
	if agentPath != "" {
		if s.childParentByAgentPath == nil {
			s.childParentByAgentPath = make(map[string]string)
		}
		if s.agentPathByThread == nil {
			s.agentPathByThread = make(map[string]string)
		}
		if s.childThreadByAgentPath == nil {
			s.childThreadByAgentPath = make(map[string]string)
		}
		if s.childPathOwnerLive == nil {
			s.childPathOwnerLive = make(map[string]bool)
		}
		if existing := s.agentPathByThread[childThreadID]; existing != "" && existing != agentPath {
			log.Printf("codex: reject conflicting canonical path for child %q: existing=%q incoming=%q", childThreadID, existing, agentPath)
			return false
		}
		if currentThreadID := s.childThreadByAgentPath[agentPath]; currentThreadID != "" && currentThreadID != childThreadID {
			if s.agentPathByThread[childThreadID] == agentPath {
				log.Printf("codex: reject replay from superseded child path ownership %q: current=%s replay=%s", agentPath, currentThreadID, childThreadID)
				return false
			}
			if !fromHistory && s.childPathOwnerLive[agentPath] {
				log.Printf("codex: reject conflicting live child path ownership %q: current=%s incoming=%s", agentPath, currentThreadID, childThreadID)
				return false
			}
		}
	}
	s.childParentByThread[childThreadID] = parentToolUseID
	if agentPath != "" {
		s.agentPathByThread[childThreadID] = agentPath
		currentThreadID := s.childThreadByAgentPath[agentPath]
		preserveLiveOwner := fromHistory && currentThreadID != "" && s.childPathOwnerLive[agentPath]
		if !preserveLiveOwner {
			s.childParentByAgentPath[agentPath] = parentToolUseID
			s.childThreadByAgentPath[agentPath] = childThreadID
			s.childPathOwnerLive[agentPath] = !fromHistory
		}
	}
	return true
}

func (s *Session) rememberSubAgentActivityOwnership(sourceThreadID string, activity subAgentActivity) []string {
	if activity.Kind != "started" || activity.ItemID == "" || activity.AgentThreadID == "" {
		return nil
	}
	if !s.registerChildOwnership(sourceThreadID, activity.AgentThreadID, activity.AgentPath, activity.ItemID) {
		return nil
	}
	model, reasoningEffort := s.collabProfileForThread(sourceThreadID)
	launchMeta := collabLaunchMeta{
		Model:             model,
		ReasoningEffort:   reasoningEffort,
		AgentPath:         activity.AgentPath,
		ReceiverThreadIDs: []string{activity.AgentThreadID},
	}
	s.scheduleCollabMetadataRead(activity.AgentThreadID, activity.ItemID, launchMeta)
	return []string{activity.AgentThreadID}
}

func (s *Session) activeCollabModel() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model, s.reasoningEffort
}

func (s *Session) collabProfileForThread(providerThreadID string) (string, string) {
	providerThreadID = strings.TrimSpace(providerThreadID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if providerThreadID != "" && providerThreadID != s.codexThreadID {
		if meta := s.agentMetaByThread[providerThreadID]; meta.Model != "" || meta.ReasoningEffort != "" {
			return meta.Model, meta.ReasoningEffort
		}
	}
	return s.model, s.reasoningEffort
}

func (s *Session) rememberCollabProfile(providerThreadID, model, reasoningEffort string) {
	providerThreadID = strings.TrimSpace(providerThreadID)
	if providerThreadID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agentMetaByThread == nil {
		s.agentMetaByThread = make(map[string]collabReceiverMeta)
	}
	meta := s.agentMetaByThread[providerThreadID]
	meta.ThreadID = providerThreadID
	meta.Model = strings.TrimSpace(model)
	meta.ReasoningEffort = strings.TrimSpace(reasoningEffort)
	s.agentMetaByThread[providerThreadID] = meta
}

func (s *Session) observeSubAgentActivityOwnership(method string, params json.RawMessage) []string {
	activity, ok := decodeSubAgentActivity(method, params)
	if !ok {
		return nil
	}
	return s.rememberSubAgentActivityOwnership(providerThreadIDFromParams(params), activity)
}

func collabSpawnReceiverThreadIDs(method string, params json.RawMessage) []string {
	if method != "item/completed" {
		return nil
	}
	item := readNestedObject(params, "item")
	if item == nil || readRawString(item, "type") != "collabAgentToolCall" ||
		normalizeCollabToolName(readRawString(item, "tool")) != "spawn_agent" {
		return nil
	}
	return readRawStringArray(item, "receiverThreadIds")
}

func (s *Session) rememberRawSpawnAgentIDForSubagentNotifications(agentID, parentToolUseID string) {
	// Raw spawn_agent output in older/current app-server builds returns
	// `agent_id` before the richer typed child-thread metadata arrives.
	// Detached completion notifications can later use that exact value as
	// `agent_path`, so index it through the same resolver as receiver
	// thread IDs.
	s.setParentToolUseForProviderThread(strings.TrimSpace(agentID), strings.TrimSpace(parentToolUseID))
}

func (s *Session) deleteParentToolUseForProviderThread(providerThreadID string) {
	if providerThreadID == "" {
		return
	}
	s.mu.Lock()
	if agentPath := s.agentPathByThread[providerThreadID]; agentPath != "" {
		if s.childThreadByAgentPath[agentPath] == providerThreadID {
			delete(s.childParentByAgentPath, agentPath)
			delete(s.childThreadByAgentPath, agentPath)
			delete(s.childPathOwnerLive, agentPath)
		}
		delete(s.agentPathByThread, providerThreadID)
	}
	delete(s.childParentByThread, providerThreadID)
	delete(s.agentMetaByThread, providerThreadID)
	s.mu.Unlock()
}

func (s *Session) emitCollabReceiverMetaUpdate(parentToolUseID string, meta collabReceiverMeta, launchMeta collabLaunchMeta) {
	if s.onEvent == nil || strings.TrimSpace(parentToolUseID) == "" || strings.TrimSpace(meta.ThreadID) == "" {
		return
	}
	input := collabReceiverMetaUpdateInput(meta, launchMeta)
	if input == nil {
		return
	}
	encodedMeta, err := json.Marshal(map[string]any{
		"meta_update_only": true,
		"toolName":         "collab_agent",
		"input":            input,
	})
	if err != nil {
		return
	}
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  s.threadID,
		ItemID:    parentToolUseID,
		ItemType:  "collab_agent",
		Meta:      encodedMeta,
		Timestamp: time.Now(),
	})
}

func collabReceiverMetaUpdateInput(meta collabReceiverMeta, launchMeta collabLaunchMeta) map[string]any {
	receiverThreadIDs := nonEmptyStrings(launchMeta.ReceiverThreadIDs)
	if len(receiverThreadIDs) == 0 && strings.TrimSpace(meta.ThreadID) != "" {
		receiverThreadIDs = []string{strings.TrimSpace(meta.ThreadID)}
	}
	if len(receiverThreadIDs) == 0 {
		return nil
	}
	input := map[string]any{
		"tool":              "spawn_agent",
		"receiverThreadIds": receiverThreadIDs,
	}
	if meta.AgentNickname != "" {
		input["newAgentNickname"] = meta.AgentNickname
	}
	if meta.AgentRole != "" {
		input["newAgentRole"] = meta.AgentRole
	}
	if strings.TrimSpace(launchMeta.Prompt) != "" {
		input["prompt"] = strings.TrimSpace(launchMeta.Prompt)
	}
	if strings.TrimSpace(launchMeta.Model) != "" {
		input["model"] = strings.TrimSpace(launchMeta.Model)
	}
	if strings.TrimSpace(launchMeta.ReasoningEffort) != "" {
		input["reasoningEffort"] = strings.TrimSpace(launchMeta.ReasoningEffort)
	}
	if strings.TrimSpace(launchMeta.AgentPath) != "" {
		input["agentPath"] = strings.TrimSpace(launchMeta.AgentPath)
		input["taskName"] = strings.TrimSpace(launchMeta.AgentPath)
	}
	return input
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (s *Session) rememberAgentPathForProviderThread(providerThreadID, parentToolUseID string, params json.RawMessage) {
	agentPath := subagentThreadStartedAgentPath(params)
	if providerThreadID == "" || parentToolUseID == "" || agentPath == "" {
		return
	}
	s.registerChildOwnership("", providerThreadID, agentPath, parentToolUseID)
}

func (s *Session) rememberAgentMetaForProviderThread(providerThreadID string, params json.RawMessage) {
	providerThreadID = strings.TrimSpace(providerThreadID)
	if providerThreadID == "" {
		return
	}
	meta := collabReceiverMeta{
		ThreadID:      providerThreadID,
		AgentNickname: subagentThreadStartedNickname(params),
		AgentRole:     subagentThreadStartedRole(params),
	}
	if meta.AgentNickname == "" && meta.AgentRole == "" {
		return
	}
	s.rememberCollabReceiverMeta(meta)
}

func (s *Session) deleteUnownedAgentMeta(providerThreadID string) {
	providerThreadID = strings.TrimSpace(providerThreadID)
	if providerThreadID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.childParentByThread[providerThreadID] == "" {
		delete(s.agentMetaByThread, providerThreadID)
	}
}

func providerThreadIDFromParams(params json.RawMessage) string {
	// One top-level decode serves all three key probes — this runs per
	// notification on the read-loop goroutine, so the former three
	// readTopLevelString calls paid three full map decodes each time the
	// earlier keys were absent.
	var m map[string]json.RawMessage
	if json.Unmarshal(params, &m) != nil {
		return ""
	}
	if id := readRawString(m, "threadId"); id != "" {
		return id
	}
	if id := readRawString(m, "conversationId"); id != "" {
		return id
	}
	return readRawString(readRawObject(m, "thread"), "id")
}

func subagentThreadStartedAgentPath(params json.RawMessage) string {
	candidates := []string{
		readNestedString(params, "thread", "source", "subAgent", "thread_spawn", "agent_path"),
		readNestedString(params, "thread", "source", "subAgent", "threadSpawn", "agentPath"),
		readNestedString(params, "thread", "source", "subAgent", "thread_spawn", "agentPath"),
		readNestedString(params, "thread", "source", "subAgent", "threadSpawn", "agent_path"),
	}
	for _, candidate := range candidates {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

func subagentThreadStartedNickname(params json.RawMessage) string {
	return stringsx.FirstNonEmptyTrimmed(
		readNestedString(params, "thread", "agentNickname"),
		readNestedString(params, "thread", "agent_nickname"),
		readNestedString(params, "thread", "source", "subAgent", "thread_spawn", "agent_nickname"),
		readNestedString(params, "thread", "source", "subAgent", "threadSpawn", "agentNickname"),
	)
}

func subagentThreadStartedRole(params json.RawMessage) string {
	return stringsx.FirstNonEmptyTrimmed(
		readNestedString(params, "thread", "agentRole"),
		readNestedString(params, "thread", "agent_role"),
		readNestedString(params, "thread", "source", "subAgent", "thread_spawn", "agent_role"),
		readNestedString(params, "thread", "source", "subAgent", "threadSpawn", "agentRole"),
	)
}

func (s *Session) maybeRememberCollabReceiverThreads(method string, params json.RawMessage) {
	if method != "item/completed" {
		return
	}
	item := readNestedObject(params, "item")
	if item == nil || readRawString(item, "type") != "collabAgentToolCall" {
		return
	}
	tool := normalizeCollabToolName(readRawString(item, "tool"))
	itemID := readRawString(item, "id")
	receiverThreadIDs := readRawStringArray(item, "receiverThreadIds")
	if itemID == "" || len(receiverThreadIDs) == 0 {
		return
	}

	switch tool {
	case "spawn_agent":
		launchMeta := collabLaunchMeta{
			Prompt:            readRawString(item, "prompt"),
			Model:             readRawString(item, "model"),
			ReasoningEffort:   readRawString(item, "reasoningEffort"),
			ReceiverThreadIDs: append([]string(nil), receiverThreadIDs...),
		}
		for _, receiverThreadID := range receiverThreadIDs {
			if !s.registerChildOwnership(providerThreadIDFromParams(params), receiverThreadID, "", itemID) {
				continue
			}
			s.scheduleCollabMetadataRead(receiverThreadID, itemID, launchMeta)
		}
	case "close_agent":
		for _, receiverThreadID := range receiverThreadIDs {
			s.deleteParentToolUseForProviderThread(receiverThreadID)
		}
	}
}

func (s *Session) maybeRewriteCollabControlItemID(evt *provider.ProviderEvent, params json.RawMessage) {
	if evt == nil {
		return
	}
	item := readNestedObject(params, "item")
	if item == nil {
		return
	}
	if readRawString(item, "type") == "subAgentActivity" &&
		readRawString(item, "kind") == "interrupted" &&
		evt.Kind == provider.EventSubagentStatus {
		if parentToolUseID := s.parentToolUseForProviderThread(readRawString(item, "agentThreadId")); parentToolUseID != "" {
			evt.ItemID = parentToolUseID
			evt.ParentToolUseID = parentToolUseID
		}
		return
	}
	if evt.ItemType != "collab_agent" || readRawString(item, "type") != "collabAgentToolCall" {
		return
	}
	switch normalizeCollabToolName(readRawString(item, "tool")) {
	case "close_agent", "resume_agent":
		for _, receiverThreadID := range readRawStringArray(item, "receiverThreadIds") {
			if parentToolUseID := s.parentToolUseForProviderThread(receiverThreadID); parentToolUseID != "" {
				evt.ItemID = parentToolUseID
				return
			}
		}
	}
}

func (s *Session) enrichCollabReceiverMetadata(evt *provider.ProviderEvent) {
	if evt == nil {
		return
	}
	switch evt.ItemType {
	case "collab_agent", "send_input", "wait_agent", "close_agent", "resume_agent":
	default:
		return
	}
	mutateEventMetaInput(evt, false, func(input map[string]json.RawMessage) {
		receiverThreadIDs := readRawStringArray(input, "receiverThreadIds")
		requestedReceiverThreadIDs := readRawStringArray(input, "requestedReceiverThreadIds")
		if len(receiverThreadIDs) == 0 && len(requestedReceiverThreadIDs) == 0 {
			return
		}
		s.setCollabReceiverMetadata(input, "receiverAgents", receiverThreadIDs)
		s.setCollabReceiverMetadata(input, "requestedReceiverAgents", requestedReceiverThreadIDs)
	})
}

func (s *Session) setCollabReceiverMetadata(input map[string]json.RawMessage, key string, receiverThreadIDs []string) {
	if len(receiverThreadIDs) == 0 {
		return
	}
	receiverAgents := s.collabReceiverMetadataForThreads(receiverThreadIDs)
	if len(receiverAgents) == 0 {
		return
	}
	encodedReceiverAgents, err := json.Marshal(receiverAgents)
	if err == nil {
		input[key] = encodedReceiverAgents
	}
}

func (s *Session) preserveWaitAgentReceiverTargets(evt *provider.ProviderEvent) {
	if evt == nil || evt.ItemType != "wait_agent" {
		return
	}
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		return
	}
	switch evt.Kind {
	case provider.EventToolStart:
		receiverThreadIDs := receiverThreadIDsFromEventMeta(evt.Meta)
		if len(receiverThreadIDs) == 0 {
			return
		}
		s.mu.Lock()
		if s.waitReceiverIDsByCall == nil {
			s.waitReceiverIDsByCall = make(map[string][]string)
		}
		s.waitReceiverIDsByCall[itemID] = append([]string(nil), receiverThreadIDs...)
		s.mu.Unlock()
	case provider.EventToolComplete:
		s.mu.Lock()
		receiverThreadIDs := append([]string(nil), s.waitReceiverIDsByCall[itemID]...)
		delete(s.waitReceiverIDsByCall, itemID)
		s.mu.Unlock()
		if len(receiverThreadIDs) == 0 {
			return
		}
		mutateEventMetaInput(evt, true, func(input map[string]json.RawMessage) {
			setRawStringArray(input, "requestedReceiverThreadIds", receiverThreadIDs)
		})
	}
}

func receiverThreadIDsFromEventMeta(meta json.RawMessage) []string {
	var decoded struct {
		Input map[string]json.RawMessage `json:"input"`
	}
	if len(meta) == 0 || json.Unmarshal(meta, &decoded) != nil || decoded.Input == nil {
		return nil
	}
	return readRawStringArray(decoded.Input, "receiverThreadIds")
}

func mutateEventMetaInput(evt *provider.ProviderEvent, createInput bool, mutate func(map[string]json.RawMessage)) {
	if evt == nil || mutate == nil {
		return
	}
	var meta map[string]json.RawMessage
	if len(evt.Meta) == 0 || json.Unmarshal(evt.Meta, &meta) != nil || meta == nil {
		if !createInput {
			return
		}
		meta = map[string]json.RawMessage{}
	}
	var input map[string]json.RawMessage
	if raw, ok := meta["input"]; ok {
		_ = json.Unmarshal(raw, &input)
	}
	if input == nil {
		if !createInput {
			return
		}
		input = map[string]json.RawMessage{}
	}
	mutate(input)
	if len(input) == 0 {
		return
	}
	encodedInput, err := json.Marshal(input)
	if err != nil {
		return
	}
	meta["input"] = encodedInput
	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return
	}
	evt.Meta = encodedMeta
}

func setRawStringIfMissing(input map[string]json.RawMessage, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if existing := strings.TrimSpace(readRawString(input, key)); existing != "" {
		return
	}
	encoded, err := json.Marshal(value)
	if err == nil {
		input[key] = encoded
	}
}

func setRawStringArray(input map[string]json.RawMessage, key string, values []string) {
	if len(values) == 0 {
		return
	}
	encoded, err := json.Marshal(values)
	if err == nil {
		input[key] = encoded
	}
}

func (s *Session) collabReceiverMetadataForThreads(receiverThreadIDs []string) []collabReceiverMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.agentMetaByThread) == 0 {
		return nil
	}
	agents := make([]collabReceiverMeta, 0, len(receiverThreadIDs))
	hasLabel := false
	for _, threadID := range receiverThreadIDs {
		threadID = strings.TrimSpace(threadID)
		if threadID == "" {
			continue
		}
		meta := s.agentMetaByThread[threadID]
		if meta.ThreadID == "" {
			meta.ThreadID = threadID
		}
		if meta.AgentNickname != "" || meta.AgentRole != "" {
			hasLabel = true
		}
		agents = append(agents, meta)
	}
	if !hasLabel {
		return nil
	}
	return agents
}

func (s *Session) scheduleCollabMetadataRead(providerThreadID, parentToolUseID string, launchMeta collabLaunchMeta) {
	if s.proc == nil || strings.TrimSpace(providerThreadID) == "" || strings.TrimSpace(parentToolUseID) == "" {
		return
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if !s.acquireCollabMetadataRead(ctx) {
		return
	}
	if !s.startCollabAsync(func() {
		defer s.releaseCollabMetadataRead()
		s.readChildThreadMetadata(providerThreadID, parentToolUseID, launchMeta)
	}) {
		s.releaseCollabMetadataRead()
	}
}

func (s *Session) readChildThreadMetadata(providerThreadID, parentToolUseID string, launchMeta collabLaunchMeta) {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	meta, ok, err := s.readChildThreadMetadataWithRetry(ctx, providerThreadID)
	if err != nil {
		log.Printf("codex: read child thread metadata for spawn %s: %v", parentToolUseID, err)
		return
	}
	if !ok || s.closing.Load() {
		return
	}
	s.rememberCollabReceiverMeta(meta)
	s.emitCollabReceiverMetaUpdate(parentToolUseID, meta, launchMeta)
}

func (s *Session) acquireCollabMetadataRead(ctx context.Context) bool {
	if s.collabMetadataReads == nil {
		return true
	}
	select {
	case s.collabMetadataReads <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	default:
		// Metadata is display-only enrichment. Never block the JSON-RPC read
		// loop behind slow thread/read calls; canonical activity metadata still
		// carries the child id/path needed for routing and UI fallback labels.
		return false
	}
}

func (s *Session) releaseCollabMetadataRead() {
	if s.collabMetadataReads == nil {
		return
	}
	select {
	case <-s.collabMetadataReads:
	default:
	}
}

func (s *Session) readChildThreadMetadataWithRetry(ctx context.Context, providerThreadID string) (collabReceiverMeta, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			if !ctxutil.Sleep(ctx, time.Duration(attempt)*100*time.Millisecond) {
				return collabReceiverMeta{}, false, nil
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		meta, ok, err := s.readChildThreadMetadataOnce(attemptCtx, providerThreadID)
		cancel()
		if err == nil && ok {
			return meta, ok, nil
		}
		lastErr = err
		if s.closing.Load() {
			return collabReceiverMeta{}, false, nil
		}
	}
	return collabReceiverMeta{}, false, lastErr
}

func (s *Session) readChildThreadMetadataOnce(ctx context.Context, providerThreadID string) (collabReceiverMeta, bool, error) {
	resp, err := s.sendRequest(ctx, "thread/read", map[string]any{
		"threadId":     providerThreadID,
		"includeTurns": false,
	})
	if err != nil {
		return collabReceiverMeta{}, false, err
	}
	var decoded struct {
		Thread struct {
			ID            string `json:"id"`
			AgentNickname string `json:"agentNickname"`
			AgentRole     string `json:"agentRole"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(resp, &decoded); err != nil {
		return collabReceiverMeta{}, false, fmt.Errorf("decode thread/read response: %w", err)
	}
	meta := collabReceiverMeta{
		ThreadID:      stringsx.FirstNonEmptyTrimmed(decoded.Thread.ID, providerThreadID),
		AgentNickname: strings.TrimSpace(decoded.Thread.AgentNickname),
		AgentRole:     strings.TrimSpace(decoded.Thread.AgentRole),
	}
	if meta.AgentNickname == "" && meta.AgentRole == "" {
		return meta, false, nil
	}
	return meta, true, nil
}

func (s *Session) rememberCollabReceiverMeta(meta collabReceiverMeta) {
	meta.ThreadID = strings.TrimSpace(meta.ThreadID)
	if meta.ThreadID == "" || (meta.AgentNickname == "" && meta.AgentRole == "") {
		return
	}
	s.mu.Lock()
	if s.agentMetaByThread == nil {
		s.agentMetaByThread = make(map[string]collabReceiverMeta)
	}
	existing := s.agentMetaByThread[meta.ThreadID]
	existing.ThreadID = meta.ThreadID
	if meta.AgentNickname != "" {
		existing.AgentNickname = meta.AgentNickname
	}
	if meta.AgentRole != "" {
		existing.AgentRole = meta.AgentRole
	}
	if meta.Model != "" {
		existing.Model = meta.Model
	}
	if meta.ReasoningEffort != "" {
		existing.ReasoningEffort = meta.ReasoningEffort
	}
	s.agentMetaByThread[meta.ThreadID] = existing
	s.mu.Unlock()
}
