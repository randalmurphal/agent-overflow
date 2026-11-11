package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A caller that leaves once the PR head is fetched does not strand the
// scratch ref or leave pr/N behind an error: the branch is created and the
// scratch ref dropped. The cut lands while git creates the branch, before
// and after the ref transaction commits.
func TestFetchPRBranchCutAfterTheFetchStillFinishes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is unix-only")
	}
	for _, state := range []string{"prepared", "committed"} {
		t.Run(state, func(t *testing.T) {
			_, clone, rev := prOrigin(t)
			marks := t.TempDir()
			started, release := filepath.Join(marks, "started"), filepath.Join(marks, "release")
			// Holds git's branch creation at this transaction state until
			// the test releases it.
			hook := fmt.Sprintf(`#!/bin/sh
[ "$1" = %q ] || exit 0
while read -r old new ref; do
  if [ "$ref" = refs/heads/pr/5 ]; then
    : > %q
    while [ ! -e %q ]; do sleep 0.01; done
  fi
done
exit 0
`, state, started, release)
			hooks := filepath.Join(clone, ".git", "hooks")
			if err := os.MkdirAll(hooks, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(hook), 0o755); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type answer struct {
				branch, head string
				created      bool
				err          error
			}
			done := make(chan answer, 1)
			core := NewCore()
			go func() {
				branch, head, created, err := core.FetchPRBranch(ctx, clone, prRevisionPR)
				done <- answer{branch, head, created, err}
			}()
			deadline := time.Now().Add(20 * time.Second)
			for {
				if _, err := os.Stat(started); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the branch creation never started")
				}
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
			time.Sleep(50 * time.Millisecond)
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			var got answer
			select {
			case got = <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("FetchPRBranch did not return")
			}
			if got.err != nil || got.branch != "pr/5" || got.head != rev.HeadSHA || !got.created {
				t.Fatalf("FetchPRBranch cut at %s = (%q, %q, created %v, %v), want pr/5 created at the head", state, got.branch, got.head, got.created, got.err)
			}
			if refs, err := core.run(clone, "for-each-ref", "refs/agent-overflow"); err != nil || strings.TrimSpace(refs.stdout) != "" {
				t.Fatalf("scratch refs left behind: %q (%v)", refs.stdout, err)
			}
		})
	}
}
