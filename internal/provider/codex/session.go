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

// DynamicToolHandler is called when the provider invokes a dynamic tool (item/tool/call
// or dynamicToolCall). The handler receives the tool name and arguments, and returns
// the result content and a success flag.
type DynamicToolHandler func(toolName string, args map[string]any) (content string, success bool, err error)

// Session manages a Codex app-server subprocess.
type Session struct {
	proc               *provider.Process
	threadID           string // our internal thread ID
	codexThreadID      string // the Codex app-server's thread ID from thread/start
	activeTurnID       string // current active turn ID from turn/started; cleared on turn/completed
	model              string // model name for cost calculation
	nextID             atomic.Int64
	mu                 sync.Mutex
	pending            map[int64]chan json.RawMessage
	onEvent            func(provider.ProviderEvent)
	dynamicToolHandler DynamicToolHandler
	cancel             context.CancelFunc
	closing            atomic.Bool
	readDone           chan struct{}
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
	MCPServers     map[string]any
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
		model:    cfg.Model,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  onEvent,
		cancel:   cancel,
		readDone: make(chan struct{}),
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
	s.codexThreadID = readNestedString(resp, "thread", "id")
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

	turnID := readNestedString(resp, "turn", "id")
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

// SetDynamicToolHandler registers a handler for dynamic tool calls (item/tool/call,
// dynamicToolCall). If nil, those requests are rejected with -32601.
func (s *Session) SetDynamicToolHandler(h DynamicToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dynamicToolHandler = h
}

// handleDynamicToolCall invokes the registered dynamic tool handler and sends the
// JSON-RPC response back to the app-server.
func (s *Session) handleDynamicToolCall(rpcID int64, handler DynamicToolHandler, params json.RawMessage) {
	var parsed struct {
		Tool      string         `json:"tool"`
		ToolName  string         `json:"toolName"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &parsed); err != nil {
		if writeErr := s.writeErrorResponse(rpcID, -32602, fmt.Sprintf("invalid params: %v", err)); writeErr != nil {
			log.Printf("codex: failed to send error response for malformed dynamic tool params: %v", writeErr)
		}
		return
	}

	toolName := parsed.Tool
	if toolName == "" {
		toolName = parsed.ToolName
	}
	args := parsed.Arguments
	if args == nil {
		args = make(map[string]any)
	}

	go func() {
		content, success, err := handler(toolName, args)
		if err != nil {
			content = fmt.Sprintf("Error: %v", err)
			success = false
		}
		result := map[string]any{
			"contentItems": []map[string]string{{
				"type": "inputText",
				"text": content,
			}},
			"success": success,
		}
		if writeErr := s.writeResponse(rpcID, result); writeErr != nil {
			log.Printf("codex: failed to send dynamic tool result for %q: %v", toolName, writeErr)
		}
	}()
}

// Close shuts down the app-server process.
// Closes stdin first for graceful shutdown, then cancels the context as fallback.
func (s *Session) Close() error {
	s.closing.Store(true)
	err := s.proc.Close()
	s.cancel()
	if s.readDone != nil {
		<-s.readDone
	}
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
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("codex: %s: session stopped before request completed", method)
		}
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
		// Drain the channel so any late response is consumed and the channel can be GC'd.
		select {
		case <-ch:
		default:
		}
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

// writeErrorResponse sends a JSON-RPC error response with the given code and message.
func (s *Session) writeErrorResponse(id int64, code int, message string) error {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("codex: marshal error response: %w", err)
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
		if s.readDone != nil {
			defer close(s.readDone)
		}

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
		"item/fileRead/requestApproval",
		"applyPatchApproval",
		"execCommandApproval":

		meta := buildApprovalMeta(s.threadID, turnID, method, rpcID, params)
		s.onEvent(buildApprovalEvent(s.threadID, turnID, itemID, meta, line))

	case "mcpServer/elicitation/request":
		meta := buildElicitationMeta(s.threadID, turnID, rpcID, params)
		s.onEvent(buildApprovalEvent(s.threadID, turnID, itemID, meta, line))

	case "item/tool/call", "dynamicToolCall":
		s.mu.Lock()
		handler := s.dynamicToolHandler
		s.mu.Unlock()

		if handler != nil {
			s.handleDynamicToolCall(rpcID, handler, params)
		} else {
			if err := s.writeErrorResponse(rpcID, -32601, fmt.Sprintf("no handler registered for dynamic tool call: %s", method)); err != nil {
				log.Printf("codex: failed to send error response for %s: %v", method, err)
			}
		}

	case "item/tool/requestUserInput":
		meta := buildUserInputMeta(s.threadID, turnID, rpcID, params)
		s.onEvent(buildApprovalEvent(s.threadID, turnID, itemID, meta, line))

	case "item/permissions/requestApproval":
		meta := buildPermissionMeta(s.threadID, turnID, rpcID, params)
		s.onEvent(buildApprovalEvent(s.threadID, turnID, itemID, meta, line))

	default:
		if err := s.writeErrorResponse(rpcID, -32601, fmt.Sprintf("unsupported server request: %s", method)); err != nil {
			log.Printf("codex: failed to send error response for %s: %v", method, err)
		}
	}
}

// -- helpers --

func buildApprovalEvent(threadID, turnID, itemID string, meta json.RawMessage, raw json.RawMessage) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  threadID,
		TurnID:    turnID,
		ItemID:    itemID,
		Meta:      meta,
		Timestamp: time.Now(),
		Raw:       raw,
	}
}


