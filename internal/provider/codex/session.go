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

	"agent-overflow/internal/logging"
	"agent-overflow/internal/provider"
)

// Session manages a Codex app-server subprocess.
type Session struct {
	proc          *provider.Process
	threadID      string // our internal thread ID
	codexThreadID string // the Codex app-server's thread ID from thread/start
	activeTurnID  string // current active turn ID from turn/started; cleared on turn/completed
	nextID        atomic.Int64
	mu            sync.Mutex
	pending       map[int64]chan json.RawMessage
	onEvent       func(provider.ProviderEvent)
	cancel        context.CancelFunc
	closing       atomic.Bool
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
	EventLogger    *logging.Logger
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
		Binary:      binary,
		Args:        []string{"app-server"},
		Dir:         cfg.WorkDir,
		EventLogger: cfg.EventLogger,
		ThreadID:    threadID,
		Provider:    string(provider.Codex),
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
	if err := s.writeNotification("initialized", nil); err != nil {
		s.Close()
		return nil, fmt.Errorf("codex: send initialized notification: %w", err)
	}

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
	if s.codexThreadID == "" {
		log.Printf("codex: %s response missing thread.id; response: %s", method, string(resp))
		s.Close()
		return nil, fmt.Errorf("codex: %s: response did not contain a thread ID", method)
	}

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

// Interrupt sends turn/interrupt for the active turn.
// Returns an error if no turn is currently active.
func (s *Session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	turnID := s.activeTurnID
	s.mu.Unlock()

	if turnID == "" {
		return fmt.Errorf("codex: no active turn to interrupt")
	}

	_, err := s.sendRequest(ctx, "turn/interrupt", map[string]any{
		"turnId": turnID,
	})
	return err
}

// ThreadID returns our internal thread identifier.
func (s *Session) ThreadID() string {
	return s.threadID
}

// Close shuts down the app-server process.
// Closes stdin first for graceful shutdown, then cancels the context as fallback.
func (s *Session) Close() error {
	s.closing.Store(true)
	err := s.proc.Close()
	s.cancel()
	return err
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
		if err := json.Unmarshal(resp, &rpcResp); err == nil {
			if rpcResp.Error != nil {
				return nil, fmt.Errorf("codex: %s: %s (code %d)", method, rpcResp.Error.Message, rpcResp.Error.Code)
			}
			if len(rpcResp.Result) > 0 {
				return rpcResp.Result, nil
			}
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

		if !s.closing.Load() {
			exitErr := provider.WaitProcessExitErr(s.proc)
			if exitErr != nil {
				s.onEvent(provider.ProviderEvent{
					Kind:      provider.EventSessionStatus,
					ThreadID:  s.threadID,
					Content:   "error",
					Meta:      provider.MarshalProcessExitMeta(exitErr),
					Timestamp: time.Now(),
				})
			}
		}

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
			log.Printf("codex: response has non-integer ID %q: %v", msg.ID.String(), err)
			return
		}
		s.mu.Lock()
		ch, ok := s.pending[id]
		s.mu.Unlock()
		if ok {
			// Non-blocking send: if the request already timed out and the channel
			// was removed or is full, drop the response silently.
			select {
			case ch <- line:
			default:
			}
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
			// Track active turn ID for Interrupt.
			switch evt.Kind {
			case provider.EventTurnStart:
				if evt.TurnID != "" {
					s.mu.Lock()
					s.activeTurnID = evt.TurnID
					s.mu.Unlock()
				}
			case provider.EventTurnComplete:
				s.mu.Lock()
				s.activeTurnID = ""
				s.mu.Unlock()
			}
			s.onEvent(evt)
		}
		return
	}
}

// handleServerRequest processes server-initiated requests (approvals).
func (s *Session) handleServerRequest(method string, id *json.Number, params json.RawMessage, line []byte) {
	rpcID, err := id.Int64()
	if err != nil {
		log.Printf("codex: server request has non-integer ID %q: %v", id.String(), err)
		return
	}

	turnID, itemID := readRouteFields(params)

	switch method {
	case "item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/fileRead/requestApproval":

		meta := buildApprovalMeta(s.threadID, turnID, method, rpcID, params)
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventApprovalRequest,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			ItemID:    itemID,
			Meta:      meta,
			Timestamp: time.Now(),
			Raw:       line,
		})

	case "item/tool/requestUserInput":
		meta := buildUserInputMeta(s.threadID, turnID, rpcID, params)
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventApprovalRequest,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			ItemID:    itemID,
			Meta:      meta,
			Timestamp: time.Now(),
			Raw:       line,
		})

	case "item/permissions/requestApproval":
		meta := buildPermissionMeta(s.threadID, turnID, rpcID, params)
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventApprovalRequest,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			ItemID:    itemID,
			Meta:      meta,
			Timestamp: time.Now(),
			Raw:       line,
		})

	default:
		errMsg := map[string]any{
			"jsonrpc": "2.0",
			"id":      rpcID,
			"error": map[string]any{
				"code":    -32601,
				"message": fmt.Sprintf("unsupported server request: %s", method),
			},
		}
		data, _ := json.Marshal(errMsg)
		if err := s.proc.WriteLine(data); err != nil {
			log.Printf("codex: failed to send error response for %s: %v", method, err)
		}
	}
}

// -- helpers --

func buildThreadParams(cfg Config) map[string]any {
	params := map[string]any{}

	if cfg.WorkDir != "" {
		params["cwd"] = cfg.WorkDir
	}

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

func readRouteFields(params json.RawMessage) (string, string) {
	turnID := readTopLevelString(params, "turnId")
	if turnID == "" {
		turnID = readNestedString(params, "turn", "id")
	}

	itemID := readTopLevelString(params, "itemId")
	if itemID == "" {
		itemID = readNestedString(params, "item", "id")
	}

	return turnID, itemID
}

func buildApprovalMeta(threadID, turnID, method string, rpcID int64, params json.RawMessage) json.RawMessage {
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
		TurnID:      turnID,
		ToolName:    toolName,
		Description: description,
		Input:       input,
		Title:       title,
	}

	data, _ := json.Marshal(approval)
	return data
}

func buildUserInputMeta(threadID, turnID string, rpcID int64, params json.RawMessage) json.RawMessage {
	questions := parseUserInputQuestions(params)

	approval := provider.ApprovalRequest{
		RequestID: fmt.Sprintf("%d", rpcID),
		ThreadID:  threadID,
		TurnID:    turnID,
		ToolName:  "user_input",
		Input:     params,
		Kind:      "user-input",
		Questions: questions,
		Title:     "User Input Required",
	}
	data, _ := json.Marshal(approval)
	return data
}

func buildPermissionMeta(threadID, turnID string, rpcID int64, params json.RawMessage) json.RawMessage {
	reason, perms := parsePermissionRequest(params)

	approval := provider.ApprovalRequest{
		RequestID:   fmt.Sprintf("%d", rpcID),
		ThreadID:    threadID,
		TurnID:      turnID,
		ToolName:    "permissions",
		Kind:        "permission",
		Input:       params,
		Description: reason,
		Permissions: perms,
		Title:       "Permission Required",
	}
	data, _ := json.Marshal(approval)
	return data
}

func parseUserInputQuestions(params json.RawMessage) []provider.UserInputQuestion {
	var payload struct {
		Questions []provider.UserInputQuestion `json:"questions"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return nil
	}
	return payload.Questions
}

func parsePermissionRequest(params json.RawMessage) (string, *provider.PermissionProfile) {
	var payload struct {
		Reason      string                      `json:"reason"`
		Permissions *provider.PermissionProfile `json:"permissions"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return "", nil
	}
	return payload.Reason, payload.Permissions
}

// readStringFromResponse is an alias for readNestedString (protocol.go)
// to maintain backward compatibility for tests that reference this name.
func readStringFromResponse(data json.RawMessage, keys ...string) string {
	return readNestedString(data, keys...)
}
