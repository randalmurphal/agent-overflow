package browser

import (
	"agent-overflow/internal/mcpargs"
	"agent-overflow/internal/threadmcp"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	browserMCPInstructions = "Browser pages are shared only within this AO thread. browser_open and browser_open_file create a new background page when page_id is omitted; retain the returned page_id and pass it on later calls. When multiple pages exist, implicit page selection fails safely: call browser_pages and pass the intended page_id. Use browser_visibility with visible=true and page_id only when the user should see that page. Every page lays out at the thread viewport whether or not the user is watching. By default that is the user's pane size, so a presented page fills the pane at 1:1 and reflows when the pane resizes; with no pane it is the last pane size, or 1280x720. browser_viewport set pins a fixed size instead, which the pane shows scaled down to fit. Screenshots and coordinates always mean the current viewport; browser_viewport get reports it."
)

var cachedToolDefinitions = toolDefinitions()

// Instructions is the guide the server states on its MCP handshake. Claude
// reads it there; Codex never shows a server's instructions to the model, so
// the app appends it to that provider's developer instructions instead.
func Instructions() string { return browserMCPInstructions }

type MCPServer struct {
	*threadmcp.Server[Access]
	controller Controller
}

func NewMCPServer(controller Controller, enabled bool) *MCPServer {
	s := &MCPServer{controller: controller}
	s.Server = threadmcp.New(ServerName, browserMCPInstructions, func(Access) []map[string]any { return cachedToolDefinitions }, s.handleToolCall)
	s.SetEnabled(enabled)
	return s
}

// RegisterThread issues the thread's browser tools URL to one provider
// session, replacing any earlier session's URL.
func (s *MCPServer) RegisterThread(access Access, session string) (map[string]any, error) {
	access.ThreadID = strings.TrimSpace(access.ThreadID)
	access.Workspace = strings.TrimSpace(access.Workspace)
	if access.ThreadID == "" || access.Workspace == "" {
		return nil, fmt.Errorf("browser MCP: thread and workspace are required")
	}
	if s.controller == nil {
		return nil, fmt.Errorf("browser MCP: controller unavailable")
	}
	return s.Server.RegisterThread(access.ThreadID, session, access)
}
func (s *MCPServer) handle(w http.ResponseWriter, r *http.Request) { s.ServeHTTP(w, r) }
func (s *MCPServer) handleToolCall(w http.ResponseWriter, ctx context.Context, req threadmcp.Request, access Access) {
	call, err := threadmcp.DecodeToolCall(req.Params)
	if err != nil {
		threadmcp.WriteError(w, req.ID, http.StatusOK, -32602, "invalid tools/call params")
		return
	}
	var result any
	// note is a qualifier a tool's result carries beside its JSON payload, such
	// as an engine capability caveat; never instead of it, so the payload's
	// shape is the same on every engine.
	var note string
	switch call.Name {
	case "browser_open":
		var a struct {
			URL    string `json:"url"`
			PageID string `json:"page_id"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
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
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.OpenFile(ctx, access, a.Path, OpenOptions{PageID: a.PageID})
		}
	case "browser_pages":
		result, err = s.controller.Pages(ctx, access)
	case "browser_select_page":
		var a struct {
			PageID string `json:"page_id"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.SelectPage(ctx, access, a.PageID)
		}
	case "browser_label_page":
		var a struct {
			PageID string `json:"page_id"`
			Label  string `json:"label"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.LabelPage(ctx, access, a.PageID, a.Label)
		}
	case "browser_session":
		var a struct {
			Name string `json:"name"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.NameSession(ctx, access, a.Name)
		}
	case "browser_visibility":
		var a struct {
			Visible *bool  `json:"visible"`
			PageID  string `json:"page_id"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Visibility(ctx, access, a.Visible, a.PageID)
		}
	case "browser_viewport":
		var a ViewportOptions
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Viewport(ctx, access, a)
		}
	case "browser_close_page":
		var a struct {
			PageID string `json:"page_id"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			err = s.controller.ClosePage(ctx, access, a.PageID)
			result = map[string]any{"closed": err == nil}
		}
	case "browser_snapshot":
		var a struct {
			PageID string `json:"page_id"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Snapshot(ctx, access, a.PageID)
		}
	case "browser_screenshot":
		var a struct {
			PageID   string    `json:"page_id"`
			FullPage bool      `json:"full_page"`
			Clip     *ClipRect `json:"clip"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
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
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Click(ctx, access, a.PageID, a.Selector)
		}
	case "browser_locator":
		var a LocatorOptions
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Locator(ctx, access, a)
		}
	case "browser_pointer":
		var a PointerOptions
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Pointer(ctx, access, a)
		}
	case "browser_dom":
		var a DOMActionOptions
		err = mcpargs.Decode(call.Arguments, &a)
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
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Type(ctx, access, TypeOptions{PageID: a.PageID, Selector: a.Selector, Text: a.Text, Clear: a.Clear})
		}
	case "browser_press":
		var a struct {
			PageID string   `json:"page_id"`
			Key    string   `json:"key"`
			Keys   []string `json:"keys"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
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
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Scroll(ctx, access, a.PageID, a.Selector, a.X, a.Y)
		}
	case "browser_wait":
		var a WaitOptions
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.WaitAdvanced(ctx, access, a)
		}
	case "browser_history":
		var a struct {
			PageID string `json:"page_id"`
			Action string `json:"action"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.History(ctx, access, a.PageID, a.Action)
		}
	case "browser_evaluate":
		var a struct {
			PageID     string          `json:"page_id"`
			Expression string          `json:"expression"`
			Argument   json.RawMessage `json:"argument"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, note, err = s.controller.Evaluate(ctx, access, a.PageID, a.Expression, a.Argument)
		}
	case "browser_evaluate_readonly":
		var a struct {
			PageID     string          `json:"page_id"`
			Expression string          `json:"expression"`
			Argument   json.RawMessage `json:"argument"`
			TimeoutMS  int             `json:"timeout_ms"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			var timeout time.Duration
			timeout, err = boundedTimeout(a.TimeoutMS)
			if err != nil {
				break
			}
			evalCtx, cancel := context.WithTimeout(ctx, timeout)
			result, note, err = s.controller.EvaluateReadOnly(evalCtx, access, a.PageID, a.Expression, a.Argument)
			cancel()
		}
	case "browser_clipboard":
		var a ClipboardOptions
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Clipboard(ctx, access, a)
		}
	case "browser_console_logs":
		var a ConsoleOptions
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.ConsoleLogs(ctx, access, a)
		}
	case "browser_downloads":
		var a DownloadOptions
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Downloads(ctx, access, a)
		}
	case "browser_assets":
		var a AssetOptions
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Assets(ctx, access, a)
		}
	default:
		threadmcp.WriteError(w, req.ID, http.StatusOK, -32602, "unknown tool")
		return
	}
	if err != nil {
		writeToolError(w, req.ID, err)
		return
	}
	writeToolJSON(w, req.ID, result, note)
}

// writeToolJSON writes one tool result. A non-empty note becomes a
// SECOND content entry rather than a wrapper around the payload: the first
// entry stays the exact JSON every caller already parses, on every engine.
func writeToolJSON(w http.ResponseWriter, id json.RawMessage, value any, note string) {
	// Agents read the payload as text; markup in it reads as written, not as
	// \u003c escapes.
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		writeToolError(w, id, err)
		return
	}
	content := []map[string]any{{"type": "text", "text": strings.TrimSuffix(data.String(), "\n")}}
	if note != "" {
		content = append(content, map[string]any{"type": "text", "text": note})
	}
	threadmcp.WriteResult(w, id, map[string]any{"content": content})
}
func writeToolImage(w http.ResponseWriter, id json.RawMessage, data []byte) {
	threadmcp.WriteResult(w, id, map[string]any{"content": []map[string]any{{"type": "image", "mimeType": "image/jpeg", "data": base64.StdEncoding.EncodeToString(data)}}})
}
func writeToolError(w http.ResponseWriter, id json.RawMessage, err error) {
	message := truncateUTF8(strings.TrimSpace(err.Error()), 1000)
	threadmcp.WriteResult(w, id, map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": message}}})
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
		{"name": "browser_pages", "description": "List every page owned by this AO thread, including IDs, labels, URLs, titles, and the explicitly selected companion page. Use it to intentionally reuse another agent's page. A suspended page was unloaded while its session was idle or across a restart; the next call with its page_id reloads its URL.", "inputSchema": object(map[string]any{})},
		{"name": "browser_select_page", "description": "Explicitly pin an owned page as the companion tab without changing companion visibility.", "inputSchema": object(page, "page_id")},
		{"name": "browser_label_page", "description": "Set or clear a short thread-unique label on an owned page so agents can coordinate around it.", "inputSchema": object(map[string]any{"page_id": stringProp, "label": stringProp}, "page_id", "label")},
		{"name": "browser_close_page", "description": "Close one browser page.", "inputSchema": object(page, "page_id")},
		{"name": "browser_visibility", "description": "Get visibility, hide the companion, or explicitly present one page. Showing requires page_id when multiple pages exist; ordinary browser activity never steals the visible tab. Presenting does not change the page's viewport.", "inputSchema": object(map[string]any{"visible": boolProp, "page_id": stringProp})},
		{"name": "browser_viewport", "description": "Get, set, or reset the thread viewport every page lays out at. By default it follows the user's pane: the page fills it at 1:1 and reflows when the pane resizes (the last pane size, or 1280x720, with no pane). set pins a bounded fixed size the pane shows scaled down to fit; reset returns to following the pane. get reports the current size, whether it is pinned (viewportSet), and the pane size (paneWidth/paneHeight).", "inputSchema": object(map[string]any{"action": enumProp("get", "set", "reset"), "width": integerProp, "height": integerProp}, "action")},
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
		{"name": "browser_evaluate_readonly", "description": "Read the page without changing it, and return the result as JSON. Takes expression and argument as browser_evaluate does, but runs the code synchronously: it cannot await, and a promise result fails. Chromium rejects code it cannot prove free of side effects, including some reads such as getElementById and new Error; read with querySelector. WebKit cannot check, and its result carries a second entry saying so.", "inputSchema": object(map[string]any{"page_id": stringProp, "expression": stringProp, "argument": map[string]any{}, "timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000}}, "expression")},
		{"name": "browser_evaluate", "description": "Run JavaScript in the page and return the result as JSON. expression is an expression, such as document.title, whose value is the result; or a function, such as (arg) => arg.id or async () => { ...; return x; }, called with argument, any JSON value. Other code runs as statements in a function body and gives a value only through return. await works, and a promise result is awaited. Each call is its own function scope with this as window: declarations do not persist, so assign to window to keep a value. Results read as JSON.stringify writes them: a property that is undefined, a function or a symbol is left out, such an array item reads as null, and an undefined result reads as null. Elements read as outerHTML, text nodes as their text, NodeList, Set and typed arrays as arrays, Map as [key, value] pairs, errors as \"Name: message\", BigInt as a digit string, and a cycle as \"[Circular]\". A result over 256000 bytes or 1000 levels deep fails; return less. A throw fails with \"Uncaught\" and its message. Prefer browser_evaluate_readonly for reading.", "inputSchema": object(map[string]any{"page_id": stringProp, "expression": stringProp, "argument": map[string]any{}}, "expression")},
		{"name": "browser_clipboard", "description": "Read or write this managed tab's isolated clipboard as text or bounded MIME items; never touches the OS clipboard.", "inputSchema": object(map[string]any{"page_id": stringProp, "action": enumProp("read", "read_text", "write", "write_text"), "text": stringProp, "items": map[string]any{"type": "array", "maxItems": 100, "items": clipboardItem}}, "action")},
		{"name": "browser_console_logs", "description": "Read the tab's bounded console/runtime log ring with level, substring, and result limits.", "inputSchema": object(map[string]any{"page_id": stringProp, "filter": stringProp, "levels": map[string]any{"type": "array", "items": enumProp("debug", "info", "log", "warn", "warning", "error")}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": maxConsoleEntries}})},
		{"name": "browser_downloads", "description": "List downloads or wait for the next completed/canceled download after a sequence number. Returns the app-owned local path.", "inputSchema": object(map[string]any{"page_id": stringProp, "action": map[string]any{"type": "string", "enum": []string{"list", "wait"}}, "after": map[string]any{"type": "integer", "minimum": 0}, "timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000}}, "action")},
		{"name": "browser_assets", "description": "List observed page assets/inline SVGs or bundle selected image/font/stylesheet/video assets into a bounded local artifact directory.", "inputSchema": object(map[string]any{"page_id": stringProp, "action": enumProp("list", "bundle"), "inventory_id": stringProp, "asset_ids": map[string]any{"type": "array", "items": stringProp, "maxItems": 200}, "kinds": map[string]any{"type": "array", "items": enumProp("font", "image", "stylesheet", "video"), "maxItems": 4}}, "action")},
	}
}
