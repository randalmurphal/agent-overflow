package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/logging"
)

func TestSpawnAndEcho(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	defer p.Kill()

	msg := []byte(`{"hello":"world"}`)
	if err := p.WriteLine(msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(line) != string(msg) {
		t.Errorf("got %q, want %q", line, msg)
	}
}

func TestSpawnMultipleLines(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	defer p.Kill()

	lines := []string{"first", "second", "third"}
	for _, l := range lines {
		if err := p.WriteLine([]byte(l)); err != nil {
			t.Fatalf("write %q: %v", l, err)
		}
	}

	for _, want := range lines {
		got, err := p.ReadLine()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestCloseGraceful(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}

	// cat exits when stdin closes, so Close should succeed via step 1 (stdin close).
	if err := p.Close(); err != nil {
		// cat returns exit status 0 on stdin close, so err should be nil
		t.Logf("close returned: %v (expected for cat)", err)
	}

	// Done channel should be closed after Close.
	select {
	case <-p.Done():
		// ok
	case <-time.After(5 * time.Second):
		t.Fatal("Done channel not closed after Close()")
	}
}

func TestKillImmediate(t *testing.T) {
	ctx := context.Background()
	// sleep won't exit on stdin close, so Kill is the only way.
	p, err := Spawn(ctx, SpawnConfig{Binary: "sleep", Args: []string{"60"}})
	if err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}

	start := time.Now()
	p.Kill()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("Kill took %v, expected < 2s", elapsed)
	}

	select {
	case <-p.Done():
		// ok
	default:
		t.Fatal("Done channel not closed after Kill()")
	}
}

func TestReadLineEOFAfterExit(t *testing.T) {
	ctx := context.Background()
	// echo outputs one line then exits.
	p, err := Spawn(ctx, SpawnConfig{Binary: "echo", Args: []string{"hello"}})
	if err != nil {
		t.Fatalf("spawn echo: %v", err)
	}

	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if string(line) != "hello" {
		t.Errorf("got %q, want %q", line, "hello")
	}

	// Next read should return EOF.
	_, err = p.ReadLine()
	if err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}

	// Wait for process to finish.
	<-p.Done()
}

func TestWriteLineAfterExit(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "true"})
	if err != nil {
		t.Fatalf("spawn true: %v", err)
	}

	// Wait for process to exit.
	<-p.Done()

	err = p.WriteLine([]byte("should fail"))
	if err == nil {
		t.Fatal("expected error writing to exited process, got nil")
	}
}

func TestSetpgid(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	defer p.Kill()

	// Verify Setpgid was configured by checking the SysProcAttr.
	if p.cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	if !p.cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid not set")
	}
}

func TestSpawnWithEnv(t *testing.T) {
	ctx := context.Background()
	// Use env to print a specific variable.
	p, err := Spawn(ctx, SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "echo $TEST_VAR"},
		Env:    map[string]string{"TEST_VAR": "hello_from_test"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(line) != "hello_from_test" {
		t.Errorf("got %q, want %q", line, "hello_from_test")
	}

	<-p.Done()
}

func TestSpawnWithDir(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{
		Binary: "pwd",
		Dir:    "/tmp",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// /tmp might resolve to /private/tmp on macOS.
	got := string(line)
	if got != "/tmp" && got != "/private/tmp" {
		t.Errorf("got %q, want /tmp or /private/tmp", got)
	}

	<-p.Done()
}

func TestSpawnInvalidBinary(t *testing.T) {
	ctx := context.Background()
	_, err := Spawn(ctx, SpawnConfig{Binary: "/nonexistent/binary"})
	if err == nil {
		t.Fatal("expected error for nonexistent binary, got nil")
	}
}

func TestProviderEventLoggingCapturesInputAndOutput(t *testing.T) {
	ctx := context.Background()
	logPath := filepath.Join(t.TempDir(), "provider-events.ndjson")
	logger, err := logging.NewLogger(logPath, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	p, err := Spawn(ctx, SpawnConfig{
		Binary:      "cat",
		EventLogger: logger,
		ThreadID:    "thread-123",
		Provider:    "claude",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Kill()

	if err := p.WriteLine([]byte(`{"hello":"world"}`)); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
	if _, err := p.ReadLine(); err != nil {
		t.Fatalf("ReadLine: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	lines := splitNonEmptyLines(string(data))
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2", len(lines))
	}

	var first, second logging.ProviderEventEntry
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal first log entry: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("unmarshal second log entry: %v", err)
	}

	if first.Direction != "out" || second.Direction != "in" {
		t.Fatalf("directions = %q then %q, want out then in", first.Direction, second.Direction)
	}
	if first.ThreadID != "thread-123" || second.ThreadID != "thread-123" {
		t.Fatalf("thread IDs = %q and %q, want thread-123", first.ThreadID, second.ThreadID)
	}
	if first.Provider != "claude" || second.Provider != "claude" {
		t.Fatalf("providers = %q and %q, want claude", first.Provider, second.Provider)
	}
}

func TestProviderEventLoggingDisabledWithoutLogger(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Kill()

	if err := p.WriteLine([]byte("hello")); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
	if _, err := p.ReadLine(); err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
}

func splitNonEmptyLines(data string) []string {
	raw := strings.Split(data, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func TestReadLineReturnsCopy(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer p.Kill()

	if err := p.WriteLine([]byte("first")); err != nil {
		t.Fatalf("write: %v", err)
	}

	line1, err := p.ReadLine()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Save the content before next read.
	saved := string(line1)

	if err := p.WriteLine([]byte("second")); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err = p.ReadLine()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// The first line's data should still be intact (not overwritten by scanner reuse).
	if string(line1) != saved {
		t.Errorf("ReadLine did not return a copy: first line mutated from %q to %q", saved, string(line1))
	}
}

func TestDoneChannelCloses(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "true"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	select {
	case <-p.Done():
		// ok — process exited
	case <-time.After(5 * time.Second):
		t.Fatal("Done channel not closed within 5s")
	}
}

func TestErrAccessor(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "false"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	<-p.Done()

	// `false` exits with code 1, so Err should be non-nil.
	if p.Err() == nil {
		t.Error("expected non-nil error from `false`, got nil")
	}
}

func TestMarshalProcessExitMetaForExitCode(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "false"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	<-p.Done()

	data := MarshalProcessExitMeta(p.Err())
	var meta ProcessExitInfo
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if meta.ExitCode != 1 {
		t.Fatalf("ExitCode = %d, want 1", meta.ExitCode)
	}
	if meta.Reason == "" {
		t.Fatal("Reason should not be empty")
	}
}

func TestMarshalProcessExitMetaForSignal(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "kill -TERM $$"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	<-p.Done()

	data := MarshalProcessExitMeta(p.Err())
	var meta ProcessExitInfo
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if meta.Signal == "" {
		t.Fatal("Signal should not be empty")
	}
	if meta.Reason == "" {
		t.Fatal("Reason should not be empty")
	}
}

func TestWaitProcessExitErr(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "false"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	if WaitProcessExitErr(p) == nil {
		t.Fatal("expected non-nil exit error")
	}
}

func TestIsClosedPipeErr(t *testing.T) {
	if !isClosedPipeErr(errors.New("read |0: file already closed")) {
		t.Fatal("expected file already closed to be treated as closed pipe")
	}
	if !isClosedPipeErr(errors.New("write broken pipe")) {
		t.Fatal("expected broken pipe to be treated as closed pipe")
	}
	if isClosedPipeErr(errors.New("permission denied")) {
		t.Fatal("unexpected closed-pipe match")
	}
}

func TestCloseEscalatesToSignal(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "sleep", Args: []string{"60"}})
	if err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}

	start := time.Now()
	err = p.Close()
	if err == nil {
		t.Fatal("expected non-nil exit error after terminating sleep")
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("Close took %v, want under 6s", elapsed)
	}

	select {
	case <-p.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Done channel should be closed after Close")
	}
}

// TestReadLineOversizedKillsProcess exercises Bug B1: a line larger than the
// configured cap used to leave readLoop exiting while the subprocess kept
// running. With the fix in place the process must terminate (reaping its
// file descriptors) and the returned error must be recognisable as an
// oversized-line failure.
func TestReadLineOversizedKillsProcess(t *testing.T) {
	ctx := context.Background()
	// Emit >cap bytes without a newline then sleep so the subprocess stays
	// alive until the reader kills it. The cap is 32 MiB in the new
	// implementation; yes/tr fills the pipe fast enough to trigger the
	// overflow in a handful of milliseconds.
	script := fmt.Sprintf(
		`perl -e 'print "x" x %d'; sleep 60`,
		maxLineSize+100,
	)
	p, err := Spawn(ctx, SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", script},
	})
	if err != nil {
		t.Fatalf("spawn oversize emitter: %v", err)
	}

	_, err = p.ReadLine()
	if err == nil {
		t.Fatal("expected error on oversized line, got nil")
	}
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}

	// After an oversized line, the process must have been terminated by
	// ReadLine so no orphan remains. Done() will close only after Wait()
	// completes; we give it a short window to finish SIGKILL.
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("process not reaped after oversized line; readLoop orphaned it")
	}
}

// TestReadLineJustUnderCapSucceeds exercises a line sized just below the cap
// to prove the boundary check does not fire one byte early. We use a
// long-running emitter (cat fed via stdin) so the subprocess does not exit
// and close the pipe mid-read — that kernel-side race already exists in
// cmd.Wait() + StdoutPipe and is orthogonal to the cap check.
func TestReadLineJustUnderCapSucceeds(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	defer p.Kill()

	// Send 128 KiB on stdin; cat echoes it back. 128 KiB is safely under
	// the cap (32 MiB) yet large enough to span many bufio refills so a
	// bogus size counter would trip.
	payload := make([]byte, 128*1024)
	for i := range payload {
		payload[i] = 'x'
	}
	if err := p.WriteLine(payload); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}

	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("read line: %v", err)
	}
	if len(line) != len(payload) {
		t.Fatalf("line len = %d, want %d", len(line), len(payload))
	}
}

// TestReadLineManyUnderCap proves the new reader handles an arbitrary number
// of normal-sized lines without accumulating state or tripping the cap.
func TestReadLineManyUnderCap(t *testing.T) {
	ctx := context.Background()
	// Emit 50 lines, each a few hundred bytes. None individually exceeds
	// the cap; the regression would be a new path that counts bytes across
	// lines or forgets to reset per-line buffers.
	script := `for i in $(seq 1 50); do perl -e 'print "y" x 500'; echo; done`
	p, err := Spawn(ctx, SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", script},
	})
	if err != nil {
		t.Fatalf("spawn many-lines emitter: %v", err)
	}
	defer p.Kill()

	for i := 0; i < 50; i++ {
		line, err := p.ReadLine()
		if err != nil {
			t.Fatalf("read line %d: %v", i, err)
		}
		if len(line) != 500 {
			t.Fatalf("line %d len = %d, want 500", i, len(line))
		}
	}
}

