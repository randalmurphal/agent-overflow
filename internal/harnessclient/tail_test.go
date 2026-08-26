package harnessclient

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTailFileReturnsTheLastLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backend-stderr.log")
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	lines, err := TailFile(path, 3)
	if err != nil {
		t.Fatalf("TailFile: %v", err)
	}
	if len(lines) != 3 || lines[2] != "line 499" || lines[0] != "line 497" {
		t.Fatalf("tail = %v", lines)
	}
}

func TestTailFileReturnsAShortFileWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short.log")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	lines, err := TailFile(path, 50)
	if err != nil {
		t.Fatalf("TailFile: %v", err)
	}
	if len(lines) != 2 || lines[0] != "a" {
		t.Fatalf("tail = %v", lines)
	}
}

func TestTailFileAcrossTheChunkBoundaryDropsNoLineAndInventsNone(t *testing.T) {
	// The window doubles until it holds enough lines, and every window
	// past the first starts mid-line. Dropping that fragment is what
	// keeps a tail from printing half a log line as if it were whole.
	path := filepath.Join(t.TempDir(), "big.log")
	var b strings.Builder
	const lines = 4000
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "%04d %s\n", i, strings.Repeat("x", 60))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := TailFile(path, 2000)
	if err != nil {
		t.Fatalf("TailFile: %v", err)
	}
	if len(got) != 2000 {
		t.Fatalf("got %d lines, want 2000", len(got))
	}
	if !strings.HasPrefix(got[0], "2000 ") {
		t.Fatalf("first tailed line = %q, want line 2000 whole", got[0])
	}
}

func TestTailFileReportsAMissingFile(t *testing.T) {
	if _, err := TailFile(filepath.Join(t.TempDir(), "absent.log"), 5); err == nil {
		t.Fatal("a missing evidence file tailed cleanly; that is a finding, not an empty result")
	}
}

func TestFollowFileEmitsOnlyCompleteAppendedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "follow.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var seen []string
	done := make(chan error, 1)
	go func() {
		done <- FollowFile(ctx, path, func(line string) {
			mu.Lock()
			seen = append(seen, line)
			mu.Unlock()
		})
	}()

	// Let the follower take its starting offset before anything is
	// appended; it reads the file's end once, at start, and a test that
	// raced that read would be asserting on which goroutine ran first.
	time.Sleep(300 * time.Millisecond)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString("after\npartia"); err != nil {
		t.Fatalf("append: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Give the follower another poll interval to prove it does NOT emit
	// the fragment.
	time.Sleep(400 * time.Millisecond)
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "after" {
		t.Fatalf("followed lines = %v, want exactly the one complete appended line", got)
	}
	// The pre-existing line was before the follow started; a tail -f does
	// not replay history.
	for _, line := range got {
		if line == "before" {
			t.Fatal("follow replayed a line written before it started")
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("FollowFile: %v", err)
	}
}
