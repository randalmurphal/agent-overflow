package claudetui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"agent-overflow/internal/logging"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/terminal"
)

// session.go is the centerpiece: it implements provider.Session for the
// interactive Claude TUI. It owns the PTY that runs the real `claude`, the
// per-session gateway (ANTHROPIC_BASE_URL loopback proxy) and hook relay, and a
// single claude.Parser fed reconstructed stream-json envelopes. The two live
// sources — wire (gateway → reconstructor) and hooks (relay) — both enqueue
// envelope bytes onto one feed channel; a single feedLoop goroutine drains it
// through the parser so event emission is serialized exactly like the headless
// read loop. See docs/architecture/claude-tui-provider.md.

// Compile-time guarantee that *Session satisfies the provider.Session contract
// the app layer calls into.
var _ provider.Session = (*Session)(nil)

// agentTurnDriver assertion: the gateway drives turn reconstruction through the
// session so begin/end happen under the cross-request lock.
var _ agentTurnDriver = (*Session)(nil)

const (
	// interruptKey is the raw byte the TUI reads as "abort this turn"; Interrupt
	// writes it on Esc. Its "send" twin, submitKey, and the rest of the
	// composer/send keystroke contract live with Send in session_send.go.
	interruptKey = "\x1b"
)

// Session runs one interactive Claude TUI and reconstructs AO's normalized
// event stream from outside the process.
type Session struct {
	threadID string
	onEvent  func(provider.ProviderEvent)

	// parser turns reconstructed stream-json envelopes into ProviderEvents.
	// Driven exclusively from feedLoop, so its state stays single-goroutine.
	parser *claude.Parser

	// feed carries reconstructed envelope bytes from the wire + hook sources to
	// the single parser goroutine. done closes on teardown to stop the loop and
	// unblock any producer parked on feed.
	feed chan json.RawMessage
	done chan struct{}

	// recMu guards the reconstructor's cross-request state (turn-usage
	// accumulation, session identity, the subagent launch registry).
	// begin/end/interrupt/setSessionInfo take it; the per-request onSSE path is
	// lock-free (local assembler + feed send).
	recMu sync.Mutex
	rec   *reconstructor

	gateway *gateway
	relay   *hookRelay

	// term owns the PTY running `claude`. One terminal session per AO session.
	term       *terminal.Manager
	terminalID string

	// systemPromptPath is the temp file cfg.SystemPrompt was written to for
	// `--system-prompt-file`, or "" for a session with no override. Removed by
	// Close — which the NewSession failure path also runs, so every
	// failed-launch path drops it too. Written once during NewSession before
	// any concurrent reader exists; Close reads it under mu with the rest of
	// the teardown state.
	systemPromptPath string

	// emitMu serializes every onEvent call so the parser goroutine, the proxy
	// error path, and the PTY-exit path can't interleave events into triage —
	// preserving the headless single-goroutine emission contract.
	emitMu sync.Mutex

	mu        sync.Mutex
	sessionID string
	pid       int
	closing   bool
	// attachments is the set of live take-control claims, one per attaching
	// client (the app mints one per connection). Its size is the fan-out
	// refcount: the tee below is installed while it is non-empty and torn down
	// when the last claim goes. nil until the first attach. See attach.go.
	attachments map[*TerminalAttachment]struct{}
	// sink is the ONE live tee of raw PTY output for the take-control panes.
	// Every attachment hands in an equivalent sink, because the app answers a
	// chunk with one thread-keyed broadcast frame that every attached client
	// already receives; installing a tee per attachment would emit each chunk
	// once per client and every client would render the duplicates. The
	// terminal ring buffers output for replay regardless, so a session nobody
	// is attached to loses nothing.
	sink func(terminalID string, sequence uint64, data []byte)
	// controlHolder is the take-control input lease: the ONE attachment whose
	// human drives the PTY via WriteInput. While it is set, Send is refused so
	// AO's programmatic turns and the human's keystrokes never interleave into
	// the TUI composer, and another attachment's SetControl is refused rather
	// than silently taking the keyboard away. Cleared by the holder, or by the
	// holder's Release — which is what a dead WebSocket runs on its behalf.
	controlHolder *TerminalAttachment
	// Cold-start composer-ready gate (see composer_ready.go), all under mu.
	// onPTYOutput feeds raw output to noteComposerOutput until the composer bar
	// marker is seen (composerMarkerSeen); awaitComposerReady waits on that to
	// release the first Send. composerScanBuf is the bounded scratch tail the scan
	// matches against, released once the marker lands or the gate latches.
	// onPTYOutput already holds mu for the sink fan-out, so the gate adds no extra
	// lock on the output hot path.
	composerMarkerSeen bool
	composerReady      bool
	composerScanBuf    []byte

	// readyPoll / readyTimeout override the composer-ready poll cadence + bounded
	// fallback; zero means use the package defaults. Set once before first use
	// (a test shrinks them to exercise the timeout path fast) — read-only
	// thereafter, so they need no lock.
	readyPoll    time.Duration
	readyTimeout time.Duration

	logf func(format string, args ...any)

	// evlog, when non-nil, enables the debug-only diagnostics in debuglog.go:
	// the reconstructed envelope feed (the TUI analog of the headless provider's
	// raw stdio) and the per-request /v1/messages classification. nil unless
	// AGENT_OVERFLOW_DEBUG opts the shared provider-event logger in.
	evlog *logging.Logger
}

// NewSession spawns the interactive Claude TUI under a fresh gateway + hook
// relay and starts the parser feed. The session is live on return: the PTY is
// running and the proxy/relay are serving. ctx cancellation tears the session
// down (the app passes a background context and relies on Close).
func NewSession(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent)) (s *Session, retErr error) {
	if onEvent == nil {
		return nil, fmt.Errorf("claudetui: onEvent callback is required")
	}

	s = &Session{
		threadID: threadID,
		onEvent:  onEvent,
		parser:   claude.NewParser(),
		feed:     make(chan json.RawMessage, 256),
		done:     make(chan struct{}),
		logf:     log.Printf,
		evlog:    cfg.EventLogger,
	}
	// Seed the parser with the resolved model so result usage can be priced
	// before the reconstructed init lands (mirrors the headless seed).
	s.parser.SetModel(cfg.Model)
	s.rec = newReconstructor(s.feedEnvelope)
	// Debug-gated: record per-request routing/reconstruction decisions to the
	// "decision" event stream only when the logger is live, so a production
	// session never builds a decisionLog (the hook stays nil). See debuglog.go.
	if s.evlog != nil {
		s.rec.debug = s.logDecision
	}

	// Anything started below is torn down by Close if a later step fails.
	defer func() {
		if retErr != nil {
			_ = s.Close()
		}
	}()

	relay, err := newHookRelay(s.feedEnvelope, s.onSessionInfo, compactionHooks{
		arm:      s.armCompaction,
		finalize: s.finalizeCompaction,
	}, s.onProxyError)
	if err != nil {
		return nil, err
	}
	s.relay = relay
	relay.start()

	// Classify logging is debug-gated: attach the callback only when the event
	// logger is live so a production session does no extra per-request work.
	var onClassify func(requestClass, int, []byte)
	if s.evlog != nil {
		onClassify = s.logClassify
	}
	gw, err := newGateway(cfg.Upstream, s, s.onProxyError, onClassify)
	if err != nil {
		return nil, err
	}
	s.gateway = gw
	gw.start()

	go s.feedLoop()

	// Materialize the system-prompt override before the launch and record the
	// path on the session in the same breath, so the deferred Close above
	// removes it on every remaining failure path — a launch that never
	// happened must not leave a 0600 prompt in the temp directory.
	systemPromptPath, err := claude.WriteSystemPromptFile(cfg.SystemPrompt)
	if err != nil {
		return nil, fmt.Errorf("claudetui: %w", err)
	}
	s.mu.Lock()
	s.systemPromptPath = systemPromptPath
	s.mu.Unlock()

	launchOpts, err := buildLaunchOptions(cfg, systemPromptPath, gw.baseURL(), relay.url(), relay.authToken())
	if err != nil {
		return nil, err
	}

	s.term = terminal.NewManager(s.onPTYOutput, s.onPTYExit)
	summary, err := s.term.Open(threadID, launchOpts)
	if err != nil {
		return nil, fmt.Errorf("claudetui: launch interactive claude: %w", err)
	}
	s.mu.Lock()
	s.terminalID = summary.TerminalID
	s.pid = summary.PID
	s.mu.Unlock()

	// Honor context cancellation as a teardown trigger for callers that pass a
	// real context; with the app's background context this parks until Close.
	go s.watchContext(ctx)

	return s, nil
}

// watchContext tears the session down if the spawn context is cancelled,
// exiting cleanly once the session closes on its own.
func (s *Session) watchContext(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = s.Close()
	case <-s.done:
	}
}

// feedEnvelope enqueues one reconstructed envelope for the parser goroutine.
// Drops the envelope once the session is tearing down so a late wire/hook
// source can't block on a drained loop.
func (s *Session) feedEnvelope(line json.RawMessage) {
	select {
	case s.feed <- line:
	case <-s.done:
	}
}

// feedLoop is the single parser goroutine. It mirrors the headless read loop:
// one ParseLine call at a time, each resulting event emitted in order. A parse
// error is logged and skipped — one bad envelope must not wedge the stream.
//
// The reorder buffer restores the headless "tool start before completion"
// ordering that the wire+hook split can invert (see reorder.go). It is a
// feedLoop local: this goroutine is its only driver, so it stays lock-free and
// is torn down with the loop. Live streaming deltas — by far the highest
// frequency envelope and never part of tool ordering — skip it via a byte-prefix
// check so they pay neither the classification nor a full parse.
func (s *Session) feedLoop() {
	reorder := newFeedReorder()
	for {
		select {
		case line := <-s.feed:
			if bytes.HasPrefix(line, streamEventPrefix) {
				s.parseAndEmit(line)
				continue
			}
			for _, env := range reorder.admit(line) {
				s.parseAndEmit(env)
			}
		case <-s.done:
			return
		}
	}
}

// parseAndEmit logs, parses, and serially emits one reconstructed envelope.
// Split out of feedLoop so the reorder buffer can replay several envelopes (a
// held completion released right after its tool_use start) through the identical
// path. A parse error is logged and skipped — one bad envelope must not wedge
// the stream.
func (s *Session) parseAndEmit(line json.RawMessage) {
	s.logEnvelope(line)
	events, err := s.parser.ParseLine(s.threadID, line)
	if err != nil {
		s.logf("claudetui: parse error: %v (line: %s)", err, truncate(line, 200))
		return
	}
	for _, evt := range events {
		if evt.Kind == provider.EventInit && evt.Meta != nil {
			var info provider.SessionInfo
			if json.Unmarshal(evt.Meta, &info) == nil && info.SessionID != "" {
				s.setSessionID(info.SessionID)
			}
		}
		s.emit(evt)
	}
}

// emit serializes a single normalized event to the app. Held across onEvent so
// the parser, proxy-error, and PTY-exit paths never interleave.
func (s *Session) emit(evt provider.ProviderEvent) {
	s.emitMu.Lock()
	s.onEvent(evt)
	s.emitMu.Unlock()
}

// --- agentTurnDriver (called from the gateway handler goroutine) ----------

// beginAgentTurn opens reconstruction for one classAgent /v1/messages request.
// Guarded so a parallel-subagent request can't race the shared init/turn state.
func (s *Session) beginAgentTurn(req *messagesRequest) *agentRequest {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	return s.rec.beginAgentRequest(req)
}

// beginSubagentTurn opens nesting reconstruction for one subagent request.
// Resolves the launching Agent tool_call from the request (cached by agentID, or
// content-matched on the subagent's task prompt) under the same lock as the
// launch registry. Returns nil when no launch matches, so the gateway forwards
// the request without reconstructing it rather than mis-attributing the
// subagent's work to the main thread.
func (s *Session) beginSubagentTurn(req *messagesRequest, agentID string) *agentRequest {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	parent := s.rec.resolveSubagentParent(agentID, firstUserText(req.Messages))
	if parent == "" {
		s.logf("claudetui: subagent request agent_id=%s has no matching Agent launch; not reconstructed", agentID)
		return nil
	}
	return s.rec.beginSubagentRequest(parent)
}

// beginCompactionCapture claims a classAgent request as the compaction
// summarizer when a compaction is armed (PreCompact seen), returning nil
// otherwise so the gateway falls through to normal routing. Under recMu like the
// other reconstructor mutators (it reads + clears the armed flag).
func (s *Session) beginCompactionCapture(req *messagesRequest) *agentRequest {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	return s.rec.beginCompactionCapture()
}

// endAgentTurn closes one request's reconstruction: emits the assembled
// assistant envelope, folds usage, and synthesizes the turn-closing result when
// the model is done.
func (s *Session) endAgentTurn(ar *agentRequest) {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	ar.end()
}

// armCompaction arms the compaction-summarizer capture (PreCompact hook). Under
// recMu like the other reconstructor mutators.
func (s *Session) armCompaction() {
	s.recMu.Lock()
	s.rec.armCompaction()
	s.recMu.Unlock()
}

// finalizeCompaction emits the Compacted boundary enriched with the captured
// summarizer thinking + the committed summary (PostCompact hook). Under recMu;
// the emit runs through the same feed channel the wire path uses, so it
// serializes with the parser goroutine.
func (s *Session) finalizeCompaction(trigger, summary string) {
	s.recMu.Lock()
	s.rec.finalizeCompaction(trigger, summary)
	s.recMu.Unlock()
}

// queueUserEcho records an AO Send on the reconstructor's pending-echo FIFO so
// the next main request confirms it with a user{isReplay} echo. Under recMu like
// the other reconstructor mutators. Called by Send after the submit write lands.
func (s *Session) queueUserEcho(content, uuid string) {
	s.recMu.Lock()
	s.rec.pushUserEcho(content, uuid)
	s.recMu.Unlock()
}

// onSessionInfo records identity from the SessionStart hook for both the
// SessionID() accessor and the reconstructed init envelope.
func (s *Session) onSessionInfo(sessionID, cwd, version string) {
	if sessionID != "" {
		s.setSessionID(sessionID)
	}
	s.recMu.Lock()
	s.rec.setSessionInfo(sessionID, cwd, version)
	s.recMu.Unlock()
}

// --- provider.Session -----------------------------------------------------

// Interrupt aborts the current turn. The TUI cancels on Esc, which also aborts
// the in-flight /v1/messages request (so the wire delivers no result). Because
// the TUI path has no control-ack channel, we additionally synthesize the
// interrupt result envelope the parser classifies as a user abort so the turn
// closes. See turndriver.interruptTurn for the ordering caveat when an Esc
// lands mid-stream.
func (s *Session) Interrupt(ctx context.Context) error {
	if err := s.writePTY([]byte(interruptKey)); err != nil {
		return err
	}
	s.recMu.Lock()
	s.rec.interruptTurn()
	s.recMu.Unlock()
	return nil
}

// RespondToApproval has no work on the interactive provider: full-access
// auto-allows every tool at the relay, so no approval is ever pending. Returning
// an error surfaces a miswired call instead of silently succeeding.
func (s *Session) RespondToApproval(ctx context.Context, resp provider.ApprovalResponse) error {
	return fmt.Errorf("claudetui: tool approvals are auto-allowed in full-access; no approval pending for %q", resp.RequestID)
}

// RespondToUserInput delivers the human's AskUserQuestion answer to the blocked
// hook. The app emits EventUserInputResolved itself after this returns, so the
// session must not also emit it.
func (s *Session) RespondToUserInput(ctx context.Context, resp provider.UserInputResponse) error {
	return s.relay.respond(resp.RequestID, resp.Answers)
}

// PID returns the interactive claude's process id, which is also its
// process-group id (the PTY child is a session leader via Setsid) so the orphan
// reaper can group-kill the whole tree.
func (s *Session) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

// SessionID returns the provider session ref, learned from the SessionStart
// hook or the reconstructed init. Empty until one of those arrives.
func (s *Session) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// Close tears down the PTY, gateway, relay, and parser. Idempotent and
// nil-safe so the NewSession failure path can reuse it.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	terminalID := s.terminalID
	systemPromptPath := s.systemPromptPath
	s.mu.Unlock()

	// Stop the parser loop and release any producer parked on feed before
	// shutting the proxy/relay down, so their handler goroutines don't deadlock
	// on a feed send while Shutdown waits for them.
	close(s.done)

	var errs []error
	if s.term != nil && terminalID != "" {
		if err := s.term.Close(terminalID); err != nil {
			errs = append(errs, err)
		}
	}
	if s.gateway != nil {
		if err := s.gateway.close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.relay != nil {
		if err := s.relay.close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.parser != nil {
		s.parser.Close()
	}
	// After the PTY is down, so the file outlives every read the CLI could
	// still make of it. Best-effort with a log line, same as headless.
	claude.RemoveSystemPromptFile(systemPromptPath)
	return errors.Join(errs...)
}

// --- PTY callbacks --------------------------------------------------------

// onPTYOutput fans raw terminal output to the attach sink when one is wired.
// The terminal ring buffers output for replay independently, so a nil sink
// simply means "nobody is attached yet."
func (s *Session) onPTYOutput(threadID, terminalID string, sequence uint64, data []byte) {
	s.mu.Lock()
	// Feed the cold-start composer-ready gate until it latches: scan boot output
	// for the composer bar marker so the first Send waits until claude has mounted
	// its composer and is parked reading input. noteComposerOutput no-ops once the
	// marker is seen or the gate has latched, so a warm session pays nothing extra
	// here (this lock is already taken for the sink fan-out below).
	s.noteComposerOutput(data)
	sink := s.sink
	s.mu.Unlock()
	if sink != nil {
		sink(terminalID, sequence, data)
	}
}

// onPTYExit surfaces interactive-claude death. A non-host-initiated exit (the
// process died or the user quit it in take-control) emits the "error"
// session-status triage promotes to session_died + a truncated turn-complete;
// the trailing "disconnected" clears live state. A host-initiated Close skips
// the error and just disconnects.
func (s *Session) onPTYExit(threadID, terminalID string, status terminal.ExitStatus) {
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()

	// Same two events, same order, same reason as both stdio read loops —
	// only the meta source differs (a PTY exit status, not a subprocess
	// exit). See provider.EmitTeardownStatus for why the pair is load-bearing.
	provider.EmitTeardownStatusWithMeta(s.emit, s.threadID, ptyExitMeta(status), closing)
}

// onProxyError surfaces a gateway/relay infrastructure failure as a non-fatal
// error event and logs it. These are rare (serve failure, a mid-stream client
// write error) and never carry credential-bearing detail.
func (s *Session) onProxyError(err error) {
	if err == nil {
		return
	}
	s.logf("claudetui: %v", err)
	s.emit(provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  s.threadID,
		Content:   err.Error(),
		Meta:      mustMarshal(map[string]any{"fatal": false}),
		Timestamp: time.Now(),
	})
}

// --- helpers --------------------------------------------------------------

// writePTY writes raw bytes to the interactive claude's PTY.
func (s *Session) writePTY(data []byte) error {
	s.mu.Lock()
	term, terminalID := s.term, s.terminalID
	s.mu.Unlock()
	if term == nil || terminalID == "" {
		return fmt.Errorf("claudetui: no live terminal to write to")
	}
	return term.Write(terminalID, data)
}

func (s *Session) setSessionID(id string) {
	s.mu.Lock()
	s.sessionID = id
	s.mu.Unlock()
}

// ptyExitMeta renders a chat-safe ProcessExitInfo from a terminal exit. It
// synthesizes the reason from the controlled code/signal rather than passing
// terminal.ExitStatus.Reason (which can embed host text) into the chat-visible
// field — see provider.ProcessExitInfo's security note.
func ptyExitMeta(status terminal.ExitStatus) json.RawMessage {
	var info provider.ProcessExitInfo
	switch {
	case status.Signal != 0:
		info.Signal = status.Signal.String()
		info.Reason = "provider session terminated by signal " + info.Signal
	case status.Code != 0:
		info.ExitCode = status.Code
		info.Reason = "provider session exited with code " + strconv.Itoa(status.Code)
	default:
		info.Reason = "provider session ended"
	}
	return mustMarshal(info)
}

// truncate bounds a byte slice for log lines.
func truncate(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max])
}
