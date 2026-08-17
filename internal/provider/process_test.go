//go:build !windows

package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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

func TestBuildEnvironmentRemovesInheritedProviderHomeAndAppliesOverride(t *testing.T) {
	t.Setenv("CODEX_HOME", "/tmp/inherited-codex-home")
	t.Setenv("AGENT_OVERFLOW_ENV_TEST", "inherited")

	env := BuildEnvironment(
		map[string]string{"AGENT_OVERFLOW_ENV_TEST": "override"},
		"CODEX_HOME",
	)
	values := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}

	if _, exists := values["CODEX_HOME"]; exists {
		t.Fatal("CODEX_HOME remained in filtered environment")
	}
	if got := values["AGENT_OVERFLOW_ENV_TEST"]; got != "override" {
		t.Fatalf("AGENT_OVERFLOW_ENV_TEST = %q, want override", got)
	}

	inherited := FilterEnvironment([]string{}, "CODEX_HOME")
	if !slices.Contains(inherited, "AGENT_OVERFLOW_ENV_TEST=inherited") {
		t.Fatal("empty explicit environment did not preserve inherited values")
	}
}

// TestProviderEnvironmentAppliesTheAppImageScrub pins the integration with
// internal/appimage at both env entry points, including the one place the
// mount could re-enter after the scrub: BuildEnvironment's additive PATH
// merge, which must read the inherited half back off the SCRUBBED base.
// (The scrub's own semantics are covered in internal/appimage.)
func TestProviderEnvironmentAppliesTheAppImageScrub(t *testing.T) {
	const mount = "/tmp/.mount_agent1A2B3C"
	t.Setenv("APPDIR", mount)
	t.Setenv("APPIMAGE", "/home/dev/Apps/agent-overflow.AppImage")
	t.Setenv("ARGV0", "./agent-overflow.AppImage")
	t.Setenv("OWD", "/home/dev/projects")
	t.Setenv("PATH", mount+"/usr/bin:/usr/bin")

	assertScrubbed := func(t *testing.T, label string, env []string, wantPath string) {
		t.Helper()
		for _, marker := range []string{"APPDIR", "APPIMAGE", "ARGV0", "OWD"} {
			if slices.ContainsFunc(env, func(entry string) bool {
				key, _, ok := strings.Cut(entry, "=")
				return ok && key == marker
			}) {
				t.Errorf("%s retained the %s marker: %q", label, marker, env)
			}
		}
		if !slices.Contains(env, "PATH="+wantPath) {
			t.Errorf("%s PATH is not %q: %q", label, wantPath, env)
		}
	}

	assertScrubbed(t, "BuildEnvironment(nil)", BuildEnvironment(nil), "/usr/bin")
	assertScrubbed(t, "FilterEnvironment(nil)", FilterEnvironment(nil), "/usr/bin")
	assertScrubbed(t,
		"BuildEnvironment(PATH override)",
		BuildEnvironment(map[string]string{"PATH": "/opt/ao/bin"}),
		"/opt/ao/bin"+string(os.PathListSeparator)+"/usr/bin",
	)
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

func TestProviderEventLoggingUsesRedactor(t *testing.T) {
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
		EventLogRedactor: func(_ string, data []byte) []byte {
			return []byte(strings.ReplaceAll(string(data), "secret-token", "[redacted]"))
		},
		ThreadID: "thread-123",
		Provider: "codex",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Kill()

	if err := p.WriteLine([]byte(`{"token":"secret-token"}`)); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
	if _, err := p.ReadLine(); err != nil {
		t.Fatalf("ReadLine: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "secret-token") {
		t.Fatalf("provider event log contains unredacted secret: %s", data)
	}
	if count := strings.Count(string(data), "[redacted]"); count != 2 {
		t.Fatalf("redacted marker count = %d, want 2 in out/in log entries: %s", count, data)
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

	data := MarshalProcessExitMeta(p.Err(), "")
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

	data := MarshalProcessExitMeta(p.Err(), "")
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

func TestMarshalProcessExitMetaForNilError(t *testing.T) {
	data := MarshalProcessExitMeta(nil, "")
	var meta ProcessExitInfo
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta.Reason != "provider process exited unexpectedly" {
		t.Fatalf("Reason = %q, want generic fallback", meta.Reason)
	}
}

func TestMarshalProcessExitMetaForNonExitError(t *testing.T) {
	// Simulate a fork/exec error that would leak a filesystem path
	// if passed through verbatim.
	err := fmt.Errorf("fork/exec /home/user/.local/bin/claude: no such file or directory")
	data := MarshalProcessExitMeta(err, "")
	var meta ProcessExitInfo
	if jsonErr := json.Unmarshal(data, &meta); jsonErr != nil {
		t.Fatalf("unmarshal: %v", jsonErr)
	}
	if meta.Reason != "provider failed to start" {
		t.Fatalf("Reason = %q, want generic; raw error must not leak paths", meta.Reason)
	}
	if strings.Contains(string(data), "/home/") {
		t.Fatal("marshaled metadata leaks filesystem path")
	}
}

func TestStderrTailCapturesFinalOutput(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", `echo "error: unknown option '--thinking-display'" >&2; exit 1`},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	<-p.Done()

	// The drain goroutine consumes stderr independently of cmd.Wait, so
	// poll briefly rather than asserting on the first read. Production
	// has the same grace via WaitProcessExitErr's 100ms reap window.
	var tail string
	deadline := time.Now().Add(2 * time.Second)
	for {
		tail = p.StderrTail()
		if strings.Contains(tail, "unknown option '--thinking-display'") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("StderrTail = %q, want captured stderr line", tail)
		}
		time.Sleep(10 * time.Millisecond)
	}

	data := MarshalProcessExitMeta(p.Err(), tail)
	var meta ProcessExitInfo
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta.ExitCode != 1 {
		t.Fatalf("ExitCode = %d, want 1", meta.ExitCode)
	}
	if !strings.Contains(meta.StderrTail, "unknown option") {
		t.Fatalf("StderrTail meta = %q, want sanitized stderr", meta.StderrTail)
	}
}

func TestStderrTailKeepsMostRecentBytes(t *testing.T) {
	tee := &stderrTee{}
	if _, err := tee.Write([]byte(strings.Repeat("x", stderrTailCap))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := tee.Write([]byte("THE-END")); err != nil {
		t.Fatalf("write: %v", err)
	}
	tail := tee.Tail()
	if len(tail) != stderrTailCap {
		t.Fatalf("tail len = %d, want cap %d", len(tail), stderrTailCap)
	}
	if !strings.HasSuffix(tail, "THE-END") {
		t.Fatalf("tail should keep the most recent bytes, got suffix %q", tail[len(tail)-16:])
	}
}

func TestSanitizeChildStderrBoundsAndFlattens(t *testing.T) {
	short := SanitizeChildStderr("  ENOENT: no such file\n  ")
	if short != "ENOENT: no such file" {
		t.Fatalf("short trim got %q", short)
	}
	multiline := SanitizeChildStderr("line one\nline two\r\nline three")
	if strings.ContainsAny(multiline, "\n\r") {
		t.Fatalf("newlines not collapsed: %q", multiline)
	}
	long := SanitizeChildStderr(strings.Repeat("A", 1024))
	if !strings.HasSuffix(long, "…(truncated)") {
		t.Fatalf("expected truncation marker, got %q (len=%d)", long, len(long))
	}
	// Cap is 256B + the truncation marker.
	if len(long) > 256+len("…(truncated)") {
		t.Fatalf("oversized output: len=%d", len(long))
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
	if err != nil {
		t.Fatalf("Close returned %v, want nil (signal-terminated subprocess is still a successful close)", err)
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("Close took %v, want under 6s", elapsed)
	}

	select {
	case <-p.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Done channel should be closed after Close")
	}

	// Forensic exit details (signal name, exit code) must remain
	// accessible on Err() — Close swallows the exit for return-value
	// semantics, not for diagnostics.
	if p.Err() == nil {
		t.Fatal("Err() should still surface the signal-terminated exit for forensics")
	}
}

// TestCloseSwallowsExitCodeOnIntentionalShutdown is the regression for
// the interrupt-then-revert "Revert failed: Exit status 1" bug. When
// callers (revert, thread delete, shutdown) intentionally tear down a
// session whose subprocess exited non-zero, Close must treat "process
// is gone" as success. The exit details stay on Err() for the
// read-loop's unexpected-exit banner path, which gates on
// `!closing.Load()` so it doesn't fire for intentional teardown.
func TestCloseSwallowsExitCodeOnIntentionalShutdown(t *testing.T) {
	ctx := context.Background()
	// `false` exits with code 1 immediately. By the time Close runs
	// the Wait goroutine has already captured the *exec.ExitError into
	// p.err and closed p.done.
	p, err := Spawn(ctx, SpawnConfig{Binary: "false"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Ensure the Wait goroutine has run so Close hits the first
	// `case <-p.done` arm with a non-nil p.err.
	<-p.Done()

	if err := p.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil (subprocess exit is the close goal, not a failure)", err)
	}

	// The exit error must remain available for callers that want it
	// (session-died banner, exit-meta marshaller).
	if p.Err() == nil {
		t.Fatal("Err() should still surface the non-zero exit for forensics")
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
// to prove the boundary check does not fire one byte early. The child emits
// one long stdout line and then stays alive, which avoids the bidirectional
// pipe deadlock a cat/stdin fixture can hit before the test starts reading.
func TestReadLineJustUnderCapSucceeds(t *testing.T) {
	ctx := context.Background()
	// 128 KiB is safely under the cap (32 MiB) yet large enough to span many
	// bufio refills so a bogus size counter would trip.
	payloadLen := 128 * 1024
	script := fmt.Sprintf(`perl -e 'print "x" x %d; print "\n"'; sleep 60`, payloadLen)
	p, err := Spawn(ctx, SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", script},
	})
	if err != nil {
		t.Fatalf("spawn under-cap emitter: %v", err)
	}
	defer p.Kill()

	line, err := p.ReadLine()
	if err != nil {
		t.Fatalf("read line: %v", err)
	}
	if len(line) != payloadLen {
		t.Fatalf("line len = %d, want %d", len(line), payloadLen)
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

// boundedLineReader builds a Process whose stdout reader wraps the given
// input with the same 64 KiB buffer Spawn configures. It exercises
// readBoundedLine directly — no subprocess — so the byte-level contract
// edges (cap boundary, EOF-with-partial-line, multi-chunk lines) are cheap
// to pin.
func boundedLineReader(input string) *Process {
	return &Process{stdout: bufio.NewReaderSize(strings.NewReader(input), 64*1024)}
}

// TestReadBoundedLineExactCapSucceeds pins the boundary predicate: a line
// whose content is exactly maxLineSize bytes must succeed; the error fires
// only when content EXCEEDS the cap.
func TestReadBoundedLineExactCapSucceeds(t *testing.T) {
	content := strings.Repeat("a", maxLineSize)
	p := boundedLineReader(content + "\n")

	line, err := p.readBoundedLine()
	if err != nil {
		t.Fatalf("exact-cap line: %v", err)
	}
	if len(line) != maxLineSize {
		t.Fatalf("line len = %d, want %d", len(line), maxLineSize)
	}
}

// TestReadBoundedLineOneOverCapFails proves content of maxLineSize+1 trips
// ErrLineTooLong even when the line is newline-terminated.
func TestReadBoundedLineOneOverCapFails(t *testing.T) {
	content := strings.Repeat("a", maxLineSize+1)
	p := boundedLineReader(content + "\n")

	_, err := p.readBoundedLine()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}
}

// TestReadBoundedLineEOFPartialLine pins the EOF-with-partial-line contract:
// a final line without a trailing newline is returned intact, and the NEXT
// call reports io.EOF.
func TestReadBoundedLineEOFPartialLine(t *testing.T) {
	p := boundedLineReader("complete\npartial")

	line, err := p.readBoundedLine()
	if err != nil {
		t.Fatalf("first line: %v", err)
	}
	if string(line) != "complete" {
		t.Fatalf("first line = %q, want %q", line, "complete")
	}

	line, err = p.readBoundedLine()
	if err != nil {
		t.Fatalf("partial final line: %v", err)
	}
	if string(line) != "partial" {
		t.Fatalf("partial line = %q, want %q", line, "partial")
	}

	if _, err := p.readBoundedLine(); err != io.EOF {
		t.Fatalf("after partial line: err = %v, want io.EOF", err)
	}
}

// TestReadBoundedLineEOFOnly proves a reader with no pending bytes returns
// io.EOF and no line.
func TestReadBoundedLineEOFOnly(t *testing.T) {
	p := boundedLineReader("")
	if _, err := p.readBoundedLine(); err != io.EOF {
		t.Fatalf("empty input: err = %v, want io.EOF", err)
	}
}

// TestReadBoundedLineSpansManyChunks pins byte-identity for a line that
// spans several ReadSlice chunks (ErrBufferFull iterations): the assembled
// line must match the input exactly, and the following line must still
// parse.
func TestReadBoundedLineSpansManyChunks(t *testing.T) {
	// ~300 KiB spans five 64 KiB bufio chunks. Vary the content so a chunk
	// stitched in the wrong order or with an off-by-one would not compare
	// equal.
	var b strings.Builder
	for i := 0; b.Len() < 300*1024; i++ {
		fmt.Fprintf(&b, "%d:", i)
	}
	long := b.String()
	p := boundedLineReader(long + "\nnext\n")

	line, err := p.readBoundedLine()
	if err != nil {
		t.Fatalf("long line: %v", err)
	}
	if string(line) != long {
		t.Fatalf("long line mismatch: len %d vs %d", len(line), len(long))
	}

	next, err := p.readBoundedLine()
	if err != nil {
		t.Fatalf("next line: %v", err)
	}
	if string(next) != "next" {
		t.Fatalf("next line = %q, want %q", next, "next")
	}
}

// TestReadBoundedLineEOFPartialOverCap covers the overflow check on the EOF
// branch: an unterminated final line larger than the cap must error, not be
// returned as content.
func TestReadBoundedLineEOFPartialOverCap(t *testing.T) {
	p := boundedLineReader(strings.Repeat("a", maxLineSize+1))
	_, err := p.readBoundedLine()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}
}

// writeSignalTrapScript writes a shell script that records the signal it was
// asked to stop with. It stands in for a provider CLI holding a credential it
// has not written yet: SIGTERM leaves the marker, SIGKILL cannot.
func writeSignalTrapScript(t *testing.T, marker string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trap.sh")
	script := "#!/bin/sh\n" +
		"trap 'printf term > \"" + marker + "\"; exit 0' TERM\n" +
		"echo ready\n" +
		"while true; do sleep 0.05; done\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeSignalIgnoringScript writes the same loop with SIGTERM ignored
// outright: a provider CLI that is wedged rather than merely busy. It writes
// no marker under any signal, which is the property the escalation test
// asserts — a future edit that gave this script a handler would silently turn
// that test into another graceful-exit test.
func writeSignalIgnoringScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ignore-term.sh")
	script := "#!/bin/sh\n" +
		"trap '' TERM\n" +
		"echo ready\n" +
		"while true; do sleep 0.05; done\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A probe runs under a deadline while the CLI may be mid credential write, so
// context cancellation must arrive as SIGTERM the child can act on. exec's
// default cancel is an instant SIGKILL, which is what could end an account's
// refresh chain between the token endpoint answering and the write to disk.
func TestGracefulCancelSignalsTermBeforeKilling(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "signal")
	binary := writeSignalTrapScript(t, marker)

	ctx, cancel := context.WithCancel(context.Background())
	p, err := Spawn(ctx, SpawnConfig{Binary: binary, GracefulCancel: true})
	if err != nil {
		cancel()
		t.Fatalf("spawn: %v", err)
	}
	defer p.Kill()
	waitForScriptReady(t, p)

	cancel()
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after context cancellation")
	}

	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the child was killed without a chance to handle SIGTERM: %v", err)
	}
	if string(got) != "term" {
		t.Fatalf("signal marker = %q, want term", got)
	}
	// The child exits 0, but the cancellation is still the reason it stopped —
	// exec keeps that accounting because the Cancel hook returns nil rather
	// than an error. Callers distinguish "the deadline ended this probe" from
	// "the CLI answered and exited" through Err(), so a clean exit code must
	// not read as a clean run.
	if !errors.Is(p.Err(), context.Canceled) {
		t.Fatalf("Err() = %v, want the context cancellation reported", p.Err())
	}
}

// A child that ignores SIGTERM must not be able to outlive its context: the
// grace is a courtesy for a CLI mid credential write, not permission to stay.
// Without the WaitDelay escalation a wedged provider would hold its process
// group — and whatever it was writing — for as long as it liked, and the
// probe's deadline would guarantee nothing.
func TestGracefulCancelEscalatesToKillWhenTermIsIgnored(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "signal")
	binary := writeSignalIgnoringScript(t)

	ctx, cancel := context.WithCancel(context.Background())
	p, err := Spawn(ctx, SpawnConfig{Binary: binary, GracefulCancel: true})
	if err != nil {
		cancel()
		t.Fatalf("spawn: %v", err)
	}
	defer p.Kill()
	waitForScriptReady(t, p)

	cancel()
	start := time.Now()
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a child ignoring SIGTERM was never escalated to SIGKILL")
	}
	// The lower bound is loose on purpose: what matters is that the child was
	// given its grace rather than killed outright, not the exact killGrace.
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("exited %v after cancel, want the SIGTERM grace served first", elapsed)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a graceful-exit marker appeared for a child that ignores SIGTERM: %v", err)
	}
}

// The opt-in is real: without it the default instant SIGKILL still applies,
// which is what session teardown wants.
func TestSpawnWithoutGracefulCancelKillsImmediately(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "signal")
	binary := writeSignalTrapScript(t, marker)

	ctx, cancel := context.WithCancel(context.Background())
	p, err := Spawn(ctx, SpawnConfig{Binary: binary})
	if err != nil {
		cancel()
		t.Fatalf("spawn: %v", err)
	}
	defer p.Kill()
	waitForScriptReady(t, p)

	cancel()
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after context cancellation")
	}

	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SIGTERM handler ran without GracefulCancel: %v", err)
	}
}

// waitForScriptReady blocks until the trap script has installed its handler,
// so a cancellation cannot race the trap being registered.
func waitForScriptReady(t *testing.T, p *Process) {
	t.Helper()
	ready := make(chan error, 1)
	go func() {
		_, err := p.ReadLine()
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Errorf("read from trap script: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("trap script never reported ready")
	}
}
