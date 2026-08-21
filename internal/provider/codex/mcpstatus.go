package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
)

// MCPStatusFetcher runs `codex app-server`, performs the standard
// JSON-RPC `initialize` handshake, calls `mcpServerStatus/list`, and
// tears down the process. The list method does NOT require
// `thread/start`, so no turn is ever billed and no provider state is
// mutated.
//
// We use a minimal inline JSON-RPC client rather than the package's
// own Session machinery because the latter wires in turn tracking,
// approval routing, notification dispatch, etc. — none of which we
// need for a one-shot read.
//
// Binary / Env / Cwd are constructor-injected so tests can point at a
// fixture script.
type MCPStatusFetcher struct {
	Binary  string
	Args    []string // optional override; defaults to codexAppServerArgs
	Env     []string
	Cwd     string
	Timeout time.Duration // 0 → DefaultMCPStatusTimeout
}

// DefaultMCPStatusTimeout is the wall-clock ceiling. The list call
// is local-only and typically returns in <100ms; the 10s ceiling
// catches cold binary load + I/O scheduling on slow hosts without
// dragging the UI.
const DefaultMCPStatusTimeout = 10 * time.Second

// Fetch satisfies mcpstatus.Fetcher. The provider arg is ignored
// (always ProviderCodex).
func (f *MCPStatusFetcher) Fetch(ctx context.Context, _ mcpstatus.Provider) ([]mcpstatus.ServerStatus, error) {
	if strings.TrimSpace(f.Binary) == "" {
		return nil, fmt.Errorf("codex mcp status: binary path required")
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultMCPStatusTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := f.Args
	if len(args) == 0 {
		args = codexAppServerArgs()
	}
	cmd := exec.CommandContext(ctx, f.Binary, args...)
	if f.Cwd != "" {
		cmd.Dir = f.Cwd
	}
	cmd.Env = provider.FilterEnvironment(f.Env, "CODEX_HOME")
	// Bound the post-cancel wait when grandchildren hold our stdout
	// pipe open (mirrors the Claude fetcher rationale).
	cmd.WaitDelay = 500 * time.Millisecond
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex mcp status stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex mcp status stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex mcp status start: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()

	client := newMCPStatusRPCClient(stdin, stdout)

	// initialize is required before any other request — Codex enforces
	// the JSON-RPC v2 handshake. The client info is for forensics; the
	// response payload is ignored.
	//
	// Deliberately not codexInitializeParams: this fetcher makes exactly
	// one non-experimental call (`mcpServerStatus/list`), so opting a
	// throwaway process into the experimental API would buy nothing and
	// change what the server is willing to emit. It still opts out of
	// every notification — it awaits none, and `call` would otherwise
	// decode each one just to discard it.
	initParams := map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities": map[string]any{
			"optOutNotificationMethods": oneShotOptOutNotificationMethods(),
		},
		"clientInfo": map[string]any{
			"name":    "agent-overflow-mcpstatus",
			"version": "0.1.0",
		},
	}
	if _, err := client.call(ctx, "initialize", initParams); err != nil {
		return nil, fmt.Errorf("codex mcp status initialize: %w", err)
	}

	listParams := map[string]any{
		"detail": "toolsAndAuthOnly",
	}
	respBody, err := client.call(ctx, "mcpServerStatus/list", listParams)
	if err != nil {
		return nil, fmt.Errorf("codex mcpServerStatus/list: %w", err)
	}
	return parseMCPList(respBody, time.Now())
}

// MCPStatusFromList projects an `mcpServerStatus/list` entry onto the
// unified mcpstatus.Status.
//
// A list response describes SETTLED connection attempts, never
// in-flight ones. Every call builds a fresh McpConnectionSet — threadId
// only selects which config applies, it does not read a loaded thread's
// running manager — and `list_available_server_infos` awaits each
// pending client's startup before the response is assembled
// (codex-rs/app-server/src/request_processors/mcp_processor.rs
// `list_mcp_server_status`; codex-rs/codex-mcp/src/mcp/mod.rs
// `collect_mcp_server_status_snapshot_with_detail`;
// connection_manager.rs `list_available_server_infos`). So "no tools and
// no serverInfo" is a failed attempt, not a booting server, and
// StatusStarting is deliberately NOT derivable here — only a
// `mcpServer/startupStatus/updated` notification can report it.
//
// ServerInfo presence is the primary liveness signal: MCP requires a
// successful `initialize` response to carry serverInfo, and codex
// populates the field at every detail level (toolsAndAuthOnly included)
// on every version from AO's 0.143 floor. A non-empty tools map is the
// safety net — tools also can only exist past a completed initialize.
// It takes the whole entry, like MCPStatusFromNotif, so a caller cannot
// destructure the record and lose a signal. Rules:
//
//   - authStatus = "notLoggedIn" → StatusNeedsAuth
//   - authStatus ∈ {"unknown","unsupported","bearerToken","oAuth"}:
//     serverInfo present ∨ tools non-empty → StatusConnected, else StatusFailed
//   - unrecognized authStatus → StatusUnknown
//
// `unknown` is in the EVIDENCE-DECIDES set, not the give-up set, and that
// is the whole point of listing it. Codex 0.147 added the variant
// (`McpAuthStatus::Unknown`, codex-rs/app-server-protocol/src/protocol/v2/mcp.rs;
// `McpAuthState::Unknown`, codex-rs/rmcp-client/src/auth_status.rs) and
// reports it whenever OAuth metadata DISCOVERY ITSELF FAILED —
// `determine_auth_status_from_discovery` returns `Err` and
// `compute_auth_statuses` (codex-rs/codex-mcp/src/mcp/auth.rs) swallows it
// into `Unknown` — or whenever an `http_headers_helper` server cannot be
// inspected without executing the helper. A plain HTTP MCP server that
// publishes no `.well-known` OAuth metadata is the common producer; 0.146
// answered `unsupported` for the same server, so treating `unknown` as
// "we cannot tell anything" would have flipped healthy rows to
// StatusUnknown on upgrade. The auth axis never decides liveness for any
// value except `notLoggedIn`: serverInfo/tools prove a completed
// `initialize`, and that proof is independent of how the auth probe went.
func MCPStatusFromList(entry MCPServerStatus) mcpstatus.Status {
	switch strings.TrimSpace(entry.AuthStatus) {
	case "notLoggedIn":
		return mcpstatus.StatusNeedsAuth
	case "unknown", "unsupported", "bearerToken", "oAuth":
		if entry.ServerInfo != nil || len(entry.Tools) > 0 {
			return mcpstatus.StatusConnected
		}
		return mcpstatus.StatusFailed
	default:
		return mcpstatus.StatusUnknown
	}
}

// MCPFailureReasonReauthRequired is the sole variant of upstream's
// McpStartupFailureReason enum (camelCase on the v2 wire; the internal
// protocol spells it `reauthentication_required`). It means the server's
// stored OAuth grant is no longer usable — the fix is a sign-in, not a
// retry.
//
// A failure that needs a sign-in usually does NOT carry it. Codex's
// `mcp_startup_failure_reason`
// (codex-rs/codex-mcp/src/connection_manager/startup.rs, read at
// rust-v0.147.0) returns this variant only when the stored token already
// reads AuthorizationRequired — structurally unusable. A refresh token
// that is structurally intact but revoked server-side reads Usable, so
// the attempt fails with `invalid_grant`, authStatus `oAuth`, and
// `failureReason: null`. That null is deterministic, not drift: the
// mapping below stays because it is correct when it does arrive, but the
// plain failed state has to be actionable on its own.
const MCPFailureReasonReauthRequired = "reauthenticationRequired"

// MCPStatusFromNotif projects a `mcpServer/startupStatus/updated`
// payload onto the unified mcpstatus.Status. Wire values per Codex
// protocol:
//
//	"starting"                             → StatusStarting
//	"ready"                                → StatusConnected
//	"failed" + reauthenticationRequired    → StatusNeedsAuth
//	"failed"                               → StatusFailed
//	"cancelled"                            → StatusFailed
//
// It takes the whole update rather than the state string on purpose: a
// caller holding only the state cannot tell an expired grant from a
// broken server, and a projection that silently loses that distinction
// is the bug this signature prevents. Only the exact known reason is
// honoured — a future variant falls through to the state mapping instead
// of being guessed into a sign-in prompt.
func MCPStatusFromNotif(update MCPStartupUpdate) mcpstatus.Status {
	state := strings.TrimSpace(update.State)
	if strings.TrimSpace(update.FailureReason) == MCPFailureReasonReauthRequired {
		return mcpstatus.StatusNeedsAuth
	}
	switch state {
	case "starting":
		return mcpstatus.StatusStarting
	case "ready":
		return mcpstatus.StatusConnected
	case "failed", "cancelled":
		return mcpstatus.StatusFailed
	default:
		return mcpstatus.StatusUnknown
	}
}

// mcpListResponse is the wire shape for mcpServerStatus/list —
// MCPServerStatus entries (mcp.go), which decode only auth/identity/
// tool-name fields; resources and resourceTemplates are skipped
// (detail=toolsAndAuthOnly omits them anyway).
type mcpListResponse struct {
	Data []MCPServerStatus `json:"data"`
}

func parseMCPList(body []byte, now time.Time) ([]mcpstatus.ServerStatus, error) {
	if len(body) == 0 {
		return []mcpstatus.ServerStatus{}, nil
	}
	var resp mcpListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode mcpServerStatus/list response: %w", err)
	}
	out := make([]mcpstatus.ServerStatus, 0, len(resp.Data))
	for _, entry := range resp.Data {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			continue
		}
		out = append(out, mcpstatus.ServerStatus{
			Key:        mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: name},
			Status:     MCPStatusFromList(entry),
			AuthStatus: entry.AuthStatus,
			ToolCount:  len(entry.Tools),
			Tools:      entry.ToolNames(),
			Source:     mcpstatus.SourceEphemeralFetch,
			CheckedAt:  now,
		})
	}
	return out, nil
}

// sortedToolNames projects the wire's tools map onto its sorted key
// list. Names only — the map values carry tool schemas the status
// surface has no use for.
func sortedToolNames(tools map[string]json.RawMessage) []string {
	if len(tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// mcpStatusRPCClient is a tiny single-request-at-a-time JSON-RPC
// client used by the ephemeral status fetcher. We don't need
// pipelining, notifications, or concurrent responses; the two calls
// we make are strictly sequential.
type mcpStatusRPCClient struct {
	stdin   io.Writer
	scanner *bufio.Scanner
	nextID  atomic.Int64
}

func newMCPStatusRPCClient(stdin io.Writer, stdout io.Reader) *mcpStatusRPCClient {
	scanner := bufio.NewScanner(stdout)
	// Codex responses can be large (full server status with tool
	// schemas); default Scanner buffer caps at 64KB which would
	// truncate. Bump to 4MB which comfortably exceeds anything seen
	// from upstream.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &mcpStatusRPCClient{stdin: stdin, scanner: scanner}
}

type mcpStatusRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type mcpStatusRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

// call writes a request and reads lines until it sees the matching
// response. Notifications are silently dropped — we don't expect any
// during this short-lived session, but if Codex sends one we keep
// reading rather than treating it as an error.
func (c *mcpStatusRPCClient) call(ctx context.Context, method string, params any) ([]byte, error) {
	req := mcpStatusRPCRequest{
		JSONRPC: "2.0",
		ID:      c.nextID.Add(1),
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", method, err)
	}
	body = append(body, '\n')
	if _, err := c.stdin.Write(body); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	resultCh := make(chan mcpStatusRPCResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		for c.scanner.Scan() {
			line := c.scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var resp mcpStatusRPCResponse
			if err := json.Unmarshal(line, &resp); err != nil {
				continue // tolerate stray non-JSON lines
			}
			if resp.ID == req.ID {
				resultCh <- resp
				return
			}
		}
		if err := c.scanner.Err(); err != nil {
			errCh <- err
			return
		}
		errCh <- fmt.Errorf("response stream closed before %s reply", method)
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-errCh:
		return nil, err
	case resp := <-resultCh:
		if resp.Error != nil {
			// Typed, not rendered: a caller that has to tell "this binary
			// doesn't know the method" from "the request was wrong" needs the
			// code, and recovering it from a formatted string later would be
			// a parser standing in for a type.
			return nil, &RPCError{Method: method, Code: resp.Error.Code, Message: resp.Error.Message}
		}
		return resp.Result, nil
	}
}
