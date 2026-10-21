package git

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

// A fork's PR has no branch on origin: the fetch brings its head ref into
// a new local pr/N at the head, and leaves no ref of its own behind.
func TestFetchPRBranchCreatesTheLocalBranchAtTheHead(t *testing.T) {
	_, clone, rev := prOrigin(t)
	core := NewCore()
	branch, head, err := core.FetchPRBranch(context.Background(), clone, prRevisionPR)
	if err != nil {
		t.Fatalf("FetchPRBranch: %v", err)
	}
	if branch != "pr/5" || head != rev.HeadSHA {
		t.Fatalf("FetchPRBranch = (%q, %q), want (pr/5, %s)", branch, head, rev.HeadSHA)
	}
	if got := revParse(t, clone, "refs/heads/pr/5"); got != rev.HeadSHA {
		t.Fatalf("pr/5 = %s, want %s", got, rev.HeadSHA)
	}
	if refs, err := core.run(clone, "for-each-ref", "refs/agent-overflow"); err != nil || strings.TrimSpace(refs.stdout) != "" {
		t.Fatalf("scratch refs left behind: %q (%v)", refs.stdout, err)
	}
	// Again, with the branch already at the head: used as it is.
	if branch, head, err := core.FetchPRBranch(context.Background(), clone, prRevisionPR); err != nil || branch != "pr/5" || head != rev.HeadSHA {
		t.Fatalf("FetchPRBranch again = (%q, %q, %v)", branch, head, err)
	}
}

// A pr/N at another commit may hold work: it is reported, never moved.
func TestFetchPRBranchRefusesADivergedBranch(t *testing.T) {
	_, clone, rev := prOrigin(t)
	testutil.RunGit(t, clone, "branch", "pr/5", "main")
	before := revParse(t, clone, "refs/heads/pr/5")
	_, _, err := NewCore().FetchPRBranch(context.Background(), clone, prRevisionPR)
	if !errors.Is(err, ErrPRBranchDiverged) || !strings.Contains(err.Error(), "pr/5") {
		t.Fatalf("FetchPRBranch over a diverged pr/5 = %v, want ErrPRBranchDiverged naming it", err)
	}
	if got := revParse(t, clone, "refs/heads/pr/5"); got != before || got == rev.HeadSHA {
		t.Fatalf("pr/5 moved to %s", got)
	}
}

// A head ref origin does not have is the fetch's failure, and no branch is
// created.
func TestFetchPRBranchReportsAMissingHeadRef(t *testing.T) {
	_, clone, _ := prOrigin(t)
	missing := prRevisionPR
	missing.Number = 9
	if _, _, err := NewCore().FetchPRBranch(context.Background(), clone, missing); err == nil || !strings.Contains(err.Error(), "pull/9/head") {
		t.Fatalf("FetchPRBranch of a missing head ref = %v", err)
	}
	if err := testutil.RunGitAllowError(clone, "rev-parse", "--verify", "--quiet", "refs/heads/pr/9"); err == nil {
		t.Fatal("pr/9 was created")
	}
}
