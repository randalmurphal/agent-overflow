package gitdiff

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

func writeFile(t *testing.T, repo, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func commitFile(t *testing.T, repo, name, content, message string) {
	t.Helper()
	writeFile(t, repo, name, content)
	testutil.RunGit(t, repo, "add", name)
	testutil.RunGit(t, repo, "commit", "-m", message)
}

func headSHA(t *testing.T, repo string) string {
	t.Helper()
	out, _, _, err := runGit(context.Background(), repo, nil, false, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(out)
}

func TestListCommitsReturnsBranchCommitsNewestFirst(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "a.txt", "a\n", "first change")
	commitFile(t, repo, "b.txt", "b\n", "second change")

	commits, err := ListCommits(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("expected 2 commits, got %d: %+v", len(commits), commits)
	}
	if commits[0].Subject != "second change" || commits[1].Subject != "first change" {
		t.Fatalf("expected newest first, got %q then %q", commits[0].Subject, commits[1].Subject)
	}
	for _, commit := range commits {
		if len(commit.SHA) != 40 {
			t.Errorf("commit %q: SHA %q is not a full hash", commit.Subject, commit.SHA)
		}
		if !strings.HasPrefix(commit.SHA, commit.ShortSHA) || commit.ShortSHA == "" {
			t.Errorf("commit %q: short SHA %q is not a prefix of %q", commit.Subject, commit.ShortSHA, commit.SHA)
		}
		if commit.Author != "Agent Overflow" {
			t.Errorf("commit %q: author = %q", commit.Subject, commit.Author)
		}
		if commit.AuthoredAt <= 0 || commit.AuthoredAt%1000 != 0 {
			t.Errorf("commit %q: authoredAt %d is not epoch milliseconds", commit.Subject, commit.AuthoredAt)
		}
	}
}

func TestListCommitsEmptyRange(t *testing.T) {
	repo := testutil.InitGitRepo(t)

	commits, err := ListCommits(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if len(commits) != 0 {
		t.Fatalf("expected no commits on main..HEAD, got %+v", commits)
	}
}

func TestListCommitsKeepsSeparatorCharactersInSubject(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	subject := `fix: handle "quotes" | pipes %s and unicode ✓`
	commitFile(t, repo, "a.txt", "a\n", subject)

	commits, err := ListCommits(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if len(commits) != 1 || commits[0].Subject != subject {
		t.Fatalf("expected subject %q round-tripped, got %+v", subject, commits)
	}
}

func TestListCommitsRangeRejectsBadRefs(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	cases := []struct {
		name, base, head string
	}{
		{"empty base", "", "HEAD"},
		{"flag-shaped base", "--all", "HEAD"},
		{"non-sha head", "main", "feature"},
		{"flag-shaped head", "main", "--all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ListCommitsRange(context.Background(), repo, tc.base, tc.head); err == nil {
				t.Fatalf("expected error for base=%q head=%q", tc.base, tc.head)
			}
		})
	}
}

func TestListCommitsRangeToExplicitHeadSHA(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "a.txt", "a\n", "first change")
	fetchedHead := headSHA(t, repo)
	commitFile(t, repo, "b.txt", "b\n", "past the fetched head")

	commits, err := ListCommitsRange(context.Background(), repo, "main", fetchedHead)
	if err != nil {
		t.Fatalf("ListCommitsRange: %v", err)
	}
	if len(commits) != 1 || commits[0].Subject != "first change" {
		t.Fatalf("expected only the commit at the pinned head, got %+v", commits)
	}
}

func TestCommitDiffRegularCommit(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, "a.txt", "one\n", "add a")
	commitFile(t, repo, "a.txt", "one\ntwo\n", "extend a")

	patch, err := CommitDiff(context.Background(), repo, headSHA(t, repo))
	if err != nil {
		t.Fatalf("CommitDiff: %v", err)
	}
	text := string(patch)
	if !strings.Contains(text, "+two") {
		t.Fatalf("expected the commit's own addition, got:\n%s", text)
	}
	if strings.Contains(text, "+one\n+two") {
		t.Fatalf("diff should be against the parent, not the root:\n%s", text)
	}
}

func TestCommitDiffRootCommit(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	root, _, _, err := runGit(context.Background(), repo, nil, false, "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		t.Fatalf("find root commit: %v", err)
	}

	patch, err := CommitDiff(context.Background(), repo, strings.TrimSpace(root))
	if err != nil {
		t.Fatalf("CommitDiff root: %v", err)
	}
	if !strings.Contains(string(patch), "+hello") {
		t.Fatalf("expected the initial commit's content as additions, got:\n%s", patch)
	}
}

func TestCommitDiffMergeCommitUsesFirstParent(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "feature.txt", "from feature\n", "feature work")
	testutil.RunGit(t, repo, "checkout", "main")
	commitFile(t, repo, "main.txt", "from main\n", "main work")
	testutil.RunGit(t, repo, "merge", "--no-ff", "-m", "merge feature", "feature")

	patch, err := CommitDiff(context.Background(), repo, headSHA(t, repo))
	if err != nil {
		t.Fatalf("CommitDiff merge: %v", err)
	}
	text := string(patch)
	if !strings.Contains(text, "from feature") {
		t.Fatalf("first-parent diff should show what the merge brought in:\n%s", text)
	}
	if strings.Contains(text, "from main") {
		t.Fatalf("first-parent diff must not include first-parent-side changes:\n%s", text)
	}
}

func TestCommitDiffRejectsNonSHA(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	for _, sha := range []string{"", "HEAD", "main", "--all", "zzzzzzzz"} {
		if _, err := CommitDiff(context.Background(), repo, sha); err == nil {
			t.Errorf("expected error for sha %q", sha)
		}
	}
}

func TestShowFileAtCommit(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, "a.txt", "old content\n", "add a")
	older := headSHA(t, repo)
	commitFile(t, repo, "a.txt", "new content\n", "change a")

	content, err := ShowFileAtCommit(context.Background(), repo, older, "a.txt")
	if err != nil {
		t.Fatalf("ShowFileAtCommit: %v", err)
	}
	if content != "old content\n" {
		t.Fatalf("expected content at the older commit, got %q", content)
	}

	if _, err := ShowFileAtCommit(context.Background(), repo, older, "missing.txt"); err == nil {
		t.Fatal("expected error for a path absent at the commit")
	}
	if _, err := ShowFileAtCommit(context.Background(), repo, "HEAD", "a.txt"); err == nil {
		t.Fatal("expected error for a non-SHA revision")
	}
	if _, err := ShowFileAtCommit(context.Background(), repo, older, "a\x00.txt"); err == nil {
		t.Fatal("expected error for a NUL in the path")
	}
}
