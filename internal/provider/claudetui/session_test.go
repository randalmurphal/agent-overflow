package claudetui

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
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

// TestSessionReordersHookCompletionBeforeStart drives the wire+hook ordering
// inversion end to end: a fast tool's PostToolUse hook lands its tool_result on
// the feed BEFORE the gateway's end() emits the assembled assistant (the sole
// EventToolStart source). The reorder buffer must still emit EventToolStart
// before EventToolComplete, and the completion must keep its output. Without the
// buffer the completion is fed first — triage would drop it and the
// turn-complete force-close would mark the successful command failed. Red
// without reorder.go.
func TestSessionReordersHookCompletionBeforeStart(t *testing.T) {
	s, rec := newWiredSession(t)

	_, req := classifyRequest([]byte(agentReqBody))
	ar := s.beginAgentTurn(req)

	// Live deltas tee through as the gateway forwards them. stop_reason=tool_use
	// ⇒ the model is not done: end() emits the assembled assistant but no result.
	for _, sse := range []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"claude-haiku","role":"assistant","usage":{"input_tokens":5,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"echo hi\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":5,"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	} {
		ar.onSSE(json.RawMessage(sse))
	}

	// The hook completion races onto the feed BEFORE end() emits the assistant.
	s.feedEnvelope(json.RawMessage(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"hi"}]},"tool_use_result":{"stdout":"hi","stderr":"","interrupted":false}}`))

	s.endAgentTurn(ar) // emits the assembled assistant (EventToolStart for toolu_1)

	rec.waitForKind(t, provider.EventToolComplete)
	events := rec.snapshot()

	idxStart := indexOfKind(events, provider.EventToolStart)
	idxComplete := indexOfKind(events, provider.EventToolComplete)
	if idxStart < 0 {
		t.Fatalf("expected EventToolStart, got kinds %v", kindsOf(events))
	}
	if idxComplete <= idxStart {
		t.Fatalf("tool start must precede completion: start=%d complete=%d (kinds %v)", idxStart, idxComplete, kindsOf(events))
	}

	completes := findKind(events, provider.EventToolComplete)
	if len(completes) == 0 || !strings.Contains(completes[0].Content, "hi") {
		t.Errorf("completion lost its output (was it dropped?): %+v", completes)
	}
}

// TestSessionRespondToUserInputThroughRelay proves RespondToUserInput delivers
// the answer to the live relay (and that an unknown request id surfaces an
// error rather than panicking). The app — not the session — emits
// EventUserInputResolved, so no resolved event is asserted here.
func TestSessionRespondToUserInputThroughRelay(t *testing.T) {
	s, _ := newWiredSession(t)
	relay, err := newHookRelay(s.feedEnvelope, s.onSessionInfo, compactionHooks{}, func(error) {})
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

// TestSessionSendValidation covers the reject-before-write branches: a send with
// neither text nor an image attachment is refused, and an image attachment that
// arrives without a resolved on-disk path is a wiring bug we fail loudly on
// rather than silently dropping the image. Both fire before the composer-ready
// gate, so a bare session (no live PTY) exercises them.
func TestSessionSendValidation(t *testing.T) {
	s := &Session{logf: func(string, ...any) {}}
	ctx := context.Background()

	if err := s.Send(ctx, "   ", provider.SendOptions{}); err == nil {
		t.Error("Send with neither text nor attachments should error")
	}
	if err := s.Send(ctx, "", provider.SendOptions{
		Attachments: []provider.ImageAttachment{{ID: "img1"}},
	}); err == nil {
		t.Error("Send with an attachment missing its on-disk path should error")
	}
}

// TestBuildSendSteps pins the PTY keystroke contract for the send shapes,
// including the gap that must follow each block: a composer-clear FIRST (so an
// Esc-revert leftover can't fuse with this send), the message body replayed as a
// paste per segment with each image pasted at its "[Image #i]" marker, then
// submit. Every interior paste→paste boundary is the longer pasteSettle so two
// bracketed pastes don't merge into one chunk; clear→first-paste and
// last-paste→submit are the short composerSettle, and submit has no trailing gap.
func TestBuildSendSteps(t *testing.T) {
	clearData := []byte(strings.Repeat("\x15", composerClearKeystrokes))
	paste := func(s string) []byte { return []byte("\x1b[200~" + s + "\x1b[201~") }

	t.Run("text only", func(t *testing.T) {
		assertSteps(t, buildSendSteps("hello world", nil), []sendStep{
			{data: clearData, settle: composerSettle},
			{data: paste("hello world"), settle: composerSettle},
			{data: []byte("\r")},
		})
	})

	t.Run("image inline in the middle", func(t *testing.T) {
		// The user dropped the image mid-text; the composer left "[Image #1]" at
		// that offset. The path pastes in place, between the two text runs, so
		// Claude labels the image inline instead of front-loading it.
		assertSteps(t, buildSendSteps("look at [Image #1] this", []string{"/a/one.png"}), []sendStep{
			{data: clearData, settle: composerSettle},
			{data: paste("look at "), settle: pasteSettle},
			{data: paste("/a/one.png"), settle: pasteSettle},
			{data: paste(" this"), settle: composerSettle},
			{data: []byte("\r")},
		})
	})

	t.Run("image marker at the start", func(t *testing.T) {
		assertSteps(t, buildSendSteps("[Image #1] caption", []string{"/a/one.png"}), []sendStep{
			{data: clearData, settle: composerSettle},
			{data: paste("/a/one.png"), settle: pasteSettle},
			{data: paste(" caption"), settle: composerSettle},
			{data: []byte("\r")},
		})
	})

	t.Run("two images at their markers", func(t *testing.T) {
		assertSteps(t, buildSendSteps("a [Image #1] b [Image #2] c", []string{"/a/one.png", "/b/two.jpg"}), []sendStep{
			{data: clearData, settle: composerSettle},
			{data: paste("a "), settle: pasteSettle},
			{data: paste("/a/one.png"), settle: pasteSettle},
			{data: paste(" b "), settle: pasteSettle},
			{data: paste("/b/two.jpg"), settle: pasteSettle},
			{data: paste(" c"), settle: composerSettle},
			{data: []byte("\r")},
		})
	})

	t.Run("image only (marker alone, no surrounding text)", func(t *testing.T) {
		// ensureImagePlaceholders produces "[Image #1]" for an image-only send, so
		// there is no text run: the image paste is followed by composerSettle
		// (paste→submit), not pasteSettle.
		assertSteps(t, buildSendSteps("[Image #1]", []string{"/a/one.png"}), []sendStep{
			{data: clearData, settle: composerSettle},
			{data: paste("/a/one.png"), settle: composerSettle},
			{data: []byte("\r")},
		})
	})

	t.Run("missing marker appends the image after the text", func(t *testing.T) {
		// Defensive fallback: ensureImagePlaceholders should always leave a marker,
		// but if one is absent the image is appended rather than silently dropped.
		assertSteps(t, buildSendSteps("no marker here", []string{"/a/one.png"}), []sendStep{
			{data: clearData, settle: composerSettle},
			{data: paste("no marker here"), settle: pasteSettle},
			{data: paste("/a/one.png"), settle: composerSettle},
			{data: []byte("\r")},
		})
	})

	t.Run("adjacent image markers paste back-to-back", func(t *testing.T) {
		// Two images dropped with no text between them: two image pastes in a row,
		// and the image→image boundary must still be the longer pasteSettle so the
		// two paths don't merge into one chunk.
		assertSteps(t, buildSendSteps("[Image #1][Image #2]", []string{"/a/one.png", "/b/two.jpg"}), []sendStep{
			{data: clearData, settle: composerSettle},
			{data: paste("/a/one.png"), settle: pasteSettle},
			{data: paste("/b/two.jpg"), settle: composerSettle},
			{data: []byte("\r")},
		})
	})

	t.Run("two missing markers append both images after the text", func(t *testing.T) {
		// Both markers absent: text run, then both images appended in order, with the
		// interior text→image and image→image boundaries both pasteSettle.
		assertSteps(t, buildSendSteps("just text", []string{"/a/one.png", "/b/two.jpg"}), []sendStep{
			{data: clearData, settle: composerSettle},
			{data: paste("just text"), settle: pasteSettle},
			{data: paste("/a/one.png"), settle: pasteSettle},
			{data: paste("/b/two.jpg"), settle: composerSettle},
			{data: []byte("\r")},
		})
	})
}

// TestBracketedPasteStripsStrayMarkers proves a stray paste marker embedded in
// content is removed so it can't reframe the paste: a stray END would close the
// paste early (tail read as raw keystrokes), and a stray START resets Claude's
// paste buffer (dropping everything before it). Applies to both image paths and
// text, which share bracketedPaste.
func TestBracketedPasteStripsStrayMarkers(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"stray end terminator", "before\x1b[201~after", "\x1b[200~beforeafter\x1b[201~"},
		{"stray start marker", "before\x1b[200~after", "\x1b[200~beforeafter\x1b[201~"},
		{"both markers", "a\x1b[200~b\x1b[201~c", "\x1b[200~abc\x1b[201~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(bracketedPaste(tc.in)); got != tc.want {
				t.Errorf("bracketedPaste(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAttachmentPaths proves the path extractor returns each attachment's Path in
// order and fails loudly when one is missing — the app layer must resolve a path
// for this provider, and a missing one would otherwise silently drop the image.
func TestAttachmentPaths(t *testing.T) {
	got, err := attachmentPaths([]provider.ImageAttachment{
		{ID: "a", Path: "/x/a.png"},
		{ID: "b", Path: "/y/b.jpg"},
	})
	if err != nil {
		t.Fatalf("attachmentPaths: %v", err)
	}
	if want := []string{"/x/a.png", "/y/b.jpg"}; !slices.Equal(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}

	if _, err := attachmentPaths([]provider.ImageAttachment{{ID: "noPath"}}); err == nil {
		t.Error("attachmentPaths with a missing Path should error")
	}

	// A control byte in a path is refused rather than pasted: a newline would
	// split one path into two in Claude's parser, and a paste terminator / ESC
	// could break out of the bracketed paste into raw keystrokes.
	for _, bad := range []string{"/x/a\n/y/b.png", "/x/" + bracketedPasteEnd + ".png", "/x/\x7f.png"} {
		if _, err := attachmentPaths([]provider.ImageAttachment{{ID: "ctrl", Path: bad}}); err == nil {
			t.Errorf("attachmentPaths with a control byte in %q should error", bad)
		}
	}

	if got, err := attachmentPaths(nil); err != nil || got != nil {
		t.Errorf("attachmentPaths(nil) = (%v, %v), want (nil, nil)", got, err)
	}
}

// assertSteps compares an ordered send sequence (data + the gap after each block)
// against the expected contract.
func assertSteps(t *testing.T, got, want []sendStep) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d steps, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i].data, want[i].data) {
			t.Errorf("step %d data = %q, want %q", i, got[i].data, want[i].data)
		}
		if got[i].settle != want[i].settle {
			t.Errorf("step %d settle = %v, want %v", i, got[i].settle, want[i].settle)
		}
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
