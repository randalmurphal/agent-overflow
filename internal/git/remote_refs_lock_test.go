package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/testutil"
)

// Fetches of one repository that move the same remote-tracking ref all
// succeed when they overlap, from the main checkout and a linked worktree
// alike. Unserialized, the losers exit 1 with "incorrect old value
// provided" in most rounds.
func TestOverlappingFetchesOfOneRepositoryAllSucceed(t *testing.T) {
	repo, bare := testutil.InitGitRepoWithOrigin(t)
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "linked", linked)
	pusher := t.TempDir()
	testutil.RunGit(t, pusher, "clone", bare, ".")

	core := NewCore()
	fetches := []struct {
		cwd  string
		args []string
	}{
		{repo, []string{"fetch", "--quiet", "origin"}},
		{repo, []string{"fetch", "origin", "main"}},
		{linked, []string{"-c", "fetch.prune=false", "fetch", "origin", "main"}},
	}
	for round := range 8 {
		// Enough new objects that each fetch keeps a pack rather than
		// unpacking loose objects, as a real fetch of a busy branch does.
		for file := range 120 {
			name := filepath.Join(pusher, fmt.Sprintf("f%d.txt", file))
			if err := os.WriteFile(name, []byte(fmt.Sprintf("%d %d\n", round, file)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		testutil.RunGit(t, pusher, "add", "-A")
		testutil.RunGit(t, pusher, "-c", "user.email=outside@example.com", "-c", "user.name=Outside",
			"commit", "-q", "-m", fmt.Sprintf("round %d", round))
		testutil.RunGit(t, pusher, "push", "-q", "origin", "main")

		var wg sync.WaitGroup
		errs := make([]error, len(fetches))
		for i, fetch := range fetches {
			wg.Go(func() {
				_, _, errs[i] = core.Execute(fetch.cwd, fetch.args...)
			})
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: git %s: %v", round, strings.Join(fetches[i].args, " "), err)
			}
		}
	}
}

func TestIsRemoteRefsCommand(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"fetch", "origin"}, true},
		{[]string{"pull", "--ff-only"}, true},
		{[]string{"push"}, true},
		{[]string{"-c", "fetch.prune=false", "fetch", "origin"}, true},
		{[]string{"status"}, false},
		{[]string{"-c", "fetch", "status"}, false},
		{nil, false},
	} {
		if got := isRemoteRefsCommand(tc.args); got != tc.want {
			t.Errorf("isRemoteRefsCommand(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
