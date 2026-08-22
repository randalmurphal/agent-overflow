package codex

import (
	"crypto/sha256"
	"encoding/json"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/provider"
)

const maxSubagentNotificationDedupEntries = 1024

type subagentNotificationDedupKey struct {
	ParentItemID string
	// Generation is the child's own turn count at the moment the delivery was
	// seen (childTurnGenerations, advanced by each child `turn/started`). It is
	// the same tiebreak triage applies to the persisted row id: without it a
	// child that legitimately answers IDENTICALLY twice — a `followup_task`
	// wakes it and it replies "Done." again — loses the second answer HERE,
	// before triage can separate them.
	//
	// Residual, deliberate: within ONE child turn, byte-identical duplicate
	// deliveries still collapse. A child emits one FINAL_ANSWER per turn, so a
	// second identical one inside the same generation is the retry this dedupe
	// exists for (the live raw stream and the rollout tail both carrying the
	// same record), not a second answer.
	Generation uint64
	MetaHash   [sha256.Size]byte
}

// sessionSideChannelNotifications routes the notifications that are
// signals to the App layer rather than transcript content. They are
// handled upstream of the general classifier so they never get normalised
// into a ProviderEvent the rest of the pipeline has to ignore.
//
// A map rather than a chain of `if method ==` so the opt-out derivation in
// notification_catalog.go can read the same keys the dispatcher branches
// on: adding a side channel cannot leave it opted out at initialize.
var sessionSideChannelNotifications = map[string]func(*Session, json.RawMessage){
	"mcpServer/oauthLogin/completed":  (*Session).dispatchMCPOAuthCompletion,
	"mcpServer/startupStatus/updated": (*Session).dispatchMCPStartupUpdate,
	// skills/changed carries no threadId at all, so it must be claimed
	// before the child-routing check below rather than after it.
	skillsChangedMethod: (*Session).dispatchSkillsChanged,
	// thread/reverted is a statement about the thread's durable history,
	// not transcript content, and it must be claimed before child routing
	// so a revert echo is never quarantined behind ownership mapping.
	threadRevertedMethod: (*Session).dispatchThreadReverted,
}

func (s *Session) dispatchNotification(method string, params json.RawMessage) {
	if handler, ok := sessionSideChannelNotifications[method]; ok {
		handler(s, params)
		return
	}
	providerThreadID := providerThreadIDFromParams(params)
	if s.isUnmappedForeignProviderThread(providerThreadID) {
		if s.deferChildWireEvent(providerThreadID, deferredChildWireEvent{
			Method: method,
			Params: params,
		}) {
			// MultiAgentV2 creates the child thread before it emits the parent-side
			// subAgentActivity ownership item. Keep routing fail-closed, but retain
			// display metadata only after the bounded quarantine accepts the event.
			if method == "thread/started" {
				s.rememberAgentMetaForProviderThread(providerThreadID, params)
			}
			return
		}
		s.warnChildRoutingOverflow(providerThreadID, method, nil)
		return
	}

	s.dispatchRoutableNotification(method, params, providerThreadID)
}

// dispatchRoutableNotification handles a root notification or a child
// notification whose spawn ownership is already known. The outer dispatcher
// is deliberately the only entry from raw JSON-RPC so foreign child threads
// can never fall through to parent turn state. providerThreadID is the
// caller's already-derived route id — recomputed here only when
// observeRawResponseItem may have rewritten params (rawResponseItem/completed
// is the sole method it rewrites, see raw_tool_calls.go).
func (s *Session) dispatchRoutableNotification(method string, params json.RawMessage, providerThreadID string) {
	params = s.observeRawResponseItem(method, params)
	if method == "rawResponseItem/completed" {
		providerThreadID = providerThreadIDFromParams(params)
	}
	mappedChildThreadIDs := s.observeSubAgentActivityOwnership(method, params)
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
		if method == "thread/tokenUsage/updated" {
			// The child's ONLY live progress signal. Re-emitted scoped to
			// the spawn card and returned on immediately, so it reaches
			// neither usageAcct.observe below (the parent's per-turn
			// accounting) nor the classifier's EventTokenUsage (the
			// parent's context meter). Removing it from
			// isChildSuppressedThreadNotification without this intercept
			// would leak a child's window onto the parent meter, which is
			// what that list was protecting against.
			s.emitChildTokenUsageProgress(providerThreadID, parentToolUseID, params)
			return
		}
		childStateNotification := isChildSuppressedThreadNotification(method) ||
			isChildSuppressedItemNotification(method, params)
		if childStateNotification {
			// Drop child-thread state notifications that would otherwise
			// overwrite the parent thread's projection. See the suppression
			// helpers in collab_agents.go for the rationale.
			return
		}
		if isChildTurnLifecycleNotification(method) {
			s.emitChildLifecycleEvents(method, params, parentToolUseID)
			return
		}
	}

	if method == "item/plan/delta" {
		s.appendPlanDelta(params)
	}
	if method == "thread/settings/updated" {
		// Parent-thread only (child-thread notifications returned above,
		// and reconcileThreadSettings re-checks the threadId). Codex is
		// the authority for what the thread is actually configured to do;
		// the requested turn config stays untouched so a pending
		// ApplyLiveUpdate cannot be undone by an echo of the last turn.
		s.reconcileThreadSettings(params)
	}
	if method == "thread/tokenUsage/updated" {
		// Parent-thread only (child-thread notifications returned above).
		// Folds the cumulative total into the per-turn usage accounting;
		// the context-meter EventTokenUsage classification below is
		// untouched.
		s.usageAcct.observe(params)
	}
	events, handled := s.classifyNotificationWithBufferedPlan(method, params)
	if !handled {
		s.warnUnclaimedNotification(method)
	}
	if method == "thread/queue/changed" {
		// Once AO participates in the provider queue, its OWN adds, edits,
		// deletes and every automatic dispatch raise this notification too, so
		// the classifier's unconditional "queued from outside Agent Overflow"
		// notice would accuse the user of their own message. thread_queue.go
		// replaces the event with an evidence-driven one.
		events = s.reconcileThreadQueueChange(events)
	}
	suppressSubagentNotificationCarrier := s.emitSubagentNotificationsFromUserCarrier(method, params, events)

	for i := range events {
		evt := &events[i]
		if parentToolUseID != "" && isUnsafeChildProjectionEvent(evt.Kind) {
			continue
		}
		if parentToolUseID != "" && evt.Kind == provider.EventError {
			evt.Meta = mergeMetaKeys(evt.Meta, map[string]any{"fatal": false})
		}
		if suppressSubagentNotificationCarrier && evt.Kind == provider.EventUserText {
			continue
		}
		s.observeStructuredOutputCandidate(evt)
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
		s.emitEvent(*evt)
	}
	if parentToolUseID != "" && method == "error" && !readTopLevelBool(params, "willRetry") {
		s.emitChildErrorStatusEvent(providerThreadID, parentToolUseID)
	}

	// V1 establishes ownership from collabAgentToolCall spawn completion in
	// prepareNotificationEvent. V2 establishes it above from
	// subAgentActivity. In both cases, drain only after the spawn row event has
	// reached triage so fast child output cannot race ahead of its parent card.
	mappedChildThreadIDs = append(mappedChildThreadIDs, collabSpawnReceiverThreadIDs(method, params)...)
	s.drainDeferredChildWireEvents(mappedChildThreadIDs...)
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
	rootThreadID := s.rootThreadID()
	if rootThreadID != "" && strings.TrimSpace(providerThreadID) != rootThreadID {
		return false
	}

	item := readNestedObject(params, "item")
	return s.emitResolvedSubagentNotificationsFromRawMessageItem(item)
}

func (s *Session) emitResolvedSubagentNotificationsFromRawMessageItem(item map[string]json.RawMessage) bool {
	if item == nil {
		return false
	}
	if readRawString(item, "type") == "agent_message" {
		notification, ok := extractSubagentCompletionFromRawAgentMessageItem(item)
		if !ok {
			return false
		}
		return s.emitResolvedSubagentNotifications([]subagentNotification{notification}, "", true)
	}
	if readRawString(item, "type") != "message" {
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
	if !s.claimSubagentNotification(parentItemID, s.childTurnGeneration(n.AgentPath), meta) {
		return false
	}
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventSubagentNotification,
		ThreadID:  s.threadID,
		ItemID:    parentItemID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
	return true
}

func (s *Session) claimSubagentNotification(parentItemID string, generation uint64, meta json.RawMessage) bool {
	key := subagentNotificationDedupKey{
		ParentItemID: parentItemID,
		Generation:   generation,
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
	s.enrichSubAgentActivitySpawnMetadata(evt, params)
	s.preserveWaitAgentReceiverTargets(evt)
	s.enrichCollabReceiverMetadata(evt)
}

func (s *Session) enrichSubAgentActivitySpawnMetadata(evt *provider.ProviderEvent, params json.RawMessage) {
	if evt == nil || evt.ItemType != "collab_agent" {
		return
	}
	model, reasoningEffort := s.collabProfileForThread(providerThreadIDFromParams(params))
	mutateEventMetaInput(evt, false, func(input map[string]json.RawMessage) {
		if readRawString(input, "tool") != "spawn_agent" {
			return
		}
		setRawStringIfMissing(input, "model", model)
		setRawStringIfMissing(input, "reasoningEffort", reasoningEffort)
		receiverThreadIDs := readRawStringArray(input, "receiverThreadIds")
		receiverMetadata := s.collabReceiverMetadataForThreads(receiverThreadIDs)
		if len(receiverMetadata) == 1 {
			setRawStringIfMissing(input, "newAgentNickname", receiverMetadata[0].AgentNickname)
			setRawStringIfMissing(input, "newAgentRole", receiverMetadata[0].AgentRole)
		}
		setRawStringIfMissing(input, "newAgentRole", "default")
		for _, receiverThreadID := range receiverThreadIDs {
			s.rememberCollabProfile(
				receiverThreadID,
				readRawString(input, "model"),
				readRawString(input, "reasoningEffort"),
			)
		}
	})
}

func (s *Session) updateNotificationState(evt *provider.ProviderEvent) {
	switch evt.Kind {
	case provider.EventTurnStart:
		s.usageAcct.onTurnStart()
		if evt.TurnID == "" {
			return
		}
		// Adopt the turn regardless of who started it: activeTurnID is what
		// Interrupt and Steer address, and an externally injected turn that
		// AO cannot stop is worse than one it cannot attribute. The return
		// value only decides the marker.
		//
		// turnAdoptionUndecided deliberately stamps NOTHING. A queue dispatch
		// cannot be attributed until its `userMessage` echo names the
		// `clientId`, and the echo is the row that actually needs protecting —
		// it is what a reader would otherwise take for something the person in
		// front of Agent Overflow typed. A turn-start marker guessed here and
		// contradicted one event later would be worse than no marker at all.
		switch s.adoptTurnStart(evt.TurnID) {
		case turnAdoptionExternal:
			logExternalTurnAdopted(s.threadID, evt.TurnID)
			stampExternalOrigin(evt)
		case turnAdoptionUndecided, turnAdoptionLocal:
		}
		s.bindPendingTurnSchema(evt.TurnID)
		s.mu.Lock()
		s.activeTurnID = evt.TurnID
		s.mu.Unlock()
	case provider.EventUserText:
		// The user-message echo of an injected turn. Without the marker it
		// persists identically to a message typed in this app's composer.
		//
		// This is also where an UNDECIDED queue dispatch gets its verdict: the
		// echo's `clientId` is the only thing that says which queued row the
		// app-server just started, and AO owns a row exactly when that id is
		// one it minted. resolveUserEchoOrigin records the answer, so every
		// later event of the same turn reads consistently.
		external, decided := s.resolveUserEchoOrigin(evt.TurnID, userEchoClientID(evt.Meta))
		if external {
			if decided {
				// Only a verdict reached HERE is new. A turn already adopted
				// at its `turn/started` logged there.
				logExternalTurnAdopted(s.threadID, evt.TurnID)
			}
			stampExternalOrigin(evt)
		}
	case provider.EventTurnComplete:
		evt.StructuredOutput = s.takeStructuredOutput(evt.TurnID)
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

func (s *Session) observeStructuredOutputCandidate(event *provider.ProviderEvent) {
	if event == nil || event.Kind != provider.EventContentBlockStop || event.ItemType != "agentMessage" || event.TurnID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, schemaed := s.schemaedTurnIDs[event.TurnID]; !schemaed {
		return
	}
	if s.structuredOutputByTurn == nil {
		s.structuredOutputByTurn = make(map[string]json.RawMessage)
	}
	payload := []byte(event.Content)
	if !json.Valid(payload) {
		delete(s.structuredOutputByTurn, event.TurnID)
		return
	}
	s.structuredOutputByTurn[event.TurnID] = json.RawMessage(payload)
}

func (s *Session) takeStructuredOutput(turnID string) json.RawMessage {
	if turnID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	payload := s.structuredOutputByTurn[turnID]
	delete(s.structuredOutputByTurn, turnID)
	delete(s.schemaedTurnIDs, turnID)
	return payload
}

func hasProviderEventKind(events []provider.ProviderEvent, kind provider.EventKind) bool {
	for i := range events {
		if events[i].Kind == kind {
			return true
		}
	}
	return false
}

// dispatchMCPStartupUpdate decodes `mcpServer/startupStatus/updated`,
// retains it as this session's startup state for the server, and
// forwards it to the registered handler. Per-thread-only emission; AO
// routes these into the app-level mcpstatus cache so the popup reflects
// the live provider state without a refetch, and reads the retained map
// back (MCPStartupStates) when listing the thread's MCP rows. Missing
// serverName is logged and dropped (defensive — Codex source guarantees
// the field is set).
//
// Retention deliberately runs before the handler lookup: whether an
// observer happens to be registered is an app-wiring detail, and a
// session that dropped its own startup history because of it would
// answer a later listing with an inference instead of the truth it saw.
//
// The retained map lives for the session and is keyed by a
// provider-supplied name, so both dimensions are bounded here at the
// chokepoint rather than by trusting the peer: a name beyond
// mcpStartupNameMaxBytes drops the whole update (no real server name
// approaches it), the error string is clamped to
// mcpStartupErrorMaxBytes before it touches the heap (display paths
// re-clamp harder), and a full map stops admitting NEW names — updates
// for already-retained servers still land, so a chatty peer cannot
// freeze real lifecycle state out.
func (s *Session) dispatchMCPStartupUpdate(params json.RawMessage) {
	var parsed struct {
		Name          string `json:"name"`
		Status        string `json:"status"`
		Error         string `json:"error,omitempty"`
		FailureReason string `json:"failureReason,omitempty"`
	}
	if err := json.Unmarshal(params, &parsed); err != nil {
		log.Printf("codex: decode mcpServer/startupStatus/updated: %v", err)
		return
	}
	if strings.TrimSpace(parsed.Name) == "" {
		log.Printf("codex: mcpServer/startupStatus/updated: missing name")
		return
	}
	if len(parsed.Name) > mcpStartupNameMaxBytes {
		log.Printf("codex: mcpServer/startupStatus/updated: dropping update for %d-byte server name", len(parsed.Name))
		return
	}
	update := MCPStartupUpdate{
		Name:          parsed.Name,
		State:         parsed.Status,
		Error:         truncateRuneSafe(parsed.Error, mcpStartupErrorMaxBytes),
		FailureReason: strings.TrimSpace(parsed.FailureReason),
	}

	s.mu.Lock()
	if s.mcpStartupStates == nil {
		s.mcpStartupStates = make(map[string]MCPStartupUpdate)
	}
	_, known := s.mcpStartupStates[update.Name]
	if known || len(s.mcpStartupStates) < mcpStartupStateMaxEntries {
		s.mcpStartupStates[update.Name] = update
	} else {
		log.Printf("codex: mcpServer/startupStatus/updated: retention full (%d servers), not retaining %q", mcpStartupStateMaxEntries, update.Name)
	}
	handler := s.mcpStartupUpdateHandler
	s.mu.Unlock()

	if handler == nil {
		return
	}
	handler(update)
}

const (
	// mcpStartupNameMaxBytes bounds a retained server name. Names come
	// off the provider wire; real config keys are tens of bytes.
	mcpStartupNameMaxBytes = 256
	// mcpStartupErrorMaxBytes clamps a retained error string's heap
	// footprint. User-facing paths re-clamp to 256B (sanitizeMCPError);
	// this only bounds what a session retains.
	mcpStartupErrorMaxBytes = 2048
	// mcpStartupStateMaxEntries caps the retained map. Real configs hold
	// a handful of servers; the cap exists so a buggy peer cannot grow
	// the session heap through invented names.
	mcpStartupStateMaxEntries = 128
)

// truncateRuneSafe cuts s at limit bytes, backing off to the previous
// rune boundary so the cut never manufactures U+FFFD.
func truncateRuneSafe(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

func (s *Session) dispatchMCPOAuthCompletion(params json.RawMessage) {
	s.mu.Lock()
	handler := s.mcpOAuthCompletedHandler
	s.mu.Unlock()
	if handler == nil {
		return
	}
	// Wire field is `name` (McpServerOauthLoginCompletedNotification,
	// camelCase serde) — decoding `serverName` here meant OAuth completion
	// never resolved in AO.
	var parsed struct {
		Name    string `json:"name"`
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(params, &parsed); err != nil {
		log.Printf("codex: decode mcpServer/oauthLogin/completed: %v", err)
		return
	}
	if parsed.Name == "" {
		log.Printf("codex: mcpServer/oauthLogin/completed: missing name")
		return
	}
	handler(parsed.Name, parsed.Success, parsed.Error)
}
