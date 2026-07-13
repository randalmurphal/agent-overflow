package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newWorkflowMCPTestServer(t *testing.T, handler func(context.Context, string, json.RawMessage) (string, error)) *httptest.Server {
	t.Helper()
	server, err := New(Config{
		Token: "test-token", Dispatcher: NewDispatcher(), EventBus: NewEventBus(0), MCPToolCall: handler,
	})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(server.buildHTTPServer().Handler)
}

func postWorkflowMCP(t *testing.T, serverURL, body, token, threadID string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, serverURL+"/mcp/workflows", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if threadID != "" {
		request.Header.Set(MCPThreadIDHeader, threadID)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeMCPTestResponse(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode response %q: %v", data, err)
	}
	return decoded
}

func TestWorkflowMCPRequiresBearerToken(t *testing.T) {
	server := newWorkflowMCPTestServer(t, func(context.Context, string, json.RawMessage) (string, error) {
		return "unexpected", nil
	})
	defer server.Close()
	for _, token := range []string{"", "wrong"} {
		response := postWorkflowMCP(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, token, "thread-1")
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q status = %d, want 401", token, response.StatusCode)
		}
		response.Body.Close()
	}
}

func TestWorkflowMCPInitializeAndToolSchema(t *testing.T) {
	server := newWorkflowMCPTestServer(t, func(context.Context, string, json.RawMessage) (string, error) { return "ok", nil })
	defer server.Close()

	initialize := postWorkflowMCP(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, "test-token", "thread-1")
	initialized := decodeMCPTestResponse(t, initialize)
	if initialized["error"] != nil {
		t.Fatalf("initialize response = %+v", initialized)
	}
	list := postWorkflowMCP(t, server.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "test-token", "thread-1")
	decoded := decodeMCPTestResponse(t, list)
	result := decoded["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "enqueue_workflow_run" {
		t.Fatalf("tool name = %v", tool["name"])
	}
	description := tool["description"].(string)
	for _, phrase := range []string{"confirmation card", "nothing is enqueued", "only when the user explicitly asks", "never proactively"} {
		if !strings.Contains(description, phrase) {
			t.Fatalf("description %q missing %q", description, phrase)
		}
	}
	schema := tool["inputSchema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	if len(properties) != 5 || properties["project"] == nil || properties["base_branch"] == nil {
		t.Fatalf("input schema properties = %+v", properties)
	}
}

func TestWorkflowMCPToolCallRoutesThreadAndSurfacesToolErrors(t *testing.T) {
	var gotThread string
	server := newWorkflowMCPTestServer(t, func(_ context.Context, threadID string, arguments json.RawMessage) (string, error) {
		gotThread = threadID
		if strings.Contains(string(arguments), `"goal":"bad"`) {
			return "", errors.New("$.seeds.ticket must be a string")
		}
		return "proposal recorded", nil
	})
	defer server.Close()

	happy := postWorkflowMCP(t, server.URL, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"enqueue_workflow_run","arguments":{"project":"p","workflow":"w","goal":"good"}}}`, "test-token", "thread-7")
	happyResult := decodeMCPTestResponse(t, happy)["result"].(map[string]any)
	if gotThread != "thread-7" || happyResult["isError"] != nil {
		t.Fatalf("thread=%q result=%+v", gotThread, happyResult)
	}

	bad := postWorkflowMCP(t, server.URL, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"enqueue_workflow_run","arguments":{"project":"p","workflow":"w","goal":"bad"}}}`, "test-token", "thread-7")
	badResult := decodeMCPTestResponse(t, bad)["result"].(map[string]any)
	if badResult["isError"] != true {
		t.Fatalf("error result = %+v", badResult)
	}
	content := badResult["content"].([]any)[0].(map[string]any)["text"].(string)
	if content != "$.seeds.ticket must be a string" {
		t.Fatalf("tool error text = %q", content)
	}
}
