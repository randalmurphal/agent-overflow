package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

type fakeController struct {
	openedURL    string
	access       Access
	closeThreads []string
}

func (f *fakeController) Open(_ context.Context, a Access, u string, _ OpenOptions) (PageInfo, error) {
	f.access = a
	f.openedURL = u
	return PageInfo{ID: "p", URL: u}, nil
}
func (f *fakeController) OpenFile(context.Context, Access, string, OpenOptions) (PageInfo, error) {
	return PageInfo{}, nil
}
func (f *fakeController) Pages(context.Context, Access) ([]PageInfo, error) { return []PageInfo{}, nil }
func (f *fakeController) ClosePage(context.Context, Access, string) error   { return nil }
func (f *fakeController) Snapshot(context.Context, Access, string) (Snapshot, error) {
	return Snapshot{}, nil
}
func (f *fakeController) Screenshot(context.Context, Access, ScreenshotOptions) ([]byte, error) {
	return []byte("jpg"), nil
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
	if len(tools) != 13 {
		t.Fatalf("enabled tools = %d, want 13", len(tools))
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
