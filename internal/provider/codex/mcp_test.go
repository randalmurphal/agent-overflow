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
