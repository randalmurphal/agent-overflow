package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/design"
	"agent-overflow/internal/store"
)

func TestDesignMCPServerRenderDesign(t *testing.T) {
	server, threadURL, artifacts := newTestDesignMCPServer(t)

	resp := postMCPRequest(t, threadURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "render_design",
			"arguments": map[string]any{
				"html":  "<html><body>render</body></html>",
				"title": "Homepage",
			},
		},
	})

	resultText := extractToolTextResult(t, resp)
	var payload map[string]string
	if err := json.Unmarshal([]byte(resultText), &payload); err != nil {
		t.Fatalf("unmarshal render result: %v", err)
	}
	if payload["status"] != "rendered" {
		t.Fatalf("status = %q, want rendered", payload["status"])
	}
	if payload["artifactId"] == "" {
		t.Fatal("expected artifactId")
	}

	list, err := artifacts.List("thread-mcp", "render")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("render artifacts len = %d, want 1", len(list))
	}

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestDesignMCPServerPresentOptionsWaitsForChoice(t *testing.T) {
	server, threadURL, _ := newTestDesignMCPServer(t)

	resultCh := make(chan string, 1)
	go func() {
		resp := postMCPRequest(t, threadURL, map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "tools/call",
			"params": map[string]any{
				"name": "present_options",
				"arguments": map[string]any{
					"prompt": "Pick one",
					"options": []map[string]any{
						{
							"id":          "a",
							"title":       "Minimal",
							"description": "Minimal layout",
							"html":        "<html><body>A</body></html>",
						},
						{
							"id":          "b",
							"title":       "Bold",
							"description": "Bold layout",
							"html":        "<html><body>B</body></html>",
						},
					},
				},
			},
		})
		resultCh <- extractToolTextResult(t, resp)
	}()

	request := waitForPendingMCPRequest(t, server.reactor)
	if err := server.reactor.ChooseOption("thread-mcp", request.RequestID, "b"); err != nil {
		t.Fatalf("ChooseOption() error = %v", err)
	}

	select {
	case resultText := <-resultCh:
		var result design.ChoiceResult
		if err := json.Unmarshal([]byte(resultText), &result); err != nil {
			t.Fatalf("unmarshal choice result: %v", err)
		}
		if result.Chosen != "b" {
			t.Fatalf("Chosen = %q, want b", result.Chosen)
		}
		if result.Title != "Bold" {
			t.Fatalf("Title = %q, want Bold", result.Title)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for choice result")
	}

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestBuildThreadParamsIncludesMCPServers(t *testing.T) {
	params := buildThreadParams(Config{
		MCPServers: map[string]any{
			"design": map[string]any{"url": "http://127.0.0.1:1234/mcp/thread"},
		},
	})

	config, ok := params["config"].(map[string]any)
	if !ok {
		t.Fatalf("config type = %T, want map[string]any", params["config"])
	}
	if config["mcp_servers"] == nil {
		t.Fatal("expected mcp_servers config override")
	}
}

func TestDesignMCPServerUsesOpaqueThreadToken(t *testing.T) {
	server, threadURL, _ := newTestDesignMCPServer(t)

	if strings.HasSuffix(threadURL, "/mcp/thread-mcp") {
		t.Fatalf("thread URL %q exposes raw thread ID", threadURL)
	}

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func newTestDesignMCPServer(t *testing.T) (*DesignMCPServer, string, *design.ArtifactStore) {
	t.Helper()

	st, err := store.New(filepath.Join(t.TempDir(), "design-mcp.db"))
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})

	thread := store.Thread{
		ID:              "thread-mcp",
		Title:           "Design Thread",
		Provider:        "codex",
		WorkspacePath:   t.TempDir(),
		ProjectPath:     t.TempDir(),
		Model:           "gpt-5.4",
		InteractionMode: "design",
		CreatedAt:       time.Now().UnixMilli(),
		UpdatedAt:       time.Now().UnixMilli(),
	}
	if err := st.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	artifacts := design.NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), st)
	reactor := design.NewReactor(artifacts, func(string, any) {})
	server := NewDesignMCPServer(reactor)

	config, err := server.RegisterThread(thread.ID)
	if err != nil {
		t.Fatalf("RegisterThread() error = %v", err)
	}

	var threadURL string
	for _, value := range config {
		entry, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("config entry type = %T", value)
		}
		threadURL, _ = entry["url"].(string)
	}
	if threadURL == "" {
		t.Fatal("expected MCP thread URL")
	}

	t.Cleanup(func() {
		_ = server.Close()
	})
	return server, threadURL, artifacts
}

func postMCPRequest(t *testing.T, url string, payload any) map[string]any {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s error = %v", url, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, string(data))
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v body=%s", err, string(data))
	}
	return decoded
}

func extractToolTextResult(t *testing.T, response map[string]any) string {
	t.Helper()

	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map[string]any", response["result"])
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %#v", result["content"])
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("first content type = %T", content[0])
	}
	text, _ := first["text"].(string)
	return text
}

// -- UnregisterThread / RegisteredThreadCount tests --

func TestDesignMCPServerUnregisterThread(t *testing.T) {
	server, _, _ := newTestDesignMCPServer(t)

	if server.RegisteredThreadCount() != 1 {
		t.Fatalf("count = %d, want 1 after RegisterThread", server.RegisteredThreadCount())
	}

	server.UnregisterThread("thread-mcp")
	if server.RegisteredThreadCount() != 0 {
		t.Fatalf("count = %d, want 0 after UnregisterThread", server.RegisteredThreadCount())
	}

	// Unregistering the same thread again is a no-op.
	server.UnregisterThread("thread-mcp")
	if server.RegisteredThreadCount() != 0 {
		t.Fatalf("count = %d, want 0 after double UnregisterThread", server.RegisteredThreadCount())
	}
}

func TestDesignMCPServerUnregisterThreadEmptyID(t *testing.T) {
	server, _, _ := newTestDesignMCPServer(t)

	// Empty and whitespace-only IDs are silently ignored.
	server.UnregisterThread("")
	server.UnregisterThread("   ")
	if server.RegisteredThreadCount() != 1 {
		t.Fatalf("count = %d, want 1 after empty unregister", server.RegisteredThreadCount())
	}
}

func TestDesignMCPServerRegisteredThreadCountMultiple(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "design-mcp-count.db"))
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for _, id := range []string{"thread-1", "thread-2"} {
		if err := st.CreateThread(store.Thread{
			ID:              id,
			Title:           id,
			Provider:        "codex",
			WorkspacePath:   t.TempDir(),
			ProjectPath:     t.TempDir(),
			Model:           "gpt-5.4",
			InteractionMode: "design",
			CreatedAt:       time.Now().UnixMilli(),
			UpdatedAt:       time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("CreateThread(%s) error = %v", id, err)
		}
	}

	artifacts := design.NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), st)
	reactor := design.NewReactor(artifacts, func(string, any) {})
	server := NewDesignMCPServer(reactor)
	t.Cleanup(func() { _ = server.Close() })

	if _, err := server.RegisterThread("thread-1"); err != nil {
		t.Fatalf("RegisterThread(thread-1) error = %v", err)
	}
	if _, err := server.RegisterThread("thread-2"); err != nil {
		t.Fatalf("RegisterThread(thread-2) error = %v", err)
	}
	if server.RegisteredThreadCount() != 2 {
		t.Fatalf("count = %d, want 2", server.RegisteredThreadCount())
	}

	server.UnregisterThread("thread-1")
	if server.RegisteredThreadCount() != 1 {
		t.Fatalf("count = %d, want 1 after unregistering one", server.RegisteredThreadCount())
	}
}

// -- RegisterThread error cases --

func TestDesignMCPServerRegisterThreadEmptyID(t *testing.T) {
	reactor := design.NewReactor(nil, func(string, any) {})
	server := NewDesignMCPServer(reactor)
	t.Cleanup(func() { _ = server.Close() })

	_, err := server.RegisterThread("")
	if err == nil {
		t.Fatal("expected error for empty thread ID")
	}
}

func TestDesignMCPServerRegisterThreadNilReactor(t *testing.T) {
	server := NewDesignMCPServer(nil)
	t.Cleanup(func() { _ = server.Close() })

	_, err := server.RegisterThread("thread-1")
	if err == nil {
		t.Fatal("expected error for nil reactor")
	}
}

// -- designToolDefinitions test --

func TestDesignToolDefinitions(t *testing.T) {
	tools := designToolDefinitions()
	if len(tools) != 2 {
		t.Fatalf("len = %d, want 2", len(tools))
	}

	names := map[string]bool{}
	for _, tool := range tools {
		name, ok := tool["name"].(string)
		if !ok {
			t.Fatal("tool missing name")
		}
		names[name] = true
		if tool["description"] == nil {
			t.Errorf("tool %q has nil description", name)
		}
		if tool["inputSchema"] == nil {
			t.Errorf("tool %q has nil inputSchema", name)
		}
	}
	if !names["render_design"] {
		t.Error("missing render_design tool")
	}
	if !names["present_options"] {
		t.Error("missing present_options tool")
	}
}

// -- HTTP handler edge cases --

func TestDesignMCPServerHandleMethodNotAllowed(t *testing.T) {
	_, threadURL, _ := newTestDesignMCPServer(t)

	resp, err := http.Get(threadURL)
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestDesignMCPServerHandleUnknownToken(t *testing.T) {
	server, threadURL, _ := newTestDesignMCPServer(t)
	_ = server

	// Replace the last path segment with a bogus token.
	parts := strings.Split(threadURL, "/")
	parts[len(parts)-1] = "bogus-token"
	bogusURL := strings.Join(parts, "/")

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	})
	resp, err := http.Post(bogusURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestDesignMCPServerHandleInitialize(t *testing.T) {
	_, threadURL, _ := newTestDesignMCPServer(t)

	resp := postMCPRequest(t, threadURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	})

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result type = %T", resp["result"])
	}
	if result["protocolVersion"] != designMCPProtocolVersion {
		t.Errorf("protocolVersion: got %v, want %q", result["protocolVersion"], designMCPProtocolVersion)
	}
	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("serverInfo type = %T", result["serverInfo"])
	}
	if serverInfo["name"] != "agent-overflow-design" {
		t.Errorf("serverInfo.name: got %v", serverInfo["name"])
	}
}

func TestDesignMCPServerHandleToolsList(t *testing.T) {
	_, threadURL, _ := newTestDesignMCPServer(t)

	resp := postMCPRequest(t, threadURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result type = %T", resp["result"])
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools type = %T", result["tools"])
	}
	if len(tools) != 2 {
		t.Errorf("tools len = %d, want 2", len(tools))
	}
}

func TestDesignMCPServerHandleUnknownMethod(t *testing.T) {
	_, threadURL, _ := newTestDesignMCPServer(t)

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/list",
	})
	resp, err := http.Post(threadURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v body=%s", err, data)
	}
	rpcErr, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error field, got %v", decoded)
	}
	if rpcErr["code"].(float64) != -32601 {
		t.Errorf("error code: got %v, want -32601", rpcErr["code"])
	}
}

func TestDesignMCPServerHandleInvalidJSON(t *testing.T) {
	_, threadURL, _ := newTestDesignMCPServer(t)

	resp, err := http.Post(threadURL, "application/json", bytes.NewReader([]byte(`not json`)))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v body=%s", err, data)
	}
	rpcErr, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error field, got %v", decoded)
	}
	if rpcErr["code"].(float64) != -32700 {
		t.Errorf("error code: got %v, want -32700", rpcErr["code"])
	}
}

func TestDesignMCPServerHandleNotificationsInitialized(t *testing.T) {
	_, threadURL, _ := newTestDesignMCPServer(t)

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	resp, err := http.Post(threadURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestDesignMCPServerHandleToolCallUnknownTool(t *testing.T) {
	_, threadURL, _ := newTestDesignMCPServer(t)

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "nonexistent_tool",
			"arguments": map[string]any{},
		},
	})
	resp, err := http.Post(threadURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, data)
	}
	rpcErr, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error field, got %v", decoded)
	}
	if rpcErr["code"].(float64) != -32602 {
		t.Errorf("error code: got %v, want -32602", rpcErr["code"])
	}
}

func TestDesignMCPServerHandleToolCallInvalidParams(t *testing.T) {
	_, threadURL, _ := newTestDesignMCPServer(t)

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  "not an object",
	})
	resp, err := http.Post(threadURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, data)
	}
	rpcErr, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error field, got %v", decoded)
	}
	if rpcErr["code"].(float64) != -32602 {
		t.Errorf("error code: got %v, want -32602", rpcErr["code"])
	}
}

func TestDesignMCPServerHandleRenderDesignInvalidArgs(t *testing.T) {
	_, threadURL, _ := newTestDesignMCPServer(t)

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "render_design",
			"arguments": "not an object",
		},
	})
	resp, err := http.Post(threadURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, data)
	}
	rpcErr, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error field for invalid render args, got %v", decoded)
	}
	if rpcErr["code"].(float64) != -32602 {
		t.Errorf("error code: got %v, want -32602", rpcErr["code"])
	}
}

func TestDesignMCPServerHandlePresentOptionsInvalidArgs(t *testing.T) {
	_, threadURL, _ := newTestDesignMCPServer(t)

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "present_options",
			"arguments": "not an object",
		},
	})
	resp, err := http.Post(threadURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, data)
	}
	rpcErr, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error field for invalid present_options args, got %v", decoded)
	}
	if rpcErr["code"].(float64) != -32602 {
		t.Errorf("error code: got %v, want -32602", rpcErr["code"])
	}
}

// -- rawID tests --

func TestRawIDEmpty(t *testing.T) {
	result := rawID(nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestRawIDInvalidJSON(t *testing.T) {
	result := rawID(json.RawMessage(`not json`))
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestRawIDNumber(t *testing.T) {
	result := rawID(json.RawMessage(`42`))
	if result != float64(42) {
		t.Errorf("expected 42, got %v", result)
	}
}

func TestRawIDString(t *testing.T) {
	result := rawID(json.RawMessage(`"req-1"`))
	if result != "req-1" {
		t.Errorf("expected req-1, got %v", result)
	}
}

// -- threadIDForPath tests --

func TestThreadIDForPathMissingPrefix(t *testing.T) {
	server := NewDesignMCPServer(nil)
	_, ok := server.threadIDForPath("/other/path")
	if ok {
		t.Error("expected false for path without /mcp/ prefix")
	}
}

func TestThreadIDForPathEmptyToken(t *testing.T) {
	server := NewDesignMCPServer(nil)
	_, ok := server.threadIDForPath("/mcp/")
	if ok {
		t.Error("expected false for empty token")
	}
}

func TestThreadIDForPathWhitespaceToken(t *testing.T) {
	server := NewDesignMCPServer(nil)
	_, ok := server.threadIDForPath("/mcp/   ")
	if ok {
		t.Error("expected false for whitespace-only token")
	}
}

func waitForPendingMCPRequest(t *testing.T, reactor *design.Reactor) design.DesignOptionsRequest {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if request, ok := reactor.PendingRequest("thread-mcp"); ok {
			return request
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for pending design request")
	return design.DesignOptionsRequest{}
}
