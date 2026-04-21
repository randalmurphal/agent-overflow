package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"regexp"
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

// ErrApprovalAlreadyResolved is returned by RespondToApproval when the
// request ID has already been answered (either by an earlier response or
// by the auto-deny timeout). Prevents a second write landing at the
// provider with a stale decision.
var ErrApprovalAlreadyResolved = fmt.Errorf("codex: approval already resolved")

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
	// There is deliberately no background-tool classifier here: Codex has
	// no `run_in_background` concept (invariant 25, see docs/architecture/
	// invariants.md). Every Codex tool closes via `item/completed`; we do
	// not stamp `is_background=true` on any Codex item.
	childParentByThread map[string]string
	// probeFn is a test-only override for Probe(). When non-nil, Probe
	// skips the wire call and returns the result from this function.
	// Production Session construction (NewSession) never sets it.
	probeFn func(ctx context.Context) (ProbeResult, error)
	// resumeFn mirrors probeFn for Resume(). Used by
	// app_codex_reconcile_test.go to verify the post-probe rehydration
	// path without needing a live app-server. Production NewSession
	// never sets it.
	resumeFn func(ctx context.Context) error
}

// pendingApproval tracks one in-flight approval so the timer can be
// cancelled when the user responds (Bug B3) or the session closes.
type pendingApproval struct {
	cancel chan struct{}
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
		proc:                proc,
		threadID:            threadID,
		model:               cfg.Model,
		reasoningEffort:     cfg.ReasoningEffort,
		pending:             make(map[int64]chan json.RawMessage),
		onEvent:             onEvent,
		cancel:              cancel,
		readDone:            make(chan struct{}),
		childParentByThread: make(map[string]string),
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
func (s *Session) Send(ctx context.Context, content string) error {
	params := map[string]any{
		"threadId": s.codexThreadID,
		"input": []map[string]any{{
			"type":          "text",
			"text":          content,
			"text_elements": []any{},
		}},
	}
	// Per-turn effort override — Codex's TurnStartParams takes `effort` at
	// the top level. Threading it here (rather than only at thread-start)
	// means a mid-session effort change from the composer takes effect on
	// the very next turn without needing a session restart. Empty means
	// "inherit the thread default set during thread/start".
	if s.reasoningEffort != "" {
		params["effort"] = s.reasoningEffort
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
		providerThreadID := readTopLevelString(msg.Params, "threadId")
		parentToolUseID := s.parentToolUseForProviderThread(providerThreadID)
		if parentToolUseID != "" && isChildTurnLifecycleNotification(msg.Method) {
			return
		}

		events := ClassifyNotification(s.threadID, msg.Method, msg.Params)

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
					s.onEvent(provider.ProviderEvent{
						Kind:      provider.EventSubagentNotification,
						ThreadID:  s.threadID,
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
				s.clearTurnStart(evt.TurnID)
			}
			// No background-tool classification is performed here —
			// Codex has no `run_in_background` concept (invariant 25).
			// Stamping `is_background=true` on Codex items produced
			// ghost tray rows that never completed because nothing on
			// the Codex wire emits a task-lifecycle terminal to pair
			// with them.
			s.onEvent(*evt)
		}
		return
	}
}

func isChildTurnLifecycleNotification(method string) bool {
	switch method {
	case "turn/started", "turn/completed", "turn/aborted":
		return true
	default:
		return false
	}
}

// subagentNotification is the parsed shape of a single
// <subagent_notification>{...}</subagent_notification> tag that Codex
// core injects into the NEXT user-message item when a detached child
// agent has reached a terminal state without a parent `wait`
// outstanding. See codex-source's `core/src/contextual_user_message.rs`
// for the tag constants and `core/tests/suite/subagent_notifications.rs`
// for the canonical wire shape (test fixture, lines 274-296).
//
// The tag payload is a JSON object with at least `agent_id` and
// `status`; `status` is a CollabAgentStatus value
// ("completed" | "errored" | "interrupted" | "shutdown" | ...).
// Additional fields may appear in future Codex versions — we preserve
// them in `extra` so downstream can opt into richer rendering without
// a parser update.
type subagentNotification struct {
	AgentID string         `json:"agent_id"`
	Status  string         `json:"status"`
	Extra   map[string]any `json:"-"`
}

// subagentNotificationPattern matches a single
// <subagent_notification>...</subagent_notification> block with lenient
// whitespace handling. The inner body is captured in group 1 and then
// JSON-decoded. `(?s)` makes `.` cross newlines — Codex's core writes
// the tag on a single line today but the tests pin a single-line shape
// and humans might paste multi-line bodies during reproduction.
var subagentNotificationPattern = regexp.MustCompile(`(?s)<subagent_notification>\s*(.*?)\s*</subagent_notification>`)

// parseSubagentNotifications extracts every
// <subagent_notification>...</subagent_notification> tag in text,
// JSON-decoding each body into a subagentNotification. Malformed tags
// (body isn't JSON, or missing agent_id / status) are silently skipped —
// surfacing a parse error would leak a Codex-core detail into our UI
// and a single broken notification should not block the user turn from
// rendering. Multiple tags per text are supported and returned in the
// order they appear.
func parseSubagentNotifications(text string) []subagentNotification {
	if text == "" || !strings.Contains(text, "<subagent_notification>") {
		return nil
	}
	matches := subagentNotificationPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	notifications := make([]subagentNotification, 0, len(matches))
	for _, match := range matches {
		body := strings.TrimSpace(match[1])
		if body == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			continue
		}
		agentID, _ := raw["agent_id"].(string)
		status, _ := raw["status"].(string)
		if agentID == "" || status == "" {
			continue
		}
		delete(raw, "agent_id")
		delete(raw, "status")
		notifications = append(notifications, subagentNotification{
			AgentID: agentID,
			Status:  status,
			Extra:   raw,
		})
	}
	if len(notifications) == 0 {
		return nil
	}
	return notifications
}

// extractSubagentNotificationsFromUserMessage inspects an item/completed
// params envelope and returns any subagent notifications the
// user-message's content carries. Returns nil when the item is not a
// userMessage, the content array is missing, or no tags are present.
// The wire shape is:
//
//	params.item.type == "userMessage"
//	params.item.content: [{type: "text", text: "...<subagent_notification>...</subagent_notification>..."}, ...]
//
// We concatenate every text UserInput entry before running the regex so
// a notification split across multiple entries (shouldn't happen today
// but cheap to tolerate) is still detected.
func extractSubagentNotificationsFromUserMessage(params json.RawMessage) []subagentNotification {
	item := readNestedObject(params, "item")
	if item == nil {
		return nil
	}
	if readRawString(item, "type") != "userMessage" {
		return nil
	}
	contentRaw, ok := item["content"]
	if !ok {
		return nil
	}
	var content []map[string]json.RawMessage
	if err := json.Unmarshal(contentRaw, &content); err != nil {
		return nil
	}
	var builder strings.Builder
	for _, entry := range content {
		// UserInput tagged union: only the "text" variant carries the
		// notification payload; image / localImage / skill / mention
		// variants don't. See codex-source v2/UserInput.ts.
		var entryType string
		if rawType, ok := entry["type"]; ok {
			_ = json.Unmarshal(rawType, &entryType)
		}
		if entryType != "text" {
			continue
		}
		rawText, ok := entry["text"]
		if !ok {
			continue
		}
		var text string
		if err := json.Unmarshal(rawText, &text); err != nil {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(text)
	}
	return parseSubagentNotifications(builder.String())
}

// buildSubagentNotificationMeta serialises a parsed subagentNotification
// into the json.RawMessage the triage handler forwards on
// `provider:subagent_notification`. Any `Extra` fields are merged onto
// the top level so future Codex core versions can add fields without a
// parser update; the load-bearing `agent_id` / `status` keys win on
// collision so a stray Extra entry can't poison the contract.
func buildSubagentNotificationMeta(n subagentNotification) json.RawMessage {
	fields := make(map[string]any, 2+len(n.Extra))
	for k, v := range n.Extra {
		fields[k] = v
	}
	fields["agent_id"] = n.AgentID
	fields["status"] = n.Status
	meta, err := json.Marshal(fields)
	if err != nil {
		// Fallback to a minimal payload so the frontend still gets the
		// load-bearing fields even if a malformed Extra entry trips
		// Marshal. The parser already validated agent_id/status are
		// strings, so this encode is safe.
		minimal, _ := json.Marshal(map[string]string{
			"agent_id": n.AgentID,
			"status":   n.Status,
		})
		log.Printf("codex: subagent_notification meta marshal fallback: %v", err)
		return minimal
	}
	return meta
}

func (s *Session) parentToolUseForProviderThread(providerThreadID string) string {
	if providerThreadID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childParentByThread[providerThreadID]
}

func (s *Session) setParentToolUseForProviderThread(providerThreadID, parentToolUseID string) {
	if providerThreadID == "" || parentToolUseID == "" {
		return
	}
	s.mu.Lock()
	s.childParentByThread[providerThreadID] = parentToolUseID
	s.mu.Unlock()
}

func (s *Session) deleteParentToolUseForProviderThread(providerThreadID string) {
	if providerThreadID == "" {
		return
	}
	s.mu.Lock()
	delete(s.childParentByThread, providerThreadID)
	s.mu.Unlock()
}

func (s *Session) maybeRememberCollabReceiverThreads(method string, params json.RawMessage) {
	if method != "item/completed" && method != "item/updated" {
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
		s.startApprovalTimer(rpcID)
		s.onEvent(buildApprovalEvent(s.threadID, turnID, itemID, meta, line))

	case "mcpServer/elicitation/request":
		meta := buildElicitationMeta(s.threadID, turnID, rpcID, params)
		s.startApprovalTimer(rpcID)
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
		s.startApprovalTimer(rpcID)
		s.onEvent(buildApprovalEvent(s.threadID, turnID, itemID, meta, line))

	case "item/permissions/requestApproval":
		meta := buildPermissionMeta(s.threadID, turnID, rpcID, params)
		s.startApprovalTimer(rpcID)
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
func (s *Session) startApprovalTimer(rpcID int64) {
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
	s.pendingApprovals[requestID] = &pendingApproval{cancel: cancel}
	// Starting a new timer re-opens the ID: e.g. if we previously
	// resolved it and the provider re-sent the request (unusual, but
	// cheap to support).
	delete(s.resolvedApprovals, requestID)
	s.approvalsMu.Unlock()
	go s.runApprovalTimer(rpcID, requestID, timeout, cancel)
}

// runApprovalTimer fires the auto-decline when the user fails to respond
// in time. The cancel signal means the user answered first or the session
// is shutting down.
func (s *Session) runApprovalTimer(rpcID int64, requestID string, timeout time.Duration, cancel <-chan struct{}) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-cancel:
		return
	case <-timer.C:
	}

	if !s.claimApproval(requestID) {
		return
	}

	s.onEvent(provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  s.threadID,
		Content:   fmt.Sprintf("codex: approval timed out for request %s after %s — auto-denied to keep session alive", requestID, timeout),
		Timestamp: time.Now(),
	})
	meta, _ := json.Marshal(map[string]any{
		"requestId": requestID,
		"decision":  "timeout",
	})
	s.onEvent(provider.ProviderEvent{
		Kind:      provider.EventApprovalResolved,
		ThreadID:  s.threadID,
		ItemID:    requestID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
	if err := s.writeResponse(rpcID, map[string]any{"decision": "decline"}); err != nil {
		log.Printf("codex: write auto-deny for %s: %v", requestID, err)
	}
}

// claimApproval returns true when the caller is the first to answer the
// approval for requestID. False means either we already answered (Bug B9
// dedup) or the session is closing. Cancels any pending auto-deny timer
// so the goroutine exits.
//
// Callers that want to respond even when no request was tracked (e.g. the
// legacy app-level callers that don't wait for handleServerRequest) should
// still succeed on the first call: the absence of a pending entry is not
// an error — but a second call for the same ID is.
func (s *Session) claimApproval(requestID string) bool {
	s.approvalsMu.Lock()
	if _, already := s.resolvedApprovals[requestID]; already {
		s.approvalsMu.Unlock()
		return false
	}
	pending, hadPending := s.pendingApprovals[requestID]
	if hadPending {
		delete(s.pendingApprovals, requestID)
	}
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

// clearPendingApprovals cancels every outstanding auto-deny timer. Called
// by Close so the goroutines exit cleanly instead of racing the teardown.
// Also drops the resolvedApprovals dedup set — once Close has been
// called, no duplicate response can reach the provider anyway.
func (s *Session) clearPendingApprovals() {
	s.approvalsMu.Lock()
	s.approvalsClosed = true
	pending := s.pendingApprovals
	s.pendingApprovals = nil
	s.resolvedApprovals = nil
	s.approvalsMu.Unlock()
	for requestID, p := range pending {
		close(p.cancel)
		meta, _ := json.Marshal(map[string]any{
			"requestId": requestID,
			"decision":  "lost",
		})
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventApprovalResolved,
			ThreadID:  s.threadID,
			ItemID:    requestID,
			Meta:      meta,
			Timestamp: time.Now(),
		})
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
