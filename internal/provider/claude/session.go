package claude

import (
	"bytes"
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

// controlRequestPrefix is the first bytes of a Claude control_request
// NDJSON line. Gating the ExitPlanMode handler on this prefix saves a
// json.Unmarshal on every assistant/user/stream_event line (which is
// the hot path during streaming). False positives are benign — the
// subsequent Request.Subtype / ToolName check still filters.
var controlRequestPrefix = []byte(`{"type":"control_request"`)

// controlResponsePrefix matches the CLI's reply to our outbound
// control_requests (stop_task, set_permission_mode). Prefix-gating
// it the same way as controlRequestPrefix keeps streaming deltas off
// the secondary json.Unmarshal path.
var controlResponsePrefix = []byte(`{"type":"control_response"`)

type controlRequestEnvelope struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype  string          `json:"subtype"`
		ToolName string          `json:"tool_name"`
		Input    json.RawMessage `json:"input"`
	} `json:"request"`
}

// DefaultControlRequestTimeout bounds how long outbound Claude control
// requests wait for the CLI's control_response before returning a timeout
// error. The verified stop_task spike observed sub-100ms round-trips on
// Claude CLI 2.1.112; ten seconds is a generous ceiling that still fails
// loudly if the CLI is wedged.
const DefaultControlRequestTimeout = 10 * time.Second

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
	// basePermissionMode is the runtime/access mode restored after a plan
	// turn. currentPermissionMode mirrors the last successful
	// set_permission_mode request so we avoid redundant control round-trips.
	permissionModeMu      sync.RWMutex
	basePermissionMode    string
	currentPermissionMode string
	interactionMode       provider.InteractionMode
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
	// controlRequestTimeout overrides DefaultControlRequestTimeout when non-zero.
	// Tests set this to a short window so a non-responsive fake CLI
	// doesn't stall the suite. Production leaves it zero.
	controlRequestTimeout time.Duration
	// controlRequestMu guards pendingControlRequests and controlRequestSeq.
	controlRequestMu sync.Mutex
	// pendingControlRequests maps an outbound control_request id to the
	// channel the read loop delivers the matching control_response on.
	// Entry is created before the write, then drained by readLoop or by the
	// caller on timeout / cancellation.
	pendingControlRequests map[string]chan *controlResponseResult
	// controlRequestSeq is a per-session counter so concurrent outbound
	// control_requests never collide on request_id. The session pointer is mixed
	// in (by map lifetime — a second session allocates a fresh map)
	// so request_ids don't need to be globally unique, only unique
	// within a single CLI subprocess.
	controlRequestSeq uint64
}

// controlResponseResult carries the outcome of an outbound control_request
// round-trip from the read loop back to the waiting caller. Exactly one of
// errMsg or ok is set: ok=true on subtype=success, errMsg populated on
// subtype=error. A nil pointer means the session closed before the response
// arrived.
type controlResponseResult struct {
	ok     bool
	errMsg string
}

// pendingApproval tracks a single in-flight tool-use approval so we can
// cancel its auto-deny timer when the user responds (Bug B3) and so we can
// reject duplicate responses for the same request ID (Bug B9).
type pendingApproval struct {
	cancel             chan struct{}
	resolveKind        provider.EventKind
	userInputQuestions []provider.UserInputQuestion
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
	// PermissionFlags carries the full permission flag sequence. Nil / empty
	// means "don't pass any permission-related flag".
	PermissionFlags    []string
	BasePermissionMode string
	InteractionMode    provider.InteractionMode
	MaxTurns           int
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
		Env:         withClaudeCodeEntrypoint(cfg.Env),
		EventLogger: cfg.EventLogger,
		ThreadID:    threadID,
		Provider:    string(provider.Claude),
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude: spawn: %w", err)
	}

	s := &Session{
		proc:                  proc,
		threadID:              threadID,
		model:                 cfg.Model,
		onEvent:               onEvent,
		cancel:                cancel,
		readDone:              make(chan struct{}),
		parser:                NewParser(),
		basePermissionMode:    normalizeClaudePermissionMode(cfg.BasePermissionMode),
		currentPermissionMode: normalizeClaudePermissionMode(cfg.BasePermissionMode),
		interactionMode:       cfg.InteractionMode,
	}
	// Seed the parser with the configured model so early assistant usage
	// events can be priced even if the init envelope lands late. The
	// init handler still overrides this when Claude echoes a different
	// model (auto-reroute).
	s.parser.SetModel(cfg.Model)

	go s.readLoop()

	return s, nil
}

// claudeCodeEntrypointEnv tags spawned `claude` processes so the
// resulting session JSONL header records `entrypoint: agent-overflow`
// instead of the auto-detected `sdk-cli` (which the CLI's resume
// picker filters out — see [docs/references/claude.md](../../../docs/references/claude.md)).
//
// The CLI's `initializeEntrypoint` only rewrites the env value when it
// equals the literal string `"cli"`; any other preset value (including
// custom strings) survives the early return. Setting `agent-overflow`
// keeps the session resumable from a normal `claude --resume` while
// cleanly identifying our client in telemetry.
const (
	claudeCodeEntrypointEnvVar = "CLAUDE_CODE_ENTRYPOINT"
	claudeCodeEntrypointValue  = "agent-overflow"
)

// withClaudeCodeEntrypoint returns a copy of env with
// CLAUDE_CODE_ENTRYPOINT set to "agent-overflow", unless the caller
// already provided an explicit value (tests can opt out).
func withClaudeCodeEntrypoint(env map[string]string) map[string]string {
	if _, ok := env[claudeCodeEntrypointEnvVar]; ok {
		return env
	}
	merged := make(map[string]string, len(env)+1)
	for k, v := range env {
		merged[k] = v
	}
	merged[claudeCodeEntrypointEnvVar] = claudeCodeEntrypointValue
	return merged
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
	// PermissionFlags is either nil (default CLI prompting) or a complete
	// permission-related CLI flag sequence for the selected runtime mode.
	args = append(args, cfg.PermissionFlags...)
	if cfg.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(cfg.MaxTurns))
	}
	for _, tool := range cfg.AllowedTools {
		args = append(args, "--allowedTools", tool)
	}

	return args
}

func normalizeClaudePermissionMode(mode string) string {
	switch mode {
	case "acceptEdits", "bypassPermissions", "plan":
		return mode
	default:
		return "default"
	}
}

func (s *Session) desiredPermissionModeForTurn(mode provider.InteractionMode) string {
	if provider.NormalizeInteractionMode(string(mode)) == provider.ModePlan {
		return "plan"
	}
	return s.basePermissionMode
}

func (s *Session) getCurrentPermissionMode() string {
	s.permissionModeMu.RLock()
	defer s.permissionModeMu.RUnlock()
	return normalizeClaudePermissionMode(s.currentPermissionMode)
}

func (s *Session) setCurrentPermissionMode(mode string) {
	s.permissionModeMu.Lock()
	s.currentPermissionMode = normalizeClaudePermissionMode(mode)
	s.permissionModeMu.Unlock()
}

func (s *Session) setPermissionMode(ctx context.Context, mode string) error {
	mode = normalizeClaudePermissionMode(mode)
	if mode == s.getCurrentPermissionMode() {
		return nil
	}
	opName := "set permission mode " + mode
	res, err := s.sendControlRequest(ctx, opName, map[string]any{
		"subtype": "set_permission_mode",
		"mode":    mode,
	})
	if err != nil {
		return err
	}
	if err := interpretControlResponse(res, opName); err != nil {
		return err
	}
	s.setCurrentPermissionMode(mode)
	return nil
}

// SetInteractionMode applies a chat/plan mode change to the live Claude
// permission mode. The next Send also sets this defensively, but exposing the
// operation lets the app reflect a user toggling Plan Mode while the session is
// already running.
func (s *Session) SetInteractionMode(ctx context.Context, mode provider.InteractionMode) error {
	normalized := provider.NormalizeInteractionMode(string(mode))
	if err := s.setPermissionMode(ctx, s.desiredPermissionModeForTurn(normalized)); err != nil {
		return err
	}
	s.interactionMode = normalized
	return nil
}

// Send sends a user message. The message is written as a JSON object to stdin.
// Send also arms the idle watchdog: if no stdout line arrives within the
// configured idle window, the watchdog closes the session and emits a
// timeout error so the UI is never left waiting on a wedged subprocess.
func (s *Session) Send(ctx context.Context, content string, opts provider.SendOptions) error {
	interactionMode := opts.InteractionMode
	if interactionMode == "" {
		interactionMode = s.interactionMode
	}
	if err := s.setPermissionMode(ctx, s.desiredPermissionModeForTurn(interactionMode)); err != nil {
		return err
	}
	attachments := opts.Attachments

	message := map[string]any{
		"role": "user",
	}
	if len(attachments) == 0 {
		message["content"] = content
	} else {
		blocks := make([]map[string]any, 0, 1+len(attachments))
		if strings.TrimSpace(content) != "" {
			blocks = append(blocks, map[string]any{
				"type": "text",
				"text": content,
			})
		}
		for _, attachment := range attachments {
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": attachment.MimeType,
					"data":       base64.StdEncoding.EncodeToString(attachment.Data),
				},
			})
		}
		if len(blocks) == 0 {
			return fmt.Errorf("claude: user message requires text or image content")
		}
		message["content"] = blocks
	}

	msg := map[string]any{
		"type":    "user",
		"message": message,
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

// Interrupt aborts the current turn by sending a control_request with
// subtype "interrupt" and waiting for the CLI's control_response. Per
// claude-wire.md §control_request, the CLI's interrupt handler stops
// the model and reaps in-flight foreground tool subprocesses;
// backgrounded tasks (Bash run_in_background:true, Task subagents)
// survive by design and are stopped individually via stop_task.
//
// If the CLI never acks (timeout or caller-context cancellation), the
// error surfaces to the caller — the failure is the CLI's to fix
// (every Anthropic SDK uses the same control_request primitive). We
// deliberately do NOT escalate to a process kill here: a kill would
// take down backgrounded tasks too, inverting the documented
// foreground-only behaviour and silently masking a Claude Code bug.
func (s *Session) Interrupt(ctx context.Context) error {
	res, err := s.sendControlRequest(ctx, "interrupt", map[string]any{
		"subtype": "interrupt",
	})
	if err != nil {
		return err
	}
	return interpretControlResponse(res, "interrupt")
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
// Returns a timeout error after controlRequestTimeout (or ctx.Done) if the
// CLI never answers.
func (s *Session) StopTask(ctx context.Context, taskID string) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("claude: stop_task: empty task_id")
	}
	opName := "stop_task " + taskID
	res, err := s.sendControlRequest(ctx, opName, map[string]any{
		"subtype": "stop_task",
		"task_id": taskID,
	})
	if err != nil {
		return err
	}
	return interpretControlResponse(res, opName)
}

// sendControlRequest is the shared round-trip for every outbound
// control_request the session originates (interrupt, stop_task,
// set_permission_mode). It allocates a request_id, registers the
// pending response channel, marshals + writes the envelope, and
// blocks on either ctx.Done, the configured timeout, or the matching
// control_response. Errors are wrapped with "claude: <opName>: ..."
// so callers don't repeat the prefix; the raw result is returned for
// the caller to interpret (success vs error subtype) — usually via
// interpretControlResponse, except where the caller has additional
// per-success side effects (e.g. setPermissionMode caching the new
// mode).
func (s *Session) sendControlRequest(ctx context.Context, opName string, request map[string]any) (*controlResponseResult, error) {
	requestID := s.allocateControlRequestID()
	ch := make(chan *controlResponseResult, 1)
	if !s.registerControlRequest(requestID, ch) {
		return nil, fmt.Errorf("claude: %s: session closing", opName)
	}

	msg := map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request":    request,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		s.releaseControlRequest(requestID)
		return nil, fmt.Errorf("claude: marshal %s: %w", opName, err)
	}
	if err := s.proc.WriteLine(data); err != nil {
		s.releaseControlRequest(requestID)
		return nil, fmt.Errorf("claude: write %s: %w", opName, err)
	}

	timeout := s.controlRequestTimeout
	if timeout <= 0 {
		timeout = DefaultControlRequestTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		s.releaseControlRequest(requestID)
		return nil, fmt.Errorf("claude: %s: %w", opName, ctx.Err())
	case <-timer.C:
		s.releaseControlRequest(requestID)
		return nil, fmt.Errorf("claude: %s: timeout after %s", opName, timeout)
	case res, ok := <-ch:
		// deliverControlResponse already removed the entry under lock;
		// nothing for us to release here.
		if !ok || res == nil {
			return nil, fmt.Errorf("claude: %s: session closed before response", opName)
		}
		return res, nil
	}
}

// interpretControlResponse converts a delivered control_response into
// the standard success-or-wrapped-error pair every caller needs. Used
// directly by Interrupt and StopTask; setPermissionMode inlines the
// equivalent logic because it has a per-success side effect to run.
func interpretControlResponse(res *controlResponseResult, opName string) error {
	if res.ok {
		return nil
	}
	if res.errMsg == "" {
		return fmt.Errorf("claude: %s: provider returned unspecified error", opName)
	}
	return fmt.Errorf("claude: %s: %s", opName, res.errMsg)
}

// allocateControlRequestID generates a request_id unique within the
// session. Format is a short "so-<n>" prefix so logs and wire samples
// make it clear the id originated here.
func (s *Session) allocateControlRequestID() string {
	s.controlRequestMu.Lock()
	s.controlRequestSeq++
	seq := s.controlRequestSeq
	s.controlRequestMu.Unlock()
	return fmt.Sprintf("so-%d", seq)
}

// registerControlRequest stores the pending channel under the request_id.
// Returns false when Close has run (the closing flag flipped and the
// pending map has been drained) so late control callers fail fast
// instead of parking on a channel nobody will deliver to.
//
// The closing check happens UNDER controlRequestMu so the clearPendingControlRequests
// / registerControlRequest pair serialises correctly: if Close wins the
// lock first, the registration fails; if a concurrent control request wins
// it first, the entry is visible to the subsequent clearPendingControlRequests
// drain. Without this ordering, a late registration could leak a
// pending entry past Close.
func (s *Session) registerControlRequest(requestID string, ch chan *controlResponseResult) bool {
	s.controlRequestMu.Lock()
	defer s.controlRequestMu.Unlock()
	if s.closing.Load() {
		return false
	}
	if s.pendingControlRequests == nil {
		s.pendingControlRequests = make(map[string]chan *controlResponseResult)
	}
	s.pendingControlRequests[requestID] = ch
	return true
}

// releaseControlRequest removes the pending entry and drains the channel so
// a late read-loop delivery lands in a discarded buffer. Called from
// timeout / cancel / error branches so the map never leaks entries and
// the single-slot channel never blocks a reader that already gave up.
func (s *Session) releaseControlRequest(requestID string) {
	s.controlRequestMu.Lock()
	ch, ok := s.pendingControlRequests[requestID]
	if ok {
		delete(s.pendingControlRequests, requestID)
	}
	s.controlRequestMu.Unlock()
	if !ok {
		return
	}
	select {
	case <-ch:
	default:
	}
}

// deliverControlResponse is the read-loop-side half: it matches an
// inbound control_response to a pending outbound control_request and delivers the
// result. Unknown request_ids are returned as (false) so the caller
// can log once and drop.
func (s *Session) deliverControlResponse(requestID string, res *controlResponseResult) bool {
	s.controlRequestMu.Lock()
	ch, ok := s.pendingControlRequests[requestID]
	if ok {
		delete(s.pendingControlRequests, requestID)
	}
	s.controlRequestMu.Unlock()
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

// clearPendingControlRequests closes every outstanding control-request waiter so
// Close doesn't strand a caller. Mirrors clearPendingApprovals.
func (s *Session) clearPendingControlRequests() {
	s.controlRequestMu.Lock()
	pending := s.pendingControlRequests
	s.pendingControlRequests = nil
	s.controlRequestMu.Unlock()
	for _, ch := range pending {
		// A nil send signals "session closing" — the caller returns a
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
	if !s.claimApproval(resp.RequestID, provider.EventApprovalResolved) {
		return ErrApprovalAlreadyResolved
	}
	data, err := buildApprovalResponse(resp)
	if err != nil {
		return err
	}
	return s.proc.WriteLine(data)
}

func (s *Session) RespondToUserInput(ctx context.Context, resp provider.UserInputResponse) error {
	decision, err := provider.NormalizeUserInputDecision(resp.Decision)
	if err != nil {
		return err
	}
	questions := s.pendingUserInputQuestions(resp.RequestID)
	if !s.claimApproval(resp.RequestID, provider.EventUserInputResolved) {
		return ErrApprovalAlreadyResolved
	}
	approval := provider.ApprovalResponse{
		RequestID: resp.RequestID,
		Decision:  decision,
	}
	inputFields := map[string]any{
		"answers": resp.Answers,
	}
	if len(questions) > 0 {
		inputFields["questions"] = questions
	}
	input, err := json.Marshal(inputFields)
	if err != nil {
		return fmt.Errorf("claude: marshal user input answers: %w", err)
	}
	approval.UpdatedInput = input
	data, err := buildApprovalResponse(approval)
	if err != nil {
		return err
	}
	return s.proc.WriteLine(data)
}

// ErrApprovalAlreadyResolved is returned by RespondToApproval when the
// request ID has already been answered (either by an earlier response or
// by the auto-deny timeout) so callers can surface a clear message instead
// of silently shadowing the previous decision.
var ErrApprovalAlreadyResolved = fmt.Errorf("claude: approval already resolved: %w", provider.ErrStaleInteractiveRequest)

// startApprovalTimer registers a pending approval and arms the auto-deny
// timer. Subsequent responses (from the user) or calls to Close cancel
// the timer via claimApproval / clearPendingApprovals.
func (s *Session) startApprovalTimer(requestID string, resolveKind provider.EventKind) {
	s.startApprovalTimerWithQuestions(requestID, resolveKind, nil)
}

func (s *Session) startApprovalTimerWithQuestions(requestID string, resolveKind provider.EventKind, questions []provider.UserInputQuestion) {
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
	s.pendingApprovals[requestID] = &pendingApproval{
		cancel:             cancel,
		resolveKind:        resolveKind,
		userInputQuestions: append([]provider.UserInputQuestion(nil), questions...),
	}
	// Starting a new timer re-opens the ID in case the provider
	// re-sent the request after a response.
	delete(s.resolvedApprovals, requestID)
	s.approvalsMu.Unlock()

	go s.runApprovalTimer(requestID, timeout, cancel, resolveKind)
}

func (s *Session) pendingUserInputQuestions(requestID string) []provider.UserInputQuestion {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	pending := s.pendingApprovals[requestID]
	if pending == nil || len(pending.userInputQuestions) == 0 {
		return nil
	}
	return append([]provider.UserInputQuestion(nil), pending.userInputQuestions...)
}

// runApprovalTimer fires the auto-deny when the user fails to respond in
// time. A cancel signal on `cancel` means the user responded first or the
// session is closing.
func (s *Session) runApprovalTimer(requestID string, timeout time.Duration, cancel <-chan struct{}, resolveKind provider.EventKind) {
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
	}
	s.onEvent(provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  s.threadID,
		Content:   fmt.Sprintf("claude: %s timed out for request %s after %s — auto-denied to keep session alive", timeoutSubject, requestID, timeout),
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
			Kind:      p.resolveKind,
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
	s.clearPendingControlRequests()
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

		// Release any control_request callers still parked on a pending
		// control_response. If the subprocess died on its own (io.EOF,
		// crash, watchdog kill) Close won't be the path that drains
		// the map, so the caller would otherwise sit idle until its
		// own timeout fires. Signalling here surfaces "session closed
		// before response" within a handful of milliseconds of the
		// subprocess exit.
		s.clearPendingControlRequests()

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

		// Gate control_request pre-handling on a byte-prefix check so
		// every streaming text_delta line doesn't pay an extra
		// json.Unmarshal. ParseLine below still handles the line if
		// the gate skips this branch.
		if bytes.HasPrefix(line, controlRequestPrefix) {
			var raw controlRequestEnvelope
			if err := json.Unmarshal(line, &raw); err != nil {
				log.Printf("claude: control_request handling error: %v", err)
			} else if raw.Type == "control_request" {
				handled, fatalMessage, err := s.handleControlRequest(raw)
				if err != nil {
					if fatalMessage != "" {
						meta, _ := json.Marshal(map[string]any{"fatal": true})
						s.onEvent(provider.ProviderEvent{
							Kind:      provider.EventError,
							ThreadID:  s.threadID,
							Content:   fmt.Sprintf("%s: %v", fatalMessage, err),
							Meta:      meta,
							Timestamp: time.Now(),
						})
						_ = s.proc.Close()
						return
					}
					log.Printf("claude: control_request handling error: %v", err)
				}
				if handled {
					continue
				}
			}
		}

		// Same prefix gating for control_response — the CLI emits these
		// only in reply to our outbound control_requests. Parse once
		// here and deliver to the waiting caller so we don't pay a
		// second json.Unmarshal on the streaming hot path.
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
				s.startApprovalTimer(evt.ItemID, provider.EventApprovalResolved)
			}
			if evt.Kind == provider.EventUserInputRequest && evt.ItemID != "" {
				var request provider.UserInputRequest
				_ = json.Unmarshal(evt.Meta, &request)
				s.startApprovalTimerWithQuestions(evt.ItemID, provider.EventUserInputResolved, request.Questions)
			}
			s.onEvent(evt)
		}
	}
}

func (s *Session) maybeHandleFullAccessToolRequest(line []byte) (bool, error) {
	var raw controlRequestEnvelope
	if err := json.Unmarshal(line, &raw); err != nil {
		return false, err
	}
	return s.handleFullAccessToolRequest(raw)
}

func (s *Session) handleControlRequest(raw controlRequestEnvelope) (handled bool, fatalMessage string, err error) {
	handled, err = s.handleExitPlanModeRequest(raw)
	if err != nil || handled {
		if err != nil && handled {
			return handled, "claude: exit plan mode response failed", err
		}
		return handled, "", err
	}
	handled, err = s.handleFullAccessToolRequest(raw)
	if err != nil && handled {
		return handled, "claude: full-access approval response failed", err
	}
	return handled, "", err
}

func (s *Session) handleFullAccessToolRequest(raw controlRequestEnvelope) (bool, error) {
	if s.getCurrentPermissionMode() != "bypassPermissions" {
		return false, nil
	}
	if raw.Type != "control_request" || raw.Request.Subtype != "can_use_tool" {
		return false, nil
	}
	switch raw.Request.ToolName {
	case "AskUserQuestion", "ExitPlanMode":
		return false, nil
	}

	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": raw.RequestID,
			"response": map[string]any{
				"behavior": "allow",
			},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return true, fmt.Errorf("claude: marshal full-access approval response: %w", err)
	}
	if err := s.proc.WriteLine(data); err != nil {
		return true, fmt.Errorf("claude: send full-access approval response: %w", err)
	}
	return true, nil
}

func (s *Session) maybeHandleExitPlanModeRequest(line []byte) (bool, error) {
	var raw controlRequestEnvelope
	if err := json.Unmarshal(line, &raw); err != nil {
		return false, err
	}
	return s.handleExitPlanModeRequest(raw)
}

func (s *Session) handleExitPlanModeRequest(raw controlRequestEnvelope) (bool, error) {
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
				"message":  "The client captured your proposed plan. Stop here and wait for the user's feedback or implementation request in a later turn.",
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
// and routes it to the waiting control-request caller by request_id. Called
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

	res := &controlResponseResult{}
	switch raw.Response.Subtype {
	case "success":
		res.ok = true
	case "error":
		res.errMsg = raw.Response.Error
	default:
		// The CLI only emits success / error per the wire reference;
		// unknown subtypes get recorded as errors so the waiting caller
		// surfaces a clear message rather than silently hanging.
		res.errMsg = fmt.Sprintf("unexpected control_response subtype %q", raw.Response.Subtype)
	}

	if !s.deliverControlResponse(requestID, res) {
		log.Printf("claude: control_response with no pending request_id %q (subtype=%s)", requestID, raw.Response.Subtype)
	}
}
