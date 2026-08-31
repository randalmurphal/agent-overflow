package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
)

type fakeController struct {
	openedURL      string
	openOptions    OpenOptions
	access         Access
	closeThreads   []string
	readExpression string
}

func (f *fakeController) Open(_ context.Context, a Access, u string, opts OpenOptions) (PageInfo, error) {
	f.access = a
	f.openedURL = u
	f.openOptions = opts
	return PageInfo{ID: "p", URL: u}, nil
}
func (f *fakeController) NewPage(context.Context, Access) (PageInfo, error) { return PageInfo{}, nil }
func (f *fakeController) OpenFile(context.Context, Access, string, OpenOptions) (PageInfo, error) {
	return PageInfo{}, nil
}
func (f *fakeController) Pages(context.Context, Access) ([]PageInfo, error) { return []PageInfo{}, nil }
func (f *fakeController) SelectPage(context.Context, Access, string) (PageInfo, error) {
	return PageInfo{}, nil
}
func (f *fakeController) LabelPage(context.Context, Access, string, string) (PageInfo, error) {
	return PageInfo{}, nil
}
func (f *fakeController) NameSession(context.Context, Access, string) (SessionInfo, error) {
	return SessionInfo{}, nil
}
func (f *fakeController) Visibility(context.Context, Access, *bool, string) (SessionInfo, error) {
	return SessionInfo{}, nil
}
func (f *fakeController) Viewport(context.Context, Access, ViewportOptions) (SessionInfo, error) {
	return SessionInfo{}, nil
}
func (f *fakeController) ClosePage(context.Context, Access, string) error { return nil }
func (f *fakeController) Snapshot(context.Context, Access, string) (Snapshot, error) {
	return Snapshot{}, nil
}
func (f *fakeController) Screenshot(context.Context, Access, ScreenshotOptions) ([]byte, error) {
	return []byte("jpg"), nil
}
func (f *fakeController) Locator(context.Context, Access, LocatorOptions) (LocatorResult, error) {
	return LocatorResult{}, nil
}
func (f *fakeController) Pointer(context.Context, Access, PointerOptions) (PageInfo, error) {
	return PageInfo{}, nil
}
func (f *fakeController) DOMAction(context.Context, Access, DOMActionOptions) (any, error) {
	return nil, nil
}
func (f *fakeController) Clipboard(context.Context, Access, ClipboardOptions) (any, error) {
	return nil, nil
}
func (f *fakeController) ConsoleLogs(context.Context, Access, ConsoleOptions) ([]ConsoleLog, error) {
	return nil, nil
}
func (f *fakeController) Downloads(context.Context, Access, DownloadOptions) (any, error) {
	return nil, nil
}
func (f *fakeController) Assets(context.Context, Access, AssetOptions) (any, error) { return nil, nil }
func (f *fakeController) WaitAdvanced(context.Context, Access, WaitOptions) (PageInfo, error) {
	return PageInfo{}, nil
}
func (f *fakeController) Click(context.Context, Access, string, string) (PageInfo, error) {
	return PageInfo{}, nil
}
func (f *fakeController) Type(context.Context, Access, TypeOptions) (PageInfo, error) {
	return PageInfo{}, nil
}
func (f *fakeController) Press(context.Context, Access, string, string) (PageInfo, error) {
	return PageInfo{}, nil
}
func (f *fakeController) Scroll(context.Context, Access, string, string, float64, float64) (PageInfo, error) {
	return PageInfo{}, nil
}
func (f *fakeController) Wait(context.Context, Access, string, string, int) (PageInfo, error) {
	return PageInfo{}, nil
}
func (f *fakeController) History(context.Context, Access, string, string) (PageInfo, error) {
	return PageInfo{}, nil
}
func (f *fakeController) Evaluate(context.Context, Access, string, string) (any, error) {
	return nil, nil
}
func (f *fakeController) EvaluateReadOnly(_ context.Context, _ Access, _ string, expression string) (any, error) {
	f.readExpression = expression
	return nil, nil
}
func (f *fakeController) CloseThread(_ context.Context, id string) error {
	f.closeThreads = append(f.closeThreads, id)
	return nil
}
func (f *fakeController) Close() error                        { return nil }
func (f *fakeController) ClearSiteData(context.Context) error { return nil }
func (f *fakeController) Reconfigure(Config) error            { return nil }

func TestMCPServerDisabledIsConnectedButAdvertisesNoTools(t *testing.T) {
	fake := &fakeController{}
	server := NewMCPServer(fake, false)
	url := registerTestThread(t, server)
	response := postRPC(t, url, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	tools := response["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 0 {
		t.Fatalf("disabled tools = %d", len(tools))
	}
	server.SetEnabled(true)
	response = postRPC(t, url, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	tools = response["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 27 {
		t.Fatalf("enabled tools = %d, want 27", len(tools))
	}
}

func TestMCPInitializeExplainsSafeParallelPageUsage(t *testing.T) {
	server := NewMCPServer(&fakeController{}, true)
	url := registerTestThread(t, server)
	response := postRPC(t, url, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	result := response["result"].(map[string]any)
	instructions, _ := result["instructions"].(string)
	for _, required := range []string{"page_id", "browser_pages", "browser_visibility"} {
		if !strings.Contains(instructions, required) {
			t.Fatalf("MCP instructions omit %q: %q", required, instructions)
		}
	}
}

func TestMCPServerThreadToggleIsScopedAndDefaultsOn(t *testing.T) {
	server := NewMCPServer(&fakeController{}, true)
	first := registerTestThread(t, server)
	secondConfig, err := server.RegisterThread(Access{ThreadID: "second", Workspace: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	second := secondConfig[ServerName].(map[string]any)["url"].(string)

	server.SetThreadEnabled("thread", false)
	if server.ThreadEnabled("thread") || !server.ThreadEnabled("second") {
		t.Fatalf("thread toggles leaked: first=%v second=%v", server.ThreadEnabled("thread"), server.ThreadEnabled("second"))
	}
	firstTools := postRPC(t, first, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})["result"].(map[string]any)["tools"].([]any)
	secondTools := postRPC(t, second, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})["result"].(map[string]any)["tools"].([]any)
	if len(firstTools) != 0 || len(secondTools) != 27 {
		t.Fatalf("thread tool lists = %d, %d; want 0, 27", len(firstTools), len(secondTools))
	}
}

func TestMCPServerRoutesCapabilityScopedCalls(t *testing.T) {
	fake := &fakeController{}
	server := NewMCPServer(fake, true)
	url := registerTestThread(t, server)
	response := postRPC(t, url, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "browser_open", "arguments": map[string]any{"url": "https://example.com"}}})
	if response["error"] != nil {
		t.Fatalf("response = %#v", response)
	}
	if fake.openedURL != "https://example.com" || fake.access.ThreadID != "thread" || fake.access.Workspace != "/repo" {
		t.Fatalf("route = %q %#v", fake.openedURL, fake.access)
	}
	if fake.openOptions.PageID != "" {
		t.Fatalf("open without page_id unexpectedly targeted %q", fake.openOptions.PageID)
	}
	server.UnregisterThread("thread")
	if len(fake.closeThreads) != 1 || fake.closeThreads[0] != "thread" {
		t.Fatalf("closed = %v", fake.closeThreads)
	}
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoked capability status = %d", resp.StatusCode)
	}
}

func TestMCPReadOnlyEvaluationPassesJSONArgumentWithoutCodeInterpolation(t *testing.T) {
	fake := &fakeController{}
	server := NewMCPServer(fake, true)
	url := registerTestThread(t, server)
	response := postRPC(t, url, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "browser_evaluate_readonly", "arguments": map[string]any{"expression": "arg => arg.value", "argument": map[string]any{"value": "safe\") ; document.body.remove(); //"}}}})
	if response["error"] != nil {
		t.Fatalf("response=%#v", response)
	}
	want := `(arg => arg.value)({"value":"safe\") ; document.body.remove(); //"})`
	if fake.readExpression != want {
		t.Fatalf("expression=%q want %q", fake.readExpression, want)
	}
}

func TestMCPReadOnlyEvaluationInvokesZeroArgumentFunction(t *testing.T) {
	fake := &fakeController{}
	server := NewMCPServer(fake, true)
	url := registerTestThread(t, server)
	response := postRPC(t, url, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "browser_evaluate_readonly", "arguments": map[string]any{"expression": "() => document.title"}}})
	if response["error"] != nil {
		t.Fatalf("response=%#v", response)
	}
	if want := `(() => document.title)()`; fake.readExpression != want {
		t.Fatalf("expression=%q want %q", fake.readExpression, want)
	}
}

func TestMCPReadOnlyEvaluationPassesExplicitNullArgument(t *testing.T) {
	fake := &fakeController{}
	server := NewMCPServer(fake, true)
	url := registerTestThread(t, server)
	response := postRPC(t, url, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "browser_evaluate_readonly", "arguments": map[string]any{"expression": "arg => arg === null", "argument": nil}}})
	if response["error"] != nil {
		t.Fatalf("response=%#v", response)
	}
	if want := `(arg => arg === null)(null)`; fake.readExpression != want {
		t.Fatalf("expression=%q want %q", fake.readExpression, want)
	}
}

func TestReadOnlyExpressionLeavesNonFunctionsAlone(t *testing.T) {
	for _, expression := range []string{"document.title", `'text => text'`, "functionality"} {
		if got := readOnlyExpression(expression, nil); got != expression {
			t.Errorf("readOnlyExpression(%q)=%q", expression, got)
		}
	}
}

func registerTestThread(t *testing.T, server *MCPServer) string {
	t.Helper()
	config, err := server.RegisterThread(Access{ThreadID: "thread", Workspace: "/repo", ProjectRoot: "/project"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	spec := config[ServerName].(map[string]any)
	return spec["url"].(string)
}

func postRPC(t *testing.T, url string, request map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(request)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestMCPRequestChecksApplyToEveryMethod locks the request checks onto
// every method the endpoint dispatches, not just tools/call: an Origin
// header or a non-JSON content type is refused before the body is
// decoded, on initialize and tools/list too.
func TestMCPRequestChecksApplyToEveryMethod(t *testing.T) {
	fake := &fakeController{}
	server := NewMCPServer(fake, true)
	url := registerTestThread(t, server)
	bodies := map[string]string{
		"initialize": `{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		"tools/list": `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		"tools/call": `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_open","arguments":{"url":"https://example.com"}}}`,
	}
	refusals := []struct {
		name        string
		contentType string
		headers     map[string]string
		want        int
	}{
		{"cross-origin header", "application/json", map[string]string{"Origin": "https://example.com"}, http.StatusForbidden},
		{"same-origin header", "application/json", map[string]string{"Origin": "http://127.0.0.1:1234"}, http.StatusForbidden},
		{"simple content type", "text/plain;charset=UTF-8", nil, http.StatusUnsupportedMediaType},
		{"form content type", "application/x-www-form-urlencoded", nil, http.StatusUnsupportedMediaType},
		{"absent content type", "", nil, http.StatusUnsupportedMediaType},
	}
	for method, body := range bodies {
		for _, refusal := range refusals {
			resp := postMCPRequest(t, url, refusal.contentType, refusal.headers, body)
			if resp.StatusCode != refusal.want {
				t.Errorf("%s with %s: status = %d, want %d", method, refusal.name, resp.StatusCode, refusal.want)
			}
		}
	}
	if fake.openedURL != "" {
		t.Fatalf("a refused request reached the controller: opened %q", fake.openedURL)
	}

	// The same bodies still work end to end once they declare JSON.
	for method, body := range bodies {
		resp := postMCPRequest(t, url, "application/json", nil, body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", method, resp.StatusCode)
		}
	}
	if fake.openedURL != "https://example.com" {
		t.Fatalf("accepted tools/call did not reach the controller: opened %q", fake.openedURL)
	}
}

// TestMCPRequestChecksRefuseNonLoopbackPeer drives handle through a
// recorder because the peer address is the one input a request over the
// loopback listener cannot vary.
func TestMCPRequestChecksRefuseNonLoopbackPeer(t *testing.T) {
	server := NewMCPServer(&fakeController{}, true)
	endpoint, err := neturl.Parse(registerTestThread(t, server))
	if err != nil {
		t.Fatal(err)
	}
	for _, peer := range []struct {
		remoteAddr string
		want       int
	}{
		{"127.0.0.1:51000", http.StatusOK},
		{"[::1]:51000", http.StatusOK},
		{"192.0.2.10:51000", http.StatusForbidden},
		{"[2001:db8::1]:51000", http.StatusForbidden},
		{"", http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodPost, endpoint.Path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = peer.remoteAddr
		recorder := httptest.NewRecorder()
		server.handle(recorder, request)
		if recorder.Code != peer.want {
			t.Errorf("peer %q: status = %d, want %d", peer.remoteAddr, recorder.Code, peer.want)
		}
	}
}

// TestMCPPreflightIsRefusedWithoutCORSHeaders locks the property the
// content-type check depends on: requiring JSON makes a browser
// preflight first, and the preflight gets nothing it can act on.
func TestMCPPreflightIsRefusedWithoutCORSHeaders(t *testing.T) {
	server := NewMCPServer(&fakeController{}, true)
	url := registerTestThread(t, server)
	request, err := http.NewRequest(http.MethodOptions, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", "https://example.com")
	request.Header.Set("Access-Control-Request-Method", "POST")
	request.Header.Set("Access-Control-Request-Headers", "content-type")
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("preflight status = %d, want 405", resp.StatusCode)
	}
	for _, header := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Methods", "Access-Control-Allow-Headers"} {
		if value := resp.Header.Get(header); value != "" {
			t.Errorf("preflight answered %s: %q", header, value)
		}
	}
}

func TestJSONContentTypeAllowsParametersAndCasing(t *testing.T) {
	for _, accepted := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON", " application/json "} {
		if !jsonContentType(accepted) {
			t.Errorf("jsonContentType(%q) = false", accepted)
		}
	}
	for _, refused := range []string{"", "text/plain", "text/plain;charset=UTF-8", "application/json-patch+json", "multipart/form-data"} {
		if jsonContentType(refused) {
			t.Errorf("jsonContentType(%q) = true", refused)
		}
	}
}

func postMCPRequest(t *testing.T, url, contentType string, headers map[string]string, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}
