package gitdiff

import (
	"context"
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
