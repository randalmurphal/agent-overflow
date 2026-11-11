package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A fork fetch that fails after it created pr/N leaves no pr/N: the call
// that made the branch removes it, so a later open does not find an
// unowned branch and refuse it as diverged. The failure is the scratch
// ref's removal, refused by a hook.
func TestOpenPRThreadDropsTheBranchItsFailedFetchCreated(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is unix-only")
	}
	c := newPRThreadCase(t)
	c.push(t, "refs/pull/5/head")
	hook := `#!/bin/sh
[ "$1" = prepared ] || exit 0
while read -r old new ref; do
  case "$ref" in refs/agent-overflow/*) case "$new" in *[!0]*) ;; *) exit 1 ;; esac ;; esac
done
exit 0
`
	hooks := filepath.Join(c.repo, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := c.app.OpenPRThread(context.Background(), c.request("fork-work", true)); err == nil {
		t.Fatal("OpenPRThread succeeded with its scratch ref removal refused")
	}
	if branchExists(c.repo, "pr/5") {
		t.Fatal("pr/5 outlived the failed open that created it")
	}
	if threads := c.threads(t); len(threads) != 0 {
		t.Fatalf("threads = %d, want none", len(threads))
	}
}
