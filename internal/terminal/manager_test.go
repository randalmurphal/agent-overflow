package terminal

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagerOpenRequiresThreadAndCwd(t *testing.T) {
	m := NewManager(nil, nil)
	if _, err := m.Open("", SessionOptions{Cwd: t.TempDir()}); err == nil {
		t.Error("expected error for empty thread ID")
	}
	if _, err := m.Open("thread", SessionOptions{}); err == nil {
		t.Error("expected error for empty cwd")
	}
}

func TestManagerOpenAndOutput(t *testing.T) {
	var mu sync.Mutex
	var seen strings.Builder
	exitCh := make(chan ExitStatus, 1)

	onOutput := func(threadID, terminalID string, seq uint64, data []byte) {
		mu.Lock()
		seen.Write(data)
		mu.Unlock()
	}
	onExit := func(threadID, terminalID string, status ExitStatus) {
		exitCh <- status
	}

	m := NewManager(onOutput, onExit)
	summary, err := m.Open("thread-x", SessionOptions{
		Shell: "/bin/sh",
		Args:  []string{"-c", "printf marker; exit 0"},
		Cwd:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if summary.TerminalID == "" {
		t.Fatal("expected a terminal ID")
	}

	select {
	case <-exitCh:
	case <-time.After(3 * time.Second):
		t.Fatal("exit callback did not fire")
	}

	mu.Lock()
	got := seen.String()
	mu.Unlock()
	if !strings.Contains(got, "marker") {
		t.Fatalf("did not observe marker output, got %q", got)
	}

	// Manager must have cleaned up the session on exit.
	list := m.List("thread-x")
	if len(list) != 0 {
		t.Fatalf("expected list to be empty after exit, got %d", len(list))
	}
}

func TestManagerWriteResizeRoundTrip(t *testing.T) {
	m := NewManager(nil, nil)
	summary, err := m.Open("thread-w", SessionOptions{
		Shell: "/bin/sh",
		Args:  []string{"-c", "cat"},
		Cwd:   t.TempDir(),
		Rows:  24,
		Cols:  80,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close(summary.TerminalID) })

	if err := m.Write(summary.TerminalID, []byte("ping\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := m.Resize(summary.TerminalID, 30, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	// List must now reflect the new size.
	list := m.List("thread-w")
	if len(list) != 1 {
		t.Fatalf("expected 1 session, got %d", len(list))
	}
	if list[0].Rows != 30 || list[0].Cols != 120 {
		t.Errorf("size after resize: %dx%d", list[0].Rows, list[0].Cols)
	}
}

func TestManagerReplayReturnsBuffer(t *testing.T) {
	m := NewManager(nil, nil)
	summary, err := m.Open("thread-r", SessionOptions{
		Shell: "/bin/sh",
		Args:  []string{"-c", "printf hello; sleep 0.1"},
		Cwd:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close(summary.TerminalID) })

	// Wait a bit for output to accumulate.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		replay, err := m.Replay(summary.TerminalID)
		if err == nil && strings.Contains(string(replay), "hello") {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("replay did not contain expected content within timeout")
}

func TestManagerCloseRemovesSession(t *testing.T) {
	m := NewManager(nil, nil)
	summary, err := m.Open("thread-c", SessionOptions{
		Shell: "/bin/sh",
		Args:  []string{"-c", "sleep 10"},
		Cwd:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := m.Close(summary.TerminalID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if list := m.List("thread-c"); len(list) != 0 {
		t.Fatalf("expected no sessions after close, got %d", len(list))
	}
	// Second close is a no-op.
	if err := m.Close(summary.TerminalID); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestManagerCloseMissingIsNoError(t *testing.T) {
	m := NewManager(nil, nil)
	if err := m.Close("does-not-exist"); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestManagerMissingTerminalErrors(t *testing.T) {
	m := NewManager(nil, nil)
	if err := m.Write("x", []byte("hi")); !errors.Is(err, ErrTerminalNotFound) {
		t.Fatalf("Write: expected ErrTerminalNotFound, got %v", err)
	}
	if err := m.Resize("x", 1, 1); !errors.Is(err, ErrTerminalNotFound) {
		t.Fatalf("Resize: expected ErrTerminalNotFound, got %v", err)
	}
	if _, err := m.Replay("x"); !errors.Is(err, ErrTerminalNotFound) {
		t.Fatalf("Replay: expected ErrTerminalNotFound, got %v", err)
	}
	if _, err := m.Restart("x"); !errors.Is(err, ErrTerminalNotFound) {
		t.Fatalf("Restart: expected ErrTerminalNotFound, got %v", err)
	}
}

func TestManagerCloseThreadClosesAll(t *testing.T) {
	m := NewManager(nil, nil)
	for i := 0; i < 3; i++ {
		if _, err := m.Open("thread-multi", SessionOptions{
			Shell: "/bin/sh",
			Args:  []string{"-c", "sleep 10"},
			Cwd:   t.TempDir(),
		}); err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
	}
	if len(m.List("thread-multi")) != 3 {
		t.Fatalf("expected 3 sessions before CloseThread")
	}
	if err := m.CloseThread("thread-multi"); err != nil {
		t.Fatalf("CloseThread: %v", err)
	}
	if len(m.List("thread-multi")) != 0 {
		t.Fatalf("expected 0 sessions after CloseThread")
	}
}

func TestManagerRestartReplacesSession(t *testing.T) {
	m := NewManager(nil, nil)
	summary, err := m.Open("thread-x", SessionOptions{
		Shell: "/bin/sh",
		Args:  []string{"-c", "sleep 10"},
		Cwd:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.CloseThread("thread-x") })

	newSummary, err := m.Restart(summary.TerminalID)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if newSummary.TerminalID == summary.TerminalID {
		t.Fatal("restart must produce a new terminal ID")
	}
	list := m.List("thread-x")
	if len(list) != 1 {
		t.Fatalf("expected 1 session after restart, got %d", len(list))
	}
	if list[0].TerminalID != newSummary.TerminalID {
		t.Fatalf("list ID mismatch: %s vs %s", list[0].TerminalID, newSummary.TerminalID)
	}
}

func TestManagerShutdownClosesEverything(t *testing.T) {
	m := NewManager(nil, nil)
	for _, thread := range []string{"a", "b"} {
		if _, err := m.Open(thread, SessionOptions{
			Shell: "/bin/sh",
			Args:  []string{"-c", "sleep 10"},
			Cwd:   t.TempDir(),
		}); err != nil {
			t.Fatalf("Open %s: %v", thread, err)
		}
	}
	if err := m.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if len(m.List("a")) != 0 || len(m.List("b")) != 0 {
		t.Fatalf("expected no sessions after shutdown")
	}
}
