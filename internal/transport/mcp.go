package transport

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// MCPThreadIDHeader associates an authenticated provider session with its
// AO thread. It is routing metadata, not a credential; the bearer token is
// still required and verified independently.
const MCPThreadIDHeader = "X-Agent-Overflow-Thread-ID"

const enqueueWorkflowToolDescription = "Record a workflow run proposal and show the user a confirmation card; nothing is enqueued without their approval. Invoke this tool only when the user explicitly asks to queue workflow work, never proactively."

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) handleWorkflowMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	const bearerPrefix = "Bearer "
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, bearerPrefix) ||
		ConstantTimeEqual(s.token, strings.TrimSpace(strings.TrimPrefix(authorization, bearerPrefix))) != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var request mcpRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&request); err != nil {
		writeMCPResponse(w, nil, nil, &mcpError{Code: -32700, Message: "invalid JSON-RPC request"})
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeMCPResponse(w, request.ID, nil, &mcpError{Code: -32600, Message: "multiple JSON-RPC values are not supported"})
		return
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		writeMCPResponse(w, request.ID, nil, &mcpError{Code: -32600, Message: "invalid JSON-RPC request"})
		return
	}
	// MCP notifications have no response body.
	if len(request.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			writeMCPResponse(w, request.ID, nil, &mcpError{Code: -32602, Message: "invalid initialize parameters"})
			return
		}
		if params.ProtocolVersion == "" {
			params.ProtocolVersion = "2025-06-18"
		}
		writeMCPResponse(w, request.ID, map[string]any{
			"protocolVersion": params.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agent-overflow-workflows", "version": "1"},
		}, nil)
	case "tools/list":
		writeMCPResponse(w, request.ID, map[string]any{"tools": []any{enqueueWorkflowToolDefinition()}}, nil)
	case "tools/call":
		var params mcpToolCallParams
		if err := json.Unmarshal(request.Params, &params); err != nil {
			writeMCPResponse(w, request.ID, nil, &mcpError{Code: -32602, Message: "invalid tools/call parameters"})
			return
		}
		if params.Name != "enqueue_workflow_run" {
			writeMCPResponse(w, request.ID, nil, &mcpError{Code: -32602, Message: fmt.Sprintf("unknown tool %q", params.Name)})
			return
		}
		if len(params.Arguments) == 0 {
			params.Arguments = json.RawMessage(`{}`)
		}
		message, err := s.cfg.MCPToolCall(r.Context(), strings.TrimSpace(r.Header.Get(MCPThreadIDHeader)), params.Arguments)
		result := map[string]any{"content": []any{map[string]any{"type": "text", "text": message}}}
		if err != nil {
			result["isError"] = true
			result["content"] = []any{map[string]any{"type": "text", "text": err.Error()}}
		}
		writeMCPResponse(w, request.ID, result, nil)
	default:
		writeMCPResponse(w, request.ID, nil, &mcpError{Code: -32601, Message: "method not found"})
	}
}

func enqueueWorkflowToolDefinition() map[string]any {
	return map[string]any{
		"name":        "enqueue_workflow_run",
		"description": enqueueWorkflowToolDescription,
		"inputSchema": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"project", "workflow", "goal"},
			"properties": map[string]any{
				"project":     map[string]any{"type": "string", "description": "Project id or slug."},
				"workflow":    map[string]any{"type": "string", "description": "Workflow definition id."},
				"goal":        map[string]any{"type": "string", "description": "Goal for the proposed run."},
				"seeds":       map[string]any{"type": "object", "description": "Optional workflow input values."},
				"base_branch": map[string]any{"type": "string", "description": "Optional base branch override."},
			},
		},
	}
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeMCPResponse(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *mcpError) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	WriteSecurityHeaders(w.Header())
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		response["error"] = rpcErr
	} else {
		response["result"] = result
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		// The connection is already being written; logging is the only
		// remaining visible error channel at this boundary.
		log.Printf("transport: encode MCP response: %v", err)
	}
}
