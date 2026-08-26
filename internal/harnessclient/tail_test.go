package harnessclient

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// follower drives FollowFile over one file with a short poll interval and
// hands its emissions back on a channel. It blocks until the follower has
// taken its starting offset, so a test can append knowing the follow is
// already watching rather than sleeping and hoping.
type follower struct {
	path  string
	lines chan string
	done  chan error
}

func startFollowing(t *testing.T, path string) *follower {
	t.Helper()
	prevInterval := followPollInterval
	followPollInterval = 2 * time.Millisecond
	started := make(chan struct{})
	followStartedHook = func() { close(started) }
	t.Cleanup(func() {
		followPollInterval = prevInterval
		followStartedHook = nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	f := &follower{path: path, lines: make(chan string, 64), done: make(chan error, 1)}
	go func() {
		f.done <- FollowFile(ctx, path, func(line string) { f.lines <- line })
	}()
	<-started
	t.Cleanup(func() {
		cancel()
		if err := <-f.done; err != nil {
			t.Errorf("FollowFile: %v", err)
		}
	})
	return f
}

func (f *follower) append(t *testing.T, text string) {
	t.Helper()
	file, err := os.OpenFile(f.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func (f *follower) next(t *testing.T) string {
	t.Helper()
	select {
	case line := <-f.lines:
		return line
	case <-time.After(10 * time.Second):
		t.Fatal("the follower emitted nothing")
		return ""
	}
}

// quiet proves a negative, which needs a bound: several poll intervals
// with nothing emitted.
func (f *follower) quiet(t *testing.T) {
	t.Helper()
	select {
	case line := <-f.lines:
		t.Fatalf("follower emitted %q, want nothing yet", line)
	case <-time.After(50 * followPollInterval):
	}
}

func TestFollowFileEmitsOnlyCompleteAppendedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "follow.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f := startFollowing(t, path)

	f.append(t, "after\npartia")
	// The pre-existing line was written before the follow started; a
	// tail -f does not replay history.
	if line := f.next(t); line != "after" {
		t.Fatalf("first followed line = %q, want the one complete appended line", line)
	}
	f.quiet(t)

	// The fragment completes on a later append and arrives whole, once.
	f.append(t, "l\n")
	if line := f.next(t); line != "partial" {
		t.Fatalf("completed line = %q, want %q", line, "partial")
	}
	f.quiet(t)
}

// A partial line longer than one poll's read cap is the case the old
// re-read-from-the-fragment loop paid for over and over. It must still
// arrive exactly once, whole, and only after its newline.
func TestFollowFileCarriesALongPartialLineAcrossPolls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "follow.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f := startFollowing(t, path)

	// Written in chunks, each smaller than the read cap and each landing
	// on its own poll, so the fragment genuinely spans several reads.
	const chunk = 64 * 1024
	const chunks = 24 // 1.5 MiB, past followReadCap
	body := strings.Repeat("x", chunk)
	for i := 0; i < chunks; i++ {
		f.append(t, body)
	}
	f.quiet(t)
	f.append(t, "END\n")

	line := f.next(t)
	if len(line) != chunk*chunks+len("END") {
		t.Fatalf("line is %d bytes, want %d", len(line), chunk*chunks+len("END"))
	}
	if !strings.HasSuffix(line, "END") || strings.Trim(line[:len(line)-3], "x") != "" {
		t.Fatal("the reassembled line is not the bytes that were written")
	}
	f.quiet(t)
}

// A rotation drops the fragment with the file it belonged to: its newline
// is never coming, and carrying it would prepend the dead file's tail to
// the new file's first line.
func TestFollowFileDropsTheFragmentOnRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "follow.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f := startFollowing(t, path)

	f.append(t, "half a line")
	f.quiet(t)
	if err := os.WriteFile(path, []byte("fresh\n"), 0o600); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if line := f.next(t); line != "fresh" {
		t.Fatalf("after rotation = %q, want %q", line, "fresh")
	}
}
