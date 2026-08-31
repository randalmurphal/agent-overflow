package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
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
func (f *fakeController) EvaluateReadOnly(_ context.Context, _ Access, _ string, expression string) (any, string, error) {
	f.readExpression = expression
	return nil, "", nil
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
