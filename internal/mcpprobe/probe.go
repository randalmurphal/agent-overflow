package mcpprobe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"agent-overflow/internal/mcp"
)

// Result is the projected status the binding layer hands the
// frontend. ToolCount is best-effort: zero on transports / failures
// where we can't or don't fetch the tool list. CacheKey ("provider:name")
// is the wire-stable identifier the frontend uses to attribute results
// back to a server — the same name can exist for both providers.
type Result struct {
	CacheKey    string    `json:"cacheKey"`
	Provider    string    `json:"provider"`
	ServerName  string    `json:"serverName,omitempty"`
	Status      mcp.Status `json:"status"`
	Error       string    `json:"error,omitempty"`
	ProtocolVer string    `json:"protocolVersion,omitempty"`
	ToolCount   int       `json:"toolCount"`
	CheckedAt   int64     `json:"checkedAt"`
}

// DefaultStdioTimeout is the per-probe budget for stdio servers. The
// spike showed warm spawns ~600ms and cold npx spawns ~2.7s; keep the
// ceiling generous enough that a first probe of a heavyweight server
// (e.g. playwright launching chrome) doesn't false-fail.
const DefaultStdioTimeout = 15 * time.Second

// DefaultHTTPTimeout is the per-probe budget for HTTP/SSE servers.
// Healthy servers return in single-digit milliseconds; a 5s ceiling
// catches dead DNS / refused / hanging-but-not-401 cases without
// blocking the popup for long.
const DefaultHTTPTimeout = 5 * time.Second

// Probe runs the appropriate handshake for the spec's transport and
// returns the projected result. The context controls the wall-clock
// budget; callers wrap with their own timeout for back-pressure on
// the popup.
func Probe(ctx context.Context, spec mcp.Spec) Result {
	result := Result{
		CacheKey:   spec.CacheKey(),
		Provider:   spec.Provider,
		ServerName: spec.Name,
		Status:     mcp.StatusUnknown,
		CheckedAt:  time.Now().UnixMilli(),
	}
	if !spec.Enabled {
		result.Status = mcp.StatusUnknown
		result.Error = "server disabled"
		return result
	}
	switch spec.Transport {
	case mcp.TransportStdio:
		return probeStdio(ctx, spec, result)
	case mcp.TransportHTTP, mcp.TransportSSE:
		return probeHTTP(ctx, spec, result)
	default:
		result.Status = mcp.StatusFailed
		result.Error = fmt.Sprintf("unsupported transport %q", spec.Transport)
		return result
	}
}

type jsonRPCRequest struct {
	Jsonrpc string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type initResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

type toolsListResult struct {
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

func probeStdio(ctx context.Context, spec mcp.Spec, result Result) Result {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(DefaultStdioTimeout)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	cmd := exec.CommandContext(ctx, spec.Command, spec.Args...)
	cmd.Env = append(os.Environ(), envFromMap(spec.Env)...)
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		result.Status = mcp.StatusFailed
		result.Error = fmt.Sprintf("stdin pipe: %v", err)
		return result
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		result.Status = mcp.StatusFailed
		result.Error = fmt.Sprintf("stdout pipe: %v", err)
		return result
	}
	if err := cmd.Start(); err != nil {
		result.Status = mcp.StatusFailed
		result.Error = fmt.Sprintf("start: %v", err)
		return result
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	reader := bufio.NewReader(stdout)
	initReq := jsonRPCRequest{
		Jsonrpc: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "agent-overflow-probe", "version": "0.1.0"},
		},
	}
	if err := writeLine(stdin, initReq); err != nil {
		result.Status = mcp.StatusFailed
		result.Error = fmt.Sprintf("write init: %v", err)
		return result
	}
	initResp, err := readResponse(reader)
	if err != nil {
		result.Status = mcp.StatusFailed
		result.Error = fmt.Sprintf("read init: %v", err)
		return result
	}
	if initResp.Error != nil {
		result.Status = mcp.StatusFailed
		result.Error = fmt.Sprintf("init error %d: %s", initResp.Error.Code, initResp.Error.Message)
		return result
	}
	var init initResult
	if err := json.Unmarshal(initResp.Result, &init); err != nil {
		result.Status = mcp.StatusFailed
		result.Error = fmt.Sprintf("decode init: %v", err)
		return result
	}

	result.Status = mcp.StatusReady
	result.ProtocolVer = init.ProtocolVersion
	if init.ServerInfo.Name != "" {
		result.ServerName = init.ServerInfo.Name
	}

	_ = writeLine(stdin, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	if err := writeLine(stdin, jsonRPCRequest{Jsonrpc: "2.0", ID: 2, Method: "tools/list"}); err == nil {
		if resp, err := readResponse(reader); err == nil && resp.Error == nil {
			var tools toolsListResult
			if err := json.Unmarshal(resp.Result, &tools); err == nil {
				result.ToolCount = len(tools.Tools)
			}
		}
	}
	return result
}

func probeHTTP(ctx context.Context, spec mcp.Spec, result Result) Result {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(DefaultHTTPTimeout)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	body := jsonRPCRequest{
		Jsonrpc: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "agent-overflow-probe", "version": "0.1.0"},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		result.Status = mcp.StatusFailed
		result.Error = fmt.Sprintf("marshal init: %v", err)
		return result
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.URL, bytes.NewReader(buf))
	if err != nil {
		result.Status = mcp.StatusFailed
		result.Error = fmt.Sprintf("new request: %v", err)
		return result
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}
	if spec.BearerEnv != "" {
		if token := strings.TrimSpace(os.Getenv(spec.BearerEnv)); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		result.Status = mcp.StatusFailed
		result.Error = fmt.Sprintf("request: %v", err)
		return result
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		result.Status = mcp.StatusNeedsAuth
		if wwwAuth := resp.Header.Get("WWW-Authenticate"); wwwAuth != "" {
			result.Error = wwwAuth
		} else {
			result.Error = "HTTP 401"
		}
		return result
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		var rpc jsonRPCResponse
		if err := json.Unmarshal(respBody, &rpc); err != nil {
			if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
				result.Status = mcp.StatusReady
				return result
			}
			result.Status = mcp.StatusFailed
			result.Error = fmt.Sprintf("decode response: %v", err)
			return result
		}
		if rpc.Error != nil {
			result.Status = mcp.StatusFailed
			result.Error = fmt.Sprintf("init error %d: %s", rpc.Error.Code, rpc.Error.Message)
			return result
		}
		var init initResult
		if err := json.Unmarshal(rpc.Result, &init); err == nil {
			result.ProtocolVer = init.ProtocolVersion
			if init.ServerInfo.Name != "" {
				result.ServerName = init.ServerInfo.Name
			}
		}
		result.Status = mcp.StatusReady
		return result
	default:
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		result.Status = mcp.StatusFailed
		result.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		return result
	}
}

func writeLine(w io.Writer, v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	_, err = w.Write(buf)
	return err
}

func readResponse(r *bufio.Reader) (*jsonRPCResponse, error) {
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var resp jsonRPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		if resp.ID == 0 && resp.Result == nil && resp.Error == nil {
			continue
		}
		return &resp, nil
	}
}

func envFromMap(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
