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
	"agent-overflow/internal/mcpargs"
)

func TestToolCallEnvelopeAllowsMetadataButArgumentsStayStrict(t *testing.T) {
	call, err := DecodeToolCall(json.RawMessage(`{"name":"test","arguments":{"path":"file"},"_meta":{"progressToken":3,"callId":"call"},"extension":{"value":true}}`))
	if err != nil || call.Name != "test" {
		t.Fatalf("metadata-bearing call: %+v, %v", call, err)
	}
	// The envelope tolerates metadata; the arguments inside it do not.
	// internal/mcpargs owns that half and tests its refusals.
	var args struct {
		Path string `json:"path"`
	}
	if err := mcpargs.Decode(call.Arguments, &args); err != nil || args.Path != "file" {
		t.Fatalf("arguments: %+v, %v", args, err)
	}
	if err := mcpargs.Decode(json.RawMessage(`{"path":"file","_meta":{}}`), &args); err == nil {
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
	// The handler parks until the test has read a keepalive the ticker
	// produced, so the stream's promise is what is asserted rather than
	// one duration outrunning another.
	release := make(chan struct{})
	url := streamedCallServer(t, func(w http.ResponseWriter, req Request) {
		<-release
		WriteResult(w, req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": "done"}}})
	})
	resp := postToolCall(t, url, "application/json, text/event-stream")
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type %q", got)
	}
	reader := bufio.NewReader(resp.Body)
	// The first keepalive opens the stream before the handler runs; the
	// second is the ticker's, which is what keeps a long call alive.
	for keepalives := 0; keepalives < 2; {
		line, err := reader.ReadString('\n')
		switch {
		case err != nil:
			t.Fatalf("reading the stream: %v", err)
		case line == ": keepalive\n":
			keepalives++
		case line == "\n":
		default:
			t.Fatalf("stream carried %q before the handler answered", line)
		}
	}
	close(release)
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
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

// afterResponseServer registers one thread whose handler defers work with
// AfterResponse, and returns the capability URL.
func afterResponseServer(t *testing.T, handler func(http.ResponseWriter, context.Context, Request)) string {
	t.Helper()
	server := New("test-tools", "", func(string) []map[string]any { return nil },
		func(w http.ResponseWriter, ctx context.Context, req Request, _ string) { handler(w, ctx, req) })
	t.Cleanup(func() { _ = server.Close() })
	config, err := server.RegisterThread("thread", "access")
	if err != nil {
		t.Fatal(err)
	}
	return config["test-tools"].(map[string]any)["url"].(string)
}

// readEvents reads server-sent lines off a response body without waiting for
// the handler to return, so a test can assert on an answer whose call is
// still running.
func readEvents(t *testing.T, body io.Reader) <-chan string {
	t.Helper()
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		reader := bufio.NewReader(body)
		for {
			text, err := reader.ReadString('\n')
			if text != "" {
				lines <- text
			}
			if err != nil {
				return
			}
		}
	}()
	return lines
}

// awaitData waits for the stream's final message event and returns its JSON.
func awaitData(t *testing.T, lines <-chan string, what string) string {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case text, ok := <-lines:
			if !ok {
				t.Fatalf("%s: the stream ended without a response", what)
			}
			if data, found := strings.CutPrefix(text, "data: "); found {
				return strings.TrimSpace(data)
			}
		case <-deadline:
			t.Fatalf("%s: no response arrived", what)
		}
	}
}

// TestAfterResponseWorkWaitsForTheAnswerToReachTheClient pins the ordering a
// deferred step depends on: the answer is on the wire before the work runs,
// so work that ends the call (deleting the thread that asked) cannot take
// the response down with it.
func TestAfterResponseWorkWaitsForTheAnswerToReachTheClient(t *testing.T) {
	release := make(chan struct{})
	ran := make(chan bool, 1)
	url := afterResponseServer(t, func(w http.ResponseWriter, ctx context.Context, req Request) {
		AfterResponse(ctx, func(delivered bool) {
			<-release
			ran <- delivered
		})
		WriteResult(w, req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": "answered"}}})
	})
	resp := postToolCall(t, url, "application/json, text/event-stream")
	data := awaitData(t, readEvents(t, resp.Body), "deferred work holding the call")
	if !strings.Contains(data, "answered") {
		t.Fatalf("final event %q", data)
	}
	select {
	case delivered := <-ran:
		t.Fatalf("deferred work ran before the client read the answer (delivered=%v)", delivered)
	default:
	}
	close(release)
	select {
	case delivered := <-ran:
		if !delivered {
			t.Fatal("a response the client read reported delivered=false")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("deferred work never ran")
	}
}

// TestAfterResponseReportsAnUndeliveredAnswer covers the other half: a client
// that is gone before the response is written leaves the work to decide, and
// nothing may be recorded as read.
func TestAfterResponseReportsAnUndeliveredAnswer(t *testing.T) {
	ran := make(chan bool, 1)
	url := afterResponseServer(t, func(w http.ResponseWriter, ctx context.Context, req Request) {
		AfterResponse(ctx, func(delivered bool) { ran <- delivered })
		// Answer only once the client is provably gone.
		<-ctx.Done()
		WriteResult(w, req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": "answered"}}})
	})
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"slow","arguments":{}}}`))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	lines := readEvents(t, resp.Body)
	select {
	case text, ok := <-lines:
		if !ok || !strings.HasPrefix(text, ": keepalive") {
			t.Fatalf("stream did not open (%q, open=%v)", text, ok)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the stream never opened")
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case delivered := <-ran:
		if delivered {
			t.Fatal("an abandoned call reported its answer as delivered")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("deferred work never ran for an abandoned call")
	}
}
