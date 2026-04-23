package claude

import (
	"bytes"
	"context"
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

// controlRequestPrefix is the first bytes of a Claude control_request
// NDJSON line. Gating the ExitPlanMode handler on this prefix saves a
// json.Unmarshal on every assistant/user/stream_event line (which is
// the hot path during streaming). False positives are benign — the
// subsequent Request.Subtype / ToolName check still filters.
var controlRequestPrefix = []byte(`{"type":"control_request"`)

// controlResponsePrefix matches the CLI's reply to our outbound
// control_requests (today only stop_task; more to come). Prefix-gating
// it the same way as controlRequestPrefix keeps streaming deltas off
// the secondary json.Unmarshal path.
var controlResponsePrefix = []byte(`{"type":"control_response"`)

// DefaultStopTaskTimeout bounds how long StopTask waits for the CLI's
// control_response before returning a timeout error. The verified spike
// observed sub-100ms round-trips on Claude CLI 2.1.112; ten seconds is
// a generous ceiling that still fails loudly if the CLI is wedged.
const DefaultStopTaskTimeout = 10 * time.Second

// DefaultIdleTimeout is how long the watchdog waits for ANY stdout line
// after a Send before declaring the provider wedged. Claude streaming
// responses emit partial-message deltas frequently, so two minutes is
// generous — the intent is to catch provider hangs, not impatient users.
const DefaultIdleTimeout = 120 * time.Second

// DefaultApprovalTimeout is how long we wait for the user to answer a
// tool-use approval prompt before auto-denying so the subprocess does
// not wedge forever. Ten minutes is a pragmatic ceiling — most users
// respond in seconds; anything longer is likely an abandoned session.
const DefaultApprovalTimeout = 10 * time.Minute

// Compile-time guarantee that *Session satisfies the provider.Session
// interface the app layer calls into. Changing any of the methods in a
// way that breaks the contract is caught at build time.
var _ provider.Session = (*Session)(nil)

// Session manages a Claude Code CLI subprocess.
type Session struct {
	proc      *provider.Process
	threadID  string
	sessionID string
	model     string
	onEvent   func(provider.ProviderEvent)
	cancel    context.CancelFunc
	closing   atomic.Bool
	readDone  chan struct{}
	// parser holds per-session NDJSON parse state so tool_use / tool_result
	// pairs can share metadata (e.g. the `is_background` flag) across the
	// two messages that carry them.
	parser *Parser
	// idleTimeout is the per-turn watchdog window. Zero means use
	// DefaultIdleTimeout. The watchdog is armed by Send and disarmed by
	// EventTurnComplete or read errors.
	idleTimeout time.Duration
	// inFlight indicates whether a turn is currently mid-flight. The
	// watchdog only fires while inFlight is true — idle-between-turns is
	// not a hang.
	inFlight atomic.Bool
	// watchdogMu guards assignments to watchdogReset/watchdogDone so
	// pulseIdleWatchdog and armIdleWatchdog never race on the channel
	// reference.
	watchdogMu sync.Mutex
	// watchdogReset is a buffered channel the readLoop pulses on every
	// incoming line so the watchdog timer can restart. A nil channel
	// means the watchdog goroutine is not running.
	watchdogReset chan struct{}
	// watchdogDone is closed by the watchdog goroutine when it exits so
	// Close can wait for it deterministically.
	watchdogDone chan struct{}
	// watchdogFired flags that the watchdog decided the session was wedged.
	// Inspected by Close to suppress the noisy close-related error events
	// we would otherwise emit on top of the timeout error.
	watchdogFired atomic.Bool
	// approvalTimeout overrides DefaultApprovalTimeout when non-zero. Kept
	// here so tests can inject a short window without racing on package
	// globals.
	approvalTimeout time.Duration
	// approvalsMu guards pendingApprovals, resolvedApprovals, and
	// approvalsClosed.
	approvalsMu sync.Mutex
	// pendingApprovals maps approval request IDs to the cancel function
	// that stops the pending auto-deny timer.
	pendingApprovals map[string]*pendingApproval
	// resolvedApprovals remembers request IDs that have already been
	// answered so duplicate responses return ErrApprovalAlreadyResolved
	// (Bug B9) instead of writing a second control_response to the CLI.
	resolvedApprovals map[string]struct{}
	// approvalsClosed is set when Close has disarmed all pending timers
	// so late-arriving approvals do not schedule new ones.
	approvalsClosed bool
	// stopTaskTimeout overrides DefaultStopTaskTimeout when non-zero.
	// Tests set this to a short window so a non-responsive fake CLI
	// doesn't stall the suite. Production leaves it zero.
	stopTaskTimeout time.Duration
	// stopTaskMu guards pendingStopTasks and stopTaskSeq.
	stopTaskMu sync.Mutex
	// pendingStopTasks maps the request_id we send on an outbound
	// stop_task control_request to the channel the read loop delivers
	// the matching control_response on. Entry is created under the mu
	// by StopTask before the write, drained by the control_response
	// dispatch in readLoop (or by StopTask itself on timeout /
	// cancellation).
	pendingStopTasks map[string]chan *stopTaskResult
	// stopTaskSeq is a per-session counter so two concurrent StopTask
	// calls never collide on request_id. The session pointer is mixed
	// in (by map lifetime — a second session allocates a fresh map)
	// so request_ids don't need to be globally unique, only unique
	// within a single CLI subprocess.
	stopTaskSeq uint64
}

// stopTaskResult carries the outcome of a stop_task round-trip from the
// read loop back to the awaiting StopTask caller. Exactly one of errMsg
// or ok is set: ok=true on subtype=success, errMsg populated on
// subtype=error. A nil pointer means the session closed before the
// response arrived.
type stopTaskResult struct {
	ok     bool
	errMsg string
}

// pendingApproval tracks a single in-flight tool-use approval so we can
// cancel its auto-deny timer when the user responds (Bug B3) and so we can
// reject duplicate responses for the same request ID (Bug B9).
type pendingApproval struct {
	cancel chan struct{}
}

// Config for creating a Claude session.
type Config struct {
	Binary       string // default: "claude"
	Model        string
	WorkDir      string
	Resume       string // session ID to resume, empty for new
	ForkSession  bool
	SystemPrompt string
	AllowedTools []string
	// PermissionFlags carries the full `--permission-mode <value>` or
	// `--dangerously-skip-permissions` sequence emitted by
	// provider.ClaudePermissionFlags. Nil / empty means "don't pass any
	// permission-related flag".
	PermissionFlags []string
	MaxTurns        int
	// Env carries per-session environment variables appended on top of the
	// caller's process env (e.g. ANTHROPIC_BETAS for the 1M-context beta).
	// The provider.SpawnConfig.Env hook already reads this shape; we just
	// pipe it through.
	Env         map[string]string
	EventLogger *logging.Logger
}

// NewSession spawns a Claude CLI process and starts the stdout reader goroutine.
// The init event arrives after the first Send() call, not on spawn.
func NewSession(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent)) (*Session, error) {
	binary := cfg.Binary
	if binary == "" {
		binary = "claude"
	}

	args := buildArgs(cfg)

	childCtx, cancel := context.WithCancel(ctx)

	proc, err := provider.Spawn(childCtx, provider.SpawnConfig{
		Binary:      binary,
		Args:        args,
		Dir:         cfg.WorkDir,
		Env:         cfg.Env,
		EventLogger: cfg.EventLogger,
		ThreadID:    threadID,
		Provider:    string(provider.Claude),
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude: spawn: %w", err)
	}

	s := &Session{
		proc:     proc,
		threadID: threadID,
		model:    cfg.Model,
		onEvent:  onEvent,
		cancel:   cancel,
		readDone: make(chan struct{}),
		parser:   NewParser(),
	}
	// Seed the parser with the configured model so early assistant usage
	// events can be priced even if the init envelope lands late. The
	// init handler still overrides this when Claude echoes a different
	// model (auto-reroute).
	s.parser.SetModel(cfg.Model)

	go s.readLoop()

	return s, nil
}

// buildArgs constructs CLI flags from Config.
func buildArgs(cfg Config) []string {
	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		// Route tool-use approval through the CanUseTool control protocol on
		// stdin/stdout. Required for parseControlRequest to receive
		// can_use_tool events with permission_suggestions.
		"--permission-prompt-tool", "stdio",
		// Emit finer-grained content_block_delta envelopes (Gap 4: partial
		// messages). These already flow through parseStreamEvent, so no new
		// routing is needed; the flag simply increases stream fidelity.
		"--include-partial-messages",
	}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Resume != "" {
		args = append(args, "--resume", cfg.Resume)
	}
	if cfg.ForkSession {
		args = append(args, "--fork-session")
	}
	if cfg.SystemPrompt != "" {
		args = append(args, "--system-prompt", cfg.SystemPrompt)
	}
	// PermissionFlags is either nil (default CLI prompting), a two-element
	// slice for `--permission-mode <value>`, or a single-element slice for
	// boolean-style flags like `--dangerously-skip-permissions`.
	args = append(args, cfg.PermissionFlags...)
	if cfg.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(cfg.MaxTurns))
	}
	for _, tool := range cfg.AllowedTools {
		args = append(args, "--allowedTools", tool)
	}

	return args
}

// Send sends a user message. The message is written as a JSON object to stdin.
// Send also arms the idle watchdog: if no stdout line arrives within the
// configured idle window, the watchdog closes the session and emits a
// timeout error so the UI is never left waiting on a wedged subprocess.
func (s *Session) Send(ctx context.Context, content string) error {
	msg := map[string]any{
		"type": "user",
		"message": map[string]string{
			"role":    "user",
			"content": content,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("claude: marshal user message: %w", err)
	}
	if err := s.proc.WriteLine(data); err != nil {
		return err
	}
	s.armIdleWatchdog()
	return nil
}

// armIdleWatchdog starts the idle watchdog goroutine if one is not already
// running. Subsequent calls after the turn completes are no-ops until the
// previous watchdog observes EventTurnComplete and exits.
func (s *Session) armIdleWatchdog() {
	if s.inFlight.Swap(true) {
		// Already armed for the current turn.
		return
	}
	timeout := s.idleTimeout
	if timeout <= 0 {
		timeout = DefaultIdleTimeout
	}
	resetCh := make(chan struct{}, 1)
	doneCh := make(chan struct{})
	s.watchdogMu.Lock()
	s.watchdogReset = resetCh
	s.watchdogDone = doneCh
	s.watchdogMu.Unlock()
	go s.runIdleWatchdog(timeout, resetCh, doneCh)
}

// runIdleWatchdog fires when no line has been received within `timeout`.
// On expiry it marks the watchdog as fired, emits EventError, and kills the
// subprocess so the readLoop observes EOF and completes its disconnect
// routine. The goroutine exits cleanly when (a) it observes the inFlight
// flag flip back to false (turn complete or session closed), or (b) it
// fires the timeout itself.
func (s *Session) runIdleWatchdog(timeout time.Duration, resetCh <-chan struct{}, doneCh chan<- struct{}) {
	defer close(doneCh)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-resetCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if !s.inFlight.Load() {
				return
			}
			timer.Reset(timeout)
		case <-timer.C:
			if !s.inFlight.Load() {
				return
			}
			s.watchdogFired.Store(true)
			meta, _ := json.Marshal(map[string]any{"fatal": true})
			s.onEvent(provider.ProviderEvent{
				Kind:      provider.EventError,
				ThreadID:  s.threadID,
				Content:   fmt.Sprintf("claude: provider idle timeout after %s — no output received", timeout),
				Meta:      meta,
				Timestamp: time.Now(),
			})
			// Kill the subprocess so readLoop exits and emits the
			// disconnected terminal state. We use Kill rather than
			// Close to avoid waiting the shutdown grace period on a
			// subprocess that is already non-responsive.
			_ = s.proc.Kill()
			return
		}
	}
}

// pulseIdleWatchdog is called from readLoop on every received line so the
// watchdog timer restarts. Safe when the watchdog is not armed — the nil
// channel simply makes the select a no-op for this pulse.
func (s *Session) pulseIdleWatchdog() {
	s.watchdogMu.Lock()
	reset := s.watchdogReset
	s.watchdogMu.Unlock()
	if reset == nil {
		return
	}
	select {
	case reset <- struct{}{}:
	default:
	}
}

// disarmIdleWatchdog is called when EventTurnComplete is observed so the
// watchdog exits cleanly; flipping inFlight first ensures the watchdog's
// next wakeup sees a stopped turn even if the reset-channel pulse loses
// the race.
func (s *Session) disarmIdleWatchdog() {
	if !s.inFlight.Swap(false) {
		return
	}
	s.pulseIdleWatchdog()
}

// Interrupt sends a control interrupt to the CLI.
func (s *Session) Interrupt(ctx context.Context) error {
	msg := map[string]any{
		"type": "control",
		"control": map[string]string{
			"type": "interrupt",
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("claude: marshal interrupt: %w", err)
	}
	return s.proc.WriteLine(data)
}

// StopTask kills a backgrounded Claude task (Bash with
// run_in_background:true or a Task subagent) by sending a `stop_task`
// control_request and awaiting the matching control_response. The
// `task_id` argument is the id the CLI emitted on `system/task_started`
// — the Claude control protocol accepts the same id for both task
// types.
//
// On success the CLI replies with
// `{"type":"control_response","response":{"subtype":"success", ...}}`
// and fires a follow-up `system/task_updated` with
// `patch.status:"killed"` on the normal event stream (routed through
// triage just like a natural terminal). On error the response carries
// `subtype:"error"` with a human-readable message that StopTask wraps
// into the returned error.
//
// Returns a timeout error after stopTaskTimeout (or ctx.Done) if the
// CLI never answers.
func (s *Session) StopTask(ctx context.Context, taskID string) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("claude: stop_task: empty task_id")
	}

	requestID := s.allocateStopTaskRequestID()
	ch := make(chan *stopTaskResult, 1)
	if !s.registerStopTask(requestID, ch) {
		return fmt.Errorf("claude: stop_task: session closing")
	}

	msg := map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request": map[string]any{
			"subtype": "stop_task",
			"task_id": taskID,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		s.releaseStopTask(requestID)
		return fmt.Errorf("claude: marshal stop_task: %w", err)
	}

	if err := s.proc.WriteLine(data); err != nil {
		s.releaseStopTask(requestID)
		return fmt.Errorf("claude: write stop_task: %w", err)
	}

	timeout := s.stopTaskTimeout
	if timeout <= 0 {
		timeout = DefaultStopTaskTimeout
	}

	select {
	case <-ctx.Done():
		s.releaseStopTask(requestID)
		return fmt.Errorf("claude: stop_task %s: %w", taskID, ctx.Err())
	case <-time.After(timeout):
		s.releaseStopTask(requestID)
		return fmt.Errorf("claude: stop_task %s: timeout after %s", taskID, timeout)
	case res, ok := <-ch:
		if !ok || res == nil {
			return fmt.Errorf("claude: stop_task %s: session closed before response", taskID)
		}
		if res.ok {
			return nil
		}
		if res.errMsg == "" {
			return fmt.Errorf("claude: stop_task %s: provider returned unspecified error", taskID)
		}
		return fmt.Errorf("claude: stop_task %s: %s", taskID, res.errMsg)
	}
}

// allocateStopTaskRequestID generates a request_id unique within the
// session. Format is a short "so-<n>" prefix so logs and wire samples
// make it clear the id originated here (forge's stop-task tooling
// uses the same convention).
func (s *Session) allocateStopTaskRequestID() string {
	s.stopTaskMu.Lock()
	s.stopTaskSeq++
	seq := s.stopTaskSeq
	s.stopTaskMu.Unlock()
	return fmt.Sprintf("so-%d", seq)
}

// registerStopTask stores the pending channel under the request_id.
// Returns false when Close has run (the closing flag flipped and the
// pending map has been drained) so late StopTask callers fail fast
// instead of parking on a channel nobody will deliver to.
//
// The closing check happens UNDER stopTaskMu so the clearPendingStopTasks
// / registerStopTask pair serialises correctly: if Close wins the
// lock first, the registration fails; if a concurrent StopTask wins
// it first, the entry is visible to the subsequent clearPendingStopTasks
// drain. Without this ordering, a late registration could leak a
// pending entry past Close.
func (s *Session) registerStopTask(requestID string, ch chan *stopTaskResult) bool {
	s.stopTaskMu.Lock()
	defer s.stopTaskMu.Unlock()
	if s.closing.Load() {
		return false
	}
	if s.pendingStopTasks == nil {
		s.pendingStopTasks = make(map[string]chan *stopTaskResult)
	}
	s.pendingStopTasks[requestID] = ch
	return true
}

// releaseStopTask removes the pending entry and drains the channel so
// a late read-loop delivery lands in a discarded buffer. Called from
// timeout / cancel / error branches so the map never leaks entries and
// the single-slot channel never blocks a reader that already gave up.
func (s *Session) releaseStopTask(requestID string) {
	s.stopTaskMu.Lock()
	ch, ok := s.pendingStopTasks[requestID]
	if ok {
		delete(s.pendingStopTasks, requestID)
	}
	s.stopTaskMu.Unlock()
	if !ok {
		return
	}
	select {
	case <-ch:
	default:
	}
}

// deliverStopTaskResponse is the read-loop-side half: it matches an
// inbound control_response to a pending StopTask and delivers the
// result. Unknown request_ids are returned as (false) so the caller
// can log once and drop.
func (s *Session) deliverStopTaskResponse(requestID string, res *stopTaskResult) bool {
	s.stopTaskMu.Lock()
	ch, ok := s.pendingStopTasks[requestID]
	if ok {
		delete(s.pendingStopTasks, requestID)
	}
	s.stopTaskMu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- res:
	default:
		// Channel already drained by timeout — nothing to do.
	}
	return true
}

// clearPendingStopTasks closes every outstanding stop_task waiter so
// Close doesn't strand a StopTask caller. Mirrors clearPendingApprovals.
func (s *Session) clearPendingStopTasks() {
	s.stopTaskMu.Lock()
	pending := s.pendingStopTasks
	s.pendingStopTasks = nil
	s.stopTaskMu.Unlock()
	for _, ch := range pending {
		// A nil send signals "session closing" — StopTask returns a
		// clean error rather than hanging forever waiting on a
		// control_response the dead subprocess will never emit.
		select {
		case ch <- nil:
		default:
		}
	}
}

// RespondToApproval sends a tool-use approval decision back to the CLI.
// Accepts both Codex-native values (accept, acceptForSession, decline, cancel)
// and legacy values (allow, allow_session, deny) for backward compatibility.
// When resp.UpdatedInput or resp.UpdatedPermissions are non-empty and the
// decision is an allow, the raw JSON is forwarded to the CLI as the
// Claude-SDK-compatible CanUseTool response fields.
//
// Responding twice for the same RequestID returns ErrApprovalAlreadyResolved
// (Bug B9). The first response also cancels the auto-deny timer started
// for that request by Bug B3's timeout watchdog.
func (s *Session) RespondToApproval(ctx context.Context, resp provider.ApprovalResponse) error {
	if !s.claimApproval(resp.RequestID) {
		return ErrApprovalAlreadyResolved
	}
	data, err := buildApprovalResponse(resp)
	if err != nil {
		return err
	}
	return s.proc.WriteLine(data)
}

// ErrApprovalAlreadyResolved is returned by RespondToApproval when the
// request ID has already been answered (either by an earlier response or
// by the auto-deny timeout) so callers can surface a clear message instead
// of silently shadowing the previous decision.
var ErrApprovalAlreadyResolved = fmt.Errorf("claude: approval already resolved")

// startApprovalTimer registers a pending approval and arms the auto-deny
// timer. Subsequent responses (from the user) or calls to Close cancel
// the timer via claimApproval / clearPendingApprovals.
func (s *Session) startApprovalTimer(requestID string) {
	if requestID == "" {
		return
	}
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
		// Claude should not re-send the same request ID, but if it does
		// we replace the prior timer to avoid leaking it.
		close(existing.cancel)
	}
	s.pendingApprovals[requestID] = &pendingApproval{cancel: cancel}
	// Starting a new timer re-opens the ID in case the provider
	// re-sent the request after a response.
	delete(s.resolvedApprovals, requestID)
	s.approvalsMu.Unlock()

	go s.runApprovalTimer(requestID, timeout, cancel)
}

// runApprovalTimer fires the auto-deny when the user fails to respond in
// time. A cancel signal on `cancel` means the user responded first or the
// session is closing.
func (s *Session) runApprovalTimer(requestID string, timeout time.Duration, cancel <-chan struct{}) {
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
		Content:   fmt.Sprintf("claude: approval timed out for request %s after %s — auto-denied to keep session alive", requestID, timeout),
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
	data, err := buildApprovalResponse(provider.ApprovalResponse{
		RequestID: requestID,
		Decision:  "deny",
	})
	if err != nil {
		log.Printf("claude: build auto-deny for %s: %v", requestID, err)
		return
	}
	if err := s.proc.WriteLine(data); err != nil {
		log.Printf("claude: write auto-deny for %s: %v", requestID, err)
	}
}

// claimApproval returns true when the caller is the first to answer the
// approval for requestID. False means either we already answered (Bug B9
// dedup) or the session is closing. Cancels any pending auto-deny timer
// so the goroutine exits.
//
// The absence of a pending entry is NOT an error — some callers (legacy
// tests, programmatic flows) may respond without the request having gone
// through the normal readLoop path. Only a repeat response for the same
// ID is rejected.
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
// by Close so the goroutines exit instead of racing with a closing
// subprocess. Also drops the resolvedApprovals dedup set — once Close
// has been called, no duplicate response can land at the provider (the
// process is being torn down), so the memory cost of keeping the IDs
// around is pure overhead.
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

// resolvedApprovalsSoftCap bounds the per-session dedup set. Duplicate
// responses for the same requestID can only arrive while a provider is
// still mid-turn; once the session has accumulated this many answered
// approvals the oldest entries are dropped so memory stays flat on
// very long-running sessions. Exceeding the cap is not a correctness
// issue — a duplicate for a flushed entry would write one extra
// control_response, which the provider discards, so the cap is a
// pragmatic ceiling rather than a hard requirement.
const resolvedApprovalsSoftCap = 1000

// SessionID returns the provider's session identifier.
// Only valid after the init event has been received.
func (s *Session) SessionID() string {
	return s.sessionID
}

// Close shuts down the CLI process gracefully.
// Closes stdin first for graceful shutdown, then cancels the context as fallback.
func (s *Session) Close() error {
	s.closing.Store(true)
	s.disarmIdleWatchdog()
	s.clearPendingApprovals()
	s.clearPendingStopTasks()
	err := s.proc.Close()
	s.cancel()
	if s.readDone != nil {
		<-s.readDone
	}
	s.watchdogMu.Lock()
	watchdogDone := s.watchdogDone
	s.watchdogMu.Unlock()
	if watchdogDone != nil {
		<-watchdogDone
	}
	// Release parser-owned state so the dedup sets
	// (completedToolUseIDs, completedTasks, backgroundToolUses, etc.)
	// don't linger after the readLoop exits.
	s.parser.Close()
	return err
}

// readLoop reads stdout NDJSON lines and dispatches them as ProviderEvents.
func (s *Session) readLoop() {
	defer func() {
		if s.readDone != nil {
			defer close(s.readDone)
		}

		// Tear the watchdog down so the goroutine exits; if it had
		// already fired we let the original error stand and skip the
		// generic close-error event.
		s.disarmIdleWatchdog()

		// Release any StopTask callers still parked on a pending
		// control_response. If the subprocess died on its own (io.EOF,
		// crash, watchdog kill) Close won't be the path that drains
		// the map, so the caller would otherwise sit idle until its
		// own timeout fires. Signalling here surfaces "session closed
		// before response" within a handful of milliseconds of the
		// subprocess exit.
		s.clearPendingStopTasks()

		if !s.closing.Load() && !s.watchdogFired.Load() {
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
			if err != io.EOF && !s.watchdogFired.Load() {
				meta, _ := json.Marshal(map[string]any{"fatal": true})
				s.onEvent(provider.ProviderEvent{
					Kind:      provider.EventError,
					ThreadID:  s.threadID,
					Content:   fmt.Sprintf("claude: read error: %v", err),
					Meta:      meta,
					Timestamp: time.Now(),
				})
			}
			return
		}
		// Any output keeps the watchdog happy — even lines we drop below.
		s.pulseIdleWatchdog()

		// Gate the ExitPlanMode pre-parse on a byte-prefix check so
		// every streaming text_delta line doesn't pay a second
		// json.Unmarshal. ParseLine below still handles the line if
		// the gate skips this branch.
		if bytes.HasPrefix(line, controlRequestPrefix) {
			handled, err := s.maybeHandleExitPlanModeRequest(line)
			if err != nil {
				if handled {
					// Bug B7: we identified an ExitPlanMode request but
					// failed to write the synthetic deny response.
					// Leaving Claude hanging for a reply it will never
					// get is worse than exiting — surface the failure
					// and tear the subprocess down.
					meta, _ := json.Marshal(map[string]any{"fatal": true})
					s.onEvent(provider.ProviderEvent{
						Kind:      provider.EventError,
						ThreadID:  s.threadID,
						Content:   fmt.Sprintf("claude: exit plan mode response failed: %v", err),
						Meta:      meta,
						Timestamp: time.Now(),
					})
					_ = s.proc.Close()
					return
				}
				// !handled + err: JSON parse failure for a line that
				// looked like a control_request by prefix but was
				// malformed. Log and continue — ParseLine below will
				// reject the same line and we don't want to tear the
				// session down for malformed stdout.
				log.Printf("claude: exit plan mode handling error: %v", err)
			}
			if handled {
				continue
			}
		}

		// Same prefix gating for control_response — the CLI emits these
		// only in reply to our outbound control_requests (today,
		// stop_task). Parse once here and deliver to the waiting
		// StopTask caller so we don't pay a second json.Unmarshal on
		// the streaming hot path.
		if bytes.HasPrefix(line, controlResponsePrefix) {
			s.handleControlResponseLine(line)
			continue
		}

		events, err := s.parser.ParseLine(s.threadID, line)
		if err != nil {
			log.Printf("claude: parse error: %v (line: %s)", err, string(line[:min(len(line), 200)]))
			continue
		}

		for _, evt := range events {
			if evt.Kind == provider.EventInit && evt.Meta != nil {
				var info provider.SessionInfo
				if json.Unmarshal(evt.Meta, &info) == nil && info.SessionID != "" {
					s.sessionID = info.SessionID
				}
			}
			if evt.Kind == provider.EventTurnComplete {
				s.disarmIdleWatchdog()
			}
			if evt.Kind == provider.EventApprovalRequest && evt.ItemID != "" {
				s.startApprovalTimer(evt.ItemID)
			}
			s.onEvent(evt)
		}
	}
}

func (s *Session) maybeHandleExitPlanModeRequest(line []byte) (bool, error) {
	var raw struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype  string          `json:"subtype"`
			ToolName string          `json:"tool_name"`
			Input    json.RawMessage `json:"input"`
		} `json:"request"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return false, err
	}
	if raw.Type != "control_request" || raw.Request.Subtype != "can_use_tool" || raw.Request.ToolName != "ExitPlanMode" {
		return false, nil
	}

	planMarkdown := extractExitPlanModePlan(raw.Request.Input)
	if planMarkdown != "" {
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventProposedPlan,
			ThreadID:  s.threadID,
			ItemID:    raw.RequestID,
			ItemType:  raw.Request.ToolName,
			Content:   planMarkdown,
			Timestamp: time.Now(),
		})
	}

	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": raw.RequestID,
			"response": map[string]any{
				"behavior": "deny",
				"message":  "The client captured your proposed plan. Stop here and wait for follow-up.",
			},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return true, fmt.Errorf("claude: marshal exit plan mode response: %w", err)
	}
	if err := s.proc.WriteLine(data); err != nil {
		return true, fmt.Errorf("claude: send exit plan mode response: %w", err)
	}
	return true, nil
}

// handleControlResponseLine decodes a `control_response` NDJSON line
// and routes it to the waiting StopTask caller by request_id. Called
// only from the read loop's prefix-gated branch, so all the work
// happens off the streaming hot path.
//
// Unknown request_ids are logged and dropped — the CLI might emit a
// duplicate or late reply after the session has already released its
// pending entry; silently discarding it keeps the read loop alive
// while still leaving a breadcrumb. These lines are rare in practice
// (one per out-of-band reply) so the log isn't rate-limited. Malformed
// JSON is likewise logged (not fatal): a garbled control_response
// shouldn't take the subprocess down.
func (s *Session) handleControlResponseLine(line []byte) {
	var raw struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Error     string `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		log.Printf("claude: malformed control_response line: %v", err)
		return
	}
	if raw.Type != "control_response" {
		// Prefix-only false positive (e.g. an unrelated envelope whose
		// serialized bytes happened to start with `{"type":"control_response`
		// — shouldn't happen in practice, but the check is cheap).
		return
	}

	requestID := raw.Response.RequestID
	if requestID == "" {
		log.Printf("claude: control_response missing request_id: %s", string(line[:min(len(line), 200)]))
		return
	}

	res := &stopTaskResult{}
	switch raw.Response.Subtype {
	case "success":
		res.ok = true
	case "error":
		res.errMsg = raw.Response.Error
	default:
		// The CLI only emits success / error per the wire reference;
		// unknown subtypes get recorded as errors so the StopTask caller
		// surfaces a clear message rather than silently hanging.
		res.errMsg = fmt.Sprintf("unexpected control_response subtype %q", raw.Response.Subtype)
	}

	if !s.deliverStopTaskResponse(requestID, res) {
		log.Printf("claude: control_response with no pending request_id %q (subtype=%s)", requestID, raw.Response.Subtype)
	}
}
