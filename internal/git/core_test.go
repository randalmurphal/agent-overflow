package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExecuteTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock git is unix-only")
	}

	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	script := "#!/bin/sh\nsleep 2\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock git: %v", err)
	}

	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+originalPath)

	core := &Core{timeout: 50 * time.Millisecond, maxOutputBytes: defaultMaxOutputBytes}
	_, _, err := core.Execute(t.TempDir(), "status")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestExecuteReturnsStdoutAndStderrOnNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock git is unix-only")
	}

	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	script := "#!/bin/sh\necho 'out'\necho 'err' 1>&2\nexit 4\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock git: %v", err)
	}

	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+originalPath)

	core := NewCore()
	stdout, stderr, err := core.Execute(t.TempDir(), "status")
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if strings.TrimSpace(stdout) != "out" {
		t.Fatalf("stdout = %q, want out", stdout)
	}
	if strings.TrimSpace(stderr) != "err" {
		t.Fatalf("stderr = %q, want err", stderr)
	}
}

func TestLimitedBufferTruncates(t *testing.T) {
	buf := newLimitedBuffer(4)

	if _, err := buf.Write([]byte("hello")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if got := buf.String(); got != "hell" {
		t.Fatalf("String() = %q, want hell", got)
	}
	if !buf.Truncated() {
		t.Fatal("expected buffer to report truncation")
	}
}
