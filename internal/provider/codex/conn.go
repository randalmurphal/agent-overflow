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
	Env         provider.EnvInput
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

	// routesMu guards the routing tables and the child holds below. Lock
	// order: a Session's mu, then routesMu. Nothing takes a Session's mu while
	// holding routesMu; the only session call made under it is
	// enqueueEarlyFrames, which takes earlyFramesMu.
	routesMu      sync.Mutex
	allSessions   map[*Session]struct{}
	byThread      map[string]*Session
	bySessionID   map[string]*Session
	starting      *Session
	loaded        map[string]struct{}
	closedWaiters map[string][]chan struct{}
	// holds keeps the frames of each thread no session routes yet, until a
	// spawn claims the thread or its deadline expires; nil once closed.
	holds map[string]*childHold
	// heldFrames and heldBytes total every hold's frames against the
	// connection-wide budget.
	heldFrames int
	heldBytes  int
	// refusedForBudget counts frames refused because the connection-wide
	// budget was full, for threads with no hold to count them on. The next
	// claim or expiry logs and resets it.
	refusedForBudget int
	// startHoldTimer arms a hold's deadline. Set when the connection is
	// built; a test replaces it to fire deadlines itself.
	startHoldTimer           func(time.Duration, func()) holdTimer
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
		Env:              cfg.Env.Clear(provider.CodexHomeEnv),
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
		proc:          proc,
		cancel:        cancel,
		binary:        binary,
		done:          make(chan struct{}),
		pending:       make(map[int64]chan json.RawMessage),
		allSessions:   make(map[*Session]struct{}),
		byThread:      make(map[string]*Session),
		bySessionID:   make(map[string]*Session),
		loaded:        make(map[string]struct{}),
		closedWaiters: make(map[string][]chan struct{}),
		holds:         make(map[string]*childHold),
		startHoldTimer: func(d time.Duration, f func()) holdTimer {
			return time.AfterFunc(d, f)
		},
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
	var claimed *childHold
	var refused int
	c.routesMu.Lock()
	if rootID != "" {
		c.byThread[rootID] = s
		c.loaded[rootID] = struct{}{}
		if hold := c.takeHoldLocked(rootID); hold != nil {
			// Enqueue BEFORE releasing routesMu so any subsequent frame routed
			// to s by the reader goroutine sees earlyPending==true and drains
			// these older frames first in strict FIFO order.
			s.enqueueEarlyFrames(hold.frames)
			hasEarlyFrames = len(hold.frames) > 0
			claimed, refused = hold, c.takeRefusedLocked()
		}
	}
	if rootSessID != "" {
		c.bySessionID[rootSessID] = s
	}
	if startingFresh && c.starting == s {
		c.starting = nil
	}
	c.routesMu.Unlock()
	logClaimedHoldLosses(rootID, claimed, refused)
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

// registerChildRoute routes childThreadID to s and moves the thread's held
// frames into s's early queue. The route is the signal deliverOrHold reads:
// the caller registers it only for a child it owns, after the child's spawn
// event was emitted, and holds s.mu so close_agent's unregisterChildRoute
// cannot interleave (lock order: s.mu, then routesMu). It reports whether
// frames were moved; the caller flushes them once it releases s.mu.
func (c *Conn) registerChildRoute(childThreadID string, s *Session) bool {
	childThreadID = strings.TrimSpace(childThreadID)
	if c == nil || childThreadID == "" || s == nil {
		return false
	}
	var claimed *childHold
	var refused int
	c.routesMu.Lock()
	c.byThread[childThreadID] = s
	c.loaded[childThreadID] = struct{}{}
	if hold := c.takeHoldLocked(childThreadID); hold != nil {
		// Enqueue BEFORE releasing routesMu so any subsequent frame routed to s
		// on the reader goroutine drains these held frames first in FIFO order.
		s.enqueueEarlyFrames(hold.frames)
		claimed = hold
	}
	// Every claim reports the budget refusals since the last report, held
	// frames or not: a refused frame armed no hold of its own.
	refused = c.takeRefusedLocked()
	c.routesMu.Unlock()
	logClaimedHoldLosses(childThreadID, claimed, refused)
	return claimed != nil && len(claimed.frames) > 0
}

// unregisterChildRoute removes s's route for a child whose ownership ended,
// so the child's later frames are held and expire rather than reach s. The
// caller holds s.mu, as for registerChildRoute.
func (c *Conn) unregisterChildRoute(childThreadID string, s *Session) {
	if c == nil || childThreadID == "" || s == nil {
		return
	}
	c.routesMu.Lock()
	if c.byThread[childThreadID] == s {
		delete(c.byThread, childThreadID)
	}
	c.routesMu.Unlock()
}

// childHold is one thread's held frames and deadline. Its identity is what
// an expiry checks: a hold taken by a claim, or replaced by a newer hold for
// the same thread, is no longer the map entry, and its expiry does nothing.
type childHold struct {
	frames []deferredChildWireEvent
	bytes  int
	timer  holdTimer
	// droppedNotifications and rejectedRequests count frames refused
	// because this thread's queue was full.
	droppedNotifications int
	rejectedRequests     int
}

// holdTimer is the part of *time.Timer a hold uses.
type holdTimer interface{ Stop() bool }

// takeHoldLocked removes providerThreadID's hold, stops its deadline and
// releases its frames from the budget. Caller holds routesMu.
func (c *Conn) takeHoldLocked(providerThreadID string) *childHold {
	hold := c.holds[providerThreadID]
	if hold == nil {
		return nil
	}
	delete(c.holds, providerThreadID)
	hold.timer.Stop()
	c.heldFrames -= len(hold.frames)
	c.heldBytes -= hold.bytes
	return hold
}

// takeRefusedLocked returns and resets the connection-wide budget refusal
// count. Caller holds routesMu.
func (c *Conn) takeRefusedLocked() int {
	refused := c.refusedForBudget
	c.refusedForBudget = 0
	return refused
}

// logClaimedHoldLosses logs, once, the frames a claimed hold (nil when the
// claim took none) lost to its full queue and the connection-wide budget
// refusals since the last claim, expiry or teardown. Nothing reaches a
// thread: a refused frame names no root.
func logClaimedHoldLosses(providerThreadID string, hold *childHold, refused int) {
	dropped, rejected := 0, 0
	if hold != nil {
		dropped, rejected = hold.droppedNotifications, hold.rejectedRequests
	}
	if dropped+rejected+refused == 0 {
		return
	}
	log.Printf(
		"codex: a spawn claimed provider thread %s after its full queue dropped %d notifications and rejected %d requests; %d frames refused connection-wide for a full routing budget",
		providerThreadID, dropped, rejected, refused,
	)
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
		for _, hold := range c.holds {
			hold.timer.Stop()
		}
		c.holds = nil
		refused := c.takeRefusedLocked()
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
		if refused > 0 {
			log.Printf("codex: the connection closed with %d frames refused connection-wide for a full routing budget since the last claim or expiry", refused)
		}

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
		target.dispatchNotificationWithThreadID(method, params, providerThreadID)
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

// routeOrQuarantine performs route lookup and the hold under ONE routesMu
// acquisition so registerChildRoute or open() can never register and drain an
// empty hold between lookup and enqueue.
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
	rejection := c.holdLocked(providerThreadID, ev)
	c.routesMu.Unlock()
	if rejection != "" {
		c.rejectQuarantinedRequest(requestID, rejection)
	}
	return nil
}

// deliverOrHold decides, once and under routesMu, who gets a frame s received
// on a thread that is not its root: s delivers it when the thread is routed to
// s; a frame for a thread routed to another session goes to that session's
// early queue, as a claim's held frames do; any other frame joins the
// thread's hold, which a claim either takes whole or never sees. A frame is
// therefore never held for a thread that has a route. It reports whether s
// delivers the frame. A frame the hold bounds refuse is rejected when it is a
// request and dropped otherwise.
func (c *Conn) deliverOrHold(providerThreadID string, s *Session, ev deferredChildWireEvent, requestID *json.Number) bool {
	c.routesMu.Lock()
	owner := c.byThread[strings.TrimSpace(providerThreadID)]
	if owner == s {
		c.routesMu.Unlock()
		return true
	}
	if owner != nil {
		owner.enqueueEarlyFrames([]deferredChildWireEvent{ev.retained()})
		c.routesMu.Unlock()
		owner.flushEarlyFrames()
		return false
	}
	rejection := c.holdLocked(providerThreadID, ev)
	c.routesMu.Unlock()
	if rejection != "" {
		c.rejectQuarantinedRequest(requestID, rejection)
	}
	return false
}

// holdLocked adds ev to providerThreadID's hold, creating the hold and arming
// its deadline for the thread's first frame. It returns the rejection for a
// frame it could not hold, after counting it: on the thread's hold when the
// thread's own queue is full, connection-wide when the shared budget is.
// Caller holds routesMu.
func (c *Conn) holdLocked(providerThreadID string, ev deferredChildWireEvent) string {
	trimmedID := strings.TrimSpace(providerThreadID)
	evBytes := ev.sizeBytes()
	if trimmedID == "" || len(trimmedID) > maxDeferredChildThreadIDBytes {
		return "subagent ownership was not available"
	}
	if c.holds == nil {
		return "connection closed"
	}
	const full = "subagent ownership was not available before the routing buffer filled"
	hold := c.holds[trimmedID]
	if hold != nil && len(hold.frames) >= maxDeferredChildEventsPerThread {
		if ev.RequestID == "" {
			hold.droppedNotifications++
		} else {
			hold.rejectedRequests++
		}
		return full
	}
	if c.heldFrames >= maxDeferredChildEventsTotal ||
		evBytes > maxDeferredChildEventBytes ||
		c.heldBytes > maxDeferredChildEventBytes-evBytes {
		c.refusedForBudget++
		return full
	}
	if hold == nil {
		hold = &childHold{}
		c.holds[trimmedID] = hold
		hold.timer = c.startHoldTimer(deferredChildOwnershipTimeout, func() {
			c.expireHold(trimmedID, hold)
		})
	}
	hold.frames = append(hold.frames, ev.retained())
	hold.bytes += evBytes
	c.heldFrames++
	c.heldBytes += evBytes
	return ""
}

// expireHold is hold's deadline: it rejects the hold's requests, drops its
// notifications and logs the thread, unless a claim took the hold or a newer
// hold replaced it, in which case it does nothing.
func (c *Conn) expireHold(providerThreadID string, hold *childHold) {
	c.routesMu.Lock()
	if c.holds[providerThreadID] != hold {
		c.routesMu.Unlock()
		return
	}
	c.takeHoldLocked(providerThreadID)
	refused := c.takeRefusedLocked()
	sessions := len(c.allSessions)
	c.routesMu.Unlock()
	rejected := 0
	for _, ev := range hold.frames {
		if ev.RequestID == "" {
			continue
		}
		rejected++
		id := json.Number(ev.RequestID)
		c.rejectQuarantinedRequest(&id, "subagent ownership did not arrive before the routing deadline")
	}
	// A held frame names only its own thread, so the connection cannot tell
	// which root the thread belonged to: the expiry is logged, never shown on
	// a guessed thread.
	log.Printf(
		"codex: no spawn claimed provider thread %s before the routing deadline: rejected %d requests, dropped %d notifications (%d and %d more refused for its full queue); %d frames refused connection-wide for a full routing budget; %d sessions on the connection",
		providerThreadID, rejected, len(hold.frames)-rejected, hold.rejectedRequests, hold.droppedNotifications, refused, sessions,
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
