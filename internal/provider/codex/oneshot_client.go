package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"agent-overflow/internal/provider"
)

// oneshotClient is a strictly sequential request/response JSON-RPC client
// over a threadless `codex app-server` process. Most callers are one-shot
// reads (`model/list`, `skills/list`). MCP OAuth keeps the same client alive
// through one completion notification because its loopback listener belongs
// to that process.
//
// It is deliberately not the Session machinery — that wires turn tracking,
// approval routing, child-thread quarantine and notification dispatch,
// none of which a one-shot read has any use for. It is also deliberately
// not concurrent: every caller here issues one request at a time, so
// correlation is "read frames until the id matches" rather than a pending
// map.
//
// No `thread/start` is ever issued through this client, so no turn is billed
// and no provider thread is created. Individual methods can still mutate
// their own account-level state, such as installing an MCP OAuth grant.
type oneshotClient struct {
	proc *provider.Process
	// label prefixes every error so a failure names the read that caused
	// it rather than just "codex".
	label  string
	nextID int64
}

// oneshotSpec is the full launch + handshake description. Every field a
// caller could get wrong by omission is checked in startOneshotClient
// rather than left to caller discipline.
type oneshotSpec struct {
	// Binary is the codex CLI path. Empty falls back to "codex" on PATH.
	Binary string
	// WorkDir is the subprocess's working directory. Codex discovers
	// project-scoped configuration by walking up from its cwd, so an
	// inherited cwd would let whichever directory the app was launched
	// from decide what the answer is.
	WorkDir string
	// Env is the per-provider pin map merged over the inherited
	// environment. CODEX_HOME is always unset — AO owns Codex's home.
	Env map[string]string
	// ClientName is the `clientInfo.name` sent at initialize. Forensics
	// only; the app-server does not branch on it.
	ClientName string
	// Experimental opts the connection into `capabilities.experimentalApi`.
	// Set it only when the client actually calls an `#[experimental]`
	// method: the capability changes what the server is willing to emit,
	// and a throwaway process asking for more than it uses is noise.
	Experimental bool
	// KeepNotifications names the notification methods this client's own
	// read loop waits on. Everything else in the catalogue is opted out at
	// initialize. Naming them here is what keeps the opt-out honest — a
	// client that starts depending on a notification says so in the same
	// place it asks not to receive things.
	KeepNotifications []string
	// Label prefixes error messages. Defaults to ClientName.
	Label string
}

// startOneshotClient spawns the app-server and completes the
// initialize/initialized handshake. The returned client is ready for
// requests; the caller must close it.
func startOneshotClient(ctx context.Context, spec oneshotSpec) (*oneshotClient, error) {
	binary := strings.TrimSpace(spec.Binary)
	if binary == "" {
		binary = "codex"
	}
	label := strings.TrimSpace(spec.Label)
	if label == "" {
		label = spec.ClientName
	}

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary:   binary,
		Args:     codexAppServerArgs(),
		Dir:      spec.WorkDir,
		Env:      spec.Env,
		UnsetEnv: []string{"CODEX_HOME"},
		Provider: string(provider.Codex),
	})
	if err != nil {
		return nil, fmt.Errorf("codex: spawn for %s: %w", label, err)
	}
	client := &oneshotClient{proc: proc, label: label}

	capabilities := map[string]any{
		"optOutNotificationMethods": oneShotOptOutNotificationMethods(spec.KeepNotifications...),
	}
	if spec.Experimental {
		capabilities["experimentalApi"] = true
	}
	initParams := map[string]any{
		"clientInfo": map[string]any{
			"name":    spec.ClientName,
			"title":   "Agent Overflow",
			"version": "0.1.0",
		},
		"capabilities": capabilities,
	}
	if _, err := client.request(ctx, "initialize", initParams); err != nil {
		client.close()
		return nil, fmt.Errorf("codex: initialize for %s: %w", label, err)
	}
	if err := client.notify("initialized", nil); err != nil {
		client.close()
		return nil, fmt.Errorf("codex: send initialized for %s: %w", label, err)
	}
	return client, nil
}

func (c *oneshotClient) close() {
	if c != nil && c.proc != nil {
		_ = c.proc.Close()
	}
}

// request writes one JSON-RPC request and reads frames until the matching
// response arrives. Notifications and unrelated responses are skipped
// rather than treated as errors.
func (c *oneshotClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.nextID++
	id := c.nextID

	message := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		message["params"] = params
	}
	data, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("codex: marshal %s: %w", method, err)
	}
	if err := c.proc.WriteLine(data); err != nil {
		return nil, err
	}

	for {
		line, err := c.readLine(ctx)
		if err != nil {
			return nil, err
		}
		var response struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &response); err != nil {
			return nil, fmt.Errorf("codex: decode %s response: %w", method, err)
		}
		if response.ID != id {
			continue
		}
		if response.Error != nil {
			// Typed, not rendered: a caller that has to tell "this binary
			// doesn't know the method" from "the request was wrong" needs
			// the code, and recovering it from a formatted string later
			// would be a parser standing in for a type.
			return nil, &RPCError{Method: method, Code: response.Error.Code, Message: response.Error.Message}
		}
		return response.Result, nil
	}
}

func (c *oneshotClient) notify(method string, params any) error {
	message := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		message["params"] = params
	}
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("codex: marshal %s notification: %w", method, err)
	}
	return c.proc.WriteLine(data)
}

// readLine reads one NDJSON frame, honouring context cancellation. The
// blocking read runs on its own goroutine because provider.Process has no
// deadline-aware read; on cancellation the goroutine is left to finish
// against the pipe the deferred close is about to tear down.
func (c *oneshotClient) readLine(ctx context.Context) ([]byte, error) {
	type readResult struct {
		line []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := c.proc.ReadLine()
		ch <- readResult{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-ch:
		if result.err != nil {
			if errors.Is(result.err, io.EOF) {
				return nil, fmt.Errorf("codex: %s app-server exited", c.label)
			}
			return nil, result.err
		}
		return result.line, nil
	}
}
