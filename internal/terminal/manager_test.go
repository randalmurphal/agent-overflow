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
	if err := m.Refresh("x"); !errors.Is(err, ErrTerminalNotFound) {
		t.Fatalf("Refresh: expected ErrTerminalNotFound, got %v", err)
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

func TestManagerMoveThreadRekeysLiveSessions(t *testing.T) {
	outputs := make(chan string, 8)
	m := NewManager(
		func(threadID, terminalID string, seq uint64, data []byte) {
			outputs <- threadID + ":" + string(data)
		},
		nil,
	)
	summary, err := m.Open("draft:thread", SessionOptions{
		Shell: "/bin/sh",
		Args:  []string{"-c", "cat"},
		Cwd:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close(summary.TerminalID) })

	moved, err := m.MoveThread("draft:thread", "thread-real")
	if err != nil {
		t.Fatalf("MoveThread: %v", err)
	}
	if len(moved) != 1 {
		t.Fatalf("moved summaries = %d, want 1", len(moved))
	}
	if moved[0].ThreadID != "thread-real" {
		t.Fatalf("moved ThreadID = %q, want thread-real", moved[0].ThreadID)
	}
	if len(m.List("draft:thread")) != 0 {
		t.Fatalf("old thread key should have no sessions after move")
	}
	if list := m.List("thread-real"); len(list) != 1 || list[0].TerminalID != summary.TerminalID {
		t.Fatalf("new thread key list = %+v, want moved terminal", list)
	}

	if err := m.Write(summary.TerminalID, []byte("after-move\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case output := <-outputs:
			if strings.Contains(output, "after-move") {
				if !strings.HasPrefix(output, "thread-real:") {
					t.Fatalf("output emitted under old key: %q", output)
				}
				return
			}
		case <-deadline:
			t.Fatal("did not observe output after terminal move")
		}
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

// TestManagerRefreshSerializesWithConcurrentResize is the regression guard for
// the Refresh-vs-Resize lost-update race. Refresh shrinks the PTY by one row,
// pauses ~40ms, then restores the size it snapshotted. If a Resize lands inside
// that pause without serialization, Refresh's restore overwrites the new size
// with the stale one — the PTY ends mis-sized against the xterm grid, recreating
// the exact desync the refresh feature exists to clear.
//
// The shell traps WINCH and reports `stty size` per signal. We start a Refresh,
// wait for its shrink ("23 80") so it is provably mid-pause, then Resize to
// 30x120. With resizeMu the Resize blocks until Refresh's restore completes, so
// the PTY settles at the resized 30x120. Without it, the restore lands last and
// rolls the child back to 24x80 — which fails the settle assertion.
func TestManagerRefreshSerializesWithConcurrentResize(t *testing.T) {
	var mu sync.Mutex
	var buf strings.Builder
	captured := func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
	onOutput := func(threadID, terminalID string, seq uint64, data []byte) {
		mu.Lock()
		buf.Write(data)
		mu.Unlock()
	}

	m := NewManager(onOutput, nil)
	summary, err := m.Open("thread-refresh-race", SessionOptions{
		Shell: "/bin/sh",
		Args:  []string{"-c", "trap 'stty size' WINCH; echo READY; while :; do sleep 0.02; done"},
		Cwd:   t.TempDir(),
		Rows:  24,
		Cols:  80,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := summary.TerminalID
	t.Cleanup(func() { _ = m.Close(id) })

	// Wait for the trap to be installed before nudging (same startup-race guard
	// as TestProcessRefreshNudgesAndRestores).
	waitForOutput(t, captured, "READY", 3*time.Second)

	done := make(chan error, 1)
	go func() { done <- m.Refresh(id) }()

	// Block until the shrink nudge is observed: Refresh has snapshotted the
	// pre-resize size (24) and is now inside its restore pause. Issuing Resize
	// here lands it squarely in the shrink→restore window.
	waitForOutput(t, captured, "23 80", 3*time.Second)

	if err := m.Resize(id, 30, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// The PTY must settle at the resized size, not roll back to the snapshot.
	mustSettleAtSize(t, captured, "30 120", 2*time.Second)

	list := m.List("thread-refresh-race")
	if len(list) != 1 {
		t.Fatalf("expected 1 session, got %d", len(list))
	}
	if list[0].Rows != 30 || list[0].Cols != 120 {
		t.Errorf("cached size after resize: %dx%d, want 30x120", list[0].Rows, list[0].Cols)
	}
}

// waitForOutput polls the captured PTY output until it contains needle.
func waitForOutput(t *testing.T, captured func() string, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(captured(), needle) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("did not observe %q within %v; got %q", needle, timeout, captured())
}

// mustSettleAtSize waits until the last `stty size` line the child reported is
// target and stays there. The confirm re-check guards against the lost-update
// bug's transient: without serialization the resized size appears briefly before
// Refresh's restore rolls it back, so a single check could pass spuriously.
func mustSettleAtSize(t *testing.T, captured func() string, target string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if lastSizeLine(captured()) == target {
			time.Sleep(100 * time.Millisecond)
			if lastSizeLine(captured()) == target {
				return
			}
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("PTY did not settle at %q; last size line = %q\nfull output: %q",
		target, lastSizeLine(captured()), captured())
}

// lastSizeLine returns the last "<rows> <cols>" line in s (a child `stty size`
// report), or "" if none. PTY output terminates lines with \r\n.
func lastSizeLine(s string) string {
	last := ""
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimRight(raw, "\r")
		if isSizeLine(line) {
			last = line
		}
	}
	return last
}

// isSizeLine reports whether line is exactly two space-separated integer fields.
func isSizeLine(line string) bool {
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return false
	}
	for _, f := range fields {
		for _, r := range f {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}
