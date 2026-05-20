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
	ThreadID      string `json:"threadId"`
	AgentNickname string `json:"agentNickname,omitempty"`
	AgentRole     string `json:"agentRole,omitempty"`
}

type collabLaunchMeta struct {
	Prompt            string
	Model             string
	ReasoningEffort   string
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
// below). Coupled-pair with `isChildSuppressedThreadNotification`: these
// two functions partition the child-thread notifications we care about
// — anything in either set is intercepted before reaching the parent
// dispatch in session_notifications.go.
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
// (coupled-pair: adding to one without the other is a subtle bug).
//
// `error` and unrecognised `thread/*` methods (e.g. a future
// `thread/error`) are ALSO intentionally not suppressed — subagent
// failures need to surface on the parent thread so the user knows
// something went wrong; silently dropping them would hide real errors.
func isChildSuppressedThreadNotification(method string) bool {
	switch method {
	case "thread/tokenUsage/updated",
		"thread/compacted",
		"thread/name/updated":
		return true
	default:
		return false
	}
}

func (s *Session) childLifecycleEvent(method string, params json.RawMessage, parentToolUseID string) *provider.ProviderEvent {
	if method != "turn/completed" {
		return nil
	}
	providerThreadID := providerThreadIDFromParams(params)
	if providerThreadID == "" {
		return nil
	}
	status := codexSubagentStatusFromTurnCompleted(params)
	if status == "" {
		return nil
	}
	meta, err := json.Marshal(map[string]string{
		"agent_path": providerThreadID,
		"status":     status,
	})
	if err != nil {
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

func codexSubagentStatusFromTurnCompleted(params json.RawMessage) string {
	wire := decodeTurnCompletedParams(params)
	switch wire.Turn.Status {
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

func (s *Session) setParentToolUseForProviderThread(providerThreadID, parentToolUseID string) {
	if providerThreadID == "" || parentToolUseID == "" {
		return
	}
	s.mu.Lock()
	if s.childParentByThread == nil {
		s.childParentByThread = make(map[string]string)
	}
	s.childParentByThread[providerThreadID] = parentToolUseID
	s.mu.Unlock()
}

func (s *Session) deleteParentToolUseForProviderThread(providerThreadID string) {
	if providerThreadID == "" {
		return
	}
	s.mu.Lock()
	if agentPath := s.agentPathByThread[providerThreadID]; agentPath != "" {
		delete(s.childParentByAgentPath, agentPath)
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
	s.onEvent(provider.ProviderEvent{
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
	s.mu.Lock()
	if s.childParentByAgentPath == nil {
		s.childParentByAgentPath = make(map[string]string)
	}
	if s.agentPathByThread == nil {
		s.agentPathByThread = make(map[string]string)
	}
	s.childParentByAgentPath[agentPath] = parentToolUseID
	s.agentPathByThread[providerThreadID] = agentPath
	s.mu.Unlock()
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
	s.mu.Lock()
	if s.agentMetaByThread == nil {
		s.agentMetaByThread = make(map[string]collabReceiverMeta)
	}
	s.agentMetaByThread[providerThreadID] = meta
	s.mu.Unlock()
}

func providerThreadIDFromParams(params json.RawMessage) string {
	if id := readTopLevelString(params, "threadId"); id != "" {
		return id
	}
	return readNestedString(params, "thread", "id")
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
			s.setParentToolUseForProviderThread(receiverThreadID, itemID)
			go s.readChildThreadMetadata(receiverThreadID, itemID, launchMeta)
			go s.resumeChildThread(receiverThreadID)
		}
	case "close_agent":
		for _, receiverThreadID := range receiverThreadIDs {
			s.deleteParentToolUseForProviderThread(receiverThreadID)
		}
	}
}

func (s *Session) maybeRewriteCollabControlItemID(evt *provider.ProviderEvent, params json.RawMessage) {
	if evt == nil || evt.ItemType != "collab_agent" {
		return
	}
	item := readNestedObject(params, "item")
	if item == nil || readRawString(item, "type") != "collabAgentToolCall" {
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

func (s *Session) resumeChildThread(providerThreadID string) {
	if s.proc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _ = s.sendRequest(ctx, "thread/resume", map[string]any{
		"threadId": providerThreadID,
	})
}

func (s *Session) readChildThreadMetadata(providerThreadID, parentToolUseID string, launchMeta collabLaunchMeta) {
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
	defer s.releaseCollabMetadataRead()

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
	s.agentMetaByThread[meta.ThreadID] = meta
	s.mu.Unlock()
}
