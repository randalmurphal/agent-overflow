package gitdiff

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// TestGitSucceedsWhenAChildHoldsAPipe: a child git leaves behind (a hook, a
// helper, a textconv tool) can hold stdout or stderr after git exits 0. The
// wait is bounded and git's complete output is the result, not an error.
func TestGitSucceedsWhenAChildHoldsAPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell alias is unix-only")
	}
	original := gitPipeWaitDelay
	gitPipeWaitDelay = 100 * time.Millisecond
	t.Cleanup(func() { gitPipeWaitDelay = original })
	holdsStdout := []string{"-c", "alias.leak=!sleep 2 & echo done", "leak"}
	holdsStderr := []string{"-c", "alias.leak=!sleep 2 >/dev/null & echo done", "leak"}

	started := time.Now()
	stdout, _, code, err := runGit(context.Background(), t.TempDir(), nil, false, holdsStdout...)
	if err != nil || code != 0 || stdout != "done\n" {
		t.Fatalf("runGit: stdout %q, code %d, err %v; want git's output and no error", stdout, code, err)
	}
	stdout, _, code, err = runGitWithStdin(context.Background(), t.TempDir(), nil, nil, false, holdsStdout...)
	if err != nil || code != 0 || stdout != "done\n" {
		t.Fatalf("runGitWithStdin: stdout %q, code %d, err %v; want git's output and no error", stdout, code, err)
	}
	diff := newDiff(t.TempDir(), nil, nil, holdsStderr)
	patch, err := openAndRead(t, diff, nil)
	if err != nil || patch != "done\n" {
		t.Fatalf("stream: patch %q, err %v; want git's output and no error", patch, err)
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("returned after %s, held by the child", elapsed)
	}
}

// TestGitReportsCancellationDespiteAllowedExit: a run the context ended is
// an error even for a caller that accepts nonzero exits, and the error
// carries the context's. A descendant that keeps writing holds the run
// open until the deadline. A plain nonzero exit is still excused.
func TestGitReportsCancellationDespiteAllowedExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell alias is unix-only")
	}
	original := gitPipeWaitDelay
	gitPipeWaitDelay = 100 * time.Millisecond
	t.Cleanup(func() { gitPipeWaitDelay = original })
	noisy := "(i=0; while [ $i -lt 100 ]; do echo tick; sleep 0.02; i=$((i+1)); done) & "
	run := map[string]func(ctx context.Context, args ...string) (int, error){
		"runGit": func(ctx context.Context, args ...string) (int, error) {
			_, _, code, err := runGit(ctx, t.TempDir(), nil, true, args...)
			return code, err
		},
		"runGitWithStdin": func(ctx context.Context, args ...string) (int, error) {
			_, _, code, err := runGitWithStdin(ctx, t.TempDir(), nil, nil, true, args...)
			return code, err
		},
	}
	for name, run := range run {
		for _, exit := range []string{"exit 0", "exit 1"} {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			_, err := run(ctx, "-c", "alias.noisy=!"+noisy+exit, "noisy")
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%s with a noisy descendant and %s = %v, want the context's error", name, exit, err)
			}
		}
		code, err := run(context.Background(), "-c", "alias.fail=!exit 1", "fail")
		if err != nil || code != 1 {
			t.Errorf("%s exit 1 = code %d, err %v; want code 1 and no error", name, code, err)
		}
	}
}
