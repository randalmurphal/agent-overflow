package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// mcpProtocolVersion is the version this client initializes with. It
// matches what internal/threadmcp answers; a server that negotiates
// another version still gets a well-formed handshake.
const mcpProtocolVersion = "2025-03-26"

// mcpTarget is one configured MCP endpoint, as the app handed it to this
// process at spawn. It stays inside the mock: nothing here reaches a
// report, a log line or a wire frame (see the launch-evidence rule in
// AGENTS.md).
type mcpTarget struct {
	url     string
	headers map[string]string
}

// redact renders an error without the endpoint. A Go HTTP error embeds
// the URL it dialled, and a dial failure below it names the same
// host:port. Both carry the per-thread token's address into text that
// reaches stderr, the provider wire and a control report. Configured
// header values are redacted for the same reason.
func (t mcpTarget) redact(err error) string {
	text := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		// Drops the quoted URL the *url.Error prefix carries.
		text = urlErr.Err.Error()
	}
	replacements := []string{t.url}
	if parsed, perr := url.Parse(t.url); perr == nil {
		replacements = append(replacements, parsed.Host, parsed.Path)
	}
	for _, value := range t.headers {
		replacements = append(replacements, value)
	}
	for _, secret := range replacements {
		if secret != "" && secret != "/" {
			text = strings.ReplaceAll(text, secret, "<mcp endpoint>")
		}
	}
	return text
}

// mcpCall carries one resolved scenario call through the adapters: the
// identity the wire frames correlate on, the arguments as sent, and the
// outcome once the HTTP round trip is done.
type mcpCall struct {
	server    string
	tool      string
	toolUseID string
	// args is the substituted tools/call arguments object, always valid
	// JSON (`{}` when the step named none).
	args json.RawMessage
	// text is the tool's joined text content, or the failure text when
	// isError is set. Never empty for a failure.
	text    string
	isError bool
	// toolAnswered distinguishes a tool that RAN and reported failure
	// (`isError: true` in its own result) from a call that never reached
	// it. Codex frames the two differently; Claude does not.
	toolAnswered bool
}

// mcpCallSeq numbers minted tool_use ids so two calls in one process
// never collide.
var mcpCallSeq atomic.Int64

// runMcpCall performs a real MCP tools/call and frames it on the
// provider wire. The wire order is the real client's: the tool_use (or
// item/started) goes out BEFORE the call, so the app sees a running tool
// row for as long as the tool takes, and the result frame settles it.
//
// Nothing here fails a scenario. A server the app did not configure, a
// transport error, a timeout, a JSON-RPC error and a tool result with
// `isError: true` all reach the wire as an error tool result and the
// control channel as an mcp_result report, which is what a spec asserts
// a refusal on.
func (e *engine) runMcpCall(vars scenario.Vars, turn int, step *scenario.McpCallStep) {
	call := mcpCall{
		server:    vars.Substitute(step.Server),
		tool:      vars.Substitute(step.Tool),
		toolUseID: vars.Substitute(step.ToolUseID),
		args:      json.RawMessage("{}"),
	}
	if len(step.Args) > 0 {
		call.args = json.RawMessage(vars.Substitute(string(step.Args)))
	}
	if call.toolUseID == "" {
		call.toolUseID = "mcp-" + strconv.Itoa(turn) + "-" + strconv.FormatInt(mcpCallSeq.Add(1), 10)
	}

	e.adapter.writeMcpToolUse(vars, call)

	target, ok := e.adapter.mcpTarget(call.server)
	if !ok {
		call.text = fmt.Sprintf("MCP server %q is not configured for this session", call.server)
		call.isError = true
		e.finishMcpCall(vars, turn, call, false)
		return
	}

	timeout := time.Duration(step.TimeoutMs) * time.Millisecond
	if step.TimeoutMs <= 0 {
		timeout = time.Duration(scenario.DefaultMcpCallTimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// An interrupt aborts the in-flight HTTP call: a parked tool call
	// must not outlive the turn that made it.
	abort := e.turnAbortSignal(turn)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-abort:
			cancel()
		case <-done:
		}
	}()

	text, callErr := mcpToolCall(ctx, target, call.tool, call.args)
	switch {
	case callErr == nil:
		call.text = text
		call.toolAnswered = true
	default:
		var toolErr mcpToolResultError
		if errors.As(callErr, &toolErr) {
			// The tool answered and said it failed: its own text is the
			// answer the agent sees.
			call.text = toolErr.text
			call.toolAnswered = true
		} else if ctx.Err() != nil && e.turnAborted(turn) {
			call.text = "MCP call aborted by turn interrupt"
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			call.text = fmt.Sprintf("MCP call %s/%s timed out after %s", call.server, call.tool, timeout)
		} else {
			call.text = fmt.Sprintf("MCP call %s/%s failed: %s", call.server, call.tool, target.redact(callErr))
		}
		call.isError = true
	}
	e.finishMcpCall(vars, turn, call, e.turnAborted(turn))
}

// finishMcpCall writes the result frame (unless the turn was
// interrupted, whose terminal sequence settles the row instead), binds
// the call's answer for later steps of the turn, and reports it.
func (e *engine) finishMcpCall(vars scenario.Vars, turn int, call mcpCall, aborted bool) {
	if !aborted {
		e.adapter.writeMcpToolResult(vars, call)
	}
	// Bind for the steps that follow. The caller's map is the one the
	// rest of this step list reads; turnVars carries the binding to
	// snapshots taken later in the same turn (a control "emit", a nested
	// repeat body).
	vars["MCP_RESULT"] = call.text
	vars["MCP_TOOL_USE_ID"] = call.toolUseID
	if turn > 0 {
		e.setTurnVars(turn, scenario.Vars{"MCP_RESULT": call.text, "MCP_TOOL_USE_ID": call.toolUseID})
	}
	if call.isError {
		log.Printf("mcpCall %s/%s failed: %s", call.server, call.tool, call.text)
	}
	e.rep.report(control.Report{
		Kind:    control.ReportMcpResult,
		Turn:    turn,
		Detail:  call.server + "/" + call.tool,
		Result:  call.text,
		IsError: call.isError,
	})
}

// mcpToolResultError is a tool that ran and reported failure
// (`isError: true`), as opposed to a call that could not be made. Its
// text is the tool's own, which is what a spec asserting a refusal
// reads.
type mcpToolResultError struct{ text string }

func (e mcpToolResultError) Error() string { return e.text }

// mcpToolCall runs one streamable-HTTP MCP session against target:
// initialize, notifications/initialized, then tools/call. Returns the
// result's joined text content.
func mcpToolCall(ctx context.Context, target mcpTarget, tool string, args json.RawMessage) (string, error) {
	client := &http.Client{}

	_, session, err := mcpRequest(ctx, client, target, "", mcpRPC{
		ID:     1,
		Method: "initialize",
		Params: map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "ao-mockprovider", "version": mockVersionNumber},
		},
	})
	if err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}

	if _, _, err := mcpRequest(ctx, client, target, session, mcpRPC{Method: "notifications/initialized", Notification: true}); err != nil {
		return "", fmt.Errorf("notifications/initialized: %w", err)
	}

	result, _, err := mcpRequest(ctx, client, target, session, mcpRPC{
		ID:     2,
		Method: "tools/call",
		Params: map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return "", err
	}
	return mcpResultText(result)
}

// mcpRPC is one outbound JSON-RPC message. A notification carries no id
// and expects no body back.
type mcpRPC struct {
	ID           int
	Method       string
	Params       any
	Notification bool
}

// mcpRequest posts one JSON-RPC message and returns the `result` object
// of the response (nil for a notification) plus any session id the
// server assigned. A JSON-RPC error response becomes a Go error.
func mcpRequest(ctx context.Context, client *http.Client, target mcpTarget, session string, msg mcpRPC) (json.RawMessage, string, error) {
	body := map[string]any{"jsonrpc": "2.0", "method": msg.Method}
	if !msg.Notification {
		body["id"] = msg.ID
	}
	if msg.Params != nil {
		body["params"] = msg.Params
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, session, fmt.Errorf("encode %s: %w", msg.Method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.url, bytes.NewReader(payload))
	if err != nil {
		return nil, session, fmt.Errorf("build %s request: %w", msg.Method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for key, value := range target.headers {
		req.Header.Set(key, value)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, session, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()
	if assigned := resp.Header.Get("Mcp-Session-Id"); assigned != "" {
		session = assigned
	}
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusAccepted {
		return nil, session, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, session, fmt.Errorf("%s: HTTP %d", msg.Method, resp.StatusCode)
	}
	if msg.Notification {
		return nil, session, nil
	}
	frame, err := readMcpResponse(resp)
	if err != nil {
		return nil, session, fmt.Errorf("%s: %w", msg.Method, err)
	}
	var decoded struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(frame, &decoded); err != nil {
		return nil, session, fmt.Errorf("%s: decode response: %w", msg.Method, err)
	}
	if decoded.Error != nil {
		return nil, session, fmt.Errorf("%s: JSON-RPC error %d: %s", msg.Method, decoded.Error.Code, decoded.Error.Message)
	}
	return decoded.Result, session, nil
}

// readMcpResponse returns the JSON-RPC frame out of a response body that
// is either a JSON document or an SSE stream. The streaming shape is
// comment keepalives (`: ...`) followed by an `event: message` whose
// `data:` lines carry the frame.
func readMcpResponse(resp *http.Response) ([]byte, error) {
	mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	if !strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream") {
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxMcpResponseBytes))
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, errors.New("empty response body")
		}
		return data, nil
	}
	reader := bufio.NewReader(io.LimitReader(resp.Body, maxMcpResponseBytes))
	var data []string
	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, ":"):
			// Keepalive comment.
		case strings.HasPrefix(trimmed, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " "))
		case trimmed == "" && len(data) > 0:
			// End of an event: the first one carrying data is the answer.
			return []byte(strings.Join(data, "\n")), nil
		}
		if err != nil {
			if len(data) > 0 {
				return []byte(strings.Join(data, "\n")), nil
			}
			if err == io.EOF {
				return nil, errors.New("event stream ended before a response frame")
			}
			return nil, err
		}
	}
}

// maxMcpResponseBytes bounds one tool answer the mock reads into memory.
// The app's own tool results are far below it; a server streaming more
// than this is a bug a harness run should notice rather than absorb.
const maxMcpResponseBytes = 8 << 20

// mcpResultText joins the text content blocks of a tools/call result.
// A result flagged `isError` comes back as mcpToolResultError carrying
// the same text, so the caller frames the tool's own refusal rather than
// inventing one. A result with no text block at all falls back to its
// raw JSON, so the frame never claims the tool answered nothing.
func mcpResultText(result json.RawMessage) (string, error) {
	if len(result) == 0 {
		return "", errors.New("tools/call returned no result")
	}
	var decoded struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		return "", fmt.Errorf("decode tools/call result: %w", err)
	}
	parts := make([]string, 0, len(decoded.Content))
	for _, block := range decoded.Content {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if text == "" {
		text = string(result)
	}
	if decoded.IsError {
		return "", mcpToolResultError{text: text}
	}
	return text, nil
}

// mcpTargetFromSpec reads one server entry of a provider's MCP
// configuration. `url` is required (the app's built-in servers are all
// streamable HTTP); headers come from Claude's `headers` or Codex's
// `http_headers`.
func mcpTargetFromSpec(raw json.RawMessage) (mcpTarget, bool) {
	var spec struct {
		URL         string            `json:"url"`
		Headers     map[string]string `json:"headers"`
		HTTPHeaders map[string]string `json:"http_headers"`
	}
	if json.Unmarshal(raw, &spec) != nil || strings.TrimSpace(spec.URL) == "" {
		return mcpTarget{}, false
	}
	headers := spec.Headers
	if len(headers) == 0 {
		headers = spec.HTTPHeaders
	}
	return mcpTarget{url: spec.URL, headers: headers}, true
}
