package codex

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/provider"
)

// handleDynamicToolCall invokes the registered dynamic tool handler and sends the
// JSON-RPC response back to the app-server.
func (s *Session) handleDynamicToolCall(rpcID int64, handler DynamicToolHandler, params json.RawMessage) {
	var parsed struct {
		Tool      string         `json:"tool"`
		ToolName  string         `json:"toolName"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &parsed); err != nil {
		if writeErr := s.writeErrorResponse(rpcID, -32602, fmt.Sprintf("invalid params: %v", err)); writeErr != nil {
			log.Printf("codex: failed to send error response for malformed dynamic tool params: %v", writeErr)
		}
		return
	}

	toolName := parsed.Tool
	if toolName == "" {
		toolName = parsed.ToolName
	}
	args := parsed.Arguments
	if args == nil {
		args = make(map[string]any)
	}

	go func() {
		content, success, err := handler(toolName, args)
		if err != nil {
			content = fmt.Sprintf("Error: %v", err)
			success = false
		}
		result := map[string]any{
			"contentItems": []map[string]string{{
				"type": "inputText",
				"text": content,
			}},
			"success": success,
		}
		if writeErr := s.writeResponse(rpcID, result); writeErr != nil {
			log.Printf("codex: failed to send dynamic tool result for %q: %v", toolName, writeErr)
		}
	}()
}

// handleServerRequest processes server-initiated requests (approvals).
func (s *Session) handleServerRequest(method string, id *json.Number, params json.RawMessage, line []byte) {
	rpcID, err := id.Int64()
	if err != nil {
		log.Printf("codex: server request has non-integer ID %q: %v", id.String(), err)
		return
	}

	turnID, itemID := readRouteFields(params)
	parentToolUseID := s.parentToolUseForProviderThread(providerThreadIDFromParams(params))
	emit := func(evt provider.ProviderEvent) {
		if parentToolUseID != "" {
			evt.ParentToolUseID = parentToolUseID
		}
		s.emitEvent(evt)
	}

	switch method {
	case "item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/fileRead/requestApproval",
		"applyPatchApproval",
		"execCommandApproval":

		meta := buildApprovalMeta(s.threadID, turnID, method, rpcID, params)
		s.trackPendingApproval(rpcID, provider.EventApprovalResolved)
		emit(buildApprovalEvent(s.threadID, turnID, itemID, meta, line))

	case "mcpServer/elicitation/request":
		meta := buildElicitationMeta(s.threadID, turnID, rpcID, params)
		s.trackPendingApproval(rpcID, provider.EventApprovalResolved)
		emit(buildApprovalEvent(s.threadID, turnID, itemID, meta, line))

	case "item/tool/call", "dynamicToolCall":
		s.mu.Lock()
		handler := s.dynamicToolHandler
		s.mu.Unlock()

		if handler != nil {
			s.handleDynamicToolCall(rpcID, handler, params)
		} else {
			if err := s.writeErrorResponse(rpcID, -32601, fmt.Sprintf("no handler registered for dynamic tool call: %s", method)); err != nil {
				log.Printf("codex: failed to send error response for %s: %v", method, err)
			}
		}

	case "item/tool/requestUserInput":
		questions := parseUserInputQuestions(params)
		if len(questions) == 0 {
			if err := s.writeErrorResponse(rpcID, -32602, "requestUserInput requires at least one question"); err != nil {
				log.Printf("codex: failed to send invalid requestUserInput response: %v", err)
			}
			return
		}
		meta := buildUserInputMetaFromQuestions(s.threadID, turnID, itemID, rpcID, questions)
		s.trackPendingApproval(rpcID, provider.EventUserInputResolved)
		if itemID != "" {
			emit(buildUserInputToolStartEvent(s.threadID, turnID, itemID, questions, line))
		}
		emit(buildUserInputEvent(s.threadID, turnID, itemID, meta, line))

	case "item/permissions/requestApproval":
		meta := buildPermissionMeta(s.threadID, turnID, rpcID, params)
		s.trackPendingApproval(rpcID, provider.EventApprovalResolved)
		emit(buildApprovalEvent(s.threadID, turnID, itemID, meta, line))

	default:
		if err := s.writeErrorResponse(rpcID, -32601, fmt.Sprintf("unsupported server request: %s", method)); err != nil {
			log.Printf("codex: failed to send error response for %s: %v", method, err)
		}
	}
}

func buildApprovalEvent(threadID, turnID, itemID string, meta json.RawMessage, raw json.RawMessage) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  threadID,
		TurnID:    turnID,
		ItemID:    itemID,
		Meta:      meta,
		Timestamp: time.Now(),
		Raw:       raw,
	}
}

func buildUserInputEvent(threadID, turnID, itemID string, meta json.RawMessage, raw json.RawMessage) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:      provider.EventUserInputRequest,
		ThreadID:  threadID,
		TurnID:    turnID,
		ItemID:    itemID,
		Meta:      meta,
		Timestamp: time.Now(),
		Raw:       raw,
	}
}

func buildUserInputToolStartEvent(threadID, turnID, itemID string, questions []provider.UserInputQuestion, raw json.RawMessage) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  threadID,
		TurnID:    turnID,
		ItemID:    itemID,
		ItemType:  "request_user_input",
		Meta:      buildUserInputToolStartMeta(questions),
		Timestamp: time.Now(),
		Raw:       raw,
	}
}
