package git

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/testutil/mockexec"
)

// writeFakeGlab writes a shell stand-in for glab that prints its quiet
// environment and argv on stdout, noise on stderr, and exits with
// $AO_FAKE_GLAB_EXIT after sleeping $AO_FAKE_GLAB_SLEEP seconds. The sleep
// does not hold the output pipes, so a killed fake returns at once.
func writeFakeGlab(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake glab is a shell script")
	}
	path := filepath.Join(t.TempDir(), "fake-glab")
	script := `#!/bin/sh
sleep "${AO_FAKE_GLAB_SLEEP:-0}" </dev/null >/dev/null 2>&1
printf '%s|%s|%s|%s\n' "$AO_FORGE_CLI" "$GLAB_CHECK_UPDATE" "$GLAB_DEBUG_HTTP" "$*"
if [ -n "${AO_FAKE_GLAB_BODY_BYTES:-}" ]; then
	head -c "$AO_FAKE_GLAB_BODY_BYTES" /dev/zero
fi
echo "glab: 404 Project Not Found (HTTP 404)" >&2
exit "${AO_FAKE_GLAB_EXIT:-0}"
`
	mockexec.Write(t, path, script)
	return path
}

func TestStreamGitLabAPIRunsTheIsolatedFakeQuietly(t *testing.T) {
	markers := installTrapCLIs(t)
	core := NewCore(WithIsolatedForgeCLIs(writeFakeGlab(t), []string{"AO_FAKE_GLAB_EXIT=3"}))

	var out bytes.Buffer
	exitCode, stderr, err := core.StreamGitLabAPI(context.Background(),
		[]string{"--hostname", "gitlab.example.com", "--", "projects/grp%2Fapp/releases"}, &out, 1<<20)
	if err != nil {
		t.Fatalf("StreamGitLabAPI: %v", err)
	}
	want := "glab|false|false|api --hostname gitlab.example.com -- projects/grp%2Fapp/releases\n"
	if out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
	if exitCode != 3 {
		t.Errorf("exit code = %d, want 3", exitCode)
	}
	if !strings.Contains(stderr, "404 Project Not Found") || strings.Contains(out.String(), "404") {
		t.Errorf("stderr = %q, stdout = %q; want the diagnostics on stderr only", stderr, out.String())
	}
	assertNoTrapRan(t, markers)
}

func TestStreamGitLabAPIPropagatesTheWriterError(t *testing.T) {
	t.Parallel()
	core := NewCore(WithIsolatedForgeCLIs(writeFakeGlab(t), nil))
	sentinel := errors.New("destination full")
	_, _, err := core.StreamGitLabAPI(context.Background(), []string{"x"}, failingWriter{err: sentinel}, 1<<20)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the destination's error", err)
	}
}

func TestStreamGitLabAPIEnforcesTheLimit(t *testing.T) {
	t.Parallel()
	core := NewCore(WithIsolatedForgeCLIs(writeFakeGlab(t), []string{"AO_FAKE_GLAB_BODY_BYTES=65536"}))
	var out bytes.Buffer
	_, _, err := core.StreamGitLabAPI(context.Background(), []string{"x"}, &out, 1024)
	if !errors.Is(err, errOutputLimitExceeded) {
		t.Fatalf("error = %v, want the output limit refusal", err)
	}
	if out.Len() > 1024 {
		t.Fatalf("wrote %d bytes past a 1024-byte limit", out.Len())
	}
}

func TestStreamGitLabAPIReportsAMissingGlab(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	_, _, err := NewCore().StreamGitLabAPI(context.Background(), []string{"x"}, &out, 1024)
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("error = %v, want exec.ErrNotFound", err)
	}
}

// The caller's deadline bounds the command in both directions: a longer
// one outlives the Core's interactive default, and a shorter one cuts it.
func TestStreamGitLabAPITakesTheContextDeadline(t *testing.T) {
	t.Parallel()
	core := NewCore(WithIsolatedForgeCLIs(writeFakeGlab(t), []string{"AO_FAKE_GLAB_SLEEP=0.5"}))
	core.timeout = 200 * time.Millisecond

	long, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	if _, _, err := core.StreamGitLabAPI(long, []string{"x"}, &out, 1024); err != nil {
		t.Fatalf("a 0.5s call under a 10s deadline: %v", err)
	}

	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	core.timeout = time.Minute
	if _, _, err := core.StreamGitLabAPI(short, []string{"x"}, &out, 1024); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("a 0.5s call under a 100ms deadline = %v, want a timeout", err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }
