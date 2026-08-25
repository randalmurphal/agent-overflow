package codex

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider"
)

const maxRawToolCallRecords = 512

const (
	terminalWaitResultRunning = "running"
	terminalWaitResultExited  = "exited"
)

type rawToolCall struct {
	CallID          string
	Name            string
	ProcessID       string
	Command         string
	AgentType       string
	Model           string
	ReasoningEffort string
	Prompt          string
	Target          string
	Targets         []string
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
	case "exec_command":
		call.Command = readFlexibleString(args, "cmd")
		if call.Command == "" {
			call.Command = readFlexibleString(args, "command")
		}
	case "write_stdin":
		call.ProcessID = readFlexibleString(args, "session_id")
	case "spawn_agent":
		isMultiAgentV2 := strings.TrimSpace(readRawString(item, "namespace")) == "collaboration" ||
			readFlexibleString(args, "task_name") != ""
		call.AgentType = readFlexibleString(args, "agent_type")
		call.Model = readFlexibleString(args, "model")
		call.ReasoningEffort = readFlexibleString(args, "reasoning_effort")
		if !isMultiAgentV2 {
			call.Prompt = rawSpawnAgentPrompt(args)
		}
	case "wait_agent":
		call.Targets = readFlexibleStringArray(args, "targets")
	case "send_message", "followup_task":
		call.Target = readFlexibleString(args, "target")
	case "interrupt_agent":
		call.Target = readFlexibleString(args, "target")
	default:
		return
	}
	s.mu.Lock()
	if s.rawCalls.byID == nil {
		s.rawCalls.byID = make(map[string]rawToolCall)
	}
	s.rawCalls.byID[callID] = call
	s.pruneRawToolCallsLocked(callID)
	s.mu.Unlock()
}

func (s *Session) enrichRawToolCallOutput(params json.RawMessage, item map[string]json.RawMessage) json.RawMessage {
	callID := strings.TrimSpace(readRawString(item, "call_id"))
	if callID == "" {
		return params
	}
	s.mu.Lock()
	call := s.rawCalls.byID[callID]
	s.mu.Unlock()
	if call.CallID == "" {
		return params
	}
	defer s.deleteRawToolCall(callID)
	switch call.Name {
	case "exec_command":
		s.handleRawExecCommandOutput(call, params, item)
	case "spawn_agent":
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

func (s *Session) handleRawExecCommandOutput(call rawToolCall, params json.RawMessage, item map[string]json.RawMessage) {
	if s.onEvent == nil {
		return
	}
	result, processID := rawExecCommandResult(readRawString(item, "output"))
	if result == "" {
		return
	}
	if processID == "" {
		processID = strings.TrimSpace(call.ProcessID)
	}
	meta := map[string]any{
		"result": result,
	}
	if processID != "" {
		meta["process_id"] = processID
	}
	if call.Command != "" {
		meta["command"] = call.Command
		meta["input"] = map[string]any{"command": call.Command}
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		encoded = []byte("{}")
	}
	providerThreadID := providerThreadIDFromParams(params)
	parentToolUseID := s.parentToolUseForProviderThread(providerThreadID)
	s.emitEvent(provider.ProviderEvent{
		Kind:            provider.EventCodexExecResult,
		ThreadID:        s.threadID,
		TurnID:          readTopLevelString(params, "turnId"),
		ItemID:          call.CallID,
		ItemType:        "commandExecution",
		ParentToolUseID: parentToolUseID,
		Meta:            encoded,
		Timestamp:       time.Now(),
	})
}

func (s *Session) enrichRawToolCallMetadata(evt *provider.ProviderEvent) {
	if evt == nil {
		return
	}
	switch evt.ItemType {
	case "collab_agent", "send_input", "wait_agent":
	default:
		return
	}
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		return
	}
	s.mu.Lock()
	call := s.rawCalls.byID[itemID]
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
			setRawStringIfMissing(input, "model", call.Model)
			setRawStringIfMissing(input, "reasoningEffort", call.ReasoningEffort)
		case "wait_agent":
			setRawStringIfMissing(input, "tool", "wait_agent")
			setRawStringArray(input, "requestedReceiverThreadIds", call.Targets)
		case "send_message", "followup_task":
			setRawStringIfMissing(input, "tool", "send_input")
			setRawStringIfMissing(input, "target", call.Target)
			setRawStringIfMissing(input, "activityTool", call.Name)
		}
	})
}

func (s *Session) pruneRawToolCallsLocked(keepCallID string) {
	if len(s.rawCalls.byID) <= maxRawToolCallRecords {
		return
	}
	for callID := range s.rawCalls.byID {
		if callID == keepCallID {
			continue
		}
		delete(s.rawCalls.byID, callID)
		if len(s.rawCalls.byID) <= maxRawToolCallRecords {
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
	delete(s.rawCalls.byID, callID)
	s.mu.Unlock()
}

func (s *Session) handleRawSpawnAgentOutput(call rawToolCall, item map[string]json.RawMessage) {
	var output struct {
		AgentID  string `json:"agent_id"`
		TaskName string `json:"task_name"`
		Nickname string `json:"nickname"`
	}
	if json.Unmarshal([]byte(readRawString(item, "output")), &output) != nil {
		return
	}
	output.AgentID = strings.TrimSpace(output.AgentID)
	output.TaskName = strings.TrimSpace(output.TaskName)
	providerThreadID := output.AgentID
	if providerThreadID == "" && output.TaskName != "" {
		providerThreadID = s.providerThreadForAgentPath(output.TaskName, call.CallID)
	}
	if providerThreadID == "" {
		return
	}
	s.rememberRawSpawnAgentIDForSubagentNotifications(providerThreadID, call.CallID)
	meta := collabReceiverMeta{
		ThreadID:      providerThreadID,
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
	model, reasoningEffort := s.activeCollabModel()
	model = firstNonEmptyString(strings.TrimSpace(call.Model), model)
	reasoningEffort = firstNonEmptyString(strings.TrimSpace(call.ReasoningEffort), reasoningEffort)
	launchMeta := collabLaunchMeta{
		Prompt:            call.Prompt,
		Model:             model,
		ReasoningEffort:   reasoningEffort,
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
	result, _ := rawExecCommandResult(output)
	return result
}

func rawExecCommandResult(output string) (string, string) {
	header := output
	if idx := strings.Index(output, "\nOutput:"); idx >= 0 {
		header = output[:idx]
	}
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Process running with session ID ") {
			processID := strings.TrimSpace(strings.TrimPrefix(line, "Process running with session ID "))
			return terminalWaitResultRunning, processID
		}
		if strings.HasPrefix(line, "Process exited with code ") {
			return terminalWaitResultExited, ""
		}
	}
	return "", ""
}

func classifyRawResponseNotification(_ string, method string, _ json.RawMessage, _ time.Time) ([]provider.ProviderEvent, bool) {
	switch method {
	case "rawResponseItem/completed":
		// Raw response items are model transcript detail. Codex app-server
		// emits typed item notifications for UI lifecycle events; preserving
		// this as "handled" prevents the generic unknown-notification path
		// from fabricating chat rows from raw tool transcripts.
		return nil, true
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
	if json.Unmarshal(data, &root) != nil {
		return data
	}
	method := readRawString(root, "method")
	var params map[string]json.RawMessage
	if json.Unmarshal(root["params"], &params) != nil {
		return data
	}
	if method == "item/commandExecution/terminalInteraction" {
		return redactTerminalInteractionStdin(root, params, data)
	}
	if method != "rawResponseItem/completed" {
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
		name := readRawString(item, "name")
		if name == "write_stdin" {
			r.rememberWriteStdinCallID(callID)
			if redactWriteStdinArguments(item) {
				changed = true
			}
		}
		if isEncryptedCollaborationFunctionCall(item, name) {
			if !redactFunctionCallMessage(item) {
				item["arguments"] = json.RawMessage(`"[redacted]"`)
			}
			changed = true
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

func isEncryptedCollaborationFunctionCall(item map[string]json.RawMessage, name string) bool {
	switch name {
	case "send_message", "followup_task":
		return true
	case "spawn_agent":
		if readRawString(item, "namespace") == "collaboration" {
			return true
		}
		args, _ := decodeFunctionArguments(readRawString(item, "arguments"))
		return readFlexibleString(args, "task_name") != ""
	default:
		return false
	}
}

func redactFunctionCallMessage(item map[string]json.RawMessage) bool {
	args, ok := decodeFunctionArguments(readRawString(item, "arguments"))
	if !ok {
		return false
	}
	if _, ok := args["message"]; !ok {
		return false
	}
	args["message"] = json.RawMessage(`"[redacted]"`)
	encodedArgs, err := json.Marshal(args)
	if err != nil {
		return false
	}
	encodedString, err := json.Marshal(string(encodedArgs))
	if err != nil {
		return false
	}
	item["arguments"] = encodedString
	return true
}

func redactTerminalInteractionStdin(root, params map[string]json.RawMessage, original []byte) []byte {
	if readRawString(params, "stdin") == "" {
		return original
	}
	encodedRedaction, err := json.Marshal("[redacted]")
	if err != nil {
		return original
	}
	params["stdin"] = encodedRedaction
	redacted, err := encodeRedactedParamsLine(root, params)
	if err != nil {
		return original
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
	return encodeRedactedParamsLine(root, params)
}

func encodeRedactedParamsLine(root, params map[string]json.RawMessage) ([]byte, error) {
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
