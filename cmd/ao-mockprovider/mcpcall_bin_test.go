package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

// The path segment and header value below stand in for the per-thread
// token and credentials a real built-in MCP server hands the provider.
// Every test in this file asserts they never leave the mock process.
const (
	mcpTestToken      = "mcp-path-token-must-not-leak"
	mcpTestHeaderName = "X-AO-Mock-Test"
	mcpTestHeaderVal  = "mcp-header-token-must-not-leak"
)

// mcpTestServer is a minimal MCP streamable-HTTP endpoint. Tool names
// select the answer shape so one server covers every outcome an mcpCall
// step has to frame.
type mcpTestServer struct {
	*httptest.Server

	mu        sync.Mutex
	callArgs  json.RawMessage
	callTool  string
	gotHeader string
	// initialized records the handshake the client must complete before
	// tools/call.
	initialized bool
}

func startMcpTestServer(t *testing.T) *mcpTestServer {
	t.Helper()
	s := &mcpTestServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/"+mcpTestToken, s.handle)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *mcpTestServer) url() string { return s.Server.URL + "/mcp/" + mcpTestToken }

func (s *mcpTestServer) snapshot() (tool string, args string, header string, initialized bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callTool, string(s.callArgs), s.gotHeader, s.initialized
}

func (s *mcpTestServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.gotHeader = r.Header.Get(mcpTestHeaderName)
	s.mu.Unlock()

	switch req.Method {
	case "initialize":
		s.mu.Lock()
		s.initialized = false
		s.mu.Unlock()
		writeMcpJSON(w, req.ID, `{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"test","version":"1.0.0"}}`)
	case "notifications/initialized":
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "tools/call":
		s.mu.Lock()
		s.callTool, s.callArgs = req.Params.Name, req.Params.Arguments
		s.mu.Unlock()
		s.answerCall(w, r, req.ID, req.Params.Name)
	default:
		writeMcpError(w, req.ID, -32601, "method not found")
	}
}

func (s *mcpTestServer) answerCall(w http.ResponseWriter, r *http.Request, id json.RawMessage, tool string) {
	switch tool {
	case "json_ok":
		writeMcpJSON(w, id, `{"content":[{"type":"text","text":"noted: turn-1"},{"type":"text","text":"second block"}]}`)
	case "sse_ok":
		// The streaming shape the thread tools use for a parked call:
		// comment keepalives, then one `event: message` carrying the
		// JSON-RPC response.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		for range 3 {
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
		fmt.Fprintf(w, "event: message\ndata: %s\n\n",
			fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"streamed answer"}]}}`, string(id)))
		flusher.Flush()
	case "tool_refuses":
		writeMcpJSON(w, id, `{"isError":true,"content":[{"type":"text","text":"[thread_tools_disabled] Thread tools are disabled for this conversation."}]}`)
	case "rpc_error":
		writeMcpError(w, id, -32602, "invalid tools/call params")
	case "slow":
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	default:
		writeMcpError(w, id, -32601, "unknown tool")
	}
}

func writeMcpJSON(w http.ResponseWriter, id json.RawMessage, result string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, string(id), result)
}

func writeMcpError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%q}}`, string(id), code, message)
}

// mcpControl starts an in-test control server that assigns sc and
// collects every report, the way the harness backend does.
func mcpControl(t *testing.T, sc *scenario.Scenario) (*control.Server, <-chan control.Report, []string) {
	t.Helper()
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal scenario: %v", err)
	}
	if _, err := scenario.Parse(raw); err != nil {
		t.Fatalf("test scenario invalid: %v", err)
	}
	reports := make(chan control.Report, 256)
	srv, err := control.NewServer(control.ServerConfig{
		Resolve: func(control.Registration) (control.Assignment, error) {
			return control.Assignment{ScenarioName: sc.Name, ScenarioJSON: raw}, nil
		},
		OnReport: func(_ control.MockInfo, rep control.Report) { reports <- rep },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv, reports, []string{
		control.EnvAddr + "=" + srv.Addr(),
		control.EnvToken + "=" + srv.Token(),
	}
}

// claudeMcpConfig renders the --mcp-config payload the app writes for an
// HTTP MCP server (internal/provider/claude/options.go#mcpConfigForCLI).
func claudeMcpConfig(name, url string) string {
	return mustJSON(map[string]any{"mcpServers": map[string]any{
		name: map[string]any{
			"type":    "http",
			"url":     url,
			"headers": map[string]any{mcpTestHeaderName: mcpTestHeaderVal},
			"timeout": 1_200_000,
		},
	}})
}

// codexMcpStartParams renders the thread/start params AO sends, whose
// `config.mcp_servers` bag carries the same endpoint for Codex
// (internal/provider/codex/session_helpers.go).
func codexMcpStartParams(name, url string) string {
	return mustJSON(map[string]any{"config": map[string]any{"mcp_servers": map[string]any{
		name: map[string]any{
			"url":              url,
			"http_headers":     map[string]any{mcpTestHeaderName: mcpTestHeaderVal},
			"tool_timeout_sec": 1200,
		},
	}}})
}

func mcpCallStep(server, tool, args string) scenario.Step {
	step := &scenario.McpCallStep{Server: server, Tool: tool, TimeoutMs: 5_000}
	if args != "" {
		step.Args = json.RawMessage(args)
	}
	return scenario.Step{McpCall: step}
}

// assertNoMcpSecrets is the launch-evidence rule applied to the call
// path: the endpoint, its token and its headers may never appear on the
// provider wire, in a control report, or in the mock's stderr.
func assertNoMcpSecrets(t *testing.T, p *mockProc, reports []control.Report, url string) {
	t.Helper()
	secrets := []string{mcpTestToken, mcpTestHeaderVal, url}
	for _, line := range p.all {
		for _, secret := range secrets {
			if strings.Contains(line, secret) {
				t.Fatalf("mock wrote %q on the provider wire: %s", secret, line)
			}
		}
	}
	for _, secret := range secrets {
		if strings.Contains(p.stderr.String(), secret) {
			t.Fatalf("mock logged %q to stderr", secret)
		}
	}
	for _, rep := range reports {
		encoded := mustJSON(rep)
		for _, secret := range secrets {
			if strings.Contains(encoded, secret) {
				t.Fatalf("control report carries %q: %s", secret, encoded)
			}
		}
	}
}

// drainReports collects every report posted up to and including the
// first of kind, so a test can assert on the report AND check the whole
// set for leaked secrets.
func drainReports(t *testing.T, reports <-chan control.Report, kind string) ([]control.Report, control.Report) {
	t.Helper()
	var seen []control.Report
	deadline := time.After(testTimeout)
	for {
		select {
		case rep := <-reports:
			seen = append(seen, rep)
			if rep.Kind == kind {
				return seen, rep
			}
		case <-deadline:
			t.Fatalf("no %s report within %s; saw %+v", kind, testTimeout, seen)
		}
	}
}

// TestClaudeMcpCallRunsRealToolAndFramesWire is the end-to-end proof for
// the Claude half: the mock resolves the server out of the --mcp-config
// the app spawned it with, performs a REAL tools/call over HTTP, and
// frames the call and its answer as the CLI does, through the
// application's own parser.
func TestClaudeMcpCallRunsRealToolAndFramesWire(t *testing.T) {
	server := startMcpTestServer(t)
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "claude-mcp",
		Provider: scenario.ProviderClaude,
		Turns: []scenario.Turn{{Steps: []scenario.Step{
			// ${TURN} proves the args are substituted like an emit line.
			mcpCallStep("ao-thread-tools", "json_ok", `{"text":"turn-${TURN}"}`),
			// ${MCP_RESULT} / ${MCP_TOOL_USE_ID} reach the steps that follow.
			{Emit: &scenario.EmitStep{Lines: []string{
				`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg-sum","role":"assistant"}}}`,
				`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
				`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"tool ${MCP_TOOL_USE_ID} said the first line"}}}`,
				`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
				`{"type":"stream_event","event":"message_stop","data":{"type":"message_stop"}}`,
				`{"type":"assistant","message":{"id":"msg-sum","role":"assistant","content":[{"type":"text","text":"tool ${MCP_TOOL_USE_ID} said the first line"}]}}`,
				`{"type":"result","subtype":"success","is_error":false}`,
			}}},
		}}},
	}
	_, reports, env := mcpControl(t, sc)
	args := append(append([]string(nil), claudeSessionArgs...),
		"--mcp-config", claudeMcpConfig("ao-thread-tools", server.url()))
	p := startMock(t, args, env, t.TempDir())

	p.send(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"alpha"}]}}`)
	p.expectLineContaining(`"subtype":"init"`, testTimeout)
	p.expectLineContaining(`"isReplay":true`, testTimeout)
	useLine := p.expectLineContaining(`"tool_use"`, testTimeout)
	resultLine := p.expectLineContaining(`"tool_result"`, testTimeout)
	p.expectLineContaining(`"type":"result"`, testTimeout)

	tool, gotArgs, header, initialized := server.snapshot()
	if tool != "json_ok" {
		t.Fatalf("tools/call name = %q", tool)
	}
	if gotArgs != `{"text":"turn-1"}` {
		t.Fatalf("tools/call arguments = %s, want the substituted object", gotArgs)
	}
	if header != mcpTestHeaderVal {
		t.Fatalf("configured header not sent: %q", header)
	}
	if !initialized {
		t.Fatal("client never sent notifications/initialized")
	}

	// The application's real parser reads the frames.
	parser := claude.NewParser()
	t.Cleanup(parser.Close)
	start := parseOneEvent(t, parser, useLine, provider.EventToolStart)
	var startMeta struct {
		ToolName string            `json:"toolName"`
		MCP      map[string]string `json:"mcp"`
		Input    struct {
			Text string `json:"text"`
		} `json:"input"`
	}
	if err := json.Unmarshal(start.Meta, &startMeta); err != nil {
		t.Fatalf("decode tool-start meta: %v", err)
	}
	if startMeta.ToolName != "MCP/json_ok" ||
		startMeta.MCP["server"] != "ao-thread-tools" || startMeta.MCP["tool"] != "json_ok" ||
		startMeta.Input.Text != "turn-1" {
		t.Fatalf("tool-start meta = %+v", startMeta)
	}
	complete := parseOneEvent(t, parser, resultLine, provider.EventToolComplete)
	if complete.ItemID != start.ItemID {
		t.Fatalf("tool_result correlates to %q, tool_use was %q", complete.ItemID, start.ItemID)
	}
	if complete.Content != "noted: turn-1\nsecond block" {
		t.Fatalf("tool_result content = %q, want the joined text blocks", complete.Content)
	}
	if strings.Contains(mustJSON(complete.Meta), `"is_error":true`) {
		t.Fatalf("successful call reported is_error: %s", complete.Meta)
	}

	seen, report := drainReports(t, reports, control.ReportMcpResult)
	if report.Detail != "ao-thread-tools/json_ok" || report.IsError ||
		report.Result != "noted: turn-1\nsecond block" {
		t.Fatalf("mcp_result report = %+v", report)
	}

	p.closeStdinAndExpectExit(0, testTimeout)
	// ${MCP_TOOL_USE_ID} is the id the frames carried.
	if !strings.Contains(strings.Join(p.all, "\n"), "tool "+start.ItemID+" said the first line") {
		t.Fatalf("later step did not see ${MCP_TOOL_USE_ID}=%q", start.ItemID)
	}
	assertNoMcpSecrets(t, p, seen, server.url())
	validateClaudeFrames(t, p.all)
}

// parseOneEvent runs one mock frame through the real Claude parser and
// returns the single event of the wanted kind.
func parseOneEvent(t *testing.T, parser *claude.Parser, line string, kind provider.EventKind) provider.ProviderEvent {
	t.Helper()
	events, err := parser.ParseLine("thread-mcp", []byte(line))
	if err != nil {
		t.Fatalf("real Claude parser rejected %s: %v", line, err)
	}
	for _, evt := range events {
		if evt.Kind == kind {
			return evt
		}
	}
	t.Fatalf("no %s event from %s (got %+v)", kind, line, events)
	return provider.ProviderEvent{}
}

// TestClaudeMcpCallFailuresReachTheWire covers every way a call can fail.
// None of them is a scenario failure: each becomes an error tool_result
// the app can render and an mcp_result report a spec can assert on.
func TestClaudeMcpCallFailuresReachTheWire(t *testing.T) {
	cases := []struct {
		name       string
		server     string
		tool       string
		timeoutMs  int
		wantResult string
		// dead closes the endpoint before the call, so the mock meets a
		// real transport failure: the one whose Go error text embeds the
		// endpoint it dialled.
		dead bool
	}{
		{name: "sse transport", server: "ao-thread-tools", tool: "sse_ok", timeoutMs: 5_000, wantResult: "streamed answer"},
		{name: "tool refuses", server: "ao-thread-tools", tool: "tool_refuses", timeoutMs: 5_000, wantResult: "[thread_tools_disabled] Thread tools are disabled for this conversation."},
		{name: "jsonrpc error", server: "ao-thread-tools", tool: "rpc_error", timeoutMs: 5_000, wantResult: "JSON-RPC error -32602"},
		{name: "unknown server", server: "ao-nonexistent", tool: "json_ok", timeoutMs: 5_000, wantResult: `MCP server "ao-nonexistent" is not configured for this session`},
		{name: "timeout", server: "ao-thread-tools", tool: "slow", timeoutMs: 300, wantResult: "timed out"},
		{name: "transport error", server: "ao-thread-tools", tool: "json_ok", timeoutMs: 5_000, wantResult: "MCP call ao-thread-tools/json_ok failed", dead: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := startMcpTestServer(t)
			if tc.dead {
				server.Close()
			}
			sc := &scenario.Scenario{
				Version:  scenario.CurrentVersion,
				Name:     "claude-mcp-fail",
				Provider: scenario.ProviderClaude,
				Turns: []scenario.Turn{{Steps: []scenario.Step{
					{McpCall: &scenario.McpCallStep{Server: tc.server, Tool: tc.tool, TimeoutMs: tc.timeoutMs, ToolUseID: "tu-fail"}},
					{Emit: &scenario.EmitStep{Lines: []string{`{"type":"result","subtype":"success","is_error":false}`}}},
				}}},
			}
			_, reports, env := mcpControl(t, sc)
			args := append(append([]string(nil), claudeSessionArgs...),
				"--mcp-config", claudeMcpConfig("ao-thread-tools", server.url()))
			p := startMock(t, args, env, t.TempDir())

			p.send(userLine)
			p.expectLineContaining(`"tool_use"`, testTimeout)
			resultLine := p.expectLineContaining(`"tool_result"`, testTimeout)

			parser := claude.NewParser()
			t.Cleanup(parser.Close)
			complete := parseOneEvent(t, parser, resultLine, provider.EventToolComplete)
			if !strings.Contains(complete.Content, tc.wantResult) {
				t.Fatalf("tool_result content = %q, want %q", complete.Content, tc.wantResult)
			}
			wantError := tc.name != "sse transport"
			if got := strings.Contains(mustJSON(complete.Meta), `"is_error":true`); got != wantError {
				t.Fatalf("tool_result is_error = %v, want %v (meta %s)", got, wantError, complete.Meta)
			}

			seen, report := drainReports(t, reports, control.ReportMcpResult)
			if report.IsError != wantError || !strings.Contains(report.Result, tc.wantResult) {
				t.Fatalf("mcp_result report = %+v", report)
			}
			if report.Detail != tc.server+"/"+tc.tool {
				t.Fatalf("mcp_result detail = %q", report.Detail)
			}
			p.closeStdinAndExpectExit(0, testTimeout)
			assertNoMcpSecrets(t, p, seen, server.url())
		})
	}
}

// TestClaudeMcpCallInterruptAbortsTheHTTPCall pins the interrupt
// contract: the in-flight call is cancelled, no result frame settles the
// tool row (the interrupted turn's terminal frame does), and the report
// still says what happened.
func TestClaudeMcpCallInterruptAbortsTheHTTPCall(t *testing.T) {
	server := startMcpTestServer(t)
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "claude-mcp-interrupt",
		Provider: scenario.ProviderClaude,
		Turns: []scenario.Turn{{Steps: []scenario.Step{
			{McpCall: &scenario.McpCallStep{Server: "ao-thread-tools", Tool: "slow", TimeoutMs: 30_000, ToolUseID: "tu-parked"}},
			{Emit: &scenario.EmitStep{Lines: []string{`{"mock":"must-not-run"}`}}},
		}}},
	}
	_, reports, env := mcpControl(t, sc)
	args := append(append([]string(nil), claudeSessionArgs...),
		"--mcp-config", claudeMcpConfig("ao-thread-tools", server.url()))
	p := startMock(t, args, env, t.TempDir())

	p.send(userLine)
	p.expectLineContaining(`"tool_use"`, testTimeout)
	p.send(`{"type":"control_request","request_id":"stop-mcp","request":{"subtype":"interrupt"}}`)
	p.expectLineContaining(`"request_id":"stop-mcp"`, testTimeout)
	terminal := p.expectLineContaining(`"type":"result"`, testTimeout)
	if !strings.Contains(terminal, `"terminal_reason":"aborted_streaming"`) {
		t.Fatalf("interrupted terminal frame = %q", terminal)
	}

	seen, report := drainReports(t, reports, control.ReportMcpResult)
	if !report.IsError || !strings.Contains(report.Result, "aborted by turn interrupt") {
		t.Fatalf("mcp_result report = %+v", report)
	}
	p.closeStdinAndExpectExit(0, testTimeout)
	for _, line := range p.all {
		if strings.Contains(line, "tool_result") {
			t.Fatalf("aborted call still framed a tool_result: %s", line)
		}
		if strings.Contains(line, "must-not-run") {
			t.Fatalf("interrupt did not skip the remaining steps: %s", line)
		}
	}
	assertNoMcpSecrets(t, p, seen, server.url())
}

// TestCodexMcpCallFramesItemNotifications is the Codex half: the server
// comes out of the thread/start config bag, and the call is framed as an
// mcpToolCall item pair the application's Codex parser reads.
func TestCodexMcpCallFramesItemNotifications(t *testing.T) {
	server := startMcpTestServer(t)
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "codex-mcp",
		Provider: scenario.ProviderCodex,
		Turns: []scenario.Turn{{Steps: []scenario.Step{
			mcpCallStep("ao-thread-tools", "json_ok", `{"text":"${USER_INPUT}"}`),
			{Emit: &scenario.EmitStep{Lines: []string{
				`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}`,
			}}},
		}}},
	}
	_, reports, env := mcpControl(t, sc)
	p := startMock(t, []string{"app-server"}, env, t.TempDir())

	p.send(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":` + codexMcpStartParams("ao-thread-tools", server.url()) + `}`)
	p.expectLineContaining(`"id":1`, testTimeout)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"turn/start","params":{"threadId":"mock-codex-thread","input":[{"type":"text","text":"alpha"}]}}`)
	p.expectLineContaining(`"id":2`, testTimeout)

	startedLine := p.expectLineContaining(`"item/started"`, testTimeout)
	completedLine := p.expectLineContaining(`"item/completed"`, testTimeout)
	p.expectLineContaining(`"turn/completed"`, testTimeout)

	if tool, gotArgs, header, initialized := server.snapshot(); tool != "json_ok" ||
		gotArgs != `{"text":"alpha"}` || header != mcpTestHeaderVal || !initialized {
		t.Fatalf("codex call reached the server as tool=%q args=%s header=%q initialized=%v",
			tool, gotArgs, header, initialized)
	}

	start := classifyOneCodexEvent(t, startedLine, provider.EventToolStart)
	var startMeta struct {
		ToolName string            `json:"toolName"`
		MCP      map[string]string `json:"mcp"`
		Input    struct {
			Text string `json:"text"`
		} `json:"input"`
	}
	if err := json.Unmarshal(start.Meta, &startMeta); err != nil {
		t.Fatalf("decode tool-start meta: %v", err)
	}
	if startMeta.ToolName != "MCP/json_ok" || startMeta.MCP["server"] != "ao-thread-tools" ||
		startMeta.MCP["tool"] != "json_ok" || startMeta.Input.Text != "alpha" {
		t.Fatalf("codex tool-start meta = %+v", startMeta)
	}
	complete := classifyOneCodexEvent(t, completedLine, provider.EventToolComplete)
	if complete.ItemID != start.ItemID {
		t.Fatalf("item/completed id %q does not match item/started %q", complete.ItemID, start.ItemID)
	}
	if !strings.Contains(complete.Content, "noted: turn-1") || !strings.Contains(complete.Content, "second block") {
		t.Fatalf("item/completed content = %q", complete.Content)
	}
	if !strings.Contains(completedLine, `"status":"completed"`) {
		t.Fatalf("item/completed status = %s", completedLine)
	}

	seen, report := drainReports(t, reports, control.ReportMcpResult)
	if report.IsError || report.Detail != "ao-thread-tools/json_ok" {
		t.Fatalf("mcp_result report = %+v", report)
	}
	p.closeStdinAndExpectExit(0, testTimeout)
	assertNoMcpSecrets(t, p, seen, server.url())
}

// TestCodexMcpCallFailuresFrameTheRightHalf pins the Codex distinction a
// single error shape would lose: a tool that RAN and refused keeps its
// text in `result`, while a call that never reached one carries
// `error.message`. Both are `status: failed`.
func TestCodexMcpCallFailuresFrameTheRightHalf(t *testing.T) {
	cases := []struct {
		name       string
		server     string
		tool       string
		wantResult bool
		wantText   string
	}{
		{"tool refuses", "ao-thread-tools", "tool_refuses", true, "[thread_tools_disabled]"},
		{"jsonrpc error", "ao-thread-tools", "rpc_error", false, "JSON-RPC error -32602"},
		{"unknown server", "ao-nonexistent", "json_ok", false, "is not configured for this session"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := startMcpTestServer(t)
			sc := &scenario.Scenario{
				Version:  scenario.CurrentVersion,
				Name:     "codex-mcp-fail",
				Provider: scenario.ProviderCodex,
				Turns: []scenario.Turn{{Steps: []scenario.Step{
					mcpCallStep(tc.server, tc.tool, ""),
				}}},
			}
			_, reports, env := mcpControl(t, sc)
			p := startMock(t, []string{"app-server"}, env, t.TempDir())

			p.send(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":` + codexMcpStartParams("ao-thread-tools", server.url()) + `}`)
			p.expectLineContaining(`"id":1`, testTimeout)
			p.send(`{"jsonrpc":"2.0","id":2,"method":"turn/start","params":{"threadId":"mock-codex-thread","input":[]}}`)
			p.expectLineContaining(`"id":2`, testTimeout)
			p.expectLineContaining(`"item/started"`, testTimeout)
			completedLine := p.expectLineContaining(`"item/completed"`, testTimeout)

			if !strings.Contains(completedLine, `"status":"failed"`) {
				t.Fatalf("failed call framed as %s", completedLine)
			}
			if got := strings.Contains(completedLine, `"error"`); got == tc.wantResult {
				t.Fatalf("wrong failure half for %s: %s", tc.name, completedLine)
			}
			complete := classifyOneCodexEvent(t, completedLine, provider.EventToolComplete)
			if !strings.Contains(complete.Content, tc.wantText) {
				t.Fatalf("item/completed content = %q, want %q", complete.Content, tc.wantText)
			}

			seen, report := drainReports(t, reports, control.ReportMcpResult)
			if !report.IsError || !strings.Contains(report.Result, tc.wantText) {
				t.Fatalf("mcp_result report = %+v", report)
			}
			p.closeStdinAndExpectExit(0, testTimeout)
			assertNoMcpSecrets(t, p, seen, server.url())
		})
	}
}

// classifyOneCodexEvent runs one mock notification through the real Codex
// classifier and returns the single event of the wanted kind.
func classifyOneCodexEvent(t *testing.T, line string, kind provider.EventKind) provider.ProviderEvent {
	t.Helper()
	var notification struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &notification); err != nil {
		t.Fatalf("decode notification %s: %v", line, err)
	}
	events := codex.ClassifyNotification("thread-mcp", notification.Method, notification.Params)
	for _, evt := range events {
		if evt.Kind == kind {
			return evt
		}
	}
	t.Fatalf("no %s event from %s (got %+v)", kind, line, events)
	return provider.ProviderEvent{}
}
