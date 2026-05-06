package design

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
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

// makeTestPNG returns a PNG of width × height filled with a solid
// color. Used to feed the fake Capturer with predictable bytes the
// slicer can decode + tile.
func makeTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 200, G: 200, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func TestMCPServerReadScreenshotRoundTrip(t *testing.T) {
	server, threadURL, harness := newTestMCPServer(t)

	// 1280×1600 → exactly 2 tiles of 800px height, no clip.
	pngBytes := makeTestPNG(t, 1280, 1600)
	harness.capturer.setFn(func(ctx context.Context, threadID string) ([]byte, error) {
		return pngBytes, nil
	})

	resp := postMCPRequest(t, threadURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "read_screenshot",
			"arguments": map[string]any{},
		},
	})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing: %#v", resp)
	}
	content, ok := result["content"].([]any)
	if !ok {
		t.Fatalf("content type = %T, want []any", result["content"])
	}
	if len(content) != 2 {
		t.Fatalf("content blocks = %d, want 2 (one per tile, no clip note)", len(content))
	}
	for i, block := range content {
		b, ok := block.(map[string]any)
		if !ok {
			t.Fatalf("block %d type = %T", i, block)
		}
		if b["type"] != "image" {
			t.Errorf("block %d type = %v, want image", i, b["type"])
		}
		if b["mimeType"] != "image/jpeg" {
			t.Errorf("block %d mimeType = %v, want image/jpeg", i, b["mimeType"])
		}
		// The data must be a parseable JPEG of the expected slice
		// height.
		raw, err := base64.StdEncoding.DecodeString(b["data"].(string))
		if err != nil {
			t.Fatalf("block %d base64: %v", i, err)
		}
		jpg, err := jpeg.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("block %d not a valid jpeg: %v", i, err)
		}
		if jpg.Bounds().Dx() != 1280 || jpg.Bounds().Dy() != 800 {
			t.Errorf("block %d dims = %dx%d, want 1280x800",
				i, jpg.Bounds().Dx(), jpg.Bounds().Dy())
		}
	}

	// Capturer was invoked exactly once with the right thread.
	if got := harness.capturer.threadIDs(); len(got) != 1 || got[0] != "thread-mcp" {
		t.Fatalf("Capturer threadIDs = %v, want [thread-mcp]", got)
	}

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// TestMCPServerReadScreenshotClippedAppendsTextNote pins that when
// the captured PNG is taller than MaxScreenshotTiles × tile height,
// the slicer marks Clipped and the MCP layer appends a trailing
// `text` block flagging the clip — that's how the agent learns
// there's more page below.
func TestMCPServerReadScreenshotClippedAppendsTextNote(t *testing.T) {
	_, threadURL, harness := newTestMCPServer(t)

	// 9 tiles' worth → MaxScreenshotTiles=8 emitted, 1 clipped.
	pngBytes := makeTestPNG(t, 1280, 9*800)
	harness.capturer.setFn(func(ctx context.Context, threadID string) ([]byte, error) {
		return pngBytes, nil
	})

	resp := postMCPRequest(t, threadURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "read_screenshot",
			"arguments": map[string]any{},
		},
	})
	result, _ := resp["result"].(map[string]any)
	content, ok := result["content"].([]any)
	if !ok {
		t.Fatalf("content type = %T", result["content"])
	}
	if len(content) != 8+1 {
		t.Fatalf("content blocks = %d, want 9 (8 tiles + clip note)", len(content))
	}
	// Earlier blocks are still image blocks.
	for i := 0; i < 8; i++ {
		b, _ := content[i].(map[string]any)
		if b["type"] != "image" {
			t.Errorf("block %d type = %v, want image", i, b["type"])
		}
	}
	last, _ := content[len(content)-1].(map[string]any)
	if last["type"] != "text" {
		t.Errorf("trailing block type = %v, want text", last["type"])
	}
	text, _ := last["text"].(string)
	if !strings.Contains(strings.ToLower(text), "tile") {
		t.Errorf("trailing text should mention tiling, got %q", text)
	}
}

// TestMCPServerReadScreenshotCapturerErrorSurfaces pins the error
// path: a Capturer that fails (no internet, headless install busted,
// browser crashed) must surface a clean MCP tool error rather than
// hang the tool call.
//
// MCP convention is to return a `result` with `isError: true` and a
// text content block carrying the message — that's what writeToolError
// produces and what both providers parse as a tool failure on the
// agent side. A JSON-RPC `error` field would terminate the whole
// session.
func TestMCPServerReadScreenshotCapturerErrorSurfaces(t *testing.T) {
	_, threadURL, harness := newTestMCPServer(t)

	harness.capturer.setFn(func(ctx context.Context, threadID string) ([]byte, error) {
		return nil, fmt.Errorf("simulated headless launch failure")
	})

	resp := postMCPRequestRaw(t, threadURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "read_screenshot",
			"arguments": map[string]any{},
		},
	})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result field, got %#v", resp)
	}
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("expected isError=true, got result=%#v", result)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("expected at least one content block carrying the error message")
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" {
		t.Fatalf("first block type = %v, want text", first["type"])
	}
	text, _ := first["text"].(string)
	if !strings.Contains(text, "simulated headless launch failure") {
		t.Errorf("error text = %q, want to mention the underlying failure", text)
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

// fakeCapturer implements Capturer with a swappable function. Tests
// pre-load the function before issuing the MCP tool call. The
// function runs synchronously with the inbound MCP request, so the
// happy-path tests don't have to wire any goroutines.
type fakeCapturer struct {
	mu        sync.Mutex
	fn        func(ctx context.Context, threadID string) ([]byte, error)
	seenIDs   []string
}

func (f *fakeCapturer) Capture(ctx context.Context, threadID string) ([]byte, error) {
	f.mu.Lock()
	f.seenIDs = append(f.seenIDs, threadID)
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return nil, fmt.Errorf("fake capturer: no fn set")
	}
	return fn(ctx, threadID)
}

func (f *fakeCapturer) setFn(fn func(ctx context.Context, threadID string) ([]byte, error)) {
	f.mu.Lock()
	f.fn = fn
	f.mu.Unlock()
}

func (f *fakeCapturer) threadIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.seenIDs))
	copy(out, f.seenIDs)
	return out
}

type mcpTestHarness struct {
	diagnostics *DiagnosticBuffer
	capturer    *fakeCapturer
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

	diagnostics := NewDiagnosticBuffer(nil)
	capturer := &fakeCapturer{}
	reactor := NewReactor(diagnostics, capturer)
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
		capturer:    capturer,
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
	reactor := NewReactor(NewDiagnosticBuffer(nil), &fakeCapturer{})
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

// postMCPRequestRaw is like postMCPRequest but doesn't fail on
// non-200 responses or missing result fields — used for negative
// tests.
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
