package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/provider"
)

// Session manages a Codex app-server subprocess.
type Session struct {
	proc          *provider.Process
	threadID      string // our internal thread ID
	codexThreadID string // the Codex app-server's thread ID from thread/start
	nextID        atomic.Int64
	mu            sync.Mutex
	pending       map[int64]chan json.RawMessage
	onEvent       func(provider.ProviderEvent)
	cancel        context.CancelFunc
}

// Config for creating a Codex session.
type Config struct {
	Binary         string // default: "codex"
	Model          string
	WorkDir        string
	ApprovalPolicy string // "never", "on-failure", "on-request", "untrusted"
	Sandbox        string // "read-only", "workspace-write", "danger-full-access"
	ResumeThreadID string // thread ID to resume, empty for new
	SystemPrompt   string
}

// NewSession spawns codex app-server, performs the initialize handshake,
// and starts (or resumes) a thread. Returns after handshake completes.
func NewSession(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent)) (*Session, error) {
	binary := cfg.Binary
	if binary == "" {
		binary = "codex"
	}

	childCtx, cancel := context.WithCancel(ctx)

	proc, err := provider.Spawn(childCtx, provider.SpawnConfig{
		Binary: binary,
		Args:   []string{"app-server"},
		Dir:    cfg.WorkDir,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("codex: spawn: %w", err)
	}

	s := &Session{
		proc:     proc,
		threadID: threadID,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  onEvent,
		cancel:   cancel,
	}

	// Start stdout reader goroutine before sending any requests.
	go s.readLoop()

	// Initialize handshake.
	_, err = s.sendRequest(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "agent_overflow",
			"title":   "Agent Overflow",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("codex: initialize handshake failed: %w", err)
	}

	// Send initialized notification (no id, no response expected).
	s.writeNotification("initialized", nil)

	// Start or resume thread.
	threadParams := buildThreadParams(cfg)
	var method string
	if cfg.ResumeThreadID != "" {
		method = "thread/resume"
		threadParams["threadId"] = cfg.ResumeThreadID
	} else {
		method = "thread/start"
	}

	resp, err := s.sendRequest(ctx, method, threadParams)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("codex: %s failed: %w", method, err)
	}

	// Extract the Codex thread ID from response.
	s.threadID = threadID // our internal ID
	s.codexThreadID = readStringFromResponse(resp, "thread", "id")
	if s.codexThreadID != "" {
		meta, _ := json.Marshal(provider.SessionInfo{
			SessionID: s.codexThreadID,
			Model:     cfg.Model,
			CWD:       cfg.WorkDir,
		})
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventInit,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: time.Now(),
		})
	}

	return s, nil
}

// Send sends a user turn via turn/start.
func (s *Session) Send(ctx context.Context, content string) error {
	params := map[string]any{
		"threadId": s.codexThreadID,
		"input": []map[string]any{{
			"type":          "text",
			"text":          content,
			"text_elements": []any{},
		}},
	}

	resp, err := s.sendRequest(ctx, "turn/start", params)
	if err != nil {
		return fmt.Errorf("codex: turn/start: %w", err)
	}

	turnID := readStringFromResponse(resp, "turn", "id")
	if turnID != "" {
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventTurnStart,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			Timestamp: time.Now(),
		})
	}

	return nil
}

// Interrupt sends turn/interrupt for the given turn.
func (s *Session) Interrupt(ctx context.Context, turnID string) error {
	_, err := s.sendRequest(ctx, "turn/interrupt", map[string]any{
		"turnId": turnID,
	})
	return err
}

// RespondToApproval responds to a server approval request by sending a JSON-RPC response.
func (s *Session) RespondToApproval(ctx context.Context, jsonRpcID int64, decision string) error {
	var result any
	switch decision {
	case "allow":
		result = map[string]any{"decision": "accept"}
	case "allow_session":
		result = map[string]any{"decision": "acceptForSession"}
	default:
		result = map[string]any{"decision": "decline"}
	}

	return s.writeResponse(jsonRpcID, result)
}

// ThreadID returns our internal thread identifier.
func (s *Session) ThreadID() string {
	return s.threadID
}

// Close shuts down the app-server process.
func (s *Session) Close() error {
	s.cancel()
	return s.proc.Close()
}

// -- Internal methods --

// sendRequest sends a JSON-RPC request and waits for a response.
func (s *Session) sendRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextID.Add(1)

	ch := make(chan json.RawMessage, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("codex: marshal request: %w", err)
	}

	if err := s.proc.WriteLine(data); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		var rpcResp struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error,omitempty"`
			Result json.RawMessage `json:"result,omitempty"`
		}
		if json.Unmarshal(resp, &rpcResp) == nil && rpcResp.Error != nil {
			return nil, fmt.Errorf("codex: %s: %s (code %d)", method, rpcResp.Error.Message, rpcResp.Error.Code)
		}
		return resp, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("codex: %s: timeout", method)
	}
}

// writeNotification sends a JSON-RPC notification (no id, no response expected).
func (s *Session) writeNotification(method string, params any) error {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("codex: marshal notification: %w", err)
	}
	return s.proc.WriteLine(data)
}

// writeResponse sends a JSON-RPC response (to server requests like approvals).
func (s *Session) writeResponse(id int64, result any) error {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("codex: marshal response: %w", err)
	}
	return s.proc.WriteLine(data)
}

// readLoop reads stdout and dispatches JSON-RPC messages.
func (s *Session) readLoop() {
	defer func() {
		// Unblock all pending requests.
		s.mu.Lock()
		for id, ch := range s.pending {
			close(ch)
			delete(s.pending, id)
		}
		s.mu.Unlock()

		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventSessionStatus,
			ThreadID:  s.threadID,
			Content:   "disconnected",
			Timestamp: time.Now(),
		})
	}()

	for {
		line, err := s.proc.ReadLine()
		if err != nil {
			if err != io.EOF {
				s.onEvent(provider.ProviderEvent{
					Kind:      provider.EventError,
					ThreadID:  s.threadID,
					Content:   fmt.Sprintf("codex: read error: %v", err),
					Timestamp: time.Now(),
				})
			}
			return
		}

		s.dispatchLine(line)
	}
}

// dispatchLine classifies a JSON-RPC line and routes it.
func (s *Session) dispatchLine(line []byte) {
	var msg struct {
		ID     *json.Number    `json:"id,omitempty"`
		Method string          `json:"method,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  json.RawMessage `json:"error,omitempty"`
		Params json.RawMessage `json:"params,omitempty"`
	}

	if err := json.Unmarshal(line, &msg); err != nil {
		log.Printf("codex: invalid JSON line: %v", err)
		return
	}

	// Response: has id, no method.
	if msg.ID != nil && msg.Method == "" {
		id, err := msg.ID.Int64()
		if err != nil {
			return
		}
		s.mu.Lock()
		ch, ok := s.pending[id]
		s.mu.Unlock()
		if ok {
			ch <- line
		}
		return
	}

	// Server request: has both id and method (approval flow).
	if msg.ID != nil && msg.Method != "" {
		s.handleServerRequest(msg.Method, msg.ID, msg.Params, line)
		return
	}

	// Notification: has method, no id.
	if msg.Method != "" {
		events := ClassifyNotification(s.threadID, msg.Method, msg.Params)
		for _, evt := range events {
			s.onEvent(evt)
		}
		return
	}
}

// handleServerRequest processes server-initiated requests (approvals).
func (s *Session) handleServerRequest(method string, id *json.Number, params json.RawMessage, line []byte) {
	rpcID, _ := id.Int64()

	switch method {
	case "item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/fileRead/requestApproval",
		"item/tool/requestUserInput",
		"item/permissions/requestApproval":

		meta := buildApprovalMeta(s.threadID, method, rpcID, params)
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventApprovalRequest,
			ThreadID:  s.threadID,
			Meta:      meta,
			Timestamp: time.Now(),
			Raw:       line,
		})

	default:
		s.writeResponse(rpcID, map[string]any{
			"error": map[string]any{
				"code":    -32601,
				"message": fmt.Sprintf("unsupported server request: %s", method),
			},
		})
	}
}

// -- helpers --

func buildThreadParams(cfg Config) map[string]any {
	params := map[string]any{}

	if cfg.Model != "" {
		params["model"] = cfg.Model
	}

	if cfg.Sandbox != "" {
		switch cfg.Sandbox {
		case "danger-full-access":
			params["approvalPolicy"] = "never"
			params["sandboxPolicy"] = "none"
		case "workspace-write":
			params["approvalPolicy"] = cfg.ApprovalPolicy
			params["sandboxPolicy"] = "workspace"
		default:
			params["approvalPolicy"] = cfg.ApprovalPolicy
			params["sandboxPolicy"] = "read-only"
		}
	}

	if cfg.SystemPrompt != "" {
		params["baseInstructions"] = cfg.SystemPrompt
	}

	return params
}

func buildApprovalMeta(threadID, method string, rpcID int64, params json.RawMessage) json.RawMessage {
	var parsed map[string]json.RawMessage
	_ = json.Unmarshal(params, &parsed)

	toolName := method
	description := method
	var input json.RawMessage
	title := method

	if cmd, ok := parsed["command"]; ok {
		var cmdStr string
		if json.Unmarshal(cmd, &cmdStr) == nil {
			toolName = "command"
			description = cmdStr
			title = "Run command"
			input = cmd
		}
	}
	if filePath, ok := parsed["filePath"]; ok {
		var fp string
		if json.Unmarshal(filePath, &fp) == nil {
			toolName = "file_change"
			description = fp
			title = "File change"
			input = params
		}
	}

	approval := provider.ApprovalRequest{
		RequestID:   fmt.Sprintf("%d", rpcID),
		ThreadID:    threadID,
		ToolName:    toolName,
		Description: description,
		Input:       input,
		Title:       title,
	}

	data, _ := json.Marshal(approval)
	return data
}

func readStringFromResponse(data json.RawMessage, keys ...string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	for i, key := range keys {
		raw, ok := m[key]
		if !ok {
			return ""
		}
		if i == len(keys)-1 {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return s
			}
			return ""
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return ""
		}
	}
	return ""
}
