package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/codexskills"
	"agent-overflow/internal/logging"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
)

const (
	// codexUnsubscribeWaitTimeout bounds how long a shared-connection session
	// waits for thread/closed after sending thread/unsubscribe.
	codexUnsubscribeWaitTimeout = 10 * time.Second
	// codexCloseInterruptTimeout bounds best-effort turn/interrupt and
	// background-terminal cleanup during shared-session Close.
	codexCloseInterruptTimeout = 3 * time.Second
)

// Compile-time guarantee that *Conn satisfies provider.Connection.
var _ provider.Connection = (*Conn)(nil)

// ConnConfig configures a single spawned `codex app-server` process that can
// multiplex many Codex sessions.
type ConnConfig struct {
	Binary      string
	WorkDir     string
	Env         map[string]string
	EventLogger *logging.Logger
	Reaper      procutil.Reaper
	// ThreadID labels process-level event logs; shared connections pass a
	// connection label (e.g. "codex-conn").
	ThreadID string
}

// ConnMCPOAuthCompletedHandler observes connection-level MCP OAuth completions
// with the optional Codex threadId carried on the notification.
type ConnMCPOAuthCompletedHandler func(threadID, serverName string, success bool, errorMessage string)

// connTransport is the line pipe a Conn drives: the app-server's stdio
// (*provider.Process) in production, an in-memory pipe in tests.
type connTransport interface {
	WriteLine(data []byte) error
	ReadLine() ([]byte, error)
	Close() error
	PID() int
	StderrTail() string
	provider.ExitWaiter
}

// Conn owns one live `codex app-server` subprocess, its JSON-RPC framing,
// request correlation, initialize handshake, and multi-session frame routing.
type Conn struct {
	proc   connTransport
	cancel context.CancelFunc
	binary string
	done   chan struct{}

	closing           atomic.Bool
	closeOnce         sync.Once
	closeErr          error
	appServerVersion  atomic.Pointer[string]
	threadQueueNative atomic.Bool
	requestTimeoutFn  func() time.Duration

	nextID    atomic.Int64
	pendingMu sync.Mutex
	pending   map[int64]chan json.RawMessage

	// startMu serializes fresh thread/start requests so frames that race ahead
	// of the thread/start response unambiguously route to the single starting
	// session. thread/resume already names its threadId and does not take this.
	startMu sync.Mutex

	routesMu                 sync.Mutex
	allSessions              map[*Session]struct{}
	byThread                 map[string]*Session
	bySessionID              map[string]*Session
	starting                 *Session
	loaded                   map[string]struct{}
	closedWaiters            map[string][]chan struct{}
	deferredChild            map[string][]deferredChildWireEvent
	deferredChildDeadlines   map[string]*time.Timer
	deferredChildCount       int
	deferredChildBytes       int
	skillsChangedHandler     SkillsChangedHandler
	mcpStartupUpdateHandler  MCPStartupUpdateHandler
	mcpOAuthCompletedHandler ConnMCPOAuthCompletedHandler
}

// Dial spawns one `codex app-server` process, performs the initialize +
// initialized handshake, and returns a Conn ready to Open sessions and run
// connection-scoped RPCs.
func Dial(ctx context.Context, cfg ConnConfig) (*Conn, error) {
	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		binary = "codex"
	}
	logLabel := strings.TrimSpace(cfg.ThreadID)
	if logLabel == "" {
		logLabel = "codex-conn"
	}

	childCtx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(childCtx, provider.Watched(cfg.Reaper), provider.SpawnConfig{
		Binary:           binary,
		Args:             codexSharedAppServerArgs(),
		Dir:              cfg.WorkDir,
		Env:              cfg.Env,
		UnsetEnv:         []string{"CODEX_HOME"},
		EventLogger:      cfg.EventLogger,
		EventLogRedactor: newCodexProviderEventLogRedactor(),
		ThreadID:         logLabel,
		Provider:         string(provider.Codex),
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("codex: spawn: %w", err)
	}

	c := newConnWithProcess(proc, cancel, binary)
	go c.readLoop()

	initResp, err := c.sendRequest(ctx, "initialize",
		codexInitializeParams(provider.CodexClientOrigin, sessionOptOutNotificationMethods()))
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("codex: initialize handshake failed: %w", err)
	}
	c.recordInitialize(initResp)

	if err := c.writeNotification("initialized", nil); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("codex: send initialized notification: %w", err)
	}
	return c, nil
}

// recordInitialize keeps what the initialize response states about the
// app-server. A response that carries no parseable version leaves the version
// empty, which every gate reads as "too old": fail closed, since the cost of
// guessing wrong is a JSON-RPC error on every attempt.
func (c *Conn) recordInitialize(initResp json.RawMessage) {
	version := parseAppServerVersion(readNestedString(initResp, "userAgent"))
	c.appServerVersion.Store(&version)
	c.threadQueueNative.Store(provider.CodexCLIVersionAtLeast(version, threadQueueMinimumCodexVersion))
}

func newConnWithProcess(proc connTransport, cancel context.CancelFunc, binary string) *Conn {
	return &Conn{
		proc:                   proc,
		cancel:                 cancel,
		binary:                 binary,
		done:                   make(chan struct{}),
		pending:                make(map[int64]chan json.RawMessage),
		allSessions:            make(map[*Session]struct{}),
		byThread:               make(map[string]*Session),
		bySessionID:            make(map[string]*Session),
		loaded:                 make(map[string]struct{}),
		closedWaiters:          make(map[string][]chan struct{}),
		deferredChild:          make(map[string][]deferredChildWireEvent),
		deferredChildDeadlines: make(map[string]*time.Timer),
	}
}

// PID returns the OS process-group leader PID of the app-server, or 0 once exited.
func (c *Conn) PID() int {
	if c == nil || c.proc == nil {
		return 0
	}
	return c.proc.PID()
}

// Done closes after the process has exited and every attached session has
// received its teardown events.
func (c *Conn) Done() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.done
}

// Binary returns the Codex binary path this connection was spawned from.
func (c *Conn) Binary() string {
	if c == nil {
		return ""
	}
	return c.binary
}

// AppServerVersion returns the Codex version reported in the initialize handshake.
func (c *Conn) AppServerVersion() string {
	if c == nil {
		return ""
	}
	if v := c.appServerVersion.Load(); v != nil {
		return *v
	}
	return ""
}

// Close ends the underlying app-server process and waits for readLoop and all
// session teardown notifications to finish. Idempotent.
func (c *Conn) Close() error {
	if c == nil {
		return nil
	}
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

// SetSkillsChangedHandler installs the connection-level skills/changed observer
// (invoked once per notification regardless of how many sessions are attached).
func (c *Conn) SetSkillsChangedHandler(h SkillsChangedHandler) {
	c.routesMu.Lock()
	c.skillsChangedHandler = h
	c.routesMu.Unlock()
}

// SetMCPStartupUpdateHandler installs the connection-level MCP startup observer.
func (c *Conn) SetMCPStartupUpdateHandler(h MCPStartupUpdateHandler) {
	c.routesMu.Lock()
	c.mcpStartupUpdateHandler = h
	c.routesMu.Unlock()
}

// SetMCPOAuthCompletedHandler installs the connection-level MCP OAuth observer.
func (c *Conn) SetMCPOAuthCompletedHandler(h ConnMCPOAuthCompletedHandler) {
	c.routesMu.Lock()
	c.mcpOAuthCompletedHandler = h
	c.routesMu.Unlock()
}

// ListSkills runs process-wide skills/list directly on this connection.
func (c *Conn) ListSkills(ctx context.Context, cwds []string, forceReload bool) ([]codexskills.CwdSkills, error) {
	params, _, err := buildSkillsListParams(cwds, forceReload)
	if err != nil {
		return nil, err
	}
	raw, err := c.sendRequest(ctx, skillsListMethod, params)
	if err != nil {
		return nil, fmt.Errorf("codex: %s: %w", skillsListMethod, err)
	}
	return parseSkillsList(raw)
}

// ReadAccountUsage runs process-wide account/usage/read directly on this connection.
func (c *Conn) ReadAccountUsage(ctx context.Context) (AccountUsage, error) {
	raw, err := c.sendRequest(ctx, accountUsageMethod, nil)
	if err != nil {
		return AccountUsage{}, classifyAccountUsageError(err)
	}
	return parseAccountUsage(raw)
}

// AccountInfo reads the account cached by this running app-server.
func (c *Conn) AccountInfo(ctx context.Context) (provider.AccountInfo, error) {
	result, err := c.sendRequest(ctx, "account/read", map[string]any{
		"refreshToken": false,
	})
	if err != nil {
		return provider.AccountInfo{}, fmt.Errorf("codex: account/read: %w", err)
	}
	return decodeAccountInfo(result)
}

// ErrThreadUnloadFailed is returned when resuming a thread on a shared Conn
// whose server could not confirm the prior loaded instance was unloaded (so
// thread/resume would silently ignore changed sandbox/config overrides).
var ErrThreadUnloadFailed = errors.New("codex: failed to unload loaded thread before resume")

// Open attaches a new or resumed conversation Session to this shared Conn.
// Closing the returned Session detaches and unsubscribes the thread without
// closing the Conn.
func (c *Conn) Open(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent)) (*Session, error) {
	return c.open(ctx, threadID, cfg, onEvent, false, nil)
}

// OpenPooled attaches a Session to this shared Conn and calls releaseConn when
// the session closes (or if Open fails).
func (c *Conn) OpenPooled(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent), releaseConn func()) (*Session, error) {
	return c.open(ctx, threadID, cfg, onEvent, false, releaseConn)
}

// OpenOwned attaches a Session that owns this dedicated Conn (closing the
// Session closes the underlying app-server process). Used when callers need to
// run pre-open connection checks such as AccountInfo before thread/resume.
func (c *Conn) OpenOwned(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent)) (*Session, error) {
	return c.open(ctx, threadID, cfg, onEvent, true, nil)
}

func (c *Conn) open(
	ctx context.Context,
	threadID string,
	cfg Config,
	onEvent func(provider.ProviderEvent),
	ownsConn bool,
	releaseConn func(),
) (*Session, error) {
	sessionStart, err := provider.NewSessionStartDelivery(cfg.SessionStartHook, cfg.SessionStartForget)
	if err != nil {
		if releaseConn != nil {
			releaseConn()
		}
		return nil, fmt.Errorf("codex: %w", err)
	}

	// If resuming a thread that this connection still considers loaded,
	// unsubscribe first so every config/sandbox/reviewer override applies
	// cleanly (a resume of an already-loaded thread ignores overrides).
	if cfg.ResumeThreadID != "" && c.isThreadLoaded(cfg.ResumeThreadID) {
		if err := c.unsubscribeAndWait(ctx, cfg.ResumeThreadID); err != nil {
			if ownsConn {
				_ = c.Close()
			} else if releaseConn != nil {
				releaseConn()
			}
			return nil, fmt.Errorf("%w: %v", ErrThreadUnloadFailed, err)
		}
	}

	s := c.newSession(ctx, threadID, cfg, onEvent, ownsConn, releaseConn, sessionStart)

	// Register session before sending thread/start or thread/resume so early
	// notifications route to it immediately.
	startingFresh := c.registerSession(s, cfg.ResumeThreadID)

	if cfg.ResumeThreadID != "" && cfg.BeforeResume != nil {
		cfg.BeforeResume(s)
	}

	if err := s.startOrResumeThread(ctx, cfg); err != nil {
		if startingFresh {
			c.routesMu.Lock()
			if c.starting == s {
				c.starting = nil
			}
			c.routesMu.Unlock()
			c.startMu.Unlock()
		}
		c.detachSession(s, s.rootThreadID(), s.rootSessionID(), nil)
		if ownsConn {
			_ = c.Close()
		} else if releaseConn != nil {
			s.releaseConnOnce.Do(releaseConn)
		}
		return nil, err
	}

	c.bindSessionRoot(s, startingFresh)

	meta, _ := json.Marshal(provider.SessionInfo{
		SessionID: s.rootThreadID(),
		Model:     cfg.Model,
		CWD:       cfg.WorkDir,
	})
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
	return s, nil
}

// newSession builds the Session that open attaches: every map the session
// writes is allocated and the connection's version facts are copied in.
func (c *Conn) newSession(
	ctx context.Context,
	threadID string,
	cfg Config,
	onEvent func(provider.ProviderEvent),
	ownsConn bool,
	releaseConn func(),
	sessionStart *provider.SessionStartDelivery,
) *Session {
	s := &Session{
		conn:        c,
		ownsConn:    ownsConn,
		releaseConn: releaseConn,
		ctx:         ctx,
		threadID:    threadID,
		workDir:     cfg.WorkDir,
		binary:      c.binary,
		usageAcct:   newUsageAccounting(cfg.ResumeThreadID != ""),
		turnConfig: sessionTurnConfig{
			model:               cfg.Model,
			reasoningEffort:     cfg.ReasoningEffort,
			serviceTier:         cfg.ServiceTier,
			assertedServiceTier: cfg.ServiceTier,
			approvalPolicy:      cfg.ApprovalPolicy,
			sandbox:             cfg.Sandbox,
			approvalsReviewer:   threadApprovalsReviewer(cfg),
		},
		writableRoots:   slices.Clone(cfg.WritableRoots),
		nativeMemoryOff: cfg.NativeMemoryOff,
		onEvent:         onEvent,
		sessionStart:    sessionStart,
		readDone:        make(chan struct{}),
		collab: sessionCollabState{
			childParentByThread:       make(map[string]string),
			childParentByAgentPath:    make(map[string]string),
			childThreadByAgentPath:    make(map[string]string),
			childPathOwnerLive:        make(map[string]bool),
			agentPathByThread:         make(map[string]string),
			agentMetaByThread:         make(map[string]collabReceiverMeta),
			subagentNotificationDedup: make(map[subagentNotificationDedupKey]struct{}),
			childRuntimeByThread:      make(map[string]childRuntimeState),
		},
		collabMetadataReads: make(chan struct{}, 4),
		rawCalls: sessionRawToolCallState{
			byID:                  make(map[string]rawToolCall),
			waitReceiverIDsByCall: make(map[string][]string),
		},
		collabHistory: sessionCollabHistoryState{
			generation: 1,
			visited:    make(map[string]uint64),
		},
		planBuffersByItemID: make(map[string]*planBuffer),
		planBuffersByTurnID: make(map[string]*planBuffer),
		ownsQueuedClient:    cfg.OwnsQueuedClientID,
	}
	if v := c.AppServerVersion(); v != "" {
		s.appServerVersion.Store(&v)
	}
	s.threadQueueNative.Store(c.threadQueueNative.Load())

	if delivery := cfg.DeveloperInstructionsDelivery; delivery != nil {
		s.developerInstructionsRecord = delivery.Record
		if cfg.ResumeThreadID != "" {
			s.developerInstructionsReceived = delivery.Received
		}
	}
	if cfg.ResumeThreadID != "" {
		s.setRootThreadID(cfg.ResumeThreadID)
	}

	return s
}

// registerSession makes s a route on this connection before its thread/start
// or thread/resume is written, so frames that arrive first reach it. A fresh
// start (no resumeThreadID) holds startMu until bindSessionRoot, which makes
// s the one starting session. It reports whether s is starting fresh.
func (c *Conn) registerSession(s *Session, resumeThreadID string) bool {
	startingFresh := resumeThreadID == ""
	if startingFresh {
		c.startMu.Lock()
	}
	c.routesMu.Lock()
	c.allSessions[s] = struct{}{}
	if startingFresh {
		c.starting = s
	} else {
		c.byThread[resumeThreadID] = s
	}
	c.routesMu.Unlock()

	return startingFresh
}

// bindSessionRoot routes s by the root thread and session ids its start or
// resume established, ends a fresh start's hold on startMu, and delivers the
// root's frames that arrived before the route existed.
func (c *Conn) bindSessionRoot(s *Session, startingFresh bool) {
	rootID := s.rootThreadID()
	rootSessID := s.rootSessionID()
	var hasEarlyFrames bool
	c.routesMu.Lock()
	if rootID != "" {
		c.byThread[rootID] = s
		c.loaded[rootID] = struct{}{}
		if earlyRootFrames := c.takeDeferredChildLocked(rootID); len(earlyRootFrames) > 0 {
			// Enqueue BEFORE releasing routesMu so any subsequent frame routed
			// to s by the reader goroutine sees earlyPending==true and drains
			// these older frames first in strict FIFO order.
			s.enqueueEarlyFrames(earlyRootFrames)
			hasEarlyFrames = true
		}
	}
	if rootSessID != "" {
		c.bySessionID[rootSessID] = s
	}
	if startingFresh && c.starting == s {
		c.starting = nil
	}
	c.routesMu.Unlock()
	if startingFresh {
		c.startMu.Unlock()
	}
	if hasEarlyFrames {
		s.flushEarlyFrames()
	}
}

func (c *Conn) requestTimeout() time.Duration {
	if c != nil && c.requestTimeoutFn != nil {
		return c.requestTimeoutFn()
	}
	return defaultRequestTimeout
}

func (c *Conn) sendRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	wait, err := c.startRequest(method, params)
	if err != nil {
		return nil, err
	}
	return wait(ctx)
}

func (c *Conn) startRequest(method string, params any) (func(context.Context) (json.RawMessage, error), error) {
	return c.startRequestWithTimeout(method, params, 0)
}

func (c *Conn) startRequestWithTimeout(method string, params any, timeout time.Duration) (func(context.Context) (json.RawMessage, error), error) {
	if timeout <= 0 {
		timeout = c.requestTimeout()
	}
	id := c.nextID.Add(1)
	ch := make(chan json.RawMessage, 1)

	c.pendingMu.Lock()
	if c.pending == nil {
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("codex: %s: session stopped before request completed", method)
	}
	c.pending[id] = ch
	c.pendingMu.Unlock()

	abandon := func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
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
		return nil, fmt.Errorf("codex: marshal request: %w", err)
	}
	if err := c.proc.WriteLine(data); err != nil {
		abandon()
		return nil, err
	}

	return func(ctx context.Context) (json.RawMessage, error) {
		select {
		case <-ctx.Done():
			abandon()
			return nil, ctx.Err()
		case resp, ok := <-ch:
			c.pendingMu.Lock()
			delete(c.pending, id)
			c.pendingMu.Unlock()
			if !ok {
				return nil, fmt.Errorf("codex: %s: session stopped before request completed", method)
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
		case <-time.After(timeout):
			abandon()
			return nil, &RequestTimeoutError{Method: method}
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
		return fmt.Errorf("codex: marshal notification: %w", err)
	}
	return c.proc.WriteLine(data)
}

func (c *Conn) writeResponse(id int64, result any) error {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("codex: marshal response: %w", err)
	}
	return c.proc.WriteLine(data)
}

func (c *Conn) writeErrorResponseWithData(id int64, code int, message string, data any) error {
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
	return c.proc.WriteLine(encoded)
}

func (c *Conn) isThreadLoaded(providerThreadID string) bool {
	if providerThreadID == "" {
		return false
	}
	c.routesMu.Lock()
	_, ok := c.loaded[providerThreadID]
	c.routesMu.Unlock()
	return ok
}

func (c *Conn) markThreadClosed(providerThreadID string) {
	if providerThreadID == "" {
		return
	}
	c.routesMu.Lock()
	delete(c.loaded, providerThreadID)
	waiters := c.closedWaiters[providerThreadID]
	delete(c.closedWaiters, providerThreadID)
	c.routesMu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
}

// unsubscribeAndWait sends thread/unsubscribe for providerThreadID and waits
// up to codexUnsubscribeWaitTimeout for thread/closed.
func (c *Conn) unsubscribeAndWait(ctx context.Context, providerThreadID string) error {
	providerThreadID = strings.TrimSpace(providerThreadID)
	if providerThreadID == "" {
		return nil
	}
	waitCh := make(chan struct{})
	c.routesMu.Lock()
	c.closedWaiters[providerThreadID] = append(c.closedWaiters[providerThreadID], waitCh)
	c.routesMu.Unlock()

	removeWaiter := func() {
		c.routesMu.Lock()
		list := c.closedWaiters[providerThreadID]
		for i, ch := range list {
			if ch == waitCh {
				list = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(list) == 0 {
			delete(c.closedWaiters, providerThreadID)
		} else {
			c.closedWaiters[providerThreadID] = list
		}
		c.routesMu.Unlock()
	}

	reqCtx, cancel := context.WithTimeout(ctx, codexUnsubscribeWaitTimeout)
	defer cancel()

	rawResp, err := c.sendRequest(reqCtx, "thread/unsubscribe", map[string]any{
		"threadId": providerThreadID,
	})
	if err != nil {
		removeWaiter()
		if IsThreadNotFound(err) {
			c.markThreadClosed(providerThreadID)
			return nil
		}
		return fmt.Errorf("codex: thread/unsubscribe %s: %w", providerThreadID, err)
	}
	var unsubResp struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(rawResp, &unsubResp) == nil {
		switch unsubResp.Status {
		case "notLoaded":
			removeWaiter()
			c.markThreadClosed(providerThreadID)
			return nil
		case "notSubscribed":
			// Per codex-wire.md, notSubscribed means this connection had no
			// subscription while the thread may remain loaded in memory with
			// its old config; refuse to mark it closed so open() falls back.
			removeWaiter()
			return fmt.Errorf("codex: thread/unsubscribe %s returned notSubscribed", providerThreadID)
		}
	}

	select {
	case <-waitCh:
		return nil
	case <-c.done:
		removeWaiter()
		return nil
	case <-reqCtx.Done():
		removeWaiter()
		return fmt.Errorf("codex: timed out waiting for thread/closed on %s", providerThreadID)
	}
}

func (c *Conn) registerChildRoute(childThreadID string, s *Session) bool {
	childThreadID = strings.TrimSpace(childThreadID)
	if c == nil || childThreadID == "" || s == nil {
		return false
	}
	var hasHeld bool
	c.routesMu.Lock()
	c.byThread[childThreadID] = s
	c.loaded[childThreadID] = struct{}{}
	if held := c.takeDeferredChildLocked(childThreadID); len(held) > 0 {
		// Enqueue BEFORE releasing routesMu so any subsequent frame routed to s
		// on the reader goroutine drains these quarantined frames first in FIFO order.
		s.enqueueEarlyFrames(held)
		hasHeld = true
	}
	c.routesMu.Unlock()
	return hasHeld
}

func (c *Conn) takeDeferredChildLocked(childThreadID string) []deferredChildWireEvent {
	events := c.deferredChild[childThreadID]
	if len(events) == 0 {
		return nil
	}
	delete(c.deferredChild, childThreadID)
	if timer := c.deferredChildDeadlines[childThreadID]; timer != nil {
		timer.Stop()
		delete(c.deferredChildDeadlines, childThreadID)
	}
	for _, ev := range events {
		c.deferredChildBytes -= ev.sizeBytes()
	}
	c.deferredChildCount -= len(events)
	if c.deferredChildCount < 0 {
		c.deferredChildCount = 0
	}
	if c.deferredChildBytes < 0 {
		c.deferredChildBytes = 0
	}
	return events
}

func (c *Conn) detachSession(s *Session, rootThreadID, rootSessionID string, childIDs []string) {
	if c == nil || s == nil {
		return
	}
	c.routesMu.Lock()
	delete(c.allSessions, s)
	if c.starting == s {
		c.starting = nil
	}
	if rootThreadID != "" && c.byThread[rootThreadID] == s {
		delete(c.byThread, rootThreadID)
	}
	if rootSessionID != "" && c.bySessionID[rootSessionID] == s {
		delete(c.bySessionID, rootSessionID)
	}
	for _, childID := range childIDs {
		if c.byThread[childID] == s {
			delete(c.byThread, childID)
		}
	}
	for tid, owner := range c.byThread {
		if owner == s {
			delete(c.byThread, tid)
		}
	}
	c.routesMu.Unlock()
}

func (c *Conn) snapshotSessions() []*Session {
	c.routesMu.Lock()
	out := make([]*Session, 0, len(c.allSessions))
	for s := range c.allSessions {
		out = append(out, s)
	}
	c.routesMu.Unlock()
	return out
}

func (c *Conn) readLoop() {
	defer func() {
		// Unblock every pending JSON-RPC request.
		c.pendingMu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.pending = nil
		c.pendingMu.Unlock()

		// Release any thread/closed waiters and deferred child timers.
		c.routesMu.Lock()
		for tid, waiters := range c.closedWaiters {
			for _, ch := range waiters {
				close(ch)
			}
			delete(c.closedWaiters, tid)
		}
		for tid, timer := range c.deferredChildDeadlines {
			timer.Stop()
			delete(c.deferredChildDeadlines, tid)
		}
		c.deferredChild = nil
		c.loaded = make(map[string]struct{})
		sessions := make([]*Session, 0, len(c.allSessions))
		for s := range c.allSessions {
			sessions = append(sessions, s)
		}
		c.allSessions = make(map[*Session]struct{})
		c.byThread = make(map[string]*Session)
		c.bySessionID = make(map[string]*Session)
		c.starting = nil
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
		// Release pool references AFTER c.done closes so ConnPool.release
		// calling Conn.Close() on an unexpected crash never self-deadlocks
		// waiting for readLoop to close c.done.
		for _, s := range sessions {
			s.releasePoolRef()
		}
	}()

	for {
		line, err := c.proc.ReadLine()
		if err != nil {
			if err != io.EOF {
				meta, _ := json.Marshal(map[string]any{"fatal": true})
				for _, s := range c.snapshotSessions() {
					s.emitEvent(provider.ProviderEvent{
						Kind:      provider.EventError,
						ThreadID:  s.threadID,
						Content:   fmt.Sprintf("codex: read error: %v", err),
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
		c.pendingMu.Lock()
		ch, ok := c.pending[id]
		c.pendingMu.Unlock()
		if ok {
			select {
			case ch <- line:
			default:
			}
		}
		return
	}

	// Server request: has both id and method.
	if msg.ID != nil && msg.Method != "" {
		c.dispatchServerRequest(msg.Method, msg.ID, msg.Params, line)
		return
	}

	// Notification: has method, no id.
	if msg.Method != "" {
		c.dispatchNotification(msg.Method, msg.Params)
	}
}

func (c *Conn) dispatchServerRequest(method string, id *json.Number, params json.RawMessage, line []byte) {
	providerThreadID := providerThreadIDFromParams(params)
	if providerThreadID == "" {
		// Connection-level server requests without a threadId (such as
		// attestation/generate or account/chatgptAuthTokens/refresh) are
		// never routed to a session.
		if rpcID, err := id.Int64(); err == nil {
			if writeErr := c.writeErrorResponseWithData(rpcID, -32601, fmt.Sprintf("unsupported connection request: %s", method), nil); writeErr != nil {
				log.Printf("codex: reject connection request %s: %v", method, writeErr)
			}
		}
		return
	}

	target := c.routeOrQuarantine(providerThreadID, "", "", false, deferredChildWireEvent{
		Method:    method,
		Params:    params,
		RequestID: id.String(),
		RawLine:   line,
	}, id)
	if target != nil {
		target.dispatchServerRequest(method, id, params, line)
	}
}

func (c *Conn) dispatchNotification(method string, params json.RawMessage) {
	if method == "thread/closed" {
		c.markThreadClosed(providerThreadIDFromParams(params))
		return
	}

	// Connection-wide side channels.
	switch method {
	case skillsChangedMethod:
		c.routesMu.Lock()
		handler := c.skillsChangedHandler
		c.routesMu.Unlock()
		if handler != nil {
			handler()
			return
		}
		// Standalone NewSession fallback: fire the first session's handler once.
		for _, s := range c.snapshotSessions() {
			s.dispatchSkillsChanged(params)
			return
		}
		return

	case "mcpServer/startupStatus/updated":
		tid := providerThreadIDFromParams(params)
		c.routesMu.Lock()
		connHandler := c.mcpStartupUpdateHandler
		c.routesMu.Unlock()
		if tid != "" {
			if target := c.routeTarget(tid, "", "", false); target != nil {
				target.retainMCPStartupUpdate(params)
				if connHandler != nil {
					if update, ok := decodeMCPStartupUpdate(params); ok {
						connHandler(update)
					}
					return
				}
				target.notifyMCPStartupUpdate(params)
				return
			}
		}
		sessions := c.snapshotSessions()
		for _, s := range sessions {
			s.retainMCPStartupUpdate(params)
		}
		if connHandler != nil {
			if update, ok := decodeMCPStartupUpdate(params); ok {
				connHandler(update)
			}
			return
		}
		for _, s := range sessions {
			s.notifyMCPStartupUpdate(params)
			return
		}
		return

	case "mcpServer/oauthLogin/completed":
		tid := providerThreadIDFromParams(params)
		if tid != "" {
			if target := c.routeTarget(tid, "", "", false); target != nil {
				target.dispatchMCPOAuthCompletion(params)
				return
			}
		}
		c.routesMu.Lock()
		connHandler := c.mcpOAuthCompletedHandler
		c.routesMu.Unlock()
		if connHandler != nil {
			if name, success, errMsg, ok := decodeMCPOAuthCompletion(params); ok {
				connHandler(tid, name, success, errMsg)
			}
			return
		}
		// Never fan out an unscoped OAuth failure across unrelated threads on
		// a shared app-server; only forward when a single session owns the conn.
		sessions := c.snapshotSessions()
		if len(sessions) == 1 {
			sessions[0].dispatchMCPOAuthCompletion(params)
		}
		return
	}

	// Broadcast notifications that carry no threadId.
	providerThreadID := providerThreadIDFromParams(params)
	if providerThreadID == "" && isConnectionBroadcastNotification(method) {
		for _, s := range c.snapshotSessions() {
			s.dispatchNotification(method, params)
		}
		return
	}

	var sessionID, parentThreadID string
	if method == "thread/started" {
		sessionID = readNestedString(params, "thread", "sessionId")
		parentThreadID = readNestedString(params, "thread", "parentThreadId")
	}
	allowStartingFallback := providerThreadID == "" || (method == "thread/started" && parentThreadID == "")

	if providerThreadID == "" {
		if target := c.routeTarget("", sessionID, parentThreadID, allowStartingFallback); target != nil {
			target.dispatchNotification(method, params)
			return
		}
		sessions := c.snapshotSessions()
		if len(sessions) == 1 {
			sessions[0].dispatchNotification(method, params)
		}
		return
	}

	if target := c.routeOrQuarantine(providerThreadID, sessionID, parentThreadID, allowStartingFallback, deferredChildWireEvent{
		Method: method,
		Params: params,
	}, nil); target != nil {
		target.dispatchNotification(method, params)
	}
}

func isConnectionBroadcastNotification(method string) bool {
	switch method {
	case "account/rateLimits/updated", "configWarning", "deprecationNotice", "warning", "guardianWarning":
		return true
	default:
		return false
	}
}

func (c *Conn) routeTargetLocked(providerThreadID, sessionID, parentThreadID string, allowStartingFallback bool) *Session {
	if providerThreadID != "" {
		if s := c.byThread[providerThreadID]; s != nil {
			return s
		}
	}
	if sessionID != "" {
		if s := c.bySessionID[sessionID]; s != nil {
			return s
		}
	}
	if parentThreadID != "" {
		if s := c.byThread[parentThreadID]; s != nil {
			return s
		}
	}
	if allowStartingFallback && c.starting != nil {
		return c.starting
	}
	if len(c.allSessions) == 1 && (allowStartingFallback || len(c.byThread) == 0) {
		for s := range c.allSessions {
			return s
		}
	}
	return nil
}

func (c *Conn) routeTarget(providerThreadID, sessionID, parentThreadID string, allowStartingFallback bool) *Session {
	c.routesMu.Lock()
	defer c.routesMu.Unlock()
	return c.routeTargetLocked(providerThreadID, sessionID, parentThreadID, allowStartingFallback)
}

// routeOrQuarantine performs route lookup and quarantine enqueue under ONE
// routesMu acquisition so registerChildRoute or open() can never register and
// drain an empty queue between lookup and enqueue.
func (c *Conn) routeOrQuarantine(
	providerThreadID, sessionID, parentThreadID string,
	allowStartingFallback bool,
	ev deferredChildWireEvent,
	requestID *json.Number,
) *Session {
	c.routesMu.Lock()
	if target := c.routeTargetLocked(providerThreadID, sessionID, parentThreadID, allowStartingFallback); target != nil {
		c.routesMu.Unlock()
		return target
	}
	rejection := c.quarantineLocked(providerThreadID, ev)
	c.routesMu.Unlock()
	if rejection != "" {
		c.rejectQuarantinedRequest(requestID, rejection)
	}
	return nil
}

// holdChildFrame holds a frame a session received for a non-root thread it
// does not own, in the same queue, bounds and deadline as routeOrQuarantine,
// so the session's claim (drainDeferredChildWireEvents) releases it with the
// thread's other held frames. It reports whether the frame is held; a frame
// the bounds refuse is rejected when it is a request and dropped otherwise.
func (c *Conn) holdChildFrame(providerThreadID string, ev deferredChildWireEvent, requestID *json.Number) bool {
	c.routesMu.Lock()
	rejection := c.quarantineLocked(providerThreadID, ev)
	c.routesMu.Unlock()
	if rejection != "" {
		c.rejectQuarantinedRequest(requestID, rejection)
		return false
	}
	return true
}

// quarantineLocked queues ev under providerThreadID and arms the thread's
// deadline. It returns the rejection for a frame it could not hold. Caller
// holds routesMu.
func (c *Conn) quarantineLocked(providerThreadID string, ev deferredChildWireEvent) string {
	trimmedID := strings.TrimSpace(providerThreadID)
	evBytes := ev.sizeBytes()
	if trimmedID == "" || len(trimmedID) > maxDeferredChildThreadIDBytes {
		return "subagent ownership was not available"
	}
	if c.deferredChild == nil {
		return "connection closed"
	}
	queued := c.deferredChild[trimmedID]
	if len(queued) >= maxDeferredChildEventsPerThread ||
		c.deferredChildCount >= maxDeferredChildEventsTotal ||
		evBytes > maxDeferredChildEventBytes ||
		c.deferredChildBytes > maxDeferredChildEventBytes-evBytes {
		return "subagent ownership was not available before the routing buffer filled"
	}
	ev.Params = append(json.RawMessage(nil), ev.Params...)
	ev.RawLine = append(json.RawMessage(nil), ev.RawLine...)
	c.deferredChild[trimmedID] = append(queued, ev)
	c.deferredChildCount++
	c.deferredChildBytes += evBytes
	if c.deferredChildDeadlines[trimmedID] == nil {
		tid := trimmedID
		c.deferredChildDeadlines[tid] = time.AfterFunc(deferredChildOwnershipTimeout, func() {
			c.expireDeferredChild(tid)
		})
	}
	return ""
}

func (c *Conn) expireDeferredChild(providerThreadID string) {
	c.routesMu.Lock()
	events := c.takeDeferredChildLocked(providerThreadID)
	sessions := make([]*Session, 0, len(c.allSessions))
	for s := range c.allSessions {
		sessions = append(sessions, s)
	}
	c.routesMu.Unlock()
	if len(events) == 0 {
		return
	}
	rejected := 0
	for _, ev := range events {
		if ev.RequestID == "" {
			continue
		}
		rejected++
		id := json.Number(ev.RequestID)
		c.rejectQuarantinedRequest(&id, "subagent ownership did not arrive before the routing deadline")
	}
	threadIDs := make([]string, 0, len(sessions))
	for _, s := range sessions {
		threadIDs = append(threadIDs, s.threadID)
		s.forgetUnclaimedChild(providerThreadID)
	}
	slices.Sort(threadIDs)
	// A held frame names only its own thread, so the connection cannot tell
	// which root the thread belonged to: the expiry is logged, never shown on
	// a guessed thread.
	log.Printf(
		"codex: no spawn claimed provider thread %s before the routing deadline: rejected %d requests, dropped %d notifications; sessions on the connection: %v",
		providerThreadID, rejected, len(events)-rejected, threadIDs,
	)
}

func (c *Conn) rejectQuarantinedRequest(id *json.Number, message string) {
	if id == nil {
		return
	}
	rpcID, err := id.Int64()
	if err != nil {
		return
	}
	if err := c.writeErrorResponseWithData(rpcID, -32000, message, nil); err != nil {
		log.Printf("codex: reject quarantined request %d: %v", rpcID, err)
	}
}
