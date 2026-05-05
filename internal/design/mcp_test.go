package design

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

func TestMCPServerGetDiagnosticsReturnsRingContents(t *testing.T) {
	server, threadURL, harness := newTestMCPServer(t)

	harness.diagnostics.AppendBatch("thread-mcp", []Diagnostic{{
		Severity: SeverityError,
		Message:  "TypeError: cannot read properties of undefined",
		Source:   "console.error",
	}})
	harness.diagnostics.AppendBatch("thread-mcp", []Diagnostic{{
		Severity: SeverityWarn,
		Message:  "deprecated API",
		Source:   "console.warn",
	}})

	resp := postMCPRequest(t, threadURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_design_diagnostics",
			"arguments": map[string]any{"since_token": 0},
		},
	})

	resultText := extractToolTextResult(t, resp)
	var payload struct {
		Diagnostics []Diagnostic `json:"diagnostics"`
		NextToken   int64        `json:"next_token"`
	}
	if err := json.Unmarshal([]byte(resultText), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Diagnostics) != 2 {
		t.Fatalf("len = %d, want 2", len(payload.Diagnostics))
	}
	if payload.NextToken != 2 {
		t.Errorf("NextToken = %d, want 2", payload.NextToken)
	}

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestMCPServerGetDiagnosticsRespectsSinceToken(t *testing.T) {
	_, threadURL, harness := newTestMCPServer(t)

	firstBatch := harness.diagnostics.AppendBatch("thread-mcp", []Diagnostic{{
		Severity: SeverityWarn,
		Message:  "first",
	}})
	first := firstBatch[0]
	harness.diagnostics.AppendBatch("thread-mcp", []Diagnostic{{
		Severity: SeverityError,
		Message:  "second",
	}})

	resp := postMCPRequest(t, threadURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_design_diagnostics",
			"arguments": map[string]any{"since_token": first.Token},
		},
	})
	var payload struct {
		Diagnostics []Diagnostic `json:"diagnostics"`
		NextToken   int64        `json:"next_token"`
	}
	if err := json.Unmarshal([]byte(extractToolTextResult(t, resp)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Diagnostics) != 1 {
		t.Fatalf("len = %d, want 1", len(payload.Diagnostics))
	}
	if payload.Diagnostics[0].Message != "second" {
		t.Errorf("message = %q, want second", payload.Diagnostics[0].Message)
	}
}

func TestMCPServerReadScreenshotRoundTrip(t *testing.T) {
	server, threadURL, harness := newTestMCPServer(t)

	resultCh := make(chan string, 1)
	go func() {
		resp := postMCPRequest(t, threadURL, map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      "read_screenshot",
				"arguments": map[string]any{},
			},
		})
		resultCh <- extractToolTextResult(t, resp)
	}()

	captureRequest := waitForCaptureEvent(t, harness)
	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a}
	if err := harness.screenshots.Resolve(captureRequest.RequestID, pngBytes); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	select {
	case body := <-resultCh:
		var payload struct {
			PNGBase64 string `json:"png_base64"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("unmarshal screenshot: %v body=%q", err, body)
		}
		decoded, err := base64.StdEncoding.DecodeString(payload.PNGBase64)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !bytes.Equal(decoded, pngBytes) {
			t.Fatalf("png mismatch")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for screenshot result")
	}

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestMCPServerUsesOpaqueThreadToken(t *testing.T) {
	server, threadURL, _ := newTestMCPServer(t)

	if strings.HasSuffix(threadURL, "/mcp/thread-mcp") {
		t.Fatalf("thread URL %q exposes raw thread ID", threadURL)
	}

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

type mcpTestHarness struct {
	diagnostics *DiagnosticBuffer
	screenshots *ScreenshotBroker
	captures    chan ScreenshotRequest
}

func newTestMCPServer(t *testing.T) (*MCPServer, string, *mcpTestHarness) {
	t.Helper()

	st, err := store.New(filepath.Join(t.TempDir(), "design-mcp.db"))
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})

	project := testutil.EnsureProject(t, st, t.TempDir())
	thread := store.Thread{
		ID:            "thread-mcp",
		ProjectID:     project.ID,
		Title:         "Design Thread",
		Provider:      "codex",
		WorkspacePath: t.TempDir(),
		Model:         "gpt-5.4",
		Mode:          "design",
		CreatedAt:     time.Now().UnixMilli(),
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := st.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	captures := make(chan ScreenshotRequest, 4)
	emit := func(eventName string, data any) {
		if eventName != CaptureEventName {
			return
		}
		req, ok := data.(ScreenshotRequest)
		if !ok {
			return
		}
		captures <- req
	}
	diagnostics := NewDiagnosticBuffer(nil)
	screenshots := NewScreenshotBroker(emit)
	reactor := NewReactor(diagnostics, screenshots)
	server := NewMCPServer(reactor)

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
	return server, threadURL, &mcpTestHarness{
		diagnostics: diagnostics,
		screenshots: screenshots,
		captures:    captures,
	}
}

func waitForCaptureEvent(t *testing.T, harness *mcpTestHarness) ScreenshotRequest {
	t.Helper()
	select {
	case req := <-harness.captures:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for capture event")
		return ScreenshotRequest{}
	}
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

func TestMCPServerUnregisterThread(t *testing.T) {
	server, _, _ := newTestMCPServer(t)

	if server.RegisteredThreadCount() != 1 {
		t.Fatalf("count = %d, want 1 after RegisterThread", server.RegisteredThreadCount())
	}

	server.UnregisterThread("thread-mcp")
	if server.RegisteredThreadCount() != 0 {
		t.Fatalf("count = %d, want 0 after UnregisterThread", server.RegisteredThreadCount())
	}

	server.UnregisterThread("thread-mcp")
	if server.RegisteredThreadCount() != 0 {
		t.Fatalf("count = %d, want 0 after double UnregisterThread", server.RegisteredThreadCount())
	}
}

func TestMCPServerUnregisterThreadEmptyID(t *testing.T) {
	server, _, _ := newTestMCPServer(t)

	server.UnregisterThread("")
	server.UnregisterThread("   ")
	if server.RegisteredThreadCount() != 1 {
		t.Fatalf("count = %d, want 1 after empty unregister", server.RegisteredThreadCount())
	}
}

func TestMCPServerRegisterThreadEmptyID(t *testing.T) {
	reactor := NewReactor(NewDiagnosticBuffer(nil), NewScreenshotBroker(func(string, any) {}))
	server := NewMCPServer(reactor)
	t.Cleanup(func() { _ = server.Close() })

	_, err := server.RegisterThread("")
	if err == nil {
		t.Fatal("expected error for empty thread ID")
	}
}

func TestMCPServerRegisterThreadNilReactor(t *testing.T) {
	server := NewMCPServer(nil)
	t.Cleanup(func() { _ = server.Close() })

	_, err := server.RegisterThread("thread-1")
	if err == nil {
		t.Fatal("expected error for nil reactor")
	}
}

func TestToolDefinitions(t *testing.T) {
	tools := toolDefinitions()
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
	if !names[ToolGetDiagnostics] {
		t.Errorf("missing %s tool", ToolGetDiagnostics)
	}
	if !names[ToolReadScreenshot] {
		t.Errorf("missing %s tool", ToolReadScreenshot)
	}
}

// -- HTTP handler edge cases --

func TestMCPServerHandleMethodNotAllowed(t *testing.T) {
	_, threadURL, _ := newTestMCPServer(t)

	resp, err := http.Get(threadURL)
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestMCPServerHandleUnknownToken(t *testing.T) {
	_, threadURL, _ := newTestMCPServer(t)

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

func TestMCPServerHandleInitialize(t *testing.T) {
	_, threadURL, _ := newTestMCPServer(t)

	resp := postMCPRequest(t, threadURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	})

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result type = %T", resp["result"])
	}
	if result["protocolVersion"] != mcpProtocolVersion {
		t.Errorf("protocolVersion: got %v, want %q", result["protocolVersion"], mcpProtocolVersion)
	}
	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("serverInfo type = %T", result["serverInfo"])
	}
	if serverInfo["name"] != "agent-overflow-design" {
		t.Errorf("serverInfo.name: got %v", serverInfo["name"])
	}
}

func TestMCPServerHandleToolsList(t *testing.T) {
	_, threadURL, _ := newTestMCPServer(t)

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

func TestMCPServerHandleUnknownMethod(t *testing.T) {
	_, threadURL, _ := newTestMCPServer(t)

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

func TestMCPServerHandleInvalidJSON(t *testing.T) {
	_, threadURL, _ := newTestMCPServer(t)

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

func TestMCPServerHandleNotificationsInitialized(t *testing.T) {
	_, threadURL, _ := newTestMCPServer(t)

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

func TestMCPServerHandleToolCallUnknownTool(t *testing.T) {
	_, threadURL, _ := newTestMCPServer(t)

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

func TestMCPServerHandleToolCallInvalidParams(t *testing.T) {
	_, threadURL, _ := newTestMCPServer(t)

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

func TestThreadIDForPathMissingPrefix(t *testing.T) {
	server := NewMCPServer(nil)
	_, ok := server.threadIDForPath("/other/path")
	if ok {
		t.Error("expected false for path without /mcp/ prefix")
	}
}

func TestThreadIDForPathEmptyToken(t *testing.T) {
	server := NewMCPServer(nil)
	_, ok := server.threadIDForPath("/mcp/")
	if ok {
		t.Error("expected false for empty token")
	}
}

func TestThreadIDForPathWhitespaceToken(t *testing.T) {
	server := NewMCPServer(nil)
	_, ok := server.threadIDForPath("/mcp/   ")
	if ok {
		t.Error("expected false for whitespace-only token")
	}
}

// Round-trip test for the screenshot teardown path: if the design
// session ends mid-tool-call the agent receives a clean
// `design session ended` JSON-RPC error rather than a stuck connection.
func TestMCPServerReadScreenshotTeardownReleases(t *testing.T) {
	_, threadURL, harness := newTestMCPServer(t)

	var sawError atomic.Bool
	go func() {
		resp := postMCPRequestRaw(t, threadURL, map[string]any{
			"jsonrpc": "2.0",
			"id":      99,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      "read_screenshot",
				"arguments": map[string]any{},
			},
		})
		_, hasError := resp["error"].(map[string]any)
		if hasError {
			sawError.Store(true)
		}
	}()

	// Wait for the capture request to land, then tear down.
	waitForCaptureEvent(t, harness)
	harness.screenshots.TeardownThread("thread-mcp")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sawError.Load() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("teardown did not release pending screenshot tool call")
}

// postMCPRequestRaw is like postMCPRequest but doesn't fail on non-200
// responses or missing result fields — used for negative tests.
func postMCPRequestRaw(t *testing.T, url string, payload any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	return decoded
}

// Ensure the round-trip uses background ctx where needed.
var _ = context.Background
