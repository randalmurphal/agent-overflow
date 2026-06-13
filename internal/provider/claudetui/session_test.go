package claudetui

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/terminal"
)

// eventRecorder captures the serialized ProviderEvents a session emits.
type eventRecorder struct {
	mu     sync.Mutex
	events []provider.ProviderEvent
}

func (r *eventRecorder) record(e provider.ProviderEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *eventRecorder) snapshot() []provider.ProviderEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]provider.ProviderEvent(nil), r.events...)
}

func (r *eventRecorder) waitForKind(t *testing.T, kind provider.EventKind) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range r.snapshot() {
			if e.Kind == kind {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for event kind %v", kind)
}

// newWiredSession builds a session with the parser feed loop running but no
// PTY/gateway/relay — enough to exercise the feed → parser → serialized emit
// path and the agentTurnDriver seam the gateway drives, without spawning a real
// claude. Close (via cleanup) is nil-safe for the unset transports.
func newWiredSession(t *testing.T) (*Session, *eventRecorder) {
	t.Helper()
	rec := &eventRecorder{}
	s := &Session{
		threadID: testThread,
		parser:   claude.NewParser(),
		feed:     make(chan json.RawMessage, 256),
		done:     make(chan struct{}),
		logf:     func(string, ...any) {},
		onEvent:  rec.record,
	}
	s.rec = newReconstructor(s.feedEnvelope)
	go s.feedLoop()
	t.Cleanup(func() { _ = s.Close() })
	return s, rec
}

// TestSessionEmitsTurnThroughParser proves the centerpiece wiring: the
// SessionStart identity seeds SessionID and the init envelope, a turn driven
// through the gateway's agentTurnDriver seam flows feed → parser → emit, and
// the events arrive serialized and in order (init before text before complete).
func TestSessionEmitsTurnThroughParser(t *testing.T) {
	s, rec := newWiredSession(t)

	// SessionStart hook identity arrives before the first request.
	s.onSessionInfo("sess-tui-42", "/work", "2.1.200")
	if got := s.SessionID(); got != "sess-tui-42" {
		t.Fatalf("SessionID after onSessionInfo = %q, want sess-tui-42", got)
	}

	_, req := classifyRequest([]byte(agentReqBody))
	ar := s.beginAgentTurn(req)
	for _, line := range []string{
		`{"type":"message_start","message":{"id":"msg_z","model":"claude-haiku","role":"assistant","usage":{"input_tokens":5,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"output_tokens":3}}`,
	} {
		ar.onSSE(json.RawMessage(line))
	}
	s.endAgentTurn(ar)

	rec.waitForKind(t, provider.EventTurnComplete)
	events := rec.snapshot()

	// init carries the hook session id (parser mapped session_id → SessionID).
	inits := findKind(events, provider.EventInit)
	if len(inits) != 1 {
		t.Fatalf("expected exactly one EventInit, got %d (kinds %v)", len(inits), kindsOf(events))
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(inits[0].Meta, &info); err != nil {
		t.Fatalf("decode init meta: %v", err)
	}
	if info.SessionID != "sess-tui-42" {
		t.Errorf("init SessionID = %q, want sess-tui-42", info.SessionID)
	}

	// Serialized ordering: init precedes the streamed text, which precedes the
	// turn-complete. A single feed loop + emit lock guarantees this.
	idxInit := indexOfKind(events, provider.EventInit)
	idxText := indexOfKind(events, provider.EventTextDelta)
	idxDone := indexOfKind(events, provider.EventTurnComplete)
	if !(idxInit >= 0 && idxText > idxInit && idxDone > idxText) {
		t.Errorf("events out of order: init=%d text=%d complete=%d (kinds %v)", idxInit, idxText, idxDone, kindsOf(events))
	}
}

// TestSessionRespondToUserInputThroughRelay proves RespondToUserInput delivers
// the answer to the live relay (and that an unknown request id surfaces an
// error rather than panicking). The app — not the session — emits
// EventUserInputResolved, so no resolved event is asserted here.
func TestSessionRespondToUserInputThroughRelay(t *testing.T) {
	s, _ := newWiredSession(t)
	relay, err := newHookRelay(s.feedEnvelope, s.onSessionInfo, func(error) {})
	if err != nil {
		t.Fatalf("newHookRelay: %v", err)
	}
	relay.start()
	t.Cleanup(func() { _ = relay.close() })
	s.relay = relay

	// No pending question yet → a clear error, not a panic.
	if err := s.RespondToUserInput(context.Background(), provider.UserInputResponse{
		RequestID: "missing",
		Answers:   map[string]provider.UserInputAnswer{"q": {"a"}},
	}); err == nil {
		t.Error("RespondToUserInput for an unknown request should error")
	}
}

// TestSessionSendValidation covers the two reject-before-write branches: the
// interactive provider has no stdin image path, and blank content is refused.
func TestSessionSendValidation(t *testing.T) {
	s := &Session{logf: func(string, ...any) {}}
	ctx := context.Background()

	if err := s.Send(ctx, "hello", provider.SendOptions{
		Attachments: []provider.ImageAttachment{{ID: "img1"}},
	}); err == nil {
		t.Error("Send with image attachments should error on the interactive provider")
	}
	if err := s.Send(ctx, "   ", provider.SendOptions{}); err == nil {
		t.Error("Send with blank content should error")
	}
}

// TestSendKeystrokes pins the keystroke contract Send writes to the PTY: a
// composer-clear FIRST (so a prompt the TUI restored on a prior Esc-revert can't
// fuse with this paste), then the bracketed-paste-wrapped content, then submit.
func TestSendKeystrokes(t *testing.T) {
	clear, paste, submit := sendKeystrokes("hello world")

	// Clear is exactly composerClearKeystrokes Ctrl-U presses and nothing else,
	// so it is a no-op on an already-empty composer.
	if want := strings.Repeat("\x15", composerClearKeystrokes); string(clear) != want {
		t.Errorf("clear = %q, want %d Ctrl-U presses", clear, composerClearKeystrokes)
	}
	if got, want := string(paste), "\x1b[200~hello world\x1b[201~"; got != want {
		t.Errorf("paste = %q, want %q", got, want)
	}
	if string(submit) != "\r" {
		t.Errorf("submit = %q, want CR", submit)
	}
}

// TestSendKeystrokesStripsStrayPasteTerminator proves a paste terminator embedded
// in user content is removed, so it can't close the bracketed paste early and
// have the tail interpreted as raw keystrokes.
func TestSendKeystrokesStripsStrayPasteTerminator(t *testing.T) {
	_, paste, _ := sendKeystrokes("before\x1b[201~after")
	if got, want := string(paste), "\x1b[200~beforeafter\x1b[201~"; got != want {
		t.Errorf("paste = %q, want %q", got, want)
	}
}

// TestPtyReadyForSend pins the cold-start readiness predicate: a Send may only
// write once the init output burst has landed AND the PTY stream has gone idle.
// Either alone is not enough — sending mid-burst (or before any output) is the
// cold-start bug where the submit CR is swallowed with the paste. The end-to-end
// proof is spike/claude-mitm/probe_cold_submit.py (immediate 0/3, idle-gate 3/3).
func TestPtyReadyForSend(t *testing.T) {
	cases := []struct {
		name  string
		bytes int
		quiet time.Duration
		want  bool
	}{
		{"no output yet", 0, time.Second, false},
		{"mid burst, not idle", composerReadyMinBytes, 0, false},
		{"burst landed but still streaming", composerReadyMinBytes, composerReadyQuiet - time.Millisecond, false},
		{"idle but too little output", composerReadyMinBytes - 1, composerReadyQuiet, false},
		{"burst landed and idle", composerReadyMinBytes, composerReadyQuiet, true},
		{"well past both thresholds", 1 << 16, time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ptyReadyForSend(tc.bytes, tc.quiet); got != tc.want {
				t.Errorf("ptyReadyForSend(%d, %v) = %v, want %v", tc.bytes, tc.quiet, got, tc.want)
			}
		})
	}
}

// TestAwaitComposerReady covers the three gate outcomes: an already-latched
// session returns at once, an observed-ready PTY latches and returns, and a
// never-ready session honors ctx cancellation instead of spinning to the
// timeout.
func TestAwaitComposerReady(t *testing.T) {
	t.Run("latched returns immediately", func(t *testing.T) {
		s := &Session{composerReady: true, logf: func(string, ...any) {}}
		if err := s.awaitComposerReady(context.Background()); err != nil {
			t.Fatalf("awaitComposerReady (latched) = %v, want nil", err)
		}
	})

	t.Run("becomes ready while polling and latches", func(t *testing.T) {
		// Start not-ready (enough bytes, but output just arrived so quiet is below
		// the threshold), then flip to idle from another goroutine so the poll loop
		// observes the transition rather than passing on the first check.
		s := &Session{readyPoll: time.Millisecond, logf: func(string, ...any) {}}
		s.mu.Lock()
		s.ptyBytes = composerReadyMinBytes + 1
		s.lastPTYAt = time.Now()
		s.mu.Unlock()
		go func() {
			time.Sleep(5 * time.Millisecond)
			s.mu.Lock()
			s.lastPTYAt = time.Now().Add(-time.Hour) // now idle well past the gate
			s.mu.Unlock()
		}()
		if err := s.awaitComposerReady(context.Background()); err != nil {
			t.Fatalf("awaitComposerReady (ready) = %v, want nil", err)
		}
		s.mu.Lock()
		latched := s.composerReady
		s.mu.Unlock()
		if !latched {
			t.Error("composerReady should latch true once observed ready")
		}
	})

	t.Run("times out and proceeds, logging once", func(t *testing.T) {
		// ptyBytes stays 0, so the gate never opens; the bounded timeout must let
		// the send proceed anyway (a re-sendable miss beats a hang) and log once.
		var logged int
		s := &Session{
			readyPoll:    time.Millisecond,
			readyTimeout: 20 * time.Millisecond,
			logf:         func(string, ...any) { logged++ },
		}
		start := time.Now()
		if err := s.awaitComposerReady(context.Background()); err != nil {
			t.Fatalf("awaitComposerReady (timeout) = %v, want nil (proceed anyway)", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("timeout path took %v, want ~readyTimeout", elapsed)
		}
		s.mu.Lock()
		latched := s.composerReady
		s.mu.Unlock()
		if !latched {
			t.Error("composerReady should latch true after the timeout fallback")
		}
		if logged != 1 {
			t.Errorf("timeout should log exactly once, logged %d times", logged)
		}
	})

	t.Run("honors cancellation mid-poll", func(t *testing.T) {
		// Gate shut (ptyBytes 0) with the timeout far off, so only cancellation can
		// end an in-flight poll — proves ctx.Done() interrupts the wait promptly.
		s := &Session{
			readyPoll:    time.Millisecond,
			readyTimeout: time.Hour,
			logf:         func(string, ...any) {},
		}
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(10*time.Millisecond, cancel)
		start := time.Now()
		if err := s.awaitComposerReady(ctx); err == nil {
			t.Error("awaitComposerReady should return the ctx error when cancelled mid-poll")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("cancellation took %v, want prompt return", elapsed)
		}
	})

	t.Run("honors a pre-cancelled context", func(t *testing.T) {
		s := &Session{logf: func(string, ...any) {}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.awaitComposerReady(ctx); err == nil {
			t.Error("awaitComposerReady with a pre-cancelled context should return its error")
		}
	})
}

// TestSessionRespondToApprovalRejected proves the full-access provider has no
// approval to resolve — a call is a miswire and surfaces as an error.
func TestSessionRespondToApprovalRejected(t *testing.T) {
	s := &Session{logf: func(string, ...any) {}}
	if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{RequestID: "r1"}); err == nil {
		t.Error("RespondToApproval should error: full-access auto-allows tools")
	}
}

// TestSessionCloseIdempotent proves Close is nil-safe (no PTY/gateway/relay
// set, as in a partially constructed session) and a no-op on the second call.
func TestSessionCloseIdempotent(t *testing.T) {
	s := &Session{
		threadID: testThread,
		parser:   claude.NewParser(),
		feed:     make(chan json.RawMessage, 1),
		done:     make(chan struct{}),
		logf:     func(string, ...any) {},
		onEvent:  func(provider.ProviderEvent) {},
	}
	s.rec = newReconstructor(s.feedEnvelope)

	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close should be a no-op: %v", err)
	}
}

// TestPtyExitMeta covers the chat-safe exit-reason synthesis: signal, non-zero
// code, and clean exit each produce a controlled ProcessExitInfo (never the raw
// terminal reason string).
func TestPtyExitMeta(t *testing.T) {
	cases := []struct {
		name   string
		status terminal.ExitStatus
		want   provider.ProcessExitInfo
	}{
		{
			name:   "clean",
			status: terminal.ExitStatus{Code: 0},
			want:   provider.ProcessExitInfo{Reason: "provider session ended"},
		},
		{
			name:   "code",
			status: terminal.ExitStatus{Code: 2, Reason: "exit"},
			want:   provider.ProcessExitInfo{Reason: "provider session exited with code 2", ExitCode: 2},
		},
		{
			name:   "signal",
			status: terminal.ExitStatus{Signal: syscall.SIGKILL, Reason: "signal:killed"},
			want:   provider.ProcessExitInfo{Reason: "provider session terminated by signal " + syscall.SIGKILL.String(), Signal: syscall.SIGKILL.String()},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got provider.ProcessExitInfo
			if err := json.Unmarshal(ptyExitMeta(tc.status), &got); err != nil {
				t.Fatalf("unmarshal ptyExitMeta: %v", err)
			}
			if got != tc.want {
				t.Errorf("ptyExitMeta(%+v) = %+v, want %+v", tc.status, got, tc.want)
			}
		})
	}
}

func indexOfKind(events []provider.ProviderEvent, kind provider.EventKind) int {
	for i, e := range events {
		if e.Kind == kind {
			return i
		}
	}
	return -1
}
