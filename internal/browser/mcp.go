package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/loopback"
)

const (
	mcpProtocolVersion     = "2025-03-26"
	maxMCPRequestBytes     = 1 << 20
	browserMCPInstructions = "Browser pages are shared only within this AO thread. browser_open and browser_open_file create a new background page when page_id is omitted; retain the returned page_id and pass it on later calls. When multiple pages exist, implicit page selection fails safely: call browser_pages and pass the intended page_id. Use browser_visibility with visible=true and page_id only when the user should see that page."
)

var cachedToolDefinitions = toolDefinitions()

type MCPServer struct {
	controller Controller
	enabled    atomic.Bool

	mu            sync.Mutex
	server        *http.Server
	listener      net.Listener
	baseURL       string
	threadToToken map[string]string
	tokenToAccess map[string]Access
	threadEnabled map[string]bool
}

func NewMCPServer(controller Controller, enabled bool) *MCPServer {
	s := &MCPServer{controller: controller, threadToToken: make(map[string]string), tokenToAccess: make(map[string]Access), threadEnabled: make(map[string]bool)}
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
	if _, ok := s.threadEnabled[access.ThreadID]; !ok {
		s.threadEnabled[access.ThreadID] = true
	}
	return map[string]any{ServerName: map[string]any{"url": s.baseURL + "/mcp/" + token}}, nil
}

func (s *MCPServer) UnregisterThread(threadID string) {
	s.mu.Lock()
	token := s.threadToToken[threadID]
	delete(s.threadToToken, threadID)
	delete(s.threadEnabled, threadID)
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

func (s *MCPServer) SetThreadEnabled(threadID string, enabled bool) {
	s.mu.Lock()
	s.threadEnabled[strings.TrimSpace(threadID)] = enabled
	s.mu.Unlock()
}

func (s *MCPServer) ThreadEnabled(threadID string) bool {
	s.mu.Lock()
	enabled, ok := s.threadEnabled[strings.TrimSpace(threadID)]
	s.mu.Unlock()
	return !ok || enabled
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
	s.threadEnabled = make(map[string]bool)
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

// ensureStarted binds the loopback listener on first thread
// registration. It cannot defer the bind until a tool is called: the
// endpoint URL rides the provider CLI's argv at spawn, so the listener
// has to exist before the process starts.
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
		// An OPTIONS preflight lands here too, and answering it with 405
		// and no CORS headers is what the content-type check below relies
		// on: the browser stops before it sends the real request.
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !validMCPRequest(w, r) {
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
		writeRPCResult(w, req.ID, map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": ServerName, "version": "1.0.0"}, "instructions": browserMCPInstructions})
	case "notifications/initialized":
		w.WriteHeader(http.StatusNoContent)
	case "tools/list":
		tools := []map[string]any{}
		if s.enabled.Load() && s.ThreadEnabled(access.ThreadID) {
			tools = cachedToolDefinitions
		}
		writeRPCResult(w, req.ID, map[string]any{"tools": tools})
	case "tools/call":
		s.handleToolCall(w, r.Context(), req, access)
	default:
		writeRPCError(w, req.ID, http.StatusOK, -32601, "method not found")
	}
}

func (s *MCPServer) handleToolCall(w http.ResponseWriter, ctx context.Context, req rpcRequest, access Access) {
	if !s.enabled.Load() || !s.ThreadEnabled(access.ThreadID) {
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
	// note is an engine capability qualifier a tool's result carries beside its
	// JSON payload — never instead of it, so the payload's shape is the same
	// on every engine.
	var note string
	var err error
	switch call.Name {
	case "browser_open":
		var a struct {
			URL    string `json:"url"`
			PageID string `json:"page_id"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Open(ctx, access, a.URL, OpenOptions{PageID: a.PageID})
		}
	case "browser_new_page":
		result, err = s.controller.NewPage(ctx, access)
	case "browser_open_file":
		var a struct {
			Path   string `json:"path"`
			PageID string `json:"page_id"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.OpenFile(ctx, access, a.Path, OpenOptions{PageID: a.PageID})
		}
	case "browser_pages":
		result, err = s.controller.Pages(ctx, access)
	case "browser_select_page":
		var a struct {
			PageID string `json:"page_id"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.SelectPage(ctx, access, a.PageID)
		}
	case "browser_label_page":
		var a struct {
			PageID string `json:"page_id"`
			Label  string `json:"label"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.LabelPage(ctx, access, a.PageID, a.Label)
		}
	case "browser_session":
		var a struct {
			Name string `json:"name"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.NameSession(ctx, access, a.Name)
		}
	case "browser_visibility":
		var a struct {
			Visible *bool  `json:"visible"`
			PageID  string `json:"page_id"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Visibility(ctx, access, a.Visible, a.PageID)
		}
	case "browser_viewport":
		var a ViewportOptions
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Viewport(ctx, access, a)
		}
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
			PageID   string    `json:"page_id"`
			FullPage bool      `json:"full_page"`
			Clip     *ClipRect `json:"clip"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			var data []byte
			data, err = s.controller.Screenshot(ctx, access, ScreenshotOptions{PageID: a.PageID, FullPage: a.FullPage, Clip: a.Clip})
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
	case "browser_locator":
		var a LocatorOptions
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Locator(ctx, access, a)
		}
	case "browser_pointer":
		var a PointerOptions
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Pointer(ctx, access, a)
		}
	case "browser_dom":
		var a DOMActionOptions
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.DOMAction(ctx, access, a)
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
			PageID string   `json:"page_id"`
			Key    string   `json:"key"`
			Keys   []string `json:"keys"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			key := strings.TrimSpace(a.Key)
			if key == "" {
				if len(a.Keys) == 0 || len(a.Keys) > 10 {
					err = fmt.Errorf("browser: press requires key or between 1 and 10 keys")
				} else {
					key = strings.Join(a.Keys, "+")
				}
			}
			if err == nil {
				result, err = s.controller.Press(ctx, access, a.PageID, key)
			}
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
		var a WaitOptions
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.WaitAdvanced(ctx, access, a)
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
	case "browser_evaluate_readonly":
		var a struct {
			PageID     string          `json:"page_id"`
			Expression string          `json:"expression"`
			Argument   json.RawMessage `json:"argument"`
			TimeoutMS  int             `json:"timeout_ms"`
		}
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			var timeout time.Duration
			timeout, err = boundedTimeout(a.TimeoutMS)
			if err != nil {
				break
			}
			expression := readOnlyExpression(a.Expression, a.Argument)
			evalCtx, cancel := context.WithTimeout(ctx, timeout)
			result, note, err = s.controller.EvaluateReadOnly(evalCtx, access, a.PageID, expression)
			cancel()
		}
	case "browser_clipboard":
		var a ClipboardOptions
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Clipboard(ctx, access, a)
		}
	case "browser_console_logs":
		var a ConsoleOptions
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.ConsoleLogs(ctx, access, a)
		}
	case "browser_downloads":
		var a DownloadOptions
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Downloads(ctx, access, a)
		}
	case "browser_assets":
		var a AssetOptions
		err = decodeArgs(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Assets(ctx, access, a)
		}
	default:
		writeRPCError(w, req.ID, http.StatusOK, -32602, "unknown tool")
		return
	}
	if err != nil {
		writeToolError(w, req.ID, err)
		return
	}
	writeToolJSON(w, req.ID, result, note)
}

func readOnlyExpression(expression string, argument json.RawMessage) string {
	if len(argument) > 0 {
		return "(" + expression + ")(" + string(argument) + ")"
	}
	if looksLikeJSFunction(expression) {
		return "(" + expression + ")()"
	}
	return expression
}

func looksLikeJSFunction(expression string) bool {
	trimmed := strings.TrimSpace(expression)
	if hasJSFunctionPrefix(trimmed, "function") || hasJSFunctionPrefix(trimmed, "async function") {
		return true
	}
	arrow := strings.Index(trimmed, "=>")
	if arrow < 0 {
		return false
	}
	head := strings.TrimSpace(trimmed[:arrow])
	head = strings.TrimSpace(strings.TrimPrefix(head, "async "))
	if strings.HasPrefix(head, "(") && strings.HasSuffix(head, ")") {
		return true
	}
	if head == "" {
		return false
	}
	for i, r := range head {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '_' && r != '$' && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func hasJSFunctionPrefix(expression, keyword string) bool {
	if !strings.HasPrefix(expression, keyword) || len(expression) == len(keyword) {
		return false
	}
	switch expression[len(keyword)] {
	case ' ', '(', '*':
		return true
	default:
		return false
	}
}

// validMCPRequest applies the request checks every request clears before
// any method dispatch — initialize, notifications, tools/list and
// tools/call alike — and writes the refusal itself when one fails.
//
// The only client of this endpoint is a provider CLI
// this app spawned, which pins what a genuine request looks like: it
// arrives from a loopback peer, carries no Origin, and declares JSON.
// Both real clients match (verified 2026-08-30): Claude Code's HTTP
// transport and the Codex app-server's rmcp adapter each set
// `content-type: application/json` on every POST and neither sets Origin
// — Codex goes further and rejects a user-configured Origin header
// outright (codex-rs/rmcp-client/src/http_headers.rs).
//
// The per-thread UUID in the path is the only other credential, and it
// rides provider argv, so it is readable by any process of the same
// user. Same-user is already the trust boundary; these checks are what
// keeps a document in a browser — which is not the same user's
// process — from reaching the endpoint.
func validMCPRequest(w http.ResponseWriter, r *http.Request) bool {
	// Peer verification off the accepting socket, matching the claudetui
	// gateway's check (isLoopback, internal/provider/claudetui/hookrelay.go
	// — that copy also accepts the literal "localhost", which an accepted
	// connection's RemoteAddr never carries). Go fills RemoteAddr from the
	// accepted socket, so a request header cannot set it.
	if !loopback.PeerAddress(r.RemoteAddr) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	// A local process sends no Origin. A document always sends one on a
	// POST, cross-origin or same-origin, so refusing the header refuses
	// the page without touching the provider CLI.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	// Requiring JSON before the body is decoded is more than hygiene. A
	// POST declaring text/plain is a CORS simple request: it is sent with
	// no preflight, so a page could invoke a tool the browser never asked
	// permission for — it could not read the reply, but the page
	// evaluation or workspace file read would already have run. JSON is
	// not a simple content type, so the browser must preflight first, and
	// the method check in handle refuses that preflight.
	if !jsonContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// jsonContentType reports whether a Content-Type header declares JSON.
// Parameters are allowed: both provider clients send the bare type, but
// a charset is legal and some MCP clients attach one.
func jsonContentType(value string) bool {
	mediaType, _, _ := strings.Cut(value, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "application/json")
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
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid tool arguments")
	}
	if err := ensureJSONEOF(decoder); err != nil {
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

// writeToolJSON writes one tool result. A non-empty engine note becomes a
// SECOND content entry rather than a wrapper around the payload: the first
// entry stays the exact JSON every caller already parses, on every engine.
func writeToolJSON(w http.ResponseWriter, id json.RawMessage, value any, note string) {
	data, err := json.Marshal(value)
	if err != nil {
		writeToolError(w, id, err)
		return
	}
	content := []map[string]any{{"type": "text", "text": string(data)}}
	if note != "" {
		content = append(content, map[string]any{"type": "text", "text": note})
	}
	writeRPCResult(w, id, map[string]any{"content": content})
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
	numberProp := map[string]any{"type": "number"}
	integerProp := map[string]any{"type": "integer"}
	enumProp := func(values ...string) map[string]any { return map[string]any{"type": "string", "enum": values} }
	page := map[string]any{"page_id": stringProp}
	locatorRef := map[string]any{"$ref": "#/$defs/locator"}
	locatorDefinition := object(map[string]any{
		"css": stringProp, "role": stringProp, "name": stringProp,
		"text": stringProp, "label": stringProp, "placeholder": stringProp,
		"test_id": stringProp, "exact": boolProp, "regex": boolProp,
		"regex_flags": map[string]any{"type": "string", "pattern": "^[imsu]*$"},
		"frames":      map[string]any{"type": "array", "items": stringProp, "maxItems": 8},
		"scope":       locatorRef, "has": locatorRef, "has_not": locatorRef,
		"has_text": stringProp, "has_not_text": stringProp, "visible": boolProp,
		"and":   map[string]any{"type": "array", "items": locatorRef, "maxItems": 8},
		"or":    map[string]any{"type": "array", "items": locatorRef, "maxItems": 8},
		"index": map[string]any{"type": "integer", "minimum": 0},
	})
	locatorDefinition["description"] = "Stateless locator. Choose a direct strategy or compose scope/has/has_not/and/or; nested locators remain in the same frame."
	withLocator := func(schema map[string]any) map[string]any {
		schema["$defs"] = map[string]any{"locator": locatorDefinition}
		return schema
	}
	selectArg := object(map[string]any{
		"value": stringProp,
		"label": stringProp,
		"index": map[string]any{"type": "integer", "minimum": 0},
	})
	selectArg["oneOf"] = []map[string]any{{"required": []string{"value"}}, {"required": []string{"label"}}, {"required": []string{"index"}}}
	clipboardEntry := object(map[string]any{"mimeType": stringProp, "text": stringProp, "base64": stringProp}, "mimeType")
	clipboardItem := object(map[string]any{
		"entries":           map[string]any{"type": "array", "items": clipboardEntry, "maxItems": 100},
		"presentationStyle": enumProp("unspecified", "inline", "attachment"),
	}, "entries")
	clipProp := object(map[string]any{"x": numberProp, "y": numberProp, "width": numberProp, "height": numberProp}, "x", "y", "width", "height")
	locatorSchema := withLocator(object(map[string]any{
		"page_id": stringProp, "locator": locatorRef,
		"action": enumProp("count", "all", "all_text_contents", "click", "double_click", "fill", "type", "press", "check", "uncheck", "set_checked", "select_option", "get_attribute", "inner_text", "text_content", "is_enabled", "is_visible", "wait"),
		"value":  stringProp, "values": map[string]any{"type": "array", "items": stringProp, "maxItems": 100},
		"attribute": stringProp, "checked": boolProp, "button": enumProp("left", "right", "middle"),
		"modifiers": map[string]any{"type": "array", "items": enumProp("alt", "control", "ctrl", "meta", "command", "cmd", "shift", "controlormeta"), "maxItems": 5},
		"force":     boolProp, "timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000},
		"state":             enumProp("attached", "detached", "visible", "hidden"),
		"expect_navigation": boolProp, "expect_download": boolProp, "url": stringProp,
		"wait_until": enumProp("commit", "domcontentloaded", "load", "networkidle"),
		"select":     map[string]any{"type": "array", "maxItems": 100, "items": selectArg},
	}, "locator", "action"))
	waitSchema := withLocator(object(map[string]any{
		"page_id": stringProp, "selector": stringProp, "locator": locatorRef,
		"state": enumProp("attached", "detached", "visible", "hidden"), "url": stringProp,
		"load_state":   enumProp("commit", "domcontentloaded", "load", "networkidle"),
		"milliseconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000},
		"timeout_ms":   map[string]any{"type": "integer", "minimum": 0, "maximum": 30000},
	}))
	return []map[string]any{
		{"name": "browser_session", "description": "Name this thread's browser automation session for user-visible context.", "inputSchema": object(map[string]any{"name": stringProp}, "name")},
		{"name": "browser_open", "description": "Open an HTTP(S) URL. Without page_id this creates a new background page; with page_id it intentionally navigates that existing page. Retain the returned page_id.", "inputSchema": object(map[string]any{"url": stringProp, "page_id": stringProp}, "url")},
		{"name": "browser_new_page", "description": "Create a new blank background page and return its page_id.", "inputSchema": object(map[string]any{})},
		{"name": "browser_open_file", "description": "Open a local browser-renderable regular file. Without page_id this creates a new background page; with page_id it intentionally navigates that existing page.", "inputSchema": object(map[string]any{"path": stringProp, "page_id": stringProp}, "path")},
		{"name": "browser_pages", "description": "List every page owned by this AO thread, including IDs, labels, URLs, titles, and the explicitly selected companion page. Use it to intentionally reuse another agent's page.", "inputSchema": object(map[string]any{})},
		{"name": "browser_select_page", "description": "Explicitly pin an owned page as the companion tab without changing companion visibility.", "inputSchema": object(page, "page_id")},
		{"name": "browser_label_page", "description": "Set or clear a short thread-unique label on an owned page so agents can coordinate around it.", "inputSchema": object(map[string]any{"page_id": stringProp, "label": stringProp}, "page_id", "label")},
		{"name": "browser_close_page", "description": "Close one browser page.", "inputSchema": object(page, "page_id")},
		{"name": "browser_visibility", "description": "Get visibility, hide the companion, or explicitly present one page. Showing requires page_id when multiple pages exist; ordinary browser activity never steals the visible tab.", "inputSchema": object(map[string]any{"visible": boolProp, "page_id": stringProp})},
		{"name": "browser_viewport", "description": "Get, set, or reset the bounded browser viewport override.", "inputSchema": object(map[string]any{"action": enumProp("get", "set", "reset"), "width": integerProp, "height": integerProp}, "action")},
		{"name": "browser_snapshot", "description": "Read bounded visible text and interactive elements with reusable DOM node IDs and CSS selectors.", "inputSchema": object(page)},
		{"name": "browser_screenshot", "description": "Capture the viewport, a bounded clip, or a height-capped full page as JPEG.", "inputSchema": object(map[string]any{"page_id": stringProp, "full_page": boolProp, "clip": clipProp})},
		{"name": "browser_locator", "description": "Playwright-like locator query/action: count, all, all_text_contents, click, double_click, fill, type, press, check, uncheck, set_checked, select_option, get_attribute, inner_text, text_content, is_enabled, is_visible, or wait. Actions are strict and require exactly one match.", "inputSchema": locatorSchema},
		{"name": "browser_click", "description": "Click the first visible element matching a CSS selector using trusted browser input.", "inputSchema": object(map[string]any{"page_id": stringProp, "selector": stringProp}, "selector")},
		{"name": "browser_type", "description": "Focus an element and type text using trusted browser input.", "inputSchema": object(map[string]any{"page_id": stringProp, "selector": stringProp, "text": stringProp, "clear": boolProp}, "selector", "text")},
		{"name": "browser_press", "description": "Press a key or chord such as Enter, Escape, or Control+L; keys may express the chord as an array.", "inputSchema": object(map[string]any{"page_id": stringProp, "key": stringProp, "keys": map[string]any{"type": "array", "items": stringProp, "minItems": 1, "maxItems": 10}})},
		{"name": "browser_pointer", "description": "Computer-use input at viewport coordinates: click, double_click, move, scroll, or bounded-path drag.", "inputSchema": object(map[string]any{"page_id": stringProp, "action": enumProp("click", "double_click", "move", "scroll", "drag"), "x": numberProp, "y": numberProp, "button": enumProp("left", "right", "middle", "back", "forward"), "modifiers": map[string]any{"type": "array", "items": enumProp("alt", "control", "ctrl", "meta", "command", "cmd", "shift", "controlormeta"), "maxItems": 5}, "scroll_x": numberProp, "scroll_y": numberProp, "path": map[string]any{"type": "array", "maxItems": 100, "items": object(map[string]any{"x": numberProp, "y": numberProp}, "x", "y")}}, "action")},
		{"name": "browser_dom", "description": "DOM computer-use operations using node_id from browser_snapshot: get_visible_dom, click, double_click, type, keypress, or scroll; scroll without node_id targets the page.", "inputSchema": object(map[string]any{"page_id": stringProp, "action": enumProp("get_visible_dom", "click", "double_click", "type", "keypress", "scroll"), "node_id": stringProp, "text": stringProp, "key": stringProp, "keys": map[string]any{"type": "array", "items": stringProp, "minItems": 1, "maxItems": 10}, "x": numberProp, "y": numberProp}, "action")},
		{"name": "browser_scroll", "description": "Scroll the window or a selected element by CSS pixels.", "inputSchema": object(map[string]any{"page_id": stringProp, "selector": stringProp, "x": map[string]any{"type": "number"}, "y": map[string]any{"type": "number"}}, "y")},
		{"name": "browser_wait", "description": "Wait for duration, selector/locator state, URL glob, or commit/DOMContentLoaded/load/network-idle state.", "inputSchema": waitSchema},
		{"name": "browser_history", "description": "Navigate back, forward, reload, or stop loading.", "inputSchema": object(map[string]any{"page_id": stringProp, "action": map[string]any{"type": "string", "enum": []string{"back", "forward", "reload", "stop"}}}, "action")},
		{"name": "browser_evaluate_readonly", "description": "Evaluate a side-effect-free JavaScript expression or function with optional JSON argument in page scope and return a bounded JSON result; possible mutations are rejected by the engine where it can, and the result says so when they cannot be.", "inputSchema": object(map[string]any{"page_id": stringProp, "expression": stringProp, "argument": map[string]any{}, "timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000}}, "expression")},
		{"name": "browser_evaluate", "description": "Evaluate JavaScript in the page and return a bounded JSON result. Prefer browser_evaluate_readonly for inspection.", "inputSchema": object(map[string]any{"page_id": stringProp, "expression": stringProp}, "expression")},
		{"name": "browser_clipboard", "description": "Read or write this managed tab's isolated clipboard as text or bounded MIME items; never touches the OS clipboard.", "inputSchema": object(map[string]any{"page_id": stringProp, "action": enumProp("read", "read_text", "write", "write_text"), "text": stringProp, "items": map[string]any{"type": "array", "maxItems": 100, "items": clipboardItem}}, "action")},
		{"name": "browser_console_logs", "description": "Read the tab's bounded console/runtime log ring with level, substring, and result limits.", "inputSchema": object(map[string]any{"page_id": stringProp, "filter": stringProp, "levels": map[string]any{"type": "array", "items": enumProp("debug", "info", "log", "warn", "warning", "error")}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": maxConsoleEntries}})},
		{"name": "browser_downloads", "description": "List downloads or wait for the next completed/canceled download after a sequence number. Returns the app-owned local path.", "inputSchema": object(map[string]any{"page_id": stringProp, "action": map[string]any{"type": "string", "enum": []string{"list", "wait"}}, "after": map[string]any{"type": "integer", "minimum": 0}, "timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000}}, "action")},
		{"name": "browser_assets", "description": "List observed page assets/inline SVGs or bundle selected image/font/stylesheet/video assets into a bounded local artifact directory.", "inputSchema": object(map[string]any{"page_id": stringProp, "action": enumProp("list", "bundle"), "inventory_id": stringProp, "asset_ids": map[string]any{"type": "array", "items": stringProp, "maxItems": 200}, "kinds": map[string]any{"type": "array", "items": enumProp("font", "image", "stylesheet", "video"), "maxItems": 4}}, "action")},
	}
}
