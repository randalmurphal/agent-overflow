package design

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
)

const mcpProtocolVersion = "2025-03-26"

// MCPServer exposes the design-mode MCP tools over HTTP. Both Codex
// (inline `mcp_servers` config) and Claude (via `--mcp-config`) consume
// it through the same wire — the server is provider-agnostic, which
// is why it lives alongside the rest of the design machinery rather
// than inside a provider package.
//
// Tools surfaced:
//
//   - get_design_diagnostics(since_token) -> {diagnostics, next_token}
//     Returns runtime errors / console output captured from the iframe
//     since the agent's last call. Blocks up to ~1s when the file
//     watcher fired recently but no diagnostics have landed yet.
//
//   - read_screenshot() -> N image content blocks
//     Captured by a backend headless Chromium subprocess (see
//     internal/screenshot.Manager) that loads the same per-thread URL
//     the iframe shows the user. The result content array carries
//     each tile as an `image` block with `mimeType: image/jpeg`. If
//     the page exceeded the tile budget the result trails with a
//     `text` block flagging the clip so the agent knows there's more
//     page.
//
// Per-thread URL tokens isolate sessions; the tools don't take a
// thread id parameter — the URL tells the server which thread the
// caller belongs to.
type MCPServer struct {
	reactor *Reactor

	mu            sync.Mutex
	server        *http.Server
	listener      net.Listener
	baseURL       string
	threadToToken map[string]string
	tokenToThread map[string]string
}

// NewMCPServer constructs the server but does not start it. The
// listener spins up on first RegisterThread so a process that never
// spawns a design thread doesn't open an extra port.
func NewMCPServer(reactor *Reactor) *MCPServer {
	return &MCPServer{
		reactor:       reactor,
		threadToToken: make(map[string]string),
		tokenToThread: make(map[string]string),
	}
}

// RegisterThread ensures the HTTP server is running and returns a
// provider-agnostic MCP config object keyed by a thread-specific
// server name. Codex uses it inline in `thread/new` params; Claude
// writes it to a temp file and points `--mcp-config` at the path.
func (s *MCPServer) RegisterThread(threadID string) (map[string]any, error) {
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
func (s *MCPServer) UnregisterThread(threadID string) {
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

// RegisteredThreadCount returns the number of threads with active MCP
// registrations.
func (s *MCPServer) RegisteredThreadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.threadToToken)
}

// Close shuts down the HTTP server.
func (s *MCPServer) Close() error {
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

func (s *MCPServer) ensureStarted() error {
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

func (s *MCPServer) handle(w http.ResponseWriter, r *http.Request) {
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
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "agent-overflow-design",
				"version": "2.0.0",
			},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusNoContent)
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]any{
			"tools": toolDefinitions(),
		})
	case "tools/call":
		s.handleToolCall(w, req, threadID, r.Context())
	default:
		writeRPCError(w, req.ID, http.StatusOK, -32601, "method not found")
	}
}

func (s *MCPServer) handleToolCall(
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
	case ToolGetDiagnostics:
		s.handleGetDiagnostics(ctx, w, req, threadID, params.Arguments)
	case ToolReadScreenshot:
		s.handleReadScreenshot(ctx, w, req, threadID)
	default:
		writeRPCError(w, req.ID, http.StatusOK, -32602, "unknown tool")
	}
}

func (s *MCPServer) handleGetDiagnostics(
	ctx context.Context,
	w http.ResponseWriter,
	req mcpRequest,
	threadID string,
	rawArgs json.RawMessage,
) {
	var args struct {
		SinceToken int64 `json:"since_token"`
	}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			writeRPCError(w, req.ID, http.StatusOK, -32602, "invalid get_design_diagnostics arguments")
			return
		}
	}
	diags, latest, err := s.reactor.GetDiagnostics(ctx, threadID, args.SinceToken)
	if err != nil {
		writeToolError(w, req.ID, err)
		return
	}
	writeToolResult(w, req.ID, map[string]any{
		"diagnostics": diags,
		"next_token":  latest,
	})
}

func (s *MCPServer) handleReadScreenshot(
	ctx context.Context,
	w http.ResponseWriter,
	req mcpRequest,
	threadID string,
) {
	result, err := s.reactor.CaptureScreenshot(ctx, threadID)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Session torn down or request aborted while the headless
			// browser was mid-capture: clean cancellation, not a tool
			// error.
			writeRPCError(w, req.ID, http.StatusOK, -32000, "design session ended")
			return
		}
		writeToolError(w, req.ID, err)
		return
	}
	content := make([]map[string]any, 0, len(result.Tiles)+1)
	for _, tileBase64 := range result.Tiles {
		// Tiles flow through the slicer base64-encoded so we can hand
		// them straight to the wire; no decode/re-encode round-trip.
		content = append(content, map[string]any{
			"type":     "image",
			"data":     tileBase64,
			"mimeType": "image/jpeg",
		})
	}
	if result.Clipped {
		// Server-controlled text. Do NOT interpolate user-, agent-, or
		// iframe-supplied strings into this block — it's appended
		// alongside server-trusted image content blocks the agent
		// reads at face value.
		content = append(content, map[string]any{
			"type": "text",
			"text": fmt.Sprintf(
				"Note: the rendered page was taller than the screenshot tile budget (%d tiles). The captured tiles cover the top of the page; trailing content was not included.",
				len(result.Tiles),
			),
		})
	}
	writeToolContent(w, req.ID, content)
}

func (s *MCPServer) threadIDForPath(path string) (string, bool) {
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

func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        ToolGetDiagnostics,
			"description": "Return runtime diagnostics (console errors/warnings, window errors, unhandled rejections) captured from the design iframe since the caller's last call. Pass since_token=0 on first use; pass the previously-returned next_token thereafter. Blocks briefly when the iframe is mid-load to avoid stale-empty results.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"since_token": map[string]any{
						"type":        "integer",
						"description": "Monotonic token from the previous call (0 on first call).",
					},
				},
			},
		},
		{
			"name":        ToolReadScreenshot,
			"description": "Capture the live design iframe and return the rendered page as one or more JPEG image tiles ordered top-to-bottom. Tall pages produce multiple tiles; if the page exceeds the tile budget the result includes a trailing text note. Use when text diagnostics are clean but you suspect visual issues a JS error wouldn't catch.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
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

// writeToolContent emits a successful MCP `tools/call` result with the
// caller-supplied content array. Both the JSON-marshalled-text shape
// (writeToolResult) and the multi-block image+text shape
// (handleReadScreenshot) route through here so the `{content,
// isError}` envelope stays defined in one place.
func writeToolContent(w http.ResponseWriter, id json.RawMessage, content []map[string]any) {
	writeRPCResult(w, id, map[string]any{
		"content": content,
		"isError": false,
	})
}

func writeToolResult(w http.ResponseWriter, id json.RawMessage, payload any) {
	data, _ := json.Marshal(payload)
	writeToolContent(w, id, []map[string]any{{
		"type": "text",
		"text": string(data),
	}})
}

func writeToolError(w http.ResponseWriter, id json.RawMessage, err error) {
	writeRPCResult(w, id, map[string]any{
		"content": []map[string]any{{
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
