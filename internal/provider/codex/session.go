package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/logging"
	"agent-overflow/internal/provider"
)

// DefaultApprovalTimeout is the ceiling on how long Codex will wait for the
// user to answer a tool-use approval before auto-declining. Mirrors the
// Claude session constant so both providers behave identically from the
// user's perspective.
const DefaultApprovalTimeout = 10 * time.Minute

const (
	maxPlanDeltaBufferBytes = 256 * 1024
	maxPlanDeltaBuffers     = 16
)

// ErrApprovalAlreadyResolved is returned by RespondToApproval when the
// request ID has already been answered (either by an earlier response or
// by the auto-deny timeout). Prevents a second write landing at the
// provider with a stale decision.
var ErrApprovalAlreadyResolved = fmt.Errorf("codex: approval already resolved: %w", provider.ErrStaleInteractiveRequest)

// DynamicToolHandler is called when the provider invokes a dynamic tool (item/tool/call
// or dynamicToolCall). The handler receives the tool name and arguments, and returns
// the result content and a success flag.
type DynamicToolHandler func(toolName string, args map[string]any) (content string, success bool, err error)

// Compile-time guarantee that *Session satisfies the provider.Session
// interface the app layer calls into. Changing any of the methods in a
// way that breaks the contract is caught at build time.
var _ provider.Session = (*Session)(nil)

// Session manages a Codex app-server subprocess.
type Session struct {
	proc               *provider.Process
	threadID           string // our internal thread ID
	codexThreadID      string // the Codex app-server's thread ID from thread/start
	activeTurnID       string // current active turn ID from turn/started; cleared on turn/completed
	model              string // model name for cost calculation
	reasoningEffort    string // per-turn reasoning effort override; empty means inherit thread default
	approvalPolicy     string // per-turn approval override; empty means inherit thread default
	sandbox            string // per-turn sandbox override; empty means inherit thread default
	nextID             atomic.Int64
	mu                 sync.Mutex
	pending            map[int64]chan json.RawMessage
	onEvent            func(provider.ProviderEvent)
	dynamicToolHandler DynamicToolHandler
	cancel             context.CancelFunc
	closing            atomic.Bool
	readDone           chan struct{}
	// approvalTimeout overrides DefaultApprovalTimeout when non-zero.
	approvalTimeout time.Duration
	// approvalsMu guards pendingApprovals, resolvedApprovals, and
	// approvalsClosed.
	approvalsMu sync.Mutex
	// pendingApprovals maps request ID (string form, matching the
	// RequestID field of ApprovalResponse) to the cancel channel for
	// the auto-deny timer goroutine.
	pendingApprovals map[string]*pendingApproval
	// resolvedApprovals remembers request IDs that have already been
	// answered so a second RespondToApproval returns
	// ErrApprovalAlreadyResolved (Bug B9) rather than silently writing
	// another response to the provider.
	resolvedApprovals map[string]struct{}
	// approvalsClosed is set by Close so late-arriving approvals don't
	// schedule new timers after teardown.
	approvalsClosed bool
	// seenTurnStarts dedupes EventTurnStart emissions (Bug B6). Keyed by
	// turnID. Entries are added by claimTurnStart and cleared by
	// clearTurnStart on EventTurnComplete so re-used turn IDs (rare,
	// typically across resumed sessions) can fire fresh.
	seenTurnStarts map[string]struct{}
	// requestTimeoutOverride replaces defaultRequestTimeout when
	// non-zero. Set by tests that exercise the late-response path; a
	// production Session leaves it at zero to use the default.
	requestTimeoutOverride time.Duration
	// childParentByThread maps a spawned collab receiver thread back to the
	// parent SpawnAgent tool-call item id. Notifications from the child
	// thread are re-emitted onto the parent thread with ParentToolUseID set
	// to this card id.
	//
	// childParentByAgentPath maps Codex's subagent_notification
	// `agent_path` value back to the same parent card. Named Codex agents
	// report a path such as `/root/researcher`, not the receiver thread id,
	// so the detached-completion path cannot rely on receiverThreadIds
	// alone.
	//
	// agentPathByThread is the inverse index used to delete path mappings
	// when a close_agent event closes a receiver thread.
	//
	// No heuristic background-tool classifier runs here (invariant 25).
	// The wire-typed signals for a backgrounded item are
	// `CommandExecution.source == "unifiedExecStartup"` and
	// `collabAgentToolCall` with a running child in `agentsStates`; those
	// are what authorize `is_background=true`. The actual stamp happens in
	// `internal/triage/codex_background.go` on the first model-produced
	// yield or at the turn-close catchall — this session package only
	// surfaces the wire fields; it doesn't project them.
	childParentByThread    map[string]string
	childParentByAgentPath map[string]string
	agentPathByThread      map[string]string
	planBuffersByItemID    map[string]*planBuffer
	planBuffersByTurnID    map[string]*planBuffer
	// probeFn is a test-only override for Probe(). When non-nil, Probe
	// skips the wire call and returns the result from this function.
	// Production Session construction (NewSession) never sets it.
	probeFn func(ctx context.Context) (ProbeResult, error)
	// resumeFn mirrors probeFn for Resume(). Used by
	// app_codex_reconcile_test.go to verify the post-probe rehydration
	// path without needing a live app-server. Production NewSession
	// never sets it.
	resumeFn func(ctx context.Context) error
	// cleanBackgroundTerminalsFn mirrors probeFn for
	// CleanBackgroundTerminals(). Used by app_codex_background_test.go to
	// verify the binding wires through to a Codex session without
	// spinning up a real app-server. Production NewSession never sets it.
	cleanBackgroundTerminalsFn func(ctx context.Context) error
}

type planBuffer struct {
	itemID    string
	turnID    string
	text      strings.Builder
	truncated bool
}

// pendingApproval tracks one in-flight approval so the timer can be
// cancelled when the user responds (Bug B3) or the session closes.
type pendingApproval struct {
	cancel              chan struct{}
	resolveKind         provider.EventKind
	timeoutResponseKind string
}

// Config for creating a Codex session.
type Config struct {
	Binary                string // default: "codex"
	Model                 string
	WorkDir               string
	ApprovalPolicy        string // "never", "on-failure", "on-request", "untrusted"
	Sandbox               string // "read-only", "workspace-write", "danger-full-access"
	ResumeThreadID        string // thread ID to resume, empty for new
	SystemPrompt          string
	MCPServers            map[string]any
	ContextWindow         int
	AutoCompactTokenLimit int
	// ReasoningEffort is the Codex-native reasoning_effort enum value
	// (none|minimal|low|medium|high|xhigh). Applied to the thread start
	// handshake under `config.model_reasoning_effort`, and re-applied to
	// every turn/start call under the `effort` parameter so per-thread
	// tuning takes effect without a session restart.
	ReasoningEffort string
	EventLogger     *logging.Logger
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
		proc:                   proc,
		threadID:               threadID,
		model:                  cfg.Model,
		reasoningEffort:        cfg.ReasoningEffort,
		approvalPolicy:         cfg.ApprovalPolicy,
		sandbox:                cfg.Sandbox,
		pending:                make(map[int64]chan json.RawMessage),
		onEvent:                onEvent,
		cancel:                 cancel,
		readDone:               make(chan struct{}),
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		planBuffersByItemID:    make(map[string]*planBuffer),
		planBuffersByTurnID:    make(map[string]*planBuffer),
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
//
// The JSON-RPC response does not directly drive an EventTurnStart: the
// app-server reliably follows turn/start with a turn/started notification
// that ClassifyNotification turns into the event. Emitting here as well
// produced two EventTurnStart per user send (Bug B6). We still record the
// turn ID locally so Interrupt has something to cancel even if the
// notification has not yet arrived.
func (s *Session) Send(ctx context.Context, content string, opts provider.SendOptions) error {
	attachments := opts.Attachments
	input := make([]map[string]any, 0, 1+len(attachments))
	if strings.TrimSpace(content) != "" {
		input = append(input, map[string]any{
			"type":          "text",
			"text":          content,
			"text_elements": []any{},
		})
	}
	for _, attachment := range attachments {
		encoded := base64.StdEncoding.EncodeToString(attachment.Data)
		input = append(input, map[string]any{
			"type": "image",
			"url":  "data:" + attachment.MimeType + ";base64," + encoded,
		})
	}
	if len(input) == 0 {
		return fmt.Errorf("codex: turn/start requires text or image input")
	}

	params := map[string]any{
		"threadId":          s.codexThreadID,
		"input":             input,
		"collaborationMode": codexCollaborationMode(opts.InteractionMode, s.model, s.reasoningEffort),
	}
	// Per-turn effort override — Codex's TurnStartParams takes `effort` at
	// the top level. Threading it here (rather than only at thread-start)
	// means a mid-session effort change from the composer takes effect on
	// the very next turn without needing a session restart. Empty means
	// "inherit the thread default set during thread/start".
	if s.reasoningEffort != "" {
		params["effort"] = s.reasoningEffort
	}
	if s.approvalPolicy != "" {
		params["approvalPolicy"] = s.approvalPolicy
	}
	if s.sandbox != "" {
		sandboxPolicy, err := turnSandboxPolicy(s.sandbox)
		if err != nil {
			return err
		}
		params["sandboxPolicy"] = sandboxPolicy
	}

	resp, err := s.sendRequest(ctx, "turn/start", params)
	if err != nil {
		return fmt.Errorf("codex: turn/start: %w", err)
	}

	turnID := readNestedString(resp, "turn", "id")
	if turnID != "" {
		s.mu.Lock()
		s.activeTurnID = turnID
		s.mu.Unlock()
	}

	return nil
}

// Interrupt sends turn/interrupt to abort whatever the thread is
// currently doing. We pass `turnId: activeTurnID` when a turn is in
// flight and `turnId: ""` (the empty string) when the user pressed
// stop during the dispatch window before `turn/started` arrived —
// the upstream app-server treats an empty turn_id as a "startup
// interrupt" and submits Op::Interrupt to the core anyway, then
// responds immediately with `{}` because startup cancellation has
// no TurnAborted event to wait on. See
// codex-rs/app-server/src/codex_message_processor.rs:7790-7849
// (`is_startup_interrupt = turn_id.is_empty()`) and the README
// summary at codex-rs/app-server/README.md:167.
//
// On success, drains pending approvals and user-input requests so
// the frontend clears its prompt panel and Codex's pending JSON-RPC
// requests resolve. Drain on failure too: the user pressed stop and
// expects the prompt to disappear even if the JSON-RPC errored.
// This is a deliberate fix beyond t3-code's
// CodexSessionRuntime.interruptTurn (CodexSessionRuntime.ts:1238–1250),
// which only sends the JSON-RPC and leaks the local Deferreds.
func (s *Session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	turnID := s.activeTurnID
	s.mu.Unlock()

	_, err := s.sendRequest(ctx, "turn/interrupt", map[string]any{
		"threadId": s.codexThreadID,
		"turnId":   turnID,
	})

	s.drainPendingApprovals("cancel", false)
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
	s.clearPendingApprovals()
	err := s.proc.Close()
	s.cancel()
	if s.readDone != nil {
		<-s.readDone
	}
	// Drop session-scoped maps so the closed Session doesn't hold onto
	// per-turn / per-child-thread entries indefinitely. The dispatch
	// goroutine has exited by this point (readDone closed), so no
	// concurrent writer races these deletions. We deliberately leave
	// s.pending as an empty map (readLoop already drained it) — a late
	// sendRequest caller would otherwise panic writing to a nil map;
	// the existing WriteLine-on-closed-proc path handles shutdown
	// cleanly.
	s.mu.Lock()
	s.seenTurnStarts = nil
	s.childParentByThread = nil
	s.childParentByAgentPath = nil
	s.agentPathByThread = nil
	s.mu.Unlock()
	return err
}

// -- Internal methods --

// sendRequest sends a JSON-RPC request and waits for a response. On
// timeout or context cancellation we remove the pending entry
// atomically with the lock, then drop the buffered response (if any)
// so the channel does not leak a record that no one will read. A
// response that arrives AFTER we delete the pending entry is dropped
// by dispatchLine's default branch — we cannot leak a late response
// once the pending entry is gone.
func (s *Session) sendRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextID.Add(1)

	ch := make(chan json.RawMessage, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()

	// abandon removes the pending entry under the lock and drains the
	// buffered response (if any) so a late write from dispatchLine
	// lands in a channel nobody is holding. Called exactly once by
	// whichever branch of the select below runs — we don't use a
	// defer so the drain happens BEFORE we return rather than after,
	// which eliminates the window where a late response can land in
	// the buffer-1 channel unobserved.
	abandon := func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		select {
		case <-ch:
		default:
		}
	}

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
		abandon()
		return nil, fmt.Errorf("codex: marshal request: %w", err)
	}

	if err := s.proc.WriteLine(data); err != nil {
		abandon()
		return nil, err
	}

	select {
	case <-ctx.Done():
		abandon()
		return nil, ctx.Err()
	case resp, ok := <-ch:
		// The happy path also needs to clear the pending entry. We do
		// it here rather than via defer so abandon's lock pattern is
		// the single source of truth.
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
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
	case <-time.After(s.requestTimeout()):
		abandon()
		return nil, fmt.Errorf("codex: %s: timeout", method)
	}
}

// defaultRequestTimeout bounds how long sendRequest waits for a JSON-RPC
// response. Overridable by tests via Session.requestTimeoutOverride.
const defaultRequestTimeout = 30 * time.Second

// requestTimeout returns the active JSON-RPC response timeout. Tests set
// a much shorter value via the unexported requestTimeoutOverride field
// so they can exercise the timeout + late-response path without waiting
// 30 seconds.
func (s *Session) requestTimeout() time.Duration {
	if s.requestTimeoutOverride > 0 {
		return s.requestTimeoutOverride
	}
	return defaultRequestTimeout
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
	return s.writeErrorResponseWithData(id, code, message, nil)
}

// writeErrorResponseWithData sends a JSON-RPC error response with an
// optional `data` payload. Codex's app-server inspects `data.reason`
// to decide how to handle in-flight server requests when the turn is
// transitioning — see `is_turn_transition_server_request_error` at
// codex-rs/app-server/src/server_request_error.rs and the early-return
// branches in bespoke_event_handling.rs (line 2390 for patch approval,
// 2447 for exec approval, 2494 for user input, 2603 for MCP
// elicitation, 2680 for permissions).
func (s *Session) writeErrorResponseWithData(id int64, code int, message string, data any) error {
	errFields := map[string]any{
		"code":    code,
		"message": message,
	}
	if data != nil {
		errFields["data"] = data
	}
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   errFields,
	}
	encoded, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("codex: marshal error response: %w", err)
	}
	return s.proc.WriteLine(encoded)
}

// codexTurnTransitionReason is the magic string Codex's app-server
// uses to recognize a JSON-RPC error as "the turn already ended,
// drop this request silently". See
// codex-rs/app-server/src/server_request_error.rs's
// `TURN_TRANSITION_PENDING_REQUEST_ERROR_REASON` constant. Sending
// this in the error `data.reason` makes drain-on-interrupt and
// drain-on-close clean up without falling through to "request failed
// with client error" log noise on Codex's side, AND for MCP
// elicitations, it specifically maps the response to
// `McpServerElicitationAction::Cancel` (the right semantics for
// "user aborted") rather than `Decline` (which means "user said no
// to this specific action").
const codexTurnTransitionReason = "turnTransition"

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
				meta, _ := json.Marshal(map[string]any{"fatal": true})
				s.onEvent(provider.ProviderEvent{
					Kind:      provider.EventError,
					ThreadID:  s.threadID,
					Content:   fmt.Sprintf("codex: read error: %v", err),
					Meta:      meta,
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
		providerThreadID := providerThreadIDFromParams(msg.Params)
		parentToolUseID := s.parentToolUseForProviderThread(providerThreadID)
		if parentToolUseID != "" {
			s.rememberAgentPathForProviderThread(providerThreadID, parentToolUseID, msg.Params)
		}
		if parentToolUseID != "" && isChildTurnLifecycleNotification(msg.Method) {
			return
		}

		if msg.Method == "item/plan/delta" {
			s.appendPlanDelta(msg.Params)
		}
		events := s.classifyNotificationWithBufferedPlan(msg.Method, msg.Params)

		// Detect <subagent_notification> tags injected by Codex core
		// into the next user-message item after a detached child agent
		// reaches a terminal state with no parent `wait` outstanding.
		// See codex-wire.md §<subagent_notification> and
		// codex-source/core/src/contextual_user_message.rs for the tag
		// constants. Each detected notification fires a separate
		// EventSubagentNotification so the triage pass-through emits a
		// `provider:subagent_notification` event for the frontend
		// tray / subagent UI to surface.
		if msg.Method == "item/completed" {
			if notifications := extractSubagentNotificationsFromUserMessage(msg.Params); len(notifications) > 0 {
				for _, n := range notifications {
					parentItemID := s.parentToolUseForAgentPath(n.AgentPath)
					s.onEvent(provider.ProviderEvent{
						Kind:      provider.EventSubagentNotification,
						ThreadID:  s.threadID,
						ItemID:    parentItemID,
						Meta:      buildSubagentNotificationMeta(n),
						Timestamp: time.Now(),
					})
				}
			}
		}

		for i := range events {
			evt := &events[i]
			if parentToolUseID != "" {
				evt.ParentToolUseID = parentToolUseID
			}
			s.maybeRewriteCollabControlItemID(evt, msg.Params)
			s.maybeRememberCollabReceiverThreads(msg.Method, msg.Params)
			// Track active turn ID for Interrupt.
			switch evt.Kind {
			case provider.EventTurnStart:
				if evt.TurnID != "" {
					if !s.claimTurnStart(evt.TurnID) {
						// The app-server occasionally re-sends
						// turn/started (recovery, retries). Suppress
						// the duplicate so downstream persistence
						// sees exactly one turn per user send
						// (Bug B6).
						continue
					}
					s.mu.Lock()
					s.activeTurnID = evt.TurnID
					s.mu.Unlock()
				}
			case provider.EventTurnComplete:
				s.mu.Lock()
				s.activeTurnID = ""
				s.mu.Unlock()
				s.clearPlanBufferForTurn(evt.TurnID)
				s.clearTurnStart(evt.TurnID)
			}
			// No heuristic background classification here (invariant 25).
			// The wire-typed signals are exposed in evt.Meta
			// (enrichItemMeta lifts `source`, `item_status`, `process_id`,
			// `agentsStates`); the projector in
			// internal/triage/codex_background.go reads those and stamps
			// `is_background=true` on the first model-produced yield or
			// at the turn-close catchall. Keeping projection out of the
			// session package preserves the "provider → triage" seam.
			s.onEvent(*evt)
		}
		return
	}
}

func (s *Session) classifyNotificationWithBufferedPlan(method string, params json.RawMessage) []provider.ProviderEvent {
	events := ClassifyNotification(s.threadID, method, params)
	if method != "item/completed" || classifyCodexItemType(params) != "plan" {
		return events
	}
	itemID := readNestedString(params, "item", "id")
	turnID := readTopLevelString(params, "turnId")
	buffered := s.takePlanBuffer(itemID, turnID)
	if buffered == "" {
		return events
	}
	for i := range events {
		if events[i].Kind == provider.EventProposedPlan {
			if events[i].Content == "" {
				events[i].Content = buffered
			}
			return events
		}
	}
	return []provider.ProviderEvent{{
		Kind:      provider.EventProposedPlan,
		ThreadID:  s.threadID,
		TurnID:    turnID,
		ItemID:    itemID,
		ItemType:  "plan",
		Content:   buffered,
		Meta:      params,
		Timestamp: time.Now(),
	}}
}

func (s *Session) appendPlanDelta(params json.RawMessage) {
	delta := readTopLevelString(params, "delta")
	if delta == "" {
		delta = readTopLevelString(params, "textDelta")
	}
	if delta == "" {
		return
	}
	itemID := readTopLevelString(params, "itemId")
	turnID := readTopLevelString(params, "turnId")
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := s.planBufferLocked(itemID, turnID)
	if buf.truncated {
		return
	}
	if buf.text.Len()+len(delta) > maxPlanDeltaBufferBytes {
		remaining := maxPlanDeltaBufferBytes - buf.text.Len()
		if remaining > 0 {
			buf.text.WriteString(delta[:remaining])
		}
		buf.truncated = true
		log.Printf("codex: proposed plan delta buffer exceeded %d bytes; truncating buffered fallback for thread %s", maxPlanDeltaBufferBytes, s.threadID)
		return
	}
	buf.text.WriteString(delta)
}

func (s *Session) planBufferLocked(itemID, turnID string) *planBuffer {
	if s.planBuffersByItemID == nil {
		s.planBuffersByItemID = make(map[string]*planBuffer)
	}
	if s.planBuffersByTurnID == nil {
		s.planBuffersByTurnID = make(map[string]*planBuffer)
	}
	if itemID != "" {
		if buf := s.planBuffersByItemID[itemID]; buf != nil {
			return buf
		}
	}
	if turnID != "" {
		if buf := s.planBuffersByTurnID[turnID]; buf != nil {
			if itemID != "" {
				if buf.itemID != "" && buf.itemID != itemID {
					delete(s.planBuffersByItemID, buf.itemID)
				}
				buf.itemID = itemID
				s.planBuffersByItemID[itemID] = buf
			}
			return buf
		}
	}
	if len(s.planBuffersByTurnID) >= maxPlanDeltaBuffers && turnID != "" {
		for existingTurnID, existing := range s.planBuffersByTurnID {
			if existing.itemID != "" {
				delete(s.planBuffersByItemID, existing.itemID)
			}
			delete(s.planBuffersByTurnID, existingTurnID)
			break
		}
	}
	if len(s.planBuffersByItemID) >= maxPlanDeltaBuffers && itemID != "" {
		for existingItemID, existing := range s.planBuffersByItemID {
			if existing.turnID != "" {
				delete(s.planBuffersByTurnID, existing.turnID)
			}
			delete(s.planBuffersByItemID, existingItemID)
			break
		}
	}
	buf := &planBuffer{itemID: itemID, turnID: turnID}
	if itemID != "" {
		s.planBuffersByItemID[itemID] = buf
	}
	if turnID != "" {
		s.planBuffersByTurnID[turnID] = buf
	}
	return buf
}

func (s *Session) takePlanBuffer(itemID, turnID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var buf *planBuffer
	if itemID != "" {
		buf = s.planBuffersByItemID[itemID]
	}
	if buf == nil && turnID != "" {
		buf = s.planBuffersByTurnID[turnID]
	}
	if buf == nil {
		return ""
	}
	if buf.itemID != "" {
		delete(s.planBuffersByItemID, buf.itemID)
	}
	if buf.turnID != "" {
		delete(s.planBuffersByTurnID, buf.turnID)
	}
	return buf.text.String()
}

func (s *Session) clearPlanBufferForTurn(turnID string) {
	if turnID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := s.planBuffersByTurnID[turnID]
	if buf == nil {
		return
	}
	if buf.itemID != "" {
		delete(s.planBuffersByItemID, buf.itemID)
	}
	delete(s.planBuffersByTurnID, turnID)
}

func isChildTurnLifecycleNotification(method string) bool {
	switch method {
	case "turn/started", "turn/completed", "turn/aborted":
		return true
	default:
		return false
	}
}

func (s *Session) parentToolUseForProviderThread(providerThreadID string) string {
	if providerThreadID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childParentByThread[providerThreadID]
}

func (s *Session) parentToolUseForAgentPath(agentPath string) string {
	if agentPath == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childParentByAgentPath[agentPath]
}

func (s *Session) setParentToolUseForProviderThread(providerThreadID, parentToolUseID string) {
	if providerThreadID == "" || parentToolUseID == "" {
		return
	}
	s.mu.Lock()
	if s.childParentByThread == nil {
		s.childParentByThread = make(map[string]string)
	}
	s.childParentByThread[providerThreadID] = parentToolUseID
	s.mu.Unlock()
}

func (s *Session) deleteParentToolUseForProviderThread(providerThreadID string) {
	if providerThreadID == "" {
		return
	}
	s.mu.Lock()
	if agentPath := s.agentPathByThread[providerThreadID]; agentPath != "" {
		delete(s.childParentByAgentPath, agentPath)
		delete(s.agentPathByThread, providerThreadID)
	}
	delete(s.childParentByThread, providerThreadID)
	s.mu.Unlock()
}

func (s *Session) rememberAgentPathForProviderThread(providerThreadID, parentToolUseID string, params json.RawMessage) {
	agentPath := subagentThreadStartedAgentPath(params)
	if providerThreadID == "" || parentToolUseID == "" || agentPath == "" {
		return
	}
	s.mu.Lock()
	if s.childParentByAgentPath == nil {
		s.childParentByAgentPath = make(map[string]string)
	}
	if s.agentPathByThread == nil {
		s.agentPathByThread = make(map[string]string)
	}
	s.childParentByAgentPath[agentPath] = parentToolUseID
	s.agentPathByThread[providerThreadID] = agentPath
	s.mu.Unlock()
}

func providerThreadIDFromParams(params json.RawMessage) string {
	if id := readTopLevelString(params, "threadId"); id != "" {
		return id
	}
	return readNestedString(params, "thread", "id")
}

func subagentThreadStartedAgentPath(params json.RawMessage) string {
	candidates := []string{
		readNestedString(params, "thread", "source", "subAgent", "thread_spawn", "agent_path"),
		readNestedString(params, "thread", "source", "subAgent", "threadSpawn", "agentPath"),
		readNestedString(params, "thread", "source", "subAgent", "thread_spawn", "agentPath"),
		readNestedString(params, "thread", "source", "subAgent", "threadSpawn", "agent_path"),
	}
	for _, candidate := range candidates {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

func (s *Session) maybeRememberCollabReceiverThreads(method string, params json.RawMessage) {
	if method != "item/completed" {
		return
	}
	item := readNestedObject(params, "item")
	if item == nil || readRawString(item, "type") != "collabAgentToolCall" {
		return
	}
	tool := normalizeCollabToolName(readRawString(item, "tool"))
	itemID := readRawString(item, "id")
	receiverThreadIDs := readRawStringArray(item, "receiverThreadIds")
	if itemID == "" || len(receiverThreadIDs) == 0 {
		return
	}

	switch tool {
	case "spawn_agent":
		for _, receiverThreadID := range receiverThreadIDs {
			s.setParentToolUseForProviderThread(receiverThreadID, itemID)
			go s.resumeChildThread(receiverThreadID)
		}
	case "close_agent":
		for _, receiverThreadID := range receiverThreadIDs {
			s.deleteParentToolUseForProviderThread(receiverThreadID)
		}
	}
}

func (s *Session) maybeRewriteCollabControlItemID(evt *provider.ProviderEvent, params json.RawMessage) {
	if evt == nil || evt.ItemType != "collab_agent" {
		return
	}
	item := readNestedObject(params, "item")
	if item == nil || readRawString(item, "type") != "collabAgentToolCall" {
		return
	}
	switch normalizeCollabToolName(readRawString(item, "tool")) {
	case "close_agent", "resume_agent":
		for _, receiverThreadID := range readRawStringArray(item, "receiverThreadIds") {
			if parentToolUseID := s.parentToolUseForProviderThread(receiverThreadID); parentToolUseID != "" {
				evt.ItemID = parentToolUseID
				return
			}
		}
	}
}

func (s *Session) resumeChildThread(providerThreadID string) {
	if s.proc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _ = s.sendRequest(ctx, "thread/resume", map[string]any{
		"threadId": providerThreadID,
	})
}

// claimTurnStart records the first observation of a turnID, returning
// true. A second observation returns false so dispatchLine can skip the
// duplicate EventTurnStart. The map is bounded by the number of live
// turns — cleared on EventTurnComplete or session Close.
func (s *Session) claimTurnStart(turnID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenTurnStarts == nil {
		s.seenTurnStarts = make(map[string]struct{})
	}
	if _, ok := s.seenTurnStarts[turnID]; ok {
		return false
	}
	s.seenTurnStarts[turnID] = struct{}{}
	return true
}

// clearTurnStart drops the recorded turnID on completion so a follow-up
// turn with the same ID (rare, but possible across resumed sessions)
// can fire fresh.
func (s *Session) clearTurnStart(turnID string) {
	if turnID == "" {
		return
	}
	s.mu.Lock()
	delete(s.seenTurnStarts, turnID)
	s.mu.Unlock()
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
		s.startApprovalTimer(rpcID, provider.EventApprovalResolved)
		s.onEvent(buildApprovalEvent(s.threadID, turnID, itemID, meta, line))

	case "mcpServer/elicitation/request":
		meta := buildElicitationMeta(s.threadID, turnID, rpcID, params)
		s.startApprovalTimer(rpcID, provider.EventApprovalResolved, "mcp-elicitation")
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
		questions := parseUserInputQuestions(params)
		if len(questions) == 0 {
			if err := s.writeErrorResponse(rpcID, -32602, "requestUserInput requires at least one question"); err != nil {
				log.Printf("codex: failed to send invalid requestUserInput response: %v", err)
			}
			return
		}
		meta := buildUserInputMetaFromQuestions(s.threadID, turnID, rpcID, questions)
		s.startApprovalTimer(rpcID, provider.EventUserInputResolved)
		s.onEvent(buildUserInputEvent(s.threadID, turnID, itemID, meta, line))

	case "item/permissions/requestApproval":
		meta := buildPermissionMeta(s.threadID, turnID, rpcID, params)
		s.startApprovalTimer(rpcID, provider.EventApprovalResolved)
		s.onEvent(buildApprovalEvent(s.threadID, turnID, itemID, meta, line))

	default:
		if err := s.writeErrorResponse(rpcID, -32601, fmt.Sprintf("unsupported server request: %s", method)); err != nil {
			log.Printf("codex: failed to send error response for %s: %v", method, err)
		}
	}
}

// startApprovalTimer registers the approval and arms the auto-deny timer.
// Uses the numeric JSON-RPC id rendered as a string so dedup (Bug B9) and
// response routing both use the same key.
func (s *Session) startApprovalTimer(rpcID int64, resolveKind provider.EventKind, timeoutResponseKind ...string) {
	requestID := fmt.Sprintf("%d", rpcID)
	timeout := s.approvalTimeout
	if timeout <= 0 {
		timeout = DefaultApprovalTimeout
	}
	cancel := make(chan struct{})
	s.approvalsMu.Lock()
	if s.approvalsClosed {
		s.approvalsMu.Unlock()
		return
	}
	if s.pendingApprovals == nil {
		s.pendingApprovals = make(map[string]*pendingApproval)
	}
	if existing, ok := s.pendingApprovals[requestID]; ok {
		close(existing.cancel)
	}
	responseKind := ""
	if len(timeoutResponseKind) > 0 {
		responseKind = timeoutResponseKind[0]
	}
	s.pendingApprovals[requestID] = &pendingApproval{
		cancel:              cancel,
		resolveKind:         resolveKind,
		timeoutResponseKind: responseKind,
	}
	// Starting a new timer re-opens the ID: e.g. if we previously
	// resolved it and the provider re-sent the request (unusual, but
	// cheap to support).
	delete(s.resolvedApprovals, requestID)
	s.approvalsMu.Unlock()
	go s.runApprovalTimer(rpcID, requestID, timeout, cancel, resolveKind, responseKind)
}

// runApprovalTimer fires the auto-decline when the user fails to respond
// in time. The cancel signal means the user answered first or the session
// is shutting down.
func (s *Session) runApprovalTimer(rpcID int64, requestID string, timeout time.Duration, cancel <-chan struct{}, resolveKind provider.EventKind, timeoutResponseKind string) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-cancel:
		return
	case <-timer.C:
	}

	if !s.claimApproval(requestID, resolveKind) {
		return
	}

	timeoutSubject := "approval"
	if resolveKind == provider.EventUserInputResolved {
		timeoutSubject = "user input"
	} else if timeoutResponseKind == "mcp-elicitation" {
		timeoutSubject = "MCP elicitation"
	}
	s.onEvent(provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  s.threadID,
		Content:   fmt.Sprintf("codex: %s timed out for request %s after %s — auto-denied to keep session alive", timeoutSubject, requestID, timeout),
		Timestamp: time.Now(),
	})
	meta, _ := json.Marshal(map[string]any{
		"requestId": requestID,
		"decision":  "timeout",
	})
	s.onEvent(provider.ProviderEvent{
		Kind:      resolveKind,
		ThreadID:  s.threadID,
		ItemID:    requestID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
	if resolveKind == provider.EventUserInputResolved {
		if err := s.writeErrorResponse(rpcID, -32000, "user input timed out"); err != nil {
			log.Printf("codex: write user-input timeout for %s: %v", requestID, err)
		}
		return
	}
	if timeoutResponseKind == "mcp-elicitation" {
		if err := s.writeResponse(rpcID, map[string]any{"action": "decline"}); err != nil {
			log.Printf("codex: write MCP elicitation timeout for %s: %v", requestID, err)
		}
		return
	}
	if err := s.writeResponse(rpcID, map[string]any{"decision": "decline"}); err != nil {
		log.Printf("codex: write auto-deny for %s: %v", requestID, err)
	}
}

// claimApproval returns true when the caller is the first to answer the
// approval for requestID. False means either we already answered (Bug B9
// dedup) or the session is closing. Cancels any pending auto-deny timer
// so the goroutine exits.
func (s *Session) claimApproval(requestID string, expectedKind provider.EventKind) bool {
	s.approvalsMu.Lock()
	if _, already := s.resolvedApprovals[requestID]; already {
		s.approvalsMu.Unlock()
		return false
	}
	pending, hadPending := s.pendingApprovals[requestID]
	if !hadPending || pending.resolveKind != expectedKind {
		s.approvalsMu.Unlock()
		return false
	}
	delete(s.pendingApprovals, requestID)
	if s.resolvedApprovals == nil {
		s.resolvedApprovals = make(map[string]struct{})
	}
	// Soft-cap the dedup set so long-running sessions don't accumulate
	// one entry per answered approval for the life of the process. The
	// hot window is small; dropping older IDs may admit a duplicate
	// response for an ancient request at worst, which the provider
	// discards.
	if len(s.resolvedApprovals) >= resolvedApprovalsSoftCap {
		s.resolvedApprovals = make(map[string]struct{})
	}
	s.resolvedApprovals[requestID] = struct{}{}
	s.approvalsMu.Unlock()
	if hadPending {
		close(pending.cancel)
	}
	return true
}

// clearPendingApprovals is the Close-path drain. Sets approvalsClosed so
// late approvals are refused after teardown and emits resolved events
// with `decision: "lost"` (the session-died-mid-prompt signal triage
// uses to flip rows to errored).
func (s *Session) clearPendingApprovals() {
	s.drainPendingApprovals("lost", true)
}

// drainPendingApprovals resolves every outstanding approval and
// user-input request. For each one we:
//
//  1. Cancel the auto-deny timer so its goroutine exits.
//  2. Write a JSON-RPC response (decline / error) to the provider so
//     the in-flight server request unblocks. Skipped silently when the
//     request ID is malformed (defensive only — startApprovalTimer
//     formats it from int64).
//  3. Emit the matching EventApprovalResolved / EventUserInputResolved
//     so the frontend clears its prompt panel. User-input variants
//     additionally carry an empty `answers: {}` map to satisfy the
//     frontend type contract.
//
// closeSession=true is the Close path — set approvalsClosed so late
// approval requests can't schedule fresh timers, and drop the
// resolvedApprovals dedup set since no duplicate response can reach
// the provider after Close. closeSession=false is the Interrupt path —
// the session keeps running and may receive new approval requests.
func (s *Session) drainPendingApprovals(decisionWord string, closeSession bool) {
	s.approvalsMu.Lock()
	if closeSession {
		s.approvalsClosed = true
	}
	pending := s.pendingApprovals
	s.pendingApprovals = nil
	if closeSession {
		s.resolvedApprovals = nil
	}
	s.approvalsMu.Unlock()

	for requestID, p := range pending {
		close(p.cancel)
		s.writeDrainResponse(requestID, p, decisionWord)

		metaFields := map[string]any{
			"requestId": requestID,
			"decision":  decisionWord,
		}
		if p.resolveKind == provider.EventUserInputResolved {
			metaFields["answers"] = map[string]any{}
		}
		meta, _ := json.Marshal(metaFields)
		s.onEvent(provider.ProviderEvent{
			Kind:      p.resolveKind,
			ThreadID:  s.threadID,
			ItemID:    requestID,
			Meta:      meta,
			Timestamp: time.Now(),
		})
	}
}

// writeDrainResponse releases Codex from a pending server request by
// sending a JSON-RPC error with `data.reason = "turnTransition"`. This
// is the wire-correct way to abandon any kind of pending request
// (command-execution approval, file-change approval, permissions,
// user-input, MCP elicitation): Codex's app-server early-returns on
// this exact error shape via `is_turn_transition_server_request_error`
// (codex-rs/app-server/src/server_request_error.rs). Sending the
// success-shape decline instead works by accident — Codex falls
// through to a per-handler "client error" branch that logs noise and,
// for MCP elicitation specifically, picks `Decline` instead of the
// semantically-correct `Cancel`. Best-effort: a write failure during
// Close is logged, not surfaced.
func (s *Session) writeDrainResponse(requestID string, p *pendingApproval, decisionWord string) {
	rpcID, err := strconv.ParseInt(requestID, 10, 64)
	if err != nil {
		return
	}
	message := fmt.Sprintf("client request resolved because the turn state was changed (%s)", decisionWord)
	if err := s.writeErrorResponseWithData(rpcID, -1, message, map[string]any{
		"reason": codexTurnTransitionReason,
	}); err != nil {
		log.Printf("codex: drain response for %s (kind=%v): %v", requestID, p.resolveKind, err)
	}
}

// resolvedApprovalsSoftCap bounds the per-session dedup set on long
// sessions. See the Claude session's constant of the same name for
// rationale.
const resolvedApprovalsSoftCap = 1000

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

func buildUserInputEvent(threadID, turnID, itemID string, meta json.RawMessage, raw json.RawMessage) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:      provider.EventUserInputRequest,
		ThreadID:  threadID,
		TurnID:    turnID,
		ItemID:    itemID,
		Meta:      meta,
		Timestamp: time.Now(),
		Raw:       raw,
	}
}
