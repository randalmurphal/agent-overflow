package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/logging"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
)

const (
	defaultRequestTimeout = 30 * time.Second
	defaultCloseTimeout   = 5 * time.Second
)

// FSHandler optionally handles client-side ACP fs/* server requests when
// ClientCapabilities.FS is advertised. Leave nil for agents that handle file
// I/O natively (such as agy).
type FSHandler interface {
	ReadTextFile(ctx context.Context, params json.RawMessage) (any, error)
	WriteTextFile(ctx context.Context, params json.RawMessage) (any, error)
}

// TerminalHandler optionally handles client-side ACP terminal/* server requests
// when ClientCapabilities.Terminal is advertised.
type TerminalHandler interface {
	Handle(ctx context.Context, method string, params json.RawMessage) (any, error)
}

// ConnConfig configures a multiplexed ACP subprocess connection.
type ConnConfig struct {
	Binary             string
	Args               []string
	WorkDir            string
	Env                map[string]string
	UnsetEnv           []string
	EventLogger        *logging.Logger
	Reaper             procutil.Reaper
	ThreadID           string
	ProviderName       string
	ClientName         string
	ClientVersion      string
	ClientCapabilities ClientCapabilities
	RequestTimeout     time.Duration
	Flavor             Flavor
	FSHandler          FSHandler
	TerminalHandler    TerminalHandler
}

// ErrSessionBusy is returned by ReplayLoad when the requested native session
// ID is currently live or already being replayed on this Conn, so callers can
// fall back to an isolated one-shot connection without disturbing the live
// session.
var ErrSessionBusy = errors.New("acp: session is currently active on this connection")

const maxReplayCaptureBytes = 64 << 20 // 64 MiB byte budget per replay capture

type replayCapture struct {
	mu        sync.Mutex
	maxCount  int
	maxBytes  int64
	bytes     int64
	updates   []SessionUpdate
	truncated bool
	notify    chan struct{}
}

func estimateSessionUpdateBytes(u SessionUpdate) int64 {
	n := int64(len(u.SessionUpdate)+len(u.ToolCallID)+len(u.Title)+len(u.Name)+len(u.Kind)+len(u.Status)+len(u.CurrentModeID)) +
		int64(len(u.Content)+len(u.RawInput)+len(u.RawOutput)+len(u.Meta)) + 128
	return n
}

func (r *replayCapture) add(u SessionUpdate) {
	evBytes := estimateSessionUpdateBytes(u)
	r.mu.Lock()
	if (r.maxCount > 0 && len(r.updates) >= r.maxCount) ||
		(r.maxBytes > 0 && r.bytes+evBytes > r.maxBytes) {
		r.truncated = true
		r.mu.Unlock()
		return
	}
	r.updates = append(r.updates, u)
	r.bytes += evBytes
	ch := r.notify
	r.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// UpdateCapture observes session/update notifications for one sessionId until
// Stop is called.
type UpdateCapture struct {
	conn      *Conn
	sessionID string
	cap       *replayCapture
}

// WaitFor blocks until an already-captured or newly arriving update matches
// pred, timeout elapses, or ctx is cancelled.
func (u *UpdateCapture) WaitFor(ctx context.Context, pred func(SessionUpdate) bool, timeout time.Duration) bool {
	if u == nil || u.cap == nil || pred == nil {
		return false
	}
	check := func() bool {
		u.cap.mu.Lock()
		defer u.cap.mu.Unlock()
		for _, upd := range u.cap.updates {
			if pred(upd) {
				return true
			}
		}
		return false
	}
	if check() {
		return true
	}
	if timeout <= 0 {
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-u.cap.notify:
			if check() {
				return true
			}
		case <-timer.C:
			return check()
		case <-ctx.Done():
			return check()
		}
	}
}

// Stop unregisters the capture and returns all observed updates in order.
func (u *UpdateCapture) Stop() []SessionUpdate {
	if u == nil || u.conn == nil || u.cap == nil {
		return nil
	}
	u.conn.routesMu.Lock()
	if u.conn.replays[u.sessionID] == u.cap {
		delete(u.conn.replays, u.sessionID)
	}
	u.conn.routesMu.Unlock()
	u.cap.mu.Lock()
	out := append([]SessionUpdate(nil), u.cap.updates...)
	u.cap.mu.Unlock()
	return out
}

// Conn hosts one multiplexed ACP server subprocess and demultiplexes many
// Session conversations by sessionId. Implements provider.Connection.
type Conn struct {
	proc            *provider.Process
	cancel          context.CancelFunc
	binary          string
	providerName    string
	flavor          Flavor
	requestTimeout  time.Duration
	fsHandler       FSHandler
	terminalHandler TerminalHandler

	caps        AgentCapabilities
	agentInfo   AgentInfo
	authMethods []AuthMethod

	nextID             atomic.Int64
	pendingMu          sync.Mutex
	pending            map[int64]chan json.RawMessage
	pendingSessionOpen map[int64]*Session

	routesMu sync.Mutex
	sessions map[string]*Session
	replays  map[string]*replayCapture

	closing   atomic.Bool
	closeOnce sync.Once
	closeErr  error
	done      chan struct{}
}

var _ provider.Connection = (*Conn)(nil)

// Dial spawns an ACP subprocess, starts its readLoop, and completes the
// protocolVersion=1 initialize handshake.
func Dial(ctx context.Context, cfg ConnConfig) (*Conn, error) {
	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		return nil, errors.New("acp: binary path is required")
	}
	providerName := strings.TrimSpace(cfg.ProviderName)
	if providerName == "" {
		providerName = "acp"
	}
	clientName := strings.TrimSpace(cfg.ClientName)
	if clientName == "" {
		clientName = "agent-overflow"
	}
	clientVersion := strings.TrimSpace(cfg.ClientVersion)
	if clientVersion == "" {
		clientVersion = "1.0.0"
	}
	flavor := cfg.Flavor
	if flavor == nil {
		flavor = DefaultFlavor{Kind: provider.ProviderKind(providerName)}
	}
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}

	childCtx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(childCtx, provider.Watched(cfg.Reaper), provider.SpawnConfig{
		Binary:           binary,
		Args:             cfg.Args,
		Dir:              cfg.WorkDir,
		Env:              cfg.Env,
		UnsetEnv:         cfg.UnsetEnv,
		EventLogger:      cfg.EventLogger,
		EventLogRedactor: redactOutboundACPFrame,
		ThreadID:         cfg.ThreadID,
		Provider:         providerName,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("acp: spawn %s: %w", binary, err)
	}

	c := &Conn{
		proc:               proc,
		cancel:             cancel,
		binary:             binary,
		providerName:       providerName,
		flavor:             flavor,
		requestTimeout:     timeout,
		fsHandler:          cfg.FSHandler,
		terminalHandler:    cfg.TerminalHandler,
		pending:            make(map[int64]chan json.RawMessage),
		pendingSessionOpen: make(map[int64]*Session),
		sessions:           make(map[string]*Session),
		replays:            make(map[string]*replayCapture),
		done:               make(chan struct{}),
	}

	go c.readLoop()

	initParams := InitializeParams{
		ProtocolVersion:    ProtocolVersion,
		ClientCapabilities: cfg.ClientCapabilities,
		ClientInfo: ClientInfo{
			Name:    clientName,
			Version: clientVersion,
		},
	}
	rawInit, err := c.sendRequest(ctx, "initialize", initParams)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("acp: initialize: %w", err)
	}
	var initRes InitializeResult
	if err := json.Unmarshal(rawInit, &initRes); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("acp: decode initialize response: %w", err)
	}
	c.caps = initRes.AgentCapabilities
	c.agentInfo = initRes.AgentInfo
	c.authMethods = initRes.AuthMethods
	return c, nil
}

func (c *Conn) PID() int {
	if c == nil || c.proc == nil {
		return 0
	}
	return c.proc.PID()
}

func (c *Conn) Done() <-chan struct{} {
	return c.done
}

func (c *Conn) Binary() string                  { return c.binary }
func (c *Conn) Capabilities() AgentCapabilities { return c.caps }
func (c *Conn) AgentInfo() AgentInfo            { return c.agentInfo }
func (c *Conn) AuthMethods() []AuthMethod       { return append([]AuthMethod(nil), c.authMethods...) }

// Close shuts down the ACP subprocess and waits for readLoop teardown to finish.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.closing.Store(true)
		if c.proc != nil {
			c.closeErr = c.proc.Close()
		}
		if c.cancel != nil {
			c.cancel()
		}
		if c.done != nil {
			<-c.done
		}
	})
	return c.closeErr
}

// Authenticate invokes the ACP authenticate RPC for methodID.
func (c *Conn) Authenticate(ctx context.Context, methodID string) error {
	_, err := c.sendRequest(ctx, "authenticate", map[string]any{"methodId": methodID})
	return err
}

// Logout invokes the ACP logout RPC.
func (c *Conn) Logout(ctx context.Context) error {
	_, err := c.sendRequest(ctx, "logout", map[string]any{})
	return err
}

// ListSessions calls session/list and filters client-side by cwd when non-empty
// (some ACP servers such as agy 1.5.13 ignore the server-side cwd filter).
func (c *Conn) ListSessions(ctx context.Context, cwd string) ([]SessionSummary, error) {
	params := SessionListParams{CWD: cwd}
	var all []SessionSummary
	seen := make(map[string]struct{})
	wantCWD := normalizeCleanPath(cwd)

	for range 32 {
		raw, err := c.sendRequest(ctx, "session/list", params)
		if err != nil {
			return nil, err
		}
		var res SessionListResult
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("acp: decode session/list: %w", err)
		}
		for _, s := range res.Sessions {
			if s.SessionID == "" {
				continue
			}
			if _, dup := seen[s.SessionID]; dup {
				continue
			}
			seen[s.SessionID] = struct{}{}
			if wantCWD != "" && !pathsMatch(wantCWD, s.CWD) {
				continue
			}
			all = append(all, s)
		}
		if res.NextCursor == "" {
			return all, nil
		}
		params.Cursor = res.NextCursor
	}
	return nil, errors.New("acp: session/list exceeded 32 pagination pages without completing")
}

// ReplayResult holds the updates replayed by an agent during session/load.
type ReplayResult struct {
	SessionID string
	Updates   []SessionUpdate
	Truncated bool
	Open      SessionOpenResult
}

// ReplayLoad runs session/load to capture replayed history updates (bounded by
// maxUpdates and a 64 MiB byte budget) and immediately closes the loaded
// session on the agent. Returns ErrSessionBusy atomically if sessionId is live
// or already being replayed on this Conn.
func (c *Conn) ReplayLoad(ctx context.Context, params SessionLoadParams, maxUpdates int) (ReplayResult, error) {
	if strings.TrimSpace(params.SessionID) == "" {
		return ReplayResult{}, errors.New("acp: ReplayLoad requires sessionId")
	}
	if params.MCPServers == nil {
		params.MCPServers = []MCPServerConfig{}
	}
	cap := &replayCapture{maxCount: maxUpdates, maxBytes: maxReplayCaptureBytes}
	c.routesMu.Lock()
	if c.sessions[params.SessionID] != nil || c.replays[params.SessionID] != nil {
		c.routesMu.Unlock()
		return ReplayResult{}, ErrSessionBusy
	}
	c.replays[params.SessionID] = cap
	c.routesMu.Unlock()

	loadSucceeded := false
	defer func() {
		c.routesMu.Lock()
		owned := c.replays[params.SessionID] == cap
		if owned {
			delete(c.replays, params.SessionID)
		}
		liveOwner := c.sessions[params.SessionID] != nil
		c.routesMu.Unlock()
		if owned && loadSucceeded && !liveOwner {
			closeCtx, cancel := context.WithTimeout(context.Background(), defaultCloseTimeout)
			_, _ = c.sendRequest(closeCtx, "session/close", SessionCloseParams{SessionID: params.SessionID})
			cancel()
		}
	}()

	raw, err := c.sendRequest(ctx, "session/load", params)
	if err != nil {
		return ReplayResult{}, err
	}
	loadSucceeded = true
	var openRes SessionOpenResult
	_ = json.Unmarshal(raw, &openRes)
	if openRes.SessionID == "" {
		openRes.SessionID = params.SessionID
	}

	cap.mu.Lock()
	updates := append([]SessionUpdate(nil), cap.updates...)
	truncated := cap.truncated
	cap.mu.Unlock()

	return ReplayResult{
		SessionID: openRes.SessionID,
		Updates:   updates,
		Truncated: truncated,
		Open:      openRes,
	}, nil
}

// NewRawSession opens a lightweight ACP session (for zero-token catalog probes)
// and returns its initial SessionOpenResult plus a helper to capture usage_update
// context sizes as config options change.
func (c *Conn) NewRawSession(ctx context.Context, params SessionNewParams) (SessionOpenResult, error) {
	if params.MCPServers == nil {
		params.MCPServers = []MCPServerConfig{}
	}
	raw, err := c.sendRequest(ctx, "session/new", params)
	if err != nil {
		return SessionOpenResult{}, err
	}
	var res SessionOpenResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return SessionOpenResult{}, fmt.Errorf("acp: decode session/new: %w", err)
	}
	return res, nil
}

// SetConfigOption invokes session/set_config_option directly on this Conn.
func (c *Conn) SetConfigOption(ctx context.Context, sessionID, configID, value string) ([]ConfigOption, error) {
	raw, err := c.sendRequest(ctx, "session/set_config_option", SessionSetConfigOptionParams{
		SessionID: sessionID,
		ConfigID:  configID,
		Value:     value,
	})
	if err != nil {
		return nil, err
	}
	var res SessionSetConfigOptionResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("acp: decode session/set_config_option: %w", err)
	}
	return res.ConfigOptions, nil
}

// CloseRawSession closes a session created with NewRawSession.
func (c *Conn) CloseRawSession(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}
	_, err := c.sendRequest(ctx, "session/close", SessionCloseParams{SessionID: sessionID})
	if err != nil && (IsMethodNotFound(err) || IsSessionNotFound(err)) {
		return nil
	}
	return err
}

// HasActiveSession reports whether sessionID is currently bound to a live
// Session on this connection.
func (c *Conn) HasActiveSession(sessionID string) bool {
	if strings.TrimSpace(sessionID) == "" {
		return false
	}
	c.routesMu.Lock()
	_, ok := c.sessions[sessionID]
	c.routesMu.Unlock()
	return ok
}

// StartUpdateCapture installs a temporary update observer for sessionID with
// WaitFor/Stop controls (used by zero-token catalog probes to observe
// usage_update notifications emitted asynchronously after set_config_option).
func (c *Conn) StartUpdateCapture(sessionID string, maxUpdates int) *UpdateCapture {
	cap := &replayCapture{
		maxCount: maxUpdates,
		notify:   make(chan struct{}, 1),
	}
	c.routesMu.Lock()
	c.replays[sessionID] = cap
	c.routesMu.Unlock()
	return &UpdateCapture{conn: c, sessionID: sessionID, cap: cap}
}

// CaptureSessionUpdates installs a temporary update observer for sessionID
// (used by zero-token catalog probes to observe usage_update.size). Returns an
// unsubscribe function that returns all captured updates.
func (c *Conn) CaptureSessionUpdates(sessionID string, maxUpdates int) func() []SessionUpdate {
	return c.StartUpdateCapture(sessionID, maxUpdates).Stop
}

func (c *Conn) sendRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	wait, err := c.startRequestWithTimeout(method, params, c.requestTimeout)
	if err != nil {
		return nil, err
	}
	return wait(ctx)
}

// sendSessionNewRequest sends `session/new` and registers sess on Conn's
// reader goroutine synchronously as soon as the response carries sessionId,
// preventing immediate post-open notifications (such as
// available_commands_update) from being dropped before open() returns.
func (c *Conn) sendSessionNewRequest(ctx context.Context, params SessionNewParams, sess *Session) (json.RawMessage, error) {
	wait, err := c.startRequestWithSessionOpen("session/new", params, c.requestTimeout, sess)
	if err != nil {
		return nil, err
	}
	return wait(ctx)
}

// startRequestWithTimeout writes a JSON-RPC request and returns a wait function.
// A timeout <= 0 waits indefinitely until ctx cancels, the response arrives, or
// the connection closes (used for long-running session/prompt RPCs).
func (c *Conn) startRequestWithTimeout(method string, params any, timeout time.Duration) (func(context.Context) (json.RawMessage, error), error) {
	return c.startRequestWithSessionOpen(method, params, timeout, nil)
}

func (c *Conn) startRequestWithSessionOpen(method string, params any, timeout time.Duration, openSess *Session) (func(context.Context) (json.RawMessage, error), error) {
	id := c.nextID.Add(1)
	ch := make(chan json.RawMessage, 1)

	c.pendingMu.Lock()
	if c.pending == nil {
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("acp: %s: connection closed", method)
	}
	c.pending[id] = ch
	if openSess != nil {
		c.pendingSessionOpen[id] = openSess
	}
	c.pendingMu.Unlock()

	abandon := func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		delete(c.pendingSessionOpen, id)
		c.pendingMu.Unlock()
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
		return nil, fmt.Errorf("acp: marshal %s: %w", method, err)
	}
	if err := c.proc.WriteLine(data); err != nil {
		abandon()
		return nil, fmt.Errorf("acp: write %s: %w", method, err)
	}

	return func(ctx context.Context) (json.RawMessage, error) {
		var timerC <-chan time.Time
		if timeout > 0 {
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			abandon()
			return nil, ctx.Err()
		case <-timerC:
			abandon()
			return nil, fmt.Errorf("acp: %s: timed out after %s", method, timeout)
		case resp, ok := <-ch:
			c.pendingMu.Lock()
			delete(c.pending, id)
			c.pendingMu.Unlock()
			if !ok {
				return nil, fmt.Errorf("acp: %s: connection closed before response", method)
			}
			var rpcResp struct {
				Error *struct {
					Code    int             `json:"code"`
					Message string          `json:"message"`
					Data    json.RawMessage `json:"data,omitempty"`
				} `json:"error,omitempty"`
				Result json.RawMessage `json:"result,omitempty"`
			}
			if err := json.Unmarshal(resp, &rpcResp); err == nil {
				if rpcResp.Error != nil {
					return nil, &RPCError{
						Method:  method,
						Code:    rpcResp.Error.Code,
						Message: rpcResp.Error.Message,
						Data:    rpcResp.Error.Data,
					}
				}
				if len(rpcResp.Result) > 0 {
					return rpcResp.Result, nil
				}
			}
			return resp, nil
		}
	}, nil
}

func (c *Conn) writeNotification(method string, params any) error {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("acp: marshal notification %s: %w", method, err)
	}
	return c.proc.WriteLine(data)
}

func (c *Conn) writeResponse(id json.RawMessage, result any) error {
	msg := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("acp: marshal response: %w", err)
	}
	return c.proc.WriteLine(data)
}

func (c *Conn) writeErrorResponse(id json.RawMessage, code int, message string) error {
	msg := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{
		JSONRPC: "2.0",
		ID:      id,
	}
	msg.Error.Code = code
	msg.Error.Message = message
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("acp: marshal error response: %w", err)
	}
	return c.proc.WriteLine(data)
}

func (c *Conn) registerSession(sessionID string, s *Session) {
	c.routesMu.Lock()
	c.sessions[sessionID] = s
	c.routesMu.Unlock()
}

func (c *Conn) unregisterSession(sessionID string, s *Session) {
	c.routesMu.Lock()
	if c.sessions[sessionID] == s {
		delete(c.sessions, sessionID)
	}
	c.routesMu.Unlock()
}

func (c *Conn) readLoop() {
	defer func() {
		c.pendingMu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.pending = nil
		c.pendingMu.Unlock()

		c.routesMu.Lock()
		sessions := make([]*Session, 0, len(c.sessions))
		for _, s := range c.sessions {
			sessions = append(sessions, s)
		}
		c.sessions = make(map[string]*Session)
		c.replays = make(map[string]*replayCapture)
		c.routesMu.Unlock()

		connClosing := c.closing.Load()
		var exitMeta json.RawMessage
		if !connClosing && c.proc != nil {
			exitErr := provider.WaitProcessExitErr(c.proc)
			exitMeta = provider.MarshalProcessExitMeta(exitErr, c.proc.StderrTail())
		}
		for _, s := range sessions {
			s.onConnClosed(exitMeta, connClosing)
		}
		close(c.done)
		// Release pool refs AFTER c.done closes so ConnPool.release never
		// self-deadlocks waiting on readLoop during an unexpected crash.
		for _, s := range sessions {
			s.releasePoolRef()
		}
	}()

	for {
		line, err := c.proc.ReadLine()
		if err != nil {
			if err != io.EOF && !c.closing.Load() {
				meta, _ := json.Marshal(map[string]any{"fatal": true})
				c.routesMu.Lock()
				sessions := make([]*Session, 0, len(c.sessions))
				for _, s := range c.sessions {
					sessions = append(sessions, s)
				}
				c.routesMu.Unlock()
				for _, s := range sessions {
					s.emitEvent(provider.ProviderEvent{
						Kind:      provider.EventError,
						ThreadID:  s.threadID,
						Content:   fmt.Sprintf("acp: read error: %v", err),
						Meta:      meta,
						Failure:   &provider.FailureMeta{Class: provider.FailureFatal, Boundary: provider.FailureBoundaryEvent},
						Timestamp: time.Now(),
					})
				}
			}
			return
		}
		c.dispatchLine(line)
	}
}

func (c *Conn) dispatchLine(line []byte) {
	if len(line) == 0 {
		return
	}
	var env struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}
	hasID := len(env.ID) > 0 && string(env.ID) != "null"

	// Response to a client request.
	if hasID && env.Method == "" {
		var numericID int64
		if err := json.Unmarshal(env.ID, &numericID); err != nil {
			return
		}
		c.pendingMu.Lock()
		ch, ok := c.pending[numericID]
		openSess := c.pendingSessionOpen[numericID]
		delete(c.pendingSessionOpen, numericID)
		c.pendingMu.Unlock()
		if openSess != nil {
			var openResp struct {
				Result *struct {
					SessionID string `json:"sessionId"`
				} `json:"result"`
			}
			if json.Unmarshal(line, &openResp) == nil && openResp.Result != nil && openResp.Result.SessionID != "" {
				openSess.mu.Lock()
				if openSess.sessionID == "" {
					openSess.sessionID = openResp.Result.SessionID
				}
				openSess.mu.Unlock()
				c.registerSession(openResp.Result.SessionID, openSess)
			}
		}
		if ok {
			select {
			case ch <- append(json.RawMessage(nil), line...):
			default:
			}
		}
		return
	}

	// Server-initiated request (requires a response).
	if hasID && env.Method != "" {
		c.handleServerRequest(env.ID, env.Method, env.Params)
		return
	}

	// Server notification.
	if env.Method == "session/update" {
		var params SessionUpdateParams
		if err := json.Unmarshal(env.Params, &params); err != nil || params.SessionID == "" {
			return
		}
		c.routesMu.Lock()
		replay := c.replays[params.SessionID]
		sess := c.sessions[params.SessionID]
		c.routesMu.Unlock()
		if replay != nil {
			replay.add(params.Update)
			return
		}
		if sess != nil {
			sess.handleUpdate(params.Update)
		}
	}
}

func (c *Conn) handleServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "session/request_permission":
		var req RequestPermissionParams
		if err := json.Unmarshal(params, &req); err != nil {
			_ = c.writeErrorResponse(id, ErrCodeInvalidParams, fmt.Sprintf("invalid permission params: %v", err))
			return
		}
		c.routesMu.Lock()
		sess := c.sessions[req.SessionID]
		c.routesMu.Unlock()
		if sess == nil {
			_ = c.writeResponse(id, RequestPermissionResult{
				Outcome: PermissionOutcome{Outcome: "cancelled"},
			})
			return
		}
		sess.handlePermissionRequest(id, req)

	case "fs/read_text_file":
		if c.fsHandler == nil {
			_ = c.writeErrorResponse(id, ErrCodeMethodNotFound, "fs/read_text_file not enabled")
			return
		}
		go c.dispatchAsyncHandler(id, func(ctx context.Context) (any, error) {
			return c.fsHandler.ReadTextFile(ctx, params)
		})

	case "fs/write_text_file":
		if c.fsHandler == nil {
			_ = c.writeErrorResponse(id, ErrCodeMethodNotFound, "fs/write_text_file not enabled")
			return
		}
		go c.dispatchAsyncHandler(id, func(ctx context.Context) (any, error) {
			return c.fsHandler.WriteTextFile(ctx, params)
		})

	default:
		if strings.HasPrefix(method, "terminal/") && c.terminalHandler != nil {
			go c.dispatchAsyncHandler(id, func(ctx context.Context) (any, error) {
				return c.terminalHandler.Handle(ctx, method, params)
			})
			return
		}
		if err := c.writeErrorResponse(id, ErrCodeMethodNotFound, fmt.Sprintf("unsupported client method: %s", method)); err != nil {
			log.Printf("acp: write error response for %s: %v", method, err)
		}
	}
}

func (c *Conn) dispatchAsyncHandler(id json.RawMessage, fn func(context.Context) (any, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), c.requestTimeout)
	defer cancel()
	res, err := fn(ctx)
	if err != nil {
		_ = c.writeErrorResponse(id, ErrCodeInternalError, err.Error())
		return
	}
	_ = c.writeResponse(id, res)
}

func normalizeCleanPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	cleaned := filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(resolved)
	}
	return cleaned
}

func pathsMatch(wantClean, candidate string) bool {
	if wantClean == "" {
		return true
	}
	candClean := normalizeCleanPath(candidate)
	if wantClean == candClean {
		return true
	}
	return strings.TrimPrefix(wantClean, "/private") == strings.TrimPrefix(candClean, "/private")
}
