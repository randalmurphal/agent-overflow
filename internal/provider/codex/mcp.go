package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"agent-overflow/internal/design"

	"github.com/google/uuid"
)

const designMCPProtocolVersion = "2025-03-26"

// DesignMCPServer provides render_design and present_options to Codex over HTTP MCP.
type DesignMCPServer struct {
	reactor *design.Reactor

	mu            sync.Mutex
	server        *http.Server
	listener      net.Listener
	baseURL       string
	threadToToken map[string]string
	tokenToThread map[string]string
}

// NewDesignMCPServer constructs a local HTTP MCP server for design-mode tools.
func NewDesignMCPServer(reactor *design.Reactor) *DesignMCPServer {
	return &DesignMCPServer{
		reactor:       reactor,
		threadToToken: make(map[string]string),
		tokenToThread: make(map[string]string),
	}
}

// RegisterThread ensures the HTTP server is running and returns a Codex MCP
// config object keyed by a thread-specific server name.
func (s *DesignMCPServer) RegisterThread(threadID string) (map[string]any, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, fmt.Errorf("thread ID is required")
	}
	if s.reactor == nil {
		return nil, fmt.Errorf("design reactor unavailable")
	}

	if err := s.ensureStarted(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	token := s.threadToToken[threadID]
	if token == "" {
		token = uuid.NewString()
		s.threadToToken[threadID] = token
		s.tokenToThread[token] = threadID
	}

	serverName := fmt.Sprintf("agent-overflow-design-%s", threadID)
	return map[string]any{
		serverName: map[string]any{
			"url": s.baseURL + "/mcp/" + token,
		},
	}, nil
}

// UnregisterThread removes a thread's HTTP token registration.
func (s *DesignMCPServer) UnregisterThread(threadID string) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	token := s.threadToToken[threadID]
	delete(s.threadToToken, threadID)
	if token != "" {
		delete(s.tokenToThread, token)
	}
}

// RegisteredThreadCount returns the number of threads with active MCP registrations.
func (s *DesignMCPServer) RegisteredThreadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.threadToToken)
}

// Close shuts down the HTTP server.
func (s *DesignMCPServer) Close() error {
	s.mu.Lock()
	server := s.server
	listener := s.listener
	s.server = nil
	s.listener = nil
	s.baseURL = ""
	s.threadToToken = make(map[string]string)
	s.tokenToThread = make(map[string]string)
	s.mu.Unlock()

	if server != nil {
		return server.Shutdown(context.Background())
	}
	if listener != nil {
		return listener.Close()
	}
	return nil
}

func (s *DesignMCPServer) ensureStarted() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.server != nil {
		return nil
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start design MCP listener: %w", err)
	}

	server := &http.Server{Handler: http.HandlerFunc(s.handle)}
	go func() {
		_ = server.Serve(listener)
	}()

	s.server = server
	s.listener = listener
	s.baseURL = "http://" + listener.Addr().String()
	return nil
}

func (s *DesignMCPServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	threadID, ok := s.threadIDForPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	var req mcpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32700, "invalid JSON")
		return
	}

	switch req.Method {
	case "initialize":
		writeRPCResult(w, req.ID, map[string]any{
			"protocolVersion": designMCPProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "agent-overflow-design",
				"version": "1.0.0",
			},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusNoContent)
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]any{
			"tools": designToolDefinitions(),
		})
	case "tools/call":
		s.handleToolCall(w, req, threadID, r.Context())
	default:
		writeRPCError(w, req.ID, http.StatusOK, -32601, "method not found")
	}
}

func (s *DesignMCPServer) handleToolCall(
	w http.ResponseWriter,
	req mcpRequest,
	threadID string,
	ctx context.Context,
) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPCError(w, req.ID, http.StatusOK, -32602, "invalid tools/call params")
		return
	}

	switch params.Name {
	case "render_design":
		var input design.RenderInput
		if err := json.Unmarshal(params.Arguments, &input); err != nil {
			writeRPCError(w, req.ID, http.StatusOK, -32602, "invalid render_design arguments")
			return
		}
		artifact, err := s.reactor.Render(threadID, input)
		if err != nil {
			writeToolError(w, req.ID, err)
			return
		}
		writeToolResult(w, req.ID, map[string]string{
			"status":     "rendered",
			"artifactId": artifact.ID,
		})
	case "present_options":
		var input design.PresentOptionsInput
		if err := json.Unmarshal(params.Arguments, &input); err != nil {
			writeRPCError(w, req.ID, http.StatusOK, -32602, "invalid present_options arguments")
			return
		}
		result, err := s.reactor.PresentOptions(ctx, threadID, input)
		if err != nil {
			writeToolError(w, req.ID, err)
			return
		}
		writeToolResult(w, req.ID, result)
	default:
		writeRPCError(w, req.ID, http.StatusOK, -32602, "unknown tool")
	}
}

func (s *DesignMCPServer) threadIDForPath(path string) (string, bool) {
	const prefix = "/mcp/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}

	token := strings.TrimSpace(strings.TrimPrefix(path, prefix))
	if token == "" {
		return "", false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	threadID := s.tokenToThread[token]
	return threadID, threadID != ""
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func designToolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        "render_design",
			"description": "Render a complete, self-contained HTML design mockup in the preview panel.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"html":        map[string]any{"type": "string"},
					"title":       map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
				},
				"required": []string{"html", "title"},
			},
		},
		{
			"name":        "present_options",
			"description": "Present 2 or more HTML design options and wait for the user to choose one.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{"type": "string"},
					"options": map[string]any{
						"type": "array",
					},
				},
				"required": []string{"prompt", "options"},
			},
		},
	}
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"id":      rawID(id),
		"result":  result,
	})
}

func writeToolResult(w http.ResponseWriter, id json.RawMessage, payload any) {
	data, _ := json.Marshal(payload)
	writeRPCResult(w, id, map[string]any{
		"content": []map[string]string{{
			"type": "text",
			"text": string(data),
		}},
		"isError": false,
	})
}

func writeToolError(w http.ResponseWriter, id json.RawMessage, err error) {
	writeRPCResult(w, id, map[string]any{
		"content": []map[string]string{{
			"type": "text",
			"text": err.Error(),
		}},
		"isError": true,
	})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, status, code int, message string) {
	writeJSON(w, status, map[string]any{
		"jsonrpc": "2.0",
		"id":      rawID(id),
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func rawID(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}

	var value any
	if err := json.Unmarshal(id, &value); err != nil {
		return nil
	}
	return value
}
