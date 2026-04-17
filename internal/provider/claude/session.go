package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/logging"
	"agent-overflow/internal/provider"
)

// DefaultIdleTimeout is how long the watchdog waits for ANY stdout line
// after a Send before declaring the provider wedged. Claude streaming
// responses emit partial-message deltas frequently, so two minutes is
// generous — the intent is to catch provider hangs, not impatient users.
const DefaultIdleTimeout = 120 * time.Second

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
}

// Config for creating a Claude session.
type Config struct {
	Binary         string // default: "claude"
	Model          string
	WorkDir        string
	Resume         string // session ID to resume, empty for new
	ForkSession    bool
	SystemPrompt   string
	AllowedTools   []string
	PermissionMode string // "default", "acceptEdits", "bypassPermissions"
	MaxTurns       int
	EventLogger    *logging.Logger
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
	}

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
	if cfg.PermissionMode != "" && cfg.PermissionMode != "default" {
		args = append(args, "--permission-mode", cfg.PermissionMode)
	}
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
			s.onEvent(provider.ProviderEvent{
				Kind:      provider.EventError,
				ThreadID:  s.threadID,
				Content:   fmt.Sprintf("claude: provider idle timeout after %s — no output received", timeout),
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

// RespondToApproval sends a tool-use approval decision back to the CLI.
// Accepts both Codex-native values (accept, acceptForSession, decline, cancel)
// and legacy values (allow, allow_session, deny) for backward compatibility.
// When resp.UpdatedInput or resp.UpdatedPermissions are non-empty and the
// decision is an allow, the raw JSON is forwarded to the CLI as the
// Claude-SDK-compatible CanUseTool response fields.
func (s *Session) RespondToApproval(ctx context.Context, resp provider.ApprovalResponse) error {
	data, err := buildApprovalResponse(resp)
	if err != nil {
		return err
	}
	return s.proc.WriteLine(data)
}

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
				s.onEvent(provider.ProviderEvent{
					Kind:      provider.EventError,
					ThreadID:  s.threadID,
					Content:   fmt.Sprintf("claude: read error: %v", err),
					Timestamp: time.Now(),
				})
			}
			return
		}
		// Any output keeps the watchdog happy — even lines we drop below.
		s.pulseIdleWatchdog()

		handled, err := s.maybeHandleExitPlanModeRequest(line)
		if err != nil {
			log.Printf("claude: exit plan mode handling error: %v", err)
		}
		if handled {
			continue
		}

		events, err := ParseLine(s.threadID, line)
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
