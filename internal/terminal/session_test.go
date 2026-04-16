package terminal

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionEmitsOutputAndExit(t *testing.T) {
	var outputs []string
	var exit ExitStatus
	var mu sync.Mutex
	done := make(chan struct{})

	onOutput := func(id string, seq uint64, data []byte) {
		mu.Lock()
		defer mu.Unlock()
		outputs = append(outputs, string(data))
	}
	onExit := func(id string, status ExitStatus) {
		mu.Lock()
		exit = status
		mu.Unlock()
		close(done)
	}

	sess, err := newSession("t1", "thread-a", SessionOptions{
		Shell: "/bin/sh",
		Args:  []string{"-c", "echo hello; exit 3"},
		Cwd:   t.TempDir(),
	}, onOutput, onExit)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Kill() })

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not exit")
	}

	mu.Lock()
	joined := strings.Join(outputs, "")
	mu.Unlock()

	if !strings.Contains(joined, "hello") {
		t.Fatalf("did not see hello in output, got %q", joined)
	}
	if exit.Code != 3 {
		t.Fatalf("exit code = %d, want 3", exit.Code)
	}

	// Replay must include the captured output.
	replay := string(sess.Replay())
	if !strings.Contains(replay, "hello") {
		t.Fatalf("replay missing hello: %q", replay)
	}
}

func TestSessionSummaryFields(t *testing.T) {
	sess, err := newSession("t-sum", "thread-sum", SessionOptions{
		Shell: "/bin/sh",
		Args:  []string{"-c", "sleep 0.2"},
		Cwd:   t.TempDir(),
		Rows:  30,
		Cols:  100,
	}, nil, nil)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Kill() })

	s := sess.Summary()
	if s.TerminalID != "t-sum" {
		t.Errorf("TerminalID = %q", s.TerminalID)
	}
	if s.ThreadID != "thread-sum" {
		t.Errorf("ThreadID = %q", s.ThreadID)
	}
	if s.Rows != 30 || s.Cols != 100 {
		t.Errorf("size %dx%d", s.Rows, s.Cols)
	}
	if s.PID <= 0 {
		t.Errorf("PID = %d, expected > 0", s.PID)
	}
	if !s.Running {
		t.Error("expected Running true before process exits")
	}

	// After natural exit, Summary.Running must flip to false.
	select {
	case <-sess.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session did not exit")
	}
	// Give pump goroutine a tick to update state.
	time.Sleep(50 * time.Millisecond)
	s2 := sess.Summary()
	if s2.Running {
		t.Error("expected Running false after exit")
	}
}

func TestSessionResizeUpdatesSummary(t *testing.T) {
	sess, err := newSession("t-resize", "thread-r", SessionOptions{
		Shell: "/bin/sh",
		Args:  []string{"-c", "sleep 1"},
		Cwd:   t.TempDir(),
		Rows:  24,
		Cols:  80,
	}, nil, nil)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Kill() })

	if err := sess.Resize(40, 160); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	s := sess.Summary()
	if s.Rows != 40 || s.Cols != 160 {
		t.Errorf("after resize: %dx%d, want 40x160", s.Rows, s.Cols)
	}
}

// TestSessionReplayBounded writes a lot of bytes through the session so that
// replay never exceeds maxReplayBytes.
func TestSessionReplayBounded(t *testing.T) {
	// Produce ~512 KiB of output so we exceed maxReplayBytes (256 KiB).
	script := fmt.Sprintf("for i in $(seq 1 %d); do printf '%%s\\n' AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA; done", 8000)
	sess, err := newSession("t-big", "thread-big", SessionOptions{
		Shell: "/bin/sh",
		Args:  []string{"-c", script},
		Cwd:   t.TempDir(),
	}, nil, nil)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Kill() })

	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("session did not exit")
	}
	// Give pump goroutine a tick to drain final output.
	time.Sleep(50 * time.Millisecond)

	replay := sess.Replay()
	if len(replay) > maxReplayBytes {
		t.Fatalf("replay size %d exceeds cap %d", len(replay), maxReplayBytes)
	}
	if len(replay) == 0 {
		t.Fatal("expected some replay content")
	}
}
