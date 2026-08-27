package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	mcpProtocolVersion = "2025-03-26"
	maxMCPRequestBytes = 1 << 20
)

type MCPServer struct {
	controller Controller
	enabled    atomic.Bool

	mu            sync.Mutex
	server        *http.Server
	listener      net.Listener
	baseURL       string
	threadToToken map[string]string
	tokenToAccess map[string]Access
}

func NewMCPServer(controller Controller, enabled bool) *MCPServer {
	s := &MCPServer{controller: controller, threadToToken: make(map[string]string), tokenToAccess: make(map[string]Access)}
	s.enabled.Store(enabled)
	return s
}

func (s *MCPServer) SetEnabled(enabled bool) { s.enabled.Store(enabled) }

func (s *MCPServer) RegisterThread(access Access) (map[string]any, error) {
	access.ThreadID = strings.TrimSpace(access.ThreadID)
	access.Workspace = strings.TrimSpace(access.Workspace)
	if access.ThreadID == "" || access.Workspace == "" {
		return nil, fmt.Errorf("browser MCP: thread and workspace are required")
	}
	if s.controller == nil {
		return nil, fmt.Errorf("browser MCP: controller unavailable")
	}
	if err := s.ensureStarted(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token := s.threadToToken[access.ThreadID]
	if token == "" {
		token = uuid.NewString()
		s.threadToToken[access.ThreadID] = token
	}
	s.tokenToAccess[token] = access
	return map[string]any{ServerName: map[string]any{"url": s.baseURL + "/mcp/" + token}}, nil
}

func (s *MCPServer) UnregisterThread(threadID string) {
	s.mu.Lock()
	token := s.threadToToken[threadID]
	delete(s.threadToToken, threadID)
	if token != "" {
		delete(s.tokenToAccess, token)
	}
	s.mu.Unlock()
	if s.controller != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.controller.CloseThread(ctx, threadID)
	}
}

func (s *MCPServer) RegisteredThreadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.threadToToken)
}

func (s *MCPServer) Close() error {
	s.mu.Lock()
	server, listener := s.server, s.listener
	s.server, s.listener, s.baseURL = nil, nil, ""
	s.threadToToken = make(map[string]string)
	s.tokenToAccess = make(map[string]Access)
	s.mu.Unlock()
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(ctx)
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
		return fmt.Errorf("browser MCP: listen: %w", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(s.handle), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 35 * time.Second, WriteTimeout: 35 * time.Minute, IdleTimeout: 60 * time.Second}
	s.server, s.listener = server, listener
	s.baseURL = "http://" + listener.Addr().String()
	go func() { _ = server.Serve(listener) }()
	return nil
}

func (s *MCPServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	access, ok := s.accessForPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMCPRequestBytes)
	defer r.Body.Close()
	var req rpcRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32700, "invalid JSON")
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32700, "invalid JSON")
		return
	}
	switch req.Method {
	case "initialize":
		writeRPCResult(w, req.ID, map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": ServerName, "version": "1.0.0"}})
	case "notifications/initialized":
		w.WriteHeader(http.StatusNoContent)
	case "tools/list":
		tools := []map[string]any{}
		if s.enabled.Load() {
			tools = toolDefinitions()
		}
		writeRPCResult(w, req.ID, map[string]any{"tools": tools})
	case "tools/call":
		s.handleToolCall(w, r.Context(), req, access)
	default:
		writeRPCError(w, req.ID, http.StatusOK, -32601, "method not found")
	}
}

func (s *MCPServer) handleToolCall(w http.ResponseWriter, ctx context.Context, req rpcRequest, access Access) {
	if !s.enabled.Load() {
		writeToolError(w, req.ID, fmt.Errorf("built-in browser tools are disabled"))
		return
	}
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &call); err != nil {
		writeRPCError(w, req.ID, http.StatusOK, -32602, "invalid tools/call params")
		return
	}
	var result any
	var err error
	switch call.Name {
	case "browser_open":
		var a struct {
			URL     string `json:"url"`
			PageID  string `json:"page_id"`
			NewPage bool   `json:"new_page"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Open(ctx, access, a.URL, OpenOptions{PageID: a.PageID, NewPage: a.NewPage})
		}
	case "browser_open_file":
		var a struct {
			Path    string `json:"path"`
			PageID  string `json:"page_id"`
			NewPage bool   `json:"new_page"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.OpenFile(ctx, access, a.Path, OpenOptions{PageID: a.PageID, NewPage: a.NewPage})
		}
	case "browser_pages":
		result, err = s.controller.Pages(ctx, access)
	case "browser_close_page":
		var a struct {
			PageID string `json:"page_id"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			err = s.controller.ClosePage(ctx, access, a.PageID)
			result = map[string]any{"closed": err == nil}
		}
	case "browser_snapshot":
		var a struct {
			PageID string `json:"page_id"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Snapshot(ctx, access, a.PageID)
		}
	case "browser_screenshot":
		var a struct {
			PageID   string `json:"page_id"`
			FullPage bool   `json:"full_page"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			var data []byte
			data, err = s.controller.Screenshot(ctx, access, ScreenshotOptions{PageID: a.PageID, FullPage: a.FullPage})
			if err == nil {
				writeToolImage(w, req.ID, data)
				return
			}
		}
	case "browser_click":
		var a struct {
			PageID   string `json:"page_id"`
			Selector string `json:"selector"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Click(ctx, access, a.PageID, a.Selector)
		}
	case "browser_type":
		var a struct {
			PageID   string `json:"page_id"`
			Selector string `json:"selector"`
			Text     string `json:"text"`
			Clear    bool   `json:"clear"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Type(ctx, access, TypeOptions{PageID: a.PageID, Selector: a.Selector, Text: a.Text, Clear: a.Clear})
		}
	case "browser_press":
		var a struct {
			PageID string `json:"page_id"`
			Key    string `json:"key"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Press(ctx, access, a.PageID, a.Key)
		}
	case "browser_scroll":
		var a struct {
			PageID   string  `json:"page_id"`
			Selector string  `json:"selector"`
			X        float64 `json:"x"`
			Y        float64 `json:"y"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Scroll(ctx, access, a.PageID, a.Selector, a.X, a.Y)
		}
	case "browser_wait":
		var a struct {
			PageID       string `json:"page_id"`
			Selector     string `json:"selector"`
			Milliseconds int    `json:"milliseconds"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Wait(ctx, access, a.PageID, a.Selector, a.Milliseconds)
		}
	case "browser_history":
		var a struct {
			PageID string `json:"page_id"`
			Action string `json:"action"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.History(ctx, access, a.PageID, a.Action)
		}
	case "browser_evaluate":
		var a struct {
			PageID     string `json:"page_id"`
			Expression string `json:"expression"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Evaluate(ctx, access, a.PageID, a.Expression)
		}
	default:
		writeRPCError(w, req.ID, http.StatusOK, -32602, "unknown tool")
		return
	}
	if err != nil {
		writeToolError(w, req.ID, err)
		return
	}
	writeToolJSON(w, req.ID, result)
}

func (s *MCPServer) accessForPath(path string) (Access, bool) {
	const prefix = "/mcp/"
	if !strings.HasPrefix(path, prefix) {
		return Access{}, false
	}
	token := strings.TrimPrefix(path, prefix)
	if token == "" || strings.Contains(token, "/") {
		return Access{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	access, ok := s.tokenToAccess[token]
	return access, ok
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func decodeArgs(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("invalid tool arguments")
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	}
	return fmt.Errorf("extra JSON value")
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeRPC(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func writeRPCError(w http.ResponseWriter, id json.RawMessage, status, code int, message string) {
	writeRPC(w, status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}
func writeRPC(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeToolJSON(w http.ResponseWriter, id json.RawMessage, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		writeToolError(w, id, err)
		return
	}
	writeRPCResult(w, id, map[string]any{"content": []map[string]any{{"type": "text", "text": string(data)}}})
}
func writeToolImage(w http.ResponseWriter, id json.RawMessage, data []byte) {
	writeRPCResult(w, id, map[string]any{"content": []map[string]any{{"type": "image", "mimeType": "image/jpeg", "data": base64.StdEncoding.EncodeToString(data)}}})
}
func writeToolError(w http.ResponseWriter, id json.RawMessage, err error) {
	message := strings.TrimSpace(err.Error())
	if len(message) > 1000 {
		message = message[:1000]
	}
	writeRPCResult(w, id, map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": message}}})
}

func toolDefinitions() []map[string]any {
	object := func(properties map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	stringProp := map[string]any{"type": "string"}
	boolProp := map[string]any{"type": "boolean"}
	page := map[string]any{"page_id": stringProp}
	return []map[string]any{
		{"name": "browser_open", "description": "Open an HTTP(S) URL in the current or a new browser page.", "inputSchema": object(map[string]any{"url": stringProp, "page_id": stringProp, "new_page": boolProp}, "url")},
		{"name": "browser_open_file", "description": "Open a local HTML, PDF, image, text, or other browser-renderable regular file.", "inputSchema": object(map[string]any{"path": stringProp, "page_id": stringProp, "new_page": boolProp}, "path")},
		{"name": "browser_pages", "description": "List browser pages owned by this agent thread.", "inputSchema": object(map[string]any{})},
		{"name": "browser_close_page", "description": "Close one browser page.", "inputSchema": object(page, "page_id")},
		{"name": "browser_snapshot", "description": "Read bounded visible text and interactive elements with CSS selectors.", "inputSchema": object(page)},
		{"name": "browser_screenshot", "description": "Capture the viewport or a height-capped full page as JPEG.", "inputSchema": object(map[string]any{"page_id": stringProp, "full_page": boolProp})},
		{"name": "browser_click", "description": "Click the first visible element matching a CSS selector using trusted browser input.", "inputSchema": object(map[string]any{"page_id": stringProp, "selector": stringProp}, "selector")},
		{"name": "browser_type", "description": "Focus an element and type text using trusted browser input.", "inputSchema": object(map[string]any{"page_id": stringProp, "selector": stringProp, "text": stringProp, "clear": boolProp}, "selector", "text")},
		{"name": "browser_press", "description": "Press a key or chord such as Enter, Escape, or Control+L.", "inputSchema": object(map[string]any{"page_id": stringProp, "key": stringProp}, "key")},
		{"name": "browser_scroll", "description": "Scroll the window or a selected element by CSS pixels.", "inputSchema": object(map[string]any{"page_id": stringProp, "selector": stringProp, "x": map[string]any{"type": "number"}, "y": map[string]any{"type": "number"}}, "y")},
		{"name": "browser_wait", "description": "Wait for a selector to become visible or for a bounded duration.", "inputSchema": object(map[string]any{"page_id": stringProp, "selector": stringProp, "milliseconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000}})},
		{"name": "browser_history", "description": "Navigate back, forward, reload, or stop loading.", "inputSchema": object(map[string]any{"page_id": stringProp, "action": map[string]any{"type": "string", "enum": []string{"back", "forward", "reload", "stop"}}}, "action")},
		{"name": "browser_evaluate", "description": "Evaluate JavaScript in the page and return a bounded JSON result.", "inputSchema": object(map[string]any{"page_id": stringProp, "expression": stringProp}, "expression")},
	}
}
