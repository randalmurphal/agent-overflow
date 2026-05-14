package codex

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider"
)

const maxRawToolCallRecords = 512

type rawToolCall struct {
	CallID    string
	Name      string
	ProcessID string
	AgentType string
	Prompt    string
	Targets   []string
}

func (s *Session) observeRawResponseItem(method string, params json.RawMessage) json.RawMessage {
	if method != "rawResponseItem/completed" {
		return params
	}
	item := readNestedObject(params, "item")
	switch readRawString(item, "type") {
	case "function_call":
		s.rememberRawToolCall(item)
	case "function_call_output":
		return s.enrichRawToolCallOutput(params, item)
	}
	return params
}

func (s *Session) rememberRawToolCall(item map[string]json.RawMessage) {
	callID := strings.TrimSpace(firstNonEmptyString(readRawString(item, "call_id"), readRawString(item, "id")))
	name := strings.TrimSpace(readRawString(item, "name"))
	if callID == "" || name == "" {
		return
	}
	args, _ := decodeFunctionArguments(readRawString(item, "arguments"))
	call := rawToolCall{
		CallID: callID,
		Name:   name,
	}
	switch name {
	case "write_stdin":
		call.ProcessID = readFlexibleString(args, "session_id")
	case "spawn_agent":
		call.AgentType = readFlexibleString(args, "agent_type")
		call.Prompt = rawSpawnAgentPrompt(args)
	case "wait_agent":
		call.Targets = readFlexibleStringArray(args, "targets")
	default:
		return
	}
	s.mu.Lock()
	if s.rawToolCallsByID == nil {
		s.rawToolCallsByID = make(map[string]rawToolCall)
	}
	s.rawToolCallsByID[callID] = call
	s.pruneRawToolCallsLocked(callID)
	s.mu.Unlock()
}

func (s *Session) enrichRawToolCallOutput(params json.RawMessage, item map[string]json.RawMessage) json.RawMessage {
	callID := strings.TrimSpace(readRawString(item, "call_id"))
	if callID == "" {
		return params
	}
	s.mu.Lock()
	call := s.rawToolCallsByID[callID]
	s.mu.Unlock()
	if call.CallID == "" {
		return params
	}
	defer s.deleteRawToolCall(callID)
	if call.Name == "spawn_agent" {
		s.handleRawSpawnAgentOutput(call, item)
	}
	extras := map[string]any{
		"rawToolName": call.Name,
	}
	if call.ProcessID != "" {
		extras["processId"] = call.ProcessID
	}
	if call.Name == "write_stdin" {
		if result := rawWriteStdinWaitResult(readRawString(item, "output")); result != "" {
			extras["waitResult"] = result
		}
	}
	return mergeRawResponseItemExtras(params, extras)
}

func (s *Session) enrichRawToolCallMetadata(evt *provider.ProviderEvent) {
	if evt == nil {
		return
	}
	switch evt.ItemType {
	case "collab_agent", "wait_agent":
	default:
		return
	}
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		return
	}
	s.mu.Lock()
	call := s.rawToolCallsByID[itemID]
	s.mu.Unlock()
	if call.CallID == "" {
		return
	}

	mutateEventMetaInput(evt, true, func(input map[string]json.RawMessage) {
		switch call.Name {
		case "spawn_agent":
			setRawStringIfMissing(input, "tool", "spawn_agent")
			setRawStringIfMissing(input, "prompt", call.Prompt)
			setRawStringIfMissing(input, "newAgentRole", call.AgentType)
		case "wait_agent":
			setRawStringIfMissing(input, "tool", "wait_agent")
			setRawStringArray(input, "requestedReceiverThreadIds", call.Targets)
		}
	})
}

func (s *Session) pruneRawToolCallsLocked(keepCallID string) {
	if len(s.rawToolCallsByID) <= maxRawToolCallRecords {
		return
	}
	for callID := range s.rawToolCallsByID {
		if callID == keepCallID {
			continue
		}
		delete(s.rawToolCallsByID, callID)
		if len(s.rawToolCallsByID) <= maxRawToolCallRecords {
			return
		}
	}
}

func (s *Session) deleteRawToolCall(callID string) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return
	}
	s.mu.Lock()
	delete(s.rawToolCallsByID, callID)
	s.mu.Unlock()
}

func (s *Session) handleRawSpawnAgentOutput(call rawToolCall, item map[string]json.RawMessage) {
	var output struct {
		AgentID  string `json:"agent_id"`
		Nickname string `json:"nickname"`
	}
	if json.Unmarshal([]byte(readRawString(item, "output")), &output) != nil {
		return
	}
	output.AgentID = strings.TrimSpace(output.AgentID)
	if output.AgentID == "" {
		return
	}
	meta := collabReceiverMeta{
		ThreadID:      output.AgentID,
		AgentNickname: strings.TrimSpace(output.Nickname),
		AgentRole:     strings.TrimSpace(call.AgentType),
	}
	if meta.AgentNickname == "" && meta.AgentRole == "" {
		return
	}
	s.rememberCollabReceiverMeta(meta)
	s.emitRawSpawnAgentMetaUpdate(call, meta)
}

func (s *Session) emitRawSpawnAgentMetaUpdate(call rawToolCall, meta collabReceiverMeta) {
	if s.onEvent == nil || strings.TrimSpace(call.CallID) == "" {
		return
	}
	launchMeta := collabLaunchMeta{
		Prompt:            call.Prompt,
		ReceiverThreadIDs: []string{meta.ThreadID},
	}
	if meta.AgentRole == "" {
		meta.AgentRole = strings.TrimSpace(call.AgentType)
	}
	s.emitCollabReceiverMetaUpdate(call.CallID, meta, launchMeta)
}

func rawSpawnAgentPrompt(args map[string]json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	if message := readFlexibleString(args, "message"); strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	rawItems, ok := args["items"]
	if !ok {
		return ""
	}
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(rawItems, &items) != nil {
		return ""
	}
	for _, item := range items {
		if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
			return strings.TrimSpace(item.Text)
		}
	}
	return ""
}

func rawWriteStdinWaitResult(output string) string {
	header := output
	if idx := strings.Index(output, "\nOutput:"); idx >= 0 {
		header = output[:idx]
	}
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Process running with session ID ") {
			return provider.TerminalWaitResultRunning
		}
		if strings.HasPrefix(line, "Process exited with code ") {
			return provider.TerminalWaitResultExited
		}
	}
	return ""
}

func classifyRawResponseNotification(threadID, method string, params json.RawMessage, now time.Time) ([]provider.ProviderEvent, bool) {
	switch method {
	case "rawResponseItem/completed":
		return classifyRawResponseItemCompleted(threadID, params, now), true
	}
	return nil, false
}

// buildTerminalInteractionMeta packages the notification fields triage
// needs to persist the "waited" row. Meta preserves the raw `stdin`
// (so the frontend / future phases can differentiate empty-poll from
// real input) alongside the PTY process_id for debugging.
func buildTerminalInteractionMeta(params json.RawMessage) json.RawMessage {
	encoded, err := json.Marshal(map[string]any{
		"process_id": readTopLevelString(params, "processId"),
		"stdin":      readTopLevelString(params, "stdin"),
	})
	if err != nil {
		return params
	}
	return encoded
}

func classifyRawResponseItemCompleted(threadID string, params json.RawMessage, now time.Time) []provider.ProviderEvent {
	item := readNestedObject(params, "item")
	itemType := readRawString(item, "type")
	toolName := firstNonEmptyString(readRawString(item, "name"), readRawString(item, "rawToolName"))
	if toolName != "write_stdin" {
		return nil
	}

	processID := readRawString(item, "processId")
	switch itemType {
	case "function_call":
		args, ok := decodeFunctionArguments(readRawString(item, "arguments"))
		if !ok || readFlexibleString(args, "chars") != "" {
			return nil
		}
		processID = readFlexibleString(args, "session_id")
	case "function_call_output":
		// Raw outputs are not a transcript lifecycle source. Codex emits the
		// canonical typed TerminalInteraction after write_stdin returns, and
		// command item/completed owns final output/status.
		return nil
	default:
		return nil
	}

	if processID == "" {
		return nil
	}

	metaMap := map[string]any{
		"process_id": processID,
		"stdin":      "",
		"source":     "rawResponseItem/function_call",
	}
	if callID := readRawString(item, "call_id"); callID != "" {
		metaMap["tool_call_id"] = callID
	}
	meta, err := json.Marshal(metaMap)
	if err != nil {
		meta = json.RawMessage(`{"stdin":""}`)
	}

	return []provider.ProviderEvent{{
		Kind:      provider.EventTerminalInteraction,
		ThreadID:  threadID,
		TurnID:    readTopLevelString(params, "turnId"),
		ItemID:    firstNonEmptyString(readRawString(item, "call_id"), readRawString(item, "id")),
		Content:   "",
		Meta:      meta,
		Timestamp: now,
	}}
}

type codexProviderEventLogRedactor struct {
	mu                sync.Mutex
	writeStdinCallIDs map[string]struct{}
}

func newCodexProviderEventLogRedactor() provider.EventLogRedactor {
	redactor := &codexProviderEventLogRedactor{
		writeStdinCallIDs: make(map[string]struct{}),
	}
	return redactor.redact
}

func (r *codexProviderEventLogRedactor) redact(direction string, data []byte) []byte {
	if direction != "in" || len(data) == 0 {
		return data
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || readRawString(root, "method") != "rawResponseItem/completed" {
		return data
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(root["params"], &params) != nil {
		return data
	}
	var item map[string]json.RawMessage
	if json.Unmarshal(params["item"], &item) != nil {
		return data
	}

	changed := false
	itemType := readRawString(item, "type")
	callID := strings.TrimSpace(readRawString(item, "call_id"))
	switch itemType {
	case "function_call":
		if readRawString(item, "name") == "write_stdin" {
			r.rememberWriteStdinCallID(callID)
			if redactWriteStdinArguments(item) {
				changed = true
			}
		}
	case "function_call_output":
		if r.takeWriteStdinCallID(callID) {
			item["output"] = json.RawMessage(`"[redacted]"`)
			changed = true
		}
	}
	if !changed {
		return data
	}
	redacted, err := encodeRedactedRawResponseLine(root, params, item)
	if err != nil {
		return data
	}
	return redacted
}

func (r *codexProviderEventLogRedactor) rememberWriteStdinCallID(callID string) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return
	}
	r.mu.Lock()
	r.writeStdinCallIDs[callID] = struct{}{}
	for existing := range r.writeStdinCallIDs {
		if len(r.writeStdinCallIDs) <= maxRawToolCallRecords {
			break
		}
		if existing != callID {
			delete(r.writeStdinCallIDs, existing)
		}
	}
	r.mu.Unlock()
}

func (r *codexProviderEventLogRedactor) takeWriteStdinCallID(callID string) bool {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.writeStdinCallIDs[callID]; !ok {
		return false
	}
	delete(r.writeStdinCallIDs, callID)
	return true
}

func redactWriteStdinArguments(item map[string]json.RawMessage) bool {
	args, ok := decodeFunctionArguments(readRawString(item, "arguments"))
	if !ok {
		return false
	}
	if _, ok := args["chars"]; !ok {
		return false
	}
	encodedRedaction, err := json.Marshal("[redacted]")
	if err != nil {
		return false
	}
	args["chars"] = encodedRedaction
	encodedArgs, err := json.Marshal(args)
	if err != nil {
		return false
	}
	encodedArgsString, err := json.Marshal(string(encodedArgs))
	if err != nil {
		return false
	}
	item["arguments"] = encodedArgsString
	return true
}

func encodeRedactedRawResponseLine(root, params, item map[string]json.RawMessage) ([]byte, error) {
	encodedItem, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}
	params["item"] = encodedItem
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	root["params"] = encodedParams
	return json.Marshal(root)
}

func mergeRawResponseItemExtras(params json.RawMessage, extras map[string]any) json.RawMessage {
	var root map[string]json.RawMessage
	if json.Unmarshal(params, &root) != nil {
		return params
	}
	itemRaw, ok := root["item"]
	if !ok {
		return params
	}
	var item map[string]any
	if json.Unmarshal(itemRaw, &item) != nil || item == nil {
		return params
	}
	for key, value := range extras {
		item[key] = value
	}
	encodedItem, err := json.Marshal(item)
	if err != nil {
		return params
	}
	root["item"] = encodedItem
	out, err := json.Marshal(root)
	if err != nil {
		return params
	}
	return out
}

func readFlexibleStringArray(m map[string]json.RawMessage, key string) []string {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var stringsOnly []string
	if json.Unmarshal(raw, &stringsOnly) == nil {
		out := make([]string, 0, len(stringsOnly))
		for _, value := range stringsOnly {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	}
	var mixed []json.RawMessage
	if json.Unmarshal(raw, &mixed) != nil {
		return nil
	}
	out := make([]string, 0, len(mixed))
	for _, rawValue := range mixed {
		value := ""
		var s string
		if json.Unmarshal(rawValue, &s) == nil {
			value = s
		} else {
			var num json.Number
			if json.Unmarshal(rawValue, &num) == nil {
				value = num.String()
			}
		}
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
