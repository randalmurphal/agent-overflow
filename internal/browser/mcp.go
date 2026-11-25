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
	browserMCPInstructions = "Use these browser_* tools for all browser work in Agent Overflow (not other IDE/in-app browser plugins). Browser pages are shared only within this AO thread. browser_open (url or path) opens a new background page unless page_id is passed; keep and pass page_id on later calls. With multiple pages, call browser_page(action=\"list\") and pass page_id explicitly. Call browser_pane(action=\"visibility\", visible=true, page_id=...) only to show a page to the user. Pages use the thread viewport (follows the user's pane by default, or 1280x720 with no pane; browser_pane viewport_set pins a fixed size). Coordinates and screenshots match the current viewport."
)

var cachedToolDefinitions = toolDefinitions()

// Instructions is the guide the server states on its MCP handshake. Claude
// reads it there; Codex never shows a server's instructions to the model, so
// the app appends it to that provider's developer instructions instead.
func Instructions() string { return browserMCPInstructions }

// ToolDefinitions returns a fresh copy of the browser MCP tool schemas.
func ToolDefinitions() []map[string]any { return toolDefinitions() }

type MCPServer struct {
	*threadmcp.Server[Caller]
	controller Controller
}

func NewMCPServer(controller Controller, enabled bool) *MCPServer {
	s := &MCPServer{controller: controller}
	s.Server = threadmcp.New(ServerName, browserMCPInstructions, func(Caller) []map[string]any { return cachedToolDefinitions }, s.handleToolCall)
	s.SetEnabled(enabled)
	return s
}

// RegisterThread issues the thread's browser tools URL to one provider
// session, replacing any earlier session's URL.
func (s *MCPServer) RegisterThread(caller Caller, session string) (map[string]any, error) {
	caller.ThreadID = caller.ThreadID.trim()
	caller.Workspace = strings.TrimSpace(caller.Workspace)
	if caller.ThreadID == "" || caller.Workspace == "" {
		return nil, fmt.Errorf("browser MCP: thread and workspace are required")
	}
	if s.controller == nil {
		return nil, fmt.Errorf("browser MCP: controller unavailable")
	}
	return s.Server.RegisterThread(string(caller.ThreadID), session, caller)
}
func (s *MCPServer) handleToolCall(w http.ResponseWriter, ctx context.Context, req threadmcp.Request, caller Caller) {
	call, err := threadmcp.DecodeToolCall(req.Params)
	if err != nil {
		threadmcp.WriteError(w, req.ID, http.StatusOK, -32602, "invalid tools/call params")
		return
	}
	// Bound at the call's entry, as a pane action is: everything the call
	// does reaches the thread's browser as it was then, and a close of the
	// thread during the call refuses what it would commit after it.
	access := s.controller.Bind(caller)
	// The tabs of the pages the call acts on show it while it runs.
	ctx, endCall := withAgentCall(ctx)
	defer endCall()
	var result any
	// note is a qualifier a tool's result carries beside its JSON payload, such
	// as an engine capability caveat; never instead of it, so the payload's
	// shape is the same on every engine.
	var note string
	switch call.Name {
	case "browser_open":
		var a struct {
			URL    string `json:"url"`
			Path   string `json:"path"`
			PageID PageID `json:"page_id"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			hasURL := strings.TrimSpace(a.URL) != ""
			hasPath := strings.TrimSpace(a.Path) != ""
			switch {
			case hasURL && hasPath:
				err = fmt.Errorf("browser_open: pass either url or path, not both")
			case hasPath:
				result, err = s.controller.OpenFile(ctx, access, a.Path, OpenOptions{PageID: a.PageID})
			case hasURL:
				result, err = s.controller.Open(ctx, access, a.URL, OpenOptions{PageID: a.PageID})
			default:
				err = fmt.Errorf("browser_open: url or path is required")
			}
		}
	case "browser_page":
		var a struct {
			Action string `json:"action"`
			PageID PageID `json:"page_id"`
			Label  string `json:"label"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			switch a.Action {
			case "list":
				result, err = s.controller.Pages(ctx, access)
			case "new":
				result, err = s.controller.NewPage(ctx, access)
			case "select":
				if strings.TrimSpace(string(a.PageID)) == "" {
					err = fmt.Errorf("browser_page select: page_id is required")
				} else {
					result, err = s.controller.SelectPage(ctx, access, a.PageID)
				}
			case "label":
				if strings.TrimSpace(string(a.PageID)) == "" {
					err = fmt.Errorf("browser_page label: page_id is required")
				} else {
					result, err = s.controller.LabelPage(ctx, access, a.PageID, a.Label)
				}
			case "close":
				if strings.TrimSpace(string(a.PageID)) == "" {
					err = fmt.Errorf("browser_page close: page_id is required")
				} else {
					err = s.controller.ClosePage(ctx, access, a.PageID)
					result = map[string]any{"closed": err == nil}
				}
			case "back", "forward", "reload", "stop":
				result, err = s.controller.History(ctx, access, a.PageID, a.Action)
			default:
				err = fmt.Errorf("browser_page: unknown action %q (want list, new, select, label, close, back, forward, reload, or stop)", a.Action)
			}
		}
	case "browser_pane":
		var a struct {
			Action  string `json:"action"`
			Name    string `json:"name"`
			Visible *bool  `json:"visible"`
			PageID  PageID `json:"page_id"`
			Width   int    `json:"width"`
			Height  int    `json:"height"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			switch a.Action {
			case "session":
				if strings.TrimSpace(a.Name) == "" {
					err = fmt.Errorf("browser_pane session: name is required")
				} else {
					result, err = s.controller.NameSession(ctx, access, a.Name)
				}
			case "visibility":
				result, err = s.controller.Visibility(ctx, access, a.Visible, a.PageID)
			case "viewport_get":
				result, err = s.controller.Viewport(ctx, access, ViewportOptions{Action: "get"})
			case "viewport_set":
				result, err = s.controller.Viewport(ctx, access, ViewportOptions{Action: "set", Width: a.Width, Height: a.Height})
			case "viewport_reset":
				result, err = s.controller.Viewport(ctx, access, ViewportOptions{Action: "reset"})
			default:
				err = fmt.Errorf("browser_pane: unknown action %q (want session, visibility, viewport_get, viewport_set, or viewport_reset)", a.Action)
			}
		}
	case "browser_snapshot":
		var a struct {
			PageID PageID `json:"page_id"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.Snapshot(ctx, access, a.PageID)
		}
	case "browser_screenshot":
		var a struct {
			PageID   PageID    `json:"page_id"`
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
		var a struct {
			PageID   PageID   `json:"page_id"`
			Action   string   `json:"action"`
			NodeID   string   `json:"node_id"`
			Selector string   `json:"selector"`
			Text     string   `json:"text"`
			Clear    bool     `json:"clear"`
			Key      string   `json:"key"`
			Keys     []string `json:"keys"`
			X        float64  `json:"x"`
			Y        float64  `json:"y"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			sel := strings.TrimSpace(a.Selector)
			node := strings.TrimSpace(a.NodeID)
			switch {
			case sel != "" && node != "":
				err = fmt.Errorf("browser_dom: pass either node_id or selector, not both")
			case sel != "" && a.Action == "click":
				result, err = s.controller.Click(ctx, access, a.PageID, sel)
			case sel != "" && a.Action == "type":
				result, err = s.controller.Type(ctx, access, TypeOptions{PageID: a.PageID, Selector: sel, Text: a.Text, Clear: a.Clear})
			case sel != "" && a.Action == "scroll":
				result, err = s.controller.Scroll(ctx, access, a.PageID, sel, a.X, a.Y)
			case sel != "":
				err = fmt.Errorf("browser_dom: selector is supported for click, type, and scroll; use browser_locator for other selector actions")
			case node == "" && a.Action == "keypress":
				key := strings.TrimSpace(a.Key)
				if key == "" {
					if len(a.Keys) == 0 || len(a.Keys) > 10 {
						err = fmt.Errorf("browser: keypress requires key or between 1 and 10 keys")
					} else {
						key = strings.Join(a.Keys, "+")
					}
				}
				if err == nil {
					result, err = s.controller.Press(ctx, access, a.PageID, key)
				}
			default:
				result, err = s.controller.DOMAction(ctx, access, DOMActionOptions{
					PageID: a.PageID,
					Action: a.Action,
					NodeID: a.NodeID,
					Text:   a.Text,
					Key:    a.Key,
					Keys:   a.Keys,
					X:      a.X,
					Y:      a.Y,
				})
			}
		}
	case "browser_wait":
		var a struct {
			PageID       PageID `json:"page_id"`
			Selector     string `json:"selector"`
			State        string `json:"state"`
			URL          string `json:"url"`
			LoadState    string `json:"load_state"`
			Milliseconds int    `json:"milliseconds"`
			TimeoutMS    int    `json:"timeout_ms"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, err = s.controller.WaitAdvanced(ctx, access, WaitOptions{
				PageID:       a.PageID,
				Selector:     a.Selector,
				State:        a.State,
				URL:          a.URL,
				LoadState:    a.LoadState,
				Milliseconds: a.Milliseconds,
				TimeoutMS:    a.TimeoutMS,
			})
		}
	case "browser_evaluate":
		var a struct {
			PageID     PageID          `json:"page_id"`
			Expression string          `json:"expression"`
			Argument   json.RawMessage `json:"argument"`
		}
		err = mcpargs.Decode(call.Arguments, &a)
		if err == nil {
			result, note, err = s.controller.Evaluate(ctx, access, a.PageID, a.Expression, a.Argument)
		}
	case "browser_evaluate_readonly":
		var a struct {
			PageID     PageID          `json:"page_id"`
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
		if hint, ok := retiredBrowserToolHint[call.Name]; ok {
			writeToolError(w, req.ID, fmt.Errorf("%s was consolidated: %s", call.Name, hint))
			return
		}
		threadmcp.WriteError(w, req.ID, http.StatusOK, -32602, "unknown tool")
		return
	}
	if err == nil {
		// What the call changed before the thread's close, the close closed.
		err = access.End()
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

var retiredBrowserToolHint = map[string]string{
	"browser_open_file":   `use browser_open(path=..., page_id=...)`,
	"browser_pages":       `use browser_page(action="list")`,
	"browser_new_page":    `use browser_page(action="new")`,
	"browser_select_page": `use browser_page(action="select", page_id=...)`,
	"browser_label_page":  `use browser_page(action="label", page_id=..., label=...)`,
	"browser_close_page":  `use browser_page(action="close", page_id=...)`,
	"browser_history":     `use browser_page(action="back"|"forward"|"reload"|"stop", page_id=...)`,
	"browser_session":     `use browser_pane(action="session", name=...)`,
	"browser_visibility":  `use browser_pane(action="visibility", visible=..., page_id=...)`,
	"browser_viewport":    `use browser_pane(action="viewport_get"|"viewport_set"|"viewport_reset", width=..., height=...)`,
	"browser_click":       `use browser_locator(action="click", locator={css:...}) or browser_dom(action="click", selector=...)`,
	"browser_type":        `use browser_locator(action="type"|"fill", locator={css:...}, value=...) or browser_dom(action="type", selector=..., text=..., clear=...)`,
	"browser_press":       `use browser_dom(action="keypress", key=... or keys=[...])`,
	"browser_scroll":      `use browser_dom(action="scroll", selector=..., x=..., y=...)`,
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
	locatorDefinition["description"] = "Stateless locator."
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
		"action":    enumProp("count", "all", "all_text_contents", "click", "double_click", "fill", "type", "press", "check", "uncheck", "set_checked", "select_option", "get_attribute", "inner_text", "text_content", "is_enabled", "is_visible", "wait"),
		"value":     map[string]any{"type": "string", "description": "Text for fill/type or key chord for press."},
		"values":    map[string]any{"type": "array", "items": stringProp, "maxItems": 100},
		"attribute": stringProp, "checked": boolProp, "button": enumProp("left", "right", "middle"),
		"modifiers": map[string]any{"type": "array", "items": enumProp("alt", "control", "ctrl", "meta", "command", "cmd", "shift", "controlormeta"), "maxItems": 5},
		"force":     boolProp, "timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000},
		"state":             enumProp("attached", "detached", "visible", "hidden"),
		"expect_navigation": boolProp, "expect_download": boolProp, "url": stringProp,
		"wait_until": enumProp("commit", "domcontentloaded", "load", "networkidle"),
		"select":     map[string]any{"type": "array", "maxItems": 100, "items": selectArg},
	}, "locator", "action"))
	waitSchema := object(map[string]any{
		"page_id": stringProp, "selector": stringProp,
		"state": enumProp("attached", "detached", "visible", "hidden"), "url": stringProp,
		"load_state":   enumProp("commit", "domcontentloaded", "load", "networkidle"),
		"milliseconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000},
		"timeout_ms":   map[string]any{"type": "integer", "minimum": 0, "maximum": 30000},
	})
	return []map[string]any{
		{"name": "browser_open", "description": "Open an HTTP(S) url or local file path (new background page unless page_id set).", "inputSchema": object(map[string]any{"url": stringProp, "path": stringProp, "page_id": stringProp})},
		{"name": "browser_page", "description": "Manage or navigate pages: list, new, select (pin tab without showing), label, close, back, forward, reload, stop.", "inputSchema": object(map[string]any{"action": enumProp("list", "new", "select", "label", "close", "back", "forward", "reload", "stop"), "page_id": stringProp, "label": stringProp}, "action")},
		{"name": "browser_pane", "description": "Session/pane controls: session (name + return cookie jar), visibility (get/hide/present; page_id required with multiple pages), viewport_get, viewport_set, viewport_reset.", "inputSchema": object(map[string]any{"action": enumProp("session", "visibility", "viewport_get", "viewport_set", "viewport_reset"), "name": stringProp, "visible": boolProp, "page_id": stringProp, "width": integerProp, "height": integerProp}, "action")},
		{"name": "browser_snapshot", "description": "Read visible text and interactive elements with node IDs and CSS selectors.", "inputSchema": object(page)},
		{"name": "browser_screenshot", "description": "Capture viewport, clip or full page as JPEG.", "inputSchema": object(map[string]any{"page_id": stringProp, "full_page": boolProp, "clip": clipProp})},
		{"name": "browser_locator", "description": "Strict Playwright-like locator query/action/wait (actions require 1 match).", "inputSchema": locatorSchema},
		{"name": "browser_dom", "description": "DOM/keyboard/scroll actions by node_id or CSS selector (keypress/scroll without target acts on page; untrusted events on WebKit).", "inputSchema": object(map[string]any{"page_id": stringProp, "action": enumProp("get_visible_dom", "click", "double_click", "type", "keypress", "scroll"), "node_id": stringProp, "selector": stringProp, "text": stringProp, "clear": boolProp, "key": stringProp, "keys": map[string]any{"type": "array", "items": stringProp, "minItems": 1, "maxItems": 10}, "x": numberProp, "y": numberProp}, "action")},
		{"name": "browser_pointer", "description": "Viewport-coordinate pointer input (click/double_click/move/scroll/drag).", "inputSchema": object(map[string]any{"page_id": stringProp, "action": enumProp("click", "double_click", "move", "scroll", "drag"), "x": numberProp, "y": numberProp, "button": enumProp("left", "right", "middle", "back", "forward"), "modifiers": map[string]any{"type": "array", "items": enumProp("alt", "control", "ctrl", "meta", "command", "cmd", "shift", "controlormeta"), "maxItems": 5}, "scroll_x": numberProp, "scroll_y": numberProp, "path": map[string]any{"type": "array", "maxItems": 100, "items": object(map[string]any{"x": numberProp, "y": numberProp}, "x", "y")}}, "action")},
		{"name": "browser_wait", "description": "Wait for duration, CSS selector state, URL glob or load state (use browser_locator action=wait for rich locators).", "inputSchema": waitSchema},
		{"name": "browser_evaluate_readonly", "description": "Sync read-only JS returning JSON (no await; Chromium rejects getElementById, use querySelector; WebKit notes unverified).", "inputSchema": object(map[string]any{"page_id": stringProp, "expression": stringProp, "argument": map[string]any{}, "timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000}}, "expression")},
		{"name": "browser_evaluate", "description": "Run JS returning JSON (<=256 KB; expression, fn(argument), or statements with return; await works). Prefer browser_evaluate_readonly for reads.", "inputSchema": object(map[string]any{"page_id": stringProp, "expression": stringProp, "argument": map[string]any{}}, "expression")},
		{"name": "browser_clipboard", "description": "Read/write isolated tab clipboard (not OS clipboard).", "inputSchema": object(map[string]any{"page_id": stringProp, "action": enumProp("read", "read_text", "write", "write_text"), "text": stringProp, "items": map[string]any{"type": "array", "maxItems": 100, "items": clipboardItem}}, "action")},
		{"name": "browser_console_logs", "description": "Read bounded console/runtime logs.", "inputSchema": object(map[string]any{"page_id": stringProp, "filter": stringProp, "levels": map[string]any{"type": "array", "items": enumProp("debug", "info", "log", "warn", "warning", "error")}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": maxConsoleEntries}})},
		{"name": "browser_downloads", "description": "List downloads or wait for next finished download after sequence number.", "inputSchema": object(map[string]any{"page_id": stringProp, "action": map[string]any{"type": "string", "enum": []string{"list", "wait"}}, "after": map[string]any{"type": "integer", "minimum": 0}, "timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000}}, "action")},
		{"name": "browser_assets", "description": "List page assets/SVGs or bundle selected assets locally.", "inputSchema": object(map[string]any{"page_id": stringProp, "action": enumProp("list", "bundle"), "inventory_id": stringProp, "asset_ids": map[string]any{"type": "array", "items": stringProp, "maxItems": 200}, "kinds": map[string]any{"type": "array", "items": enumProp("font", "image", "stylesheet", "video"), "maxItems": 4}}, "action")},
	}
}
