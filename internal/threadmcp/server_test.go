package threadmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
)

func TestToolCallEnvelopeAllowsMetadataButArgumentsStayStrict(t *testing.T) {
	call, err := DecodeToolCall(json.RawMessage(`{"name":"test","arguments":{"path":"file"},"_meta":{"progressToken":3,"callId":"call"},"extension":{"value":true}}`))
	if err != nil || call.Name != "test" {
		t.Fatalf("metadata-bearing call: %+v, %v", call, err)
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := DecodeArgs(call.Arguments, &args); err != nil || args.Path != "file" {
		t.Fatalf("arguments: %+v, %v", args, err)
	}
	if err := DecodeArgs(json.RawMessage(`{"path":"file","_meta":{}}`), &args); err == nil {
		t.Fatal("metadata inside tool arguments bypassed the closed schema")
	}
	for _, invalid := range []string{`null`, `{}`, `[]`, `{"name":3}`, `{"name":"test"} {}`} {
		if _, err := DecodeToolCall(json.RawMessage(invalid)); err == nil {
			t.Errorf("accepted invalid envelope: %s", invalid)
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

func TestCapabilitiesRotateAndCloseCannotReopen(t *testing.T) {
	server := New("test-tools", "", func(string) []map[string]any { return nil }, nil)
	t.Cleanup(func() { _ = server.Close() })
	first, err := server.RegisterThread("thread", "first")
	if err != nil {
		t.Fatal(err)
	}
	server.SetThreadEnabled("thread", false)
	second, err := server.RegisterThread("thread", "second")
	if err != nil {
		t.Fatal(err)
	}
	if first["test-tools"].(map[string]any)["url"] == second["test-tools"].(map[string]any)["url"] {
		t.Fatal("capability not rotated")
	}
	server.RevokeThread("thread", "first")
	if !server.HasThread("thread") || server.ThreadEnabled("thread") {
		t.Fatal("stale cleanup revoked replacement or reset toggle")
	}
	server.RevokeThread("thread", "second")
	if server.HasThread("thread") {
		t.Fatal("capability not revoked")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := server.RegisterThread("thread", "third"); err == nil {
		t.Fatal("closed server reopened")
	}
}

func TestArgumentErrorsNameTheFieldWithoutEchoingValues(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"count":"secret-value"}`, `"count" must be an integer`},
		{`{"typo":"secret-value"}`, `Unknown argument "typo"`},
		{`null`, `must be a JSON object`},
		{`{"count":`, `Invalid argument JSON`},
		{`{} {}`, `extra JSON`},
	} {
		var args struct {
			Count int `json:"count"`
		}
		err := DecodeArgs(json.RawMessage(tc.raw), &args)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("%s: %v", tc.raw, err)
		}
	}
}

// streamedCallServer registers one thread on a server whose handler waits for
// release before answering, and returns the capability URL.
func streamedCallServer(t *testing.T, handler func(http.ResponseWriter, Request)) string {
	t.Helper()
	server := New("test-tools", "", func(string) []map[string]any { return nil }, func(w http.ResponseWriter, _ context.Context, req Request, _ string) { handler(w, req) })
	t.Cleanup(func() { _ = server.Close() })
	config, err := server.RegisterThread("thread", "access")
	if err != nil {
		t.Fatal(err)
	}
	return config["test-tools"].(map[string]any)["url"].(string)
}

func postToolCall(t *testing.T, url, accept string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"slow","arguments":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestStreamedCallKeepsAliveAndDeliversTheResultAsTheFinalEvent(t *testing.T) {
	previous := callKeepaliveInterval
	callKeepaliveInterval = 10 * time.Millisecond
	t.Cleanup(func() { callKeepaliveInterval = previous })
	url := streamedCallServer(t, func(w http.ResponseWriter, req Request) {
		time.Sleep(80 * time.Millisecond)
		WriteResult(w, req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": "done"}}})
	})
	resp := postToolCall(t, url, "application/json, text/event-stream")
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type %q", got)
	}
	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != ": keepalive\n" {
		t.Fatalf("stream did not open with a keepalive: %q, %v", first, err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if strings.Count(body, ": keepalive\n") < 2 {
		t.Fatalf("expected keepalives while the handler ran:\n%s", body)
	}
	_, data, ok := strings.Cut(body, "event: message\ndata: ")
	if !ok || !strings.HasSuffix(data, "\n\n") {
		t.Fatalf("no final message event:\n%s", body)
	}
	var reply struct {
		ID     int `json:"id"`
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &reply); err != nil || reply.ID != 7 || len(reply.Result.Content) != 1 || reply.Result.Content[0].Text != "done" {
		t.Fatalf("final event %q: %+v, %v", data, reply, err)
	}
	if strings.Contains(data, "keepalive") {
		t.Fatalf("keepalive landed inside the final event:\n%s", body)
	}
}

func TestStreamedCallWithNoResponseBecomesAnError(t *testing.T) {
	url := streamedCallServer(t, func(http.ResponseWriter, Request) {})
	resp := postToolCall(t, url, "text/event-stream")
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_, data, ok := strings.Cut(string(raw), "event: message\ndata: ")
	if !ok || !strings.Contains(data, `"error"`) || !strings.Contains(data, `"id":7`) {
		t.Fatalf("silent handler did not become a JSON-RPC error:\n%s", raw)
	}
}

func TestPlainJSONClientStillGetsAJSONBody(t *testing.T) {
	url := streamedCallServer(t, func(w http.ResponseWriter, req Request) {
		WriteResult(w, req.ID, map[string]any{"content": []map[string]any{}})
	})
	for _, accept := range []string{"", "application/json", "*/*"} {
		resp := postToolCall(t, url, accept)
		if got := resp.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Accept %q: content type %q", accept, got)
		}
		var reply struct {
			ID int `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil || reply.ID != 7 {
			t.Fatalf("Accept %q: %+v, %v", accept, reply, err)
		}
	}
}

// postRequest sends one JSON-RPC request to a capability URL and returns
// the decoded top-level reply.
func postRequest(t *testing.T, url, body string) map[string]json.RawMessage {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

// TestInstructionsFuncReplacesTheFixedTextPerHandshake covers the optional
// hook a server whose guide depends on the ACCESS needs: the text is
// recomputed on every initialize, so a change reaches a running session's
// next handshake without re-registering the thread.
func TestInstructionsFuncReplacesTheFixedTextPerHandshake(t *testing.T) {
	server := New("test-tools", "fixed text", func(string) []map[string]any { return nil }, nil)
	t.Cleanup(func() { _ = server.Close() })
	config, err := server.RegisterThread("thread", "access-1")
	if err != nil {
		t.Fatal(err)
	}
	url := config["test-tools"].(map[string]any)["url"].(string)
	const handshake = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`

	instructions := func(reply map[string]json.RawMessage) string {
		t.Helper()
		var result struct {
			Instructions string `json:"instructions"`
		}
		if err := json.Unmarshal(reply["result"], &result); err != nil {
			t.Fatal(err)
		}
		return result.Instructions
	}

	if got := instructions(postRequest(t, url, handshake)); got != "fixed text" {
		t.Fatalf("instructions = %q, want the fixed text", got)
	}

	calls := 0
	server.SetInstructionsFunc(func(access string) string {
		calls++
		return "computed for " + access
	})
	if got := instructions(postRequest(t, url, handshake)); got != "computed for access-1" {
		t.Fatalf("instructions = %q, want the computed text", got)
	}
	if got := instructions(postRequest(t, url, handshake)); got != "computed for access-1" {
		t.Fatalf("second handshake = %q", got)
	}
	if calls != 2 {
		t.Fatalf("instructions were computed %d times, want one per handshake", calls)
	}
}

// TestDisabledErrorCarriesTheOwnersCode covers the second hook: a server
// that is switched off refuses with its own documented code, so the model
// reads a capability that is off rather than a broken tool.
func TestDisabledErrorCarriesTheOwnersCode(t *testing.T) {
	server := New("test-tools", "", func(string) []map[string]any { return nil },
		func(w http.ResponseWriter, _ context.Context, req Request, _ string) {
			WriteResult(w, req.ID, map[string]any{"content": []map[string]any{}})
		})
	t.Cleanup(func() { _ = server.Close() })
	config, err := server.RegisterThread("thread", "access")
	if err != nil {
		t.Fatal(err)
	}
	url := config["test-tools"].(map[string]any)["url"].(string)
	const call = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x","arguments":{}}}`

	server.SetDisabledError(errorsx.Public("feature_off", "Turn it on in settings.", nil))
	server.SetThreadEnabled("thread", false)
	reply := postRequest(t, url, call)
	body := string(reply["result"])
	if !strings.Contains(body, "feature_off") || !strings.Contains(body, "Turn it on in settings.") {
		t.Fatalf("refusal did not carry the owner's code: %s", body)
	}

	// Without an override the default refusal still stands, so a server
	// that sets none is unchanged.
	plain := New("plain-tools", "", func(string) []map[string]any { return nil }, nil)
	t.Cleanup(func() { _ = plain.Close() })
	plainConfig, err := plain.RegisterThread("thread", "access")
	if err != nil {
		t.Fatal(err)
	}
	plain.SetThreadEnabled("thread", false)
	plainBody := string(postRequest(t, plainConfig["plain-tools"].(map[string]any)["url"].(string), call)["result"])
	if !strings.Contains(plainBody, "disabled") {
		t.Fatalf("default refusal = %s", plainBody)
	}
}
