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
	ProfileKnown    bool   `json:"-"`
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
// drive the EventSubagentStatus emission the background projection needs.
// Routing for those lives in `isChildTurnLifecycleNotification` above
// (adding child notification handling in one helper but not the matching
// lifecycle / suppression helper is a subtle bug).
//
// `thread/tokenUsage/updated` is NO LONGER here either. It used to be,
// for the reason above — the child's window would overwrite the
// parent's meter — but dropping it outright also threw away the ONLY
// live signal Codex gives for a running child
// (docs/specs/agent-visibility.md: "Live progress ... for Codex, the
// child thread's `thread/tokenUsage/updated` (unsuppressed into a
// scoped progress event)"). It is now intercepted in
// `dispatchRoutableNotification` and re-emitted as a SCOPED
// `EventSubagentProgress` naming the spawn tool_use, which reaches
// neither the parent's usage accounting nor `EventTokenUsage`. The
// intercept returns before the classifier, so removing it from this
// list cannot make it fall through to the parent meter.
//
// `error` and unrecognised `thread/*` methods (e.g. a future
// `thread/error`) are ALSO intentionally not suppressed — subagent
// failures need to surface on the parent thread so the user knows
// something went wrong; silently dropping them would hide real errors.
func isChildSuppressedThreadNotification(method string) bool {
	switch method {
	case "thread/compacted",
		"thread/name/updated",
		"thread/started",
		"thread/settings/updated",
		"thread/status/changed",
		"thread/archived",
		"thread/unarchived",
		"thread/closed",
		"model/rerouted",
		"model/safetyBuffering/updated",
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
		// A child turn boundary is the only wire-typed thing that separates two
		// otherwise identical mailbox deliveries. Counted here, on the live
		// notification, rather than derived from the deliveries themselves —
		// which would be circular. See subagentNotificationDedupKey.
		s.advanceChildTurnGeneration(providerThreadID)
		s.recordChildTurnStarted(providerThreadID, readNestedString(params, "turn", "id"))
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
	s.recordChildTurnCompleted(providerThreadID, completed.Turn.ID)
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
//
// `EventSubagentProgress` is allowed by default (it is not listed) and
// that is deliberate: it is the child's OWN usage channel, scoped to the
// spawn tool_use and consumed as per-agent live state, never folded into
// the parent's meter. `EventTokenUsage` below stays forbidden, which is
// what keeps the two apart — a child's numbers reach the UI only under
// the agent's own id.
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

// childProgressEvent projects a CHILD thread's `thread/tokenUsage/updated`
// into the provider-neutral live-progress channel
// (docs/specs/agent-visibility.md). Codex reports no tool count and no
// elapsed time for a child, so tokens are the whole tick — which is why
// SubagentProgressMeta is merged rather than replaced downstream.
//
//   - ItemID is the spawn tool_use, so the tick lands on the live background
//     projection for the child.
//   - ParentToolUseID is the SPAWN's own parent when the wire lets us
//     resolve one, so a nested spawn's tick is attributable without a
//     row lookup. Codex publishes no direct edge; the canonical agent
//     path is the one thing that encodes the chain
//     (`/root/reviewer/deep` → `/root/reviewer`), so a build that sends
//     no path — or a depth-1 child, whose parent is the root thread and
//     owns no launch — resolves to empty, which is the honest answer.
//   - TaskID is the child provider thread id: the provider's own name
//     for the running agent.
//
// nil when the notification named no child thread, no spawn, or carried
// no usable token figures.
func (s *Session) childProgressEvent(providerThreadID, parentToolUseID string, params json.RawMessage) *provider.ProviderEvent {
	providerThreadID = strings.TrimSpace(providerThreadID)
	parentToolUseID = strings.TrimSpace(parentToolUseID)
	if providerThreadID == "" || parentToolUseID == "" {
		return nil
	}
	spend, ok := childAgentTokenSpend(params)
	if !ok {
		return nil
	}
	meta, err := json.Marshal(provider.SubagentProgressMeta{
		TaskID:      providerThreadID,
		TotalTokens: spend,
	})
	if err != nil {
		log.Printf("codex: marshal child progress for %s: %v", providerThreadID, err)
		return nil
	}
	return &provider.ProviderEvent{
		Kind:            provider.EventSubagentProgress,
		ThreadID:        s.threadID,
		ItemID:          parentToolUseID,
		ParentToolUseID: s.spawnLaunchParentToolUse(providerThreadID),
		Meta:            meta,
		Timestamp:       time.Now(),
	}
}

// emitChildTokenUsageProgress is the dispatch-side wrapper: a child's
// token usage becomes a scoped progress tick or nothing at all. It never
// reaches usageAcct.observe or the notification classifier — the caller
// returns immediately after — so the parent's own meter and its
// EventTokenUsage stream are untouched (ADR-002).
func (s *Session) emitChildTokenUsageProgress(providerThreadID, parentToolUseID string, params json.RawMessage) {
	event := s.childProgressEvent(providerThreadID, parentToolUseID, params)
	if event == nil {
		return
	}
	s.emitEvent(*event)
}

// spawnLaunchParentToolUse resolves the spawn tool_use's OWN parent for a
// nested child: the launch of the agent that spawned this child's
// spawner. See childProgressEvent for why the canonical path is the only
// available edge. Empty for a depth-1 child and for any build that sends
// no path.
func (s *Session) spawnLaunchParentToolUse(providerThreadID string) string {
	s.mu.Lock()
	agentPath := s.collab.agentPathByThread[strings.TrimSpace(providerThreadID)]
	s.mu.Unlock()
	cut := strings.LastIndex(agentPath, "/")
	if cut <= 0 {
		return ""
	}
	return s.parentToolUseForAgentPath(agentPath[:cut])
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
	return s.collab.childParentByThread[providerThreadID]
}

// childTurnGenerationKeyLocked normalises the two names ONE child answers to.
// Live child lifecycle names the provider thread id; a mailbox delivery names
// the canonical agent path (`/root/reviewer`) except on older unnamed-agent
// builds, where it is the thread id again. Both must reach the same counter.
func (s *Session) childTurnGenerationKeyLocked(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if agentPath := s.collab.agentPathByThread[id]; agentPath != "" {
		return agentPath
	}
	return id
}

// advanceChildTurnGeneration counts one child turn boundary.
//
// One entry per child this session currently owns: the entry is dropped by the
// same ownership teardown that drops the child's parent and path mappings
// (deleteParentToolUseForProviderThread, which `close_agent` drives), and the
// whole map goes with the session on close. Without that teardown a long v1
// session that spawns and closes uniquely named children would grow this map
// for the life of the process, which is exactly what the other collab maps are
// cleaned to avoid.
func (s *Session) advanceChildTurnGeneration(providerThreadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.childTurnGenerationKeyLocked(providerThreadID)
	if key == "" {
		return
	}
	if s.collab.childTurnGenerations == nil {
		s.collab.childTurnGenerations = make(map[string]uint64)
	}
	s.collab.childTurnGenerations[key]++
}

// childTurnGeneration reports the child turn a delivery belongs to. Zero is a
// legal answer — a delivery whose child this session never watched start.
func (s *Session) childTurnGeneration(agentPath string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.childTurnGenerationKeyLocked(agentPath)
	if key == "" {
		return 0
	}
	return s.collab.childTurnGenerations[key]
}

func (s *Session) parentToolUseForAgentPath(agentPath string) string {
	if agentPath == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.collab.childParentByAgentPath[agentPath]
}

func (s *Session) providerThreadForAgentPath(agentPath, parentToolUseID string) string {
	agentPath = strings.TrimSpace(agentPath)
	parentToolUseID = strings.TrimSpace(parentToolUseID)
	if agentPath == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	providerThreadID := s.collab.childThreadByAgentPath[agentPath]
	if parentToolUseID == "" || s.collab.childParentByThread[providerThreadID] == parentToolUseID {
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
// A successful LIVE registration is also the typed mid-session arming signal
// for the rollout tail. It is the one funnel both spawn shapes reach — V1's
// completed `collabAgentToolCall` `spawn_agent` and V2's completed
// `subAgentActivity` `kind:"started"` — so a resumed session that was not armed
// at resume time (nothing was outstanding then) starts tailing the moment it
// acquires a child whose mailbox delivery it could otherwise never see. The
// HISTORICAL registration deliberately does not arm: replaying a spawn out of
// the resume response proves nothing about work still in flight, which is what
// Config.ResumeHasUnresolvedSubagents answers. Arming after the mutation, not
// inside it, keeps the tail's own lock acquisition off a path that already
// holds mu.
func (s *Session) registerChildOwnership(sourceThreadID, childThreadID, agentPath, parentToolUseID string) bool {
	registered := s.registerChildOwnershipWithSource(sourceThreadID, childThreadID, agentPath, parentToolUseID, false)
	if registered {
		s.armRolloutSubagentNotificationTail("spawned child observed on the wire")
	}
	return registered
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
	if childThreadID == strings.TrimSpace(s.rootThreadID()) || (sourceThreadID != "" && childThreadID == sourceThreadID) {
		log.Printf("codex: reject self-referential child ownership source=%q child=%q item=%q", sourceThreadID, childThreadID, parentToolUseID)
		return false
	}
	if s.collab.childParentByThread == nil {
		s.collab.childParentByThread = make(map[string]string)
	}
	if existing := s.collab.childParentByThread[childThreadID]; existing != "" && existing != parentToolUseID {
		log.Printf("codex: reject conflicting child thread ownership %q: existing=%s incoming=%s", childThreadID, existing, parentToolUseID)
		return false
	}
	if agentPath != "" {
		if s.collab.childParentByAgentPath == nil {
			s.collab.childParentByAgentPath = make(map[string]string)
		}
		if s.collab.agentPathByThread == nil {
			s.collab.agentPathByThread = make(map[string]string)
		}
		if s.collab.childThreadByAgentPath == nil {
			s.collab.childThreadByAgentPath = make(map[string]string)
		}
		if s.collab.childPathOwnerLive == nil {
			s.collab.childPathOwnerLive = make(map[string]bool)
		}
		if existing := s.collab.agentPathByThread[childThreadID]; existing != "" && existing != agentPath {
			log.Printf("codex: reject conflicting canonical path for child %q: existing=%q incoming=%q", childThreadID, existing, agentPath)
			return false
		}
		if currentThreadID := s.collab.childThreadByAgentPath[agentPath]; currentThreadID != "" && currentThreadID != childThreadID {
			if s.collab.agentPathByThread[childThreadID] == agentPath {
				log.Printf("codex: reject replay from superseded child path ownership %q: current=%s replay=%s", agentPath, currentThreadID, childThreadID)
				return false
			}
			if !fromHistory && s.collab.childPathOwnerLive[agentPath] {
				log.Printf("codex: reject conflicting live child path ownership %q: current=%s incoming=%s", agentPath, currentThreadID, childThreadID)
				return false
			}
		}
	}
	s.collab.childParentByThread[childThreadID] = parentToolUseID
	if s.collab.childRuntimeByThread == nil {
		s.collab.childRuntimeByThread = make(map[string]childRuntimeState)
	}
	if runtime, exists := s.collab.childRuntimeByThread[childThreadID]; !exists || (!fromHistory && runtime.phase != childRuntimeRunning && runtime.phase != childRuntimeStopping) {
		phase := childRuntimeInactive
		if !fromHistory {
			phase = childRuntimePending
		}
		s.collab.childRuntimeByThread[childThreadID] = childRuntimeState{phase: phase}
	}
	if agentPath != "" {
		s.collab.agentPathByThread[childThreadID] = agentPath
		currentThreadID := s.collab.childThreadByAgentPath[agentPath]
		preserveLiveOwner := fromHistory && currentThreadID != "" && s.collab.childPathOwnerLive[agentPath]
		if !preserveLiveOwner {
			s.collab.childParentByAgentPath[agentPath] = parentToolUseID
			s.collab.childThreadByAgentPath[agentPath] = childThreadID
			s.collab.childPathOwnerLive[agentPath] = !fromHistory
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
	launchMeta := collabLaunchMeta{
		AgentPath:         activity.AgentPath,
		ReceiverThreadIDs: []string{activity.AgentThreadID},
	}
	s.scheduleCollabProfileRead(activity.AgentThreadID, activity.ItemID, launchMeta)
	return []string{activity.AgentThreadID}
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
	// Resolved BEFORE agentPathByThread is torn down: the counter is keyed the
	// way childTurnGeneration reads it (canonical agent path where there is
	// one, the thread id otherwise), and deleting the mapping first would leave
	// the entry unreachable and permanent.
	generationKey := s.childTurnGenerationKeyLocked(providerThreadID)
	if agentPath := s.collab.agentPathByThread[providerThreadID]; agentPath != "" {
		if s.collab.childThreadByAgentPath[agentPath] == providerThreadID {
			delete(s.collab.childParentByAgentPath, agentPath)
			delete(s.collab.childThreadByAgentPath, agentPath)
			delete(s.collab.childPathOwnerLive, agentPath)
		}
		delete(s.collab.agentPathByThread, providerThreadID)
	}
	delete(s.collab.childParentByThread, providerThreadID)
	delete(s.collab.childRuntimeByThread, providerThreadID)
	delete(s.collab.agentMetaByThread, providerThreadID)
	delete(s.collab.profileReadsByThread, providerThreadID)
	// The generation counter goes with the ownership it counted. Zero — what a
	// later delivery for this child would now read — is already the documented
	// legal answer for a child this session never watched start, and after the
	// teardown above there is no parent card left for such a delivery to reach
	// anyway.
	if generationKey != "" {
		delete(s.collab.childTurnGenerations, generationKey)
	}
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
	if meta.ProfileKnown {
		input["model"] = strings.TrimSpace(meta.Model)
		input["reasoningEffort"] = strings.TrimSpace(meta.ReasoningEffort)
	} else {
		if strings.TrimSpace(launchMeta.Model) != "" {
			input["model"] = strings.TrimSpace(launchMeta.Model)
		}
		if strings.TrimSpace(launchMeta.ReasoningEffort) != "" {
			input["reasoningEffort"] = strings.TrimSpace(launchMeta.ReasoningEffort)
		}
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
	if s.collab.childParentByThread[providerThreadID] == "" {
		delete(s.collab.agentMetaByThread, providerThreadID)
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
		evt.Kind == provider.EventSubagentStatus {
		kind := readRawString(item, "kind")
		if kind != "interrupted" && kind != "completed" {
			return
		}
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
		if s.rawCalls.waitReceiverIDsByCall == nil {
			s.rawCalls.waitReceiverIDsByCall = make(map[string][]string)
		}
		s.rawCalls.waitReceiverIDsByCall[itemID] = append([]string(nil), receiverThreadIDs...)
		s.mu.Unlock()
	case provider.EventToolComplete:
		s.mu.Lock()
		receiverThreadIDs := append([]string(nil), s.rawCalls.waitReceiverIDsByCall[itemID]...)
		delete(s.rawCalls.waitReceiverIDsByCall, itemID)
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
	if len(s.collab.agentMetaByThread) == 0 {
		return nil
	}
	agents := make([]collabReceiverMeta, 0, len(receiverThreadIDs))
	hasLabel := false
	for _, threadID := range receiverThreadIDs {
		threadID = strings.TrimSpace(threadID)
		if threadID == "" {
			continue
		}
		meta := s.collab.agentMetaByThread[threadID]
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
	meta.AgentNickname = strings.TrimSpace(meta.AgentNickname)
	meta.AgentRole = strings.TrimSpace(meta.AgentRole)
	meta.Model = strings.TrimSpace(meta.Model)
	meta.ReasoningEffort = strings.TrimSpace(meta.ReasoningEffort)
	if meta.ThreadID == "" || (meta.AgentNickname == "" && meta.AgentRole == "" && !meta.ProfileKnown) {
		return
	}
	s.mu.Lock()
	if s.collab.agentMetaByThread == nil {
		s.collab.agentMetaByThread = make(map[string]collabReceiverMeta)
	}
	existing := s.collab.agentMetaByThread[meta.ThreadID]
	existing.ThreadID = meta.ThreadID
	if meta.AgentNickname != "" {
		existing.AgentNickname = meta.AgentNickname
	}
	if meta.AgentRole != "" {
		existing.AgentRole = meta.AgentRole
	}
	if meta.ProfileKnown {
		existing.Model = meta.Model
		existing.ReasoningEffort = meta.ReasoningEffort
		existing.ProfileKnown = true
	}
	s.collab.agentMetaByThread[meta.ThreadID] = existing
	s.mu.Unlock()
}
