package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
)

// heldTextRecorder records what a session emits as labels. The text delta
// "held" blocks inside onEvent until release, as an emit blocked on a full
// event queue does, and is recorded once released.
type heldTextRecorder struct {
	mu          sync.Mutex
	got         []string
	notify      chan struct{}
	releaseCh   chan struct{}
	releaseOnce sync.Once
}

func newHeldTextRecorder() *heldTextRecorder {
	return &heldTextRecorder{notify: make(chan struct{}, 1), releaseCh: make(chan struct{})}
}

func (r *heldTextRecorder) record(e provider.ProviderEvent) {
	label := string(e.Kind)
	switch e.Kind {
	case provider.EventTextDelta:
		if e.Content == "held" {
			<-r.releaseCh
		}
		label = "text:" + e.Content
		if len(e.Content) > 16 { // the test's own words print whole
			label = fmt.Sprintf("text:%d bytes", len(e.Content))
		}
	case provider.EventSessionStatus:
		label += ":" + e.Content
	}
	r.mu.Lock()
	r.got = append(r.got, label)
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *heldTextRecorder) release() { r.releaseOnce.Do(func() { close(r.releaseCh) }) }

func (r *heldTextRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

func newOrderTestSession(rec *heldTextRecorder) *Session {
	s := &Session{
		flavor:               DefaultFlavor{Kind: provider.Agy},
		threadID:             "t-order",
		activeTurnID:         "turn-1",
		turnOpen:             true,
		inFlightPrompts:      1,
		readDone:             make(chan struct{}),
		startedTools:         make(map[string]NormalizedToolStart),
		trackedSubagents:     make(map[string]SubagentLaunchContext),
		pendingPerms:         make(map[string]pendingPermission),
		pendingToolQuestions: make(map[string]pendingToolQuestion),
	}
	s.onEvent = rec.record
	return s
}

// settle lets the fake clock run past any timer the session armed (a
// coalesce window is 35 ms) and returns once every other goroutine in the
// bubble is blocked.
func settle() {
	time.Sleep(2 * time.Second)
	synctest.Wait()
}

func chunkUpdate(text string) SessionUpdate {
	content, _ := json.Marshal(ContentBlock{Type: "text", Text: text})
	return SessionUpdate{SessionUpdate: "agent_message_chunk", Content: content}
}

// The reader publishes a chunk before it reads the next frame, so nothing
// it reads after the chunk can be published first, however long the chunk's
// publication waits. The CI failure was the first case: the coalesce timer
// took the chunk and its emit waited, while the reader read usage and the
// prompt's result and the turn's completion went out first; triage settled
// the turn, and the late text opened a streaming row nothing settled.
//
// Each case runs the reader's sequence on one goroutine with the chunk held
// in its emit, and lets the fake clock run until every goroutine is
// blocked: nothing may have been published while the chunk is held.
func TestReaderPublishesAChunkBeforeWhatItReadsNext(t *testing.T) {
	cases := []struct {
		name string
		// next is what the reader reads after the chunk.
		next func(s *Session)
		want []string
	}{
		{
			name: "usage and the prompt's result",
			next: func(s *Session) {
				s.handleUpdate(SessionUpdate{SessionUpdate: "usage_update", Used: 120, Size: 1000})
				// The reader hands the result to the prompt's waiter.
				done := make(chan struct{})
				go func() {
					defer close(done)
					s.finishPrompt(json.RawMessage(`{"stopReason":"end_turn"}`), nil, nil, nil)
				}()
				<-done
			},
			want: []string{"text:held", "token_usage", "turn_complete"},
		},
		{
			name: "a wake's completion by usage",
			next: func(s *Session) {
				s.mu.Lock()
				s.spontaneousTurnOpen = true
				s.inFlightPrompts = 0
				s.mu.Unlock()
				s.handleUpdate(SessionUpdate{SessionUpdate: "usage_update", Used: 120, Size: 1000})
			},
			want: []string{"text:held", "token_usage", "turn_complete"},
		},
		{
			name: "a tool call",
			next: func(s *Session) {
				s.handleUpdate(SessionUpdate{SessionUpdate: "tool_call", ToolCallID: "tool-1", Title: "ls", Kind: "execute", Status: "pending"})
			},
			want: []string{"text:held", "tool_start"},
		},
		{
			name: "the connection dying",
			next: func(s *Session) { s.onConnClosed(nil, false) },
			want: []string{"text:held", "session_status:error", "session_status:disconnected"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec := newHeldTextRecorder()
				t.Cleanup(rec.release)
				s := newOrderTestSession(rec)
				go func() {
					s.handleUpdate(chunkUpdate("held"))
					time.Sleep(time.Second)
					tc.next(s)
				}()
				settle()
				if got := rec.snapshot(); len(got) != 0 {
					t.Fatalf("emitted %q while the chunk was held", got)
				}
				rec.release()
				settle()
				if got := rec.snapshot(); !slices.Equal(got, tc.want) {
					t.Fatalf("emitted %q, want %q", got, tc.want)
				}
			})
		})
	}
}

// Closing the session while the reader publishes a chunk: the teardown
// waits for that chunk, so it is the session's last event.
func TestTeardownFollowsTheChunkBeingPublished(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newHeldTextRecorder()
		t.Cleanup(rec.release)
		s := newOrderTestSession(rec)
		go func() {
			s.handleUpdate(chunkUpdate("held"))
			time.Sleep(time.Second)
		}()
		settle()
		go s.emitTeardownOnce(nil, true)
		settle()
		if got := rec.snapshot(); len(got) != 0 {
			t.Fatalf("emitted %q while the chunk was held", got)
		}
		rec.release()
		settle()
		want := []string{"text:held", "session_status:disconnected"}
		if got := rec.snapshot(); !slices.Equal(got, want) {
			t.Fatalf("emitted %q, want %q", got, want)
		}
	})
}

// A chunk read once the session is closing, or after its teardown, is not
// published: nothing follows the teardown.
func TestAChunkReadWhileClosingIsNotPublished(t *testing.T) {
	cases := []struct {
		name string
		end  func(s *Session)
		want []string
	}{
		{name: "closing", end: func(s *Session) { s.closing.Store(true) }},
		{name: "after the teardown", end: func(s *Session) { s.onConnClosed(nil, false) }, want: []string{"session_status:error", "session_status:disconnected"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec := newHeldTextRecorder()
				s := newOrderTestSession(rec)
				tc.end(s)
				s.handleUpdate(chunkUpdate("late"))
				settle()
				if got := rec.snapshot(); !slices.Equal(got, tc.want) {
					t.Fatalf("emitted %q, want %q", got, tc.want)
				}
			})
		})
	}
}

// Through the real read loop on the fake server's default turn (thinking,
// a tool, text, usage, then the prompt's result): the text precedes the
// usage and the turn's completion.
func TestReadLoopPublishesTextBeforeTheTurnCompletes(t *testing.T) {
	t.Parallel()
	rec := newHeldTextRecorder()
	bin := writeFakeACPServer(t, standardFakeACPServer)
	conn, err := Dial(context.Background(), ConnConfig{
		Binary:       bin,
		WorkDir:      t.TempDir(),
		Reaper:       procutiltest.Reaper(),
		ProviderName: "acp",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	sess, err := conn.OpenPooled(context.Background(), "t-order", SessionConfig{WorkDir: t.TempDir()}, rec.record, nil)
	if err != nil {
		t.Fatalf("OpenPooled: %v", err)
	}
	if err := sess.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	deadline := time.After(10 * time.Second)
	var got []string
	for !slices.Contains(got, string(provider.EventTurnComplete)) {
		select {
		case <-rec.notify:
		case <-deadline:
			t.Fatalf("no turn completion; emitted %q", got)
		}
		got = rec.snapshot()
	}
	text := slices.IndexFunc(got, func(label string) bool { return strings.HasPrefix(label, "text:") })
	usage := slices.Index(got, string(provider.EventTokenUsage))
	done := slices.Index(got, string(provider.EventTurnComplete))
	if text < 0 || usage < 0 || !(text < usage && usage < done) {
		t.Fatalf("emitted %q, want the text, then the usage, then the turn's completion", got)
	}
}
