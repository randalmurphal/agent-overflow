package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

func TestParseStatusOutput(t *testing.T) {
	status := parseStatusOutput(
		`# branch.oid abcdef
# branch.head feature/demo
# branch.upstream origin/feature/demo
# branch.ab +2 -1
1 .M N... 100644 100644 100644 abcdef abcdef tracked.txt
? untracked.txt`,
		"3\t1\ttracked.txt\n",
		"1\t0\tstaged.txt\n",
	)

	if status.Branch != "feature/demo" {
		t.Fatalf("Branch = %q, want feature/demo", status.Branch)
	}
	if !status.HasUpstream {
		t.Fatal("expected HasUpstream=true")
	}
	if status.AheadCount != 2 || status.BehindCount != 1 {
		t.Fatalf("ahead/behind = %d/%d, want 2/1", status.AheadCount, status.BehindCount)
	}
	if !status.HasChanges {
		t.Fatal("expected HasChanges=true")
	}
	if status.FileCount != 3 {
		t.Fatalf("FileCount = %d, want 3", status.FileCount)
	}
	if status.Insertions != 4 {
		t.Fatalf("Insertions = %d, want 4", status.Insertions)
	}
	if status.Deletions != 1 {
		t.Fatalf("Deletions = %d, want 1", status.Deletions)
	}
}

func TestParseBranchList(t *testing.T) {
	branches := parseBranchList(
		"main|*|/tmp/repo\nfeature/demo| |\norigin/main| |\norigin/feature/demo| |\norigin/HEAD| |\n",
		"main",
		[]string{"origin"},
	)

	if len(branches) != 4 {
		t.Fatalf("len(branches) = %d, want 4", len(branches))
	}
	if !branches[0].IsCurrent {
		t.Fatal("expected first branch to be current")
	}
	if !branches[0].IsDefault {
		t.Fatal("expected main to be default")
	}
	if !branches[2].IsRemote {
		t.Fatal("expected origin/main to be remote")
	}
	if !branches[2].IsDefault {
		t.Fatal("expected origin/main to be default")
	}
	if branches[1].IsRemote {
		t.Fatal("expected feature/demo to remain a local branch")
	}
}

func TestStatusReturnsNotRepoForNonGitDirectory(t *testing.T) {
	core := NewCore()

	status, err := core.Status(t.TempDir())
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.IsRepo {
		t.Fatal("expected IsRepo=false")
	}
}

func TestListBranchesOnRepository(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	branches, err := core.ListBranches(repo)
	if err != nil {
		t.Fatalf("ListBranches returned error: %v", err)
	}
	if len(branches) == 0 {
		t.Fatal("expected at least one branch")
	}
	if branches[0].Name == "" {
		t.Fatal("expected branch name to be populated")
	}
}

func TestRepositoryRootReturnsGitTopLevel(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	workspace := filepath.Join(repo, "nested", "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	root, err := NewCore().RepositoryRoot(workspace)
	if err != nil {
		t.Fatalf("RepositoryRoot() error = %v", err)
	}
	if testutil.CanonicalPath(t,root) != testutil.CanonicalPath(t,repo) {
		t.Fatalf("root = %q, want %q", root, repo)
	}
}

func TestRepositoryRootReturnsErrorForNonRepo(t *testing.T) {
	root, err := NewCore().RepositoryRoot(t.TempDir())
	if err == nil {
		t.Fatalf("RepositoryRoot() = %q, want error", root)
	}
	if !strings.Contains(err.Error(), "git rev-parse --show-toplevel failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStatusOnRepositoryWithOrigin(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	testutil.RunGit(t,t.TempDir(), "init", "--bare", remote)
	testutil.RunGit(t,repo, "remote", "add", "origin", remote)
	testutil.RunGit(t,repo, "push", "-u", "origin", "main")

	readmePath := filepath.Join(repo, "README.txt")
	if err := os.WriteFile(readmePath, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("update README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "extra.txt"), []byte("extra\n"), 0o644); err != nil {
		t.Fatalf("write extra file: %v", err)
	}

	core := NewCore()
	status, err := core.Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}

	if !status.IsRepo {
		t.Fatal("expected IsRepo=true")
	}
	if status.Branch != "main" {
		t.Fatalf("Branch = %q, want main", status.Branch)
	}
	if !status.IsDefaultBranch {
		t.Fatal("expected default branch")
	}
	if !status.HasOriginRemote {
		t.Fatal("expected HasOriginRemote=true")
	}
	if !status.HasUpstream {
		t.Fatal("expected HasUpstream=true after push -u")
	}
	if !status.HasChanges {
		t.Fatal("expected HasChanges=true")
	}
	if status.FileCount < 2 {
		t.Fatalf("FileCount = %d, want at least 2", status.FileCount)
	}
	if status.Insertions == 0 {
		t.Fatal("expected insertions to be counted")
	}
}

func TestWorkingTreeDiff(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	readmePath := filepath.Join(repo, "README.txt")

	if err := os.WriteFile(readmePath, []byte("hello\nstaged\n"), 0o644); err != nil {
		t.Fatalf("write staged version: %v", err)
	}
	testutil.RunGit(t,repo, "add", "README.txt")
	if err := os.WriteFile(readmePath, []byte("hello\nstaged\nunstaged\n"), 0o644); err != nil {
		t.Fatalf("write unstaged version: %v", err)
	}

	core := NewCore()
	diff, err := core.WorkingTreeDiff(repo)
	if err != nil {
		t.Fatalf("WorkingTreeDiff returned error: %v", err)
	}
	if !strings.Contains(diff, "README.txt") {
		t.Fatalf("expected diff to mention README.txt, got %q", diff)
	}
}

func TestLookupOpenPRUsesGHWhenAvailable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PATH override in short mode")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho '[{\"url\":\"https://example.com/pr/7\",\"number\":7,\"title\":\"Demo PR\",\"state\":\"OPEN\"}]'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	url, number := core.lookupOpenPR(t.TempDir(), "main")
	if url != "https://example.com/pr/7" {
		t.Fatalf("url = %q, want https://example.com/pr/7", url)
	}
	if number != 7 {
		t.Fatalf("number = %d, want 7", number)
	}
}

func TestWorkingTreeDiffReturnsEmptyForCleanRepo(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	diff, err := core.WorkingTreeDiff(repo)
	if err != nil {
		t.Fatalf("WorkingTreeDiff returned error: %v", err)
	}
	if diff != "" {
		t.Fatalf("expected empty diff for clean repo, got %q", diff)
	}
}

func TestWorkingTreeDiffCachedOnly(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	readmePath := filepath.Join(repo, "README.txt")

	if err := os.WriteFile(readmePath, []byte("hello\nstaged change\n"), 0o644); err != nil {
		t.Fatalf("write staged version: %v", err)
	}
	testutil.RunGit(t, repo, "add", "README.txt")

	core := NewCore()
	diff, err := core.WorkingTreeDiff(repo)
	if err != nil {
		t.Fatalf("WorkingTreeDiff returned error: %v", err)
	}
	if !strings.Contains(diff, "staged change") {
		t.Fatalf("expected diff to contain staged content, got %q", diff)
	}
}

func TestWorkingTreeDiffOnNonRepo(t *testing.T) {
	core := NewCore()

	_, err := core.WorkingTreeDiff(t.TempDir())
	if err == nil {
		t.Fatal("expected error for non-repo directory")
	}
}

func TestListBranchesIncludesNewBranch(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "branch", "feature/test")

	core := NewCore()
	branches, err := core.ListBranches(repo)
	if err != nil {
		t.Fatalf("ListBranches returned error: %v", err)
	}

	found := false
	for _, b := range branches {
		if b.Name == "feature/test" {
			found = true
			if b.IsCurrent {
				t.Fatal("feature/test should not be current")
			}
		}
	}
	if !found {
		t.Fatal("expected feature/test in branch list")
	}
}

func TestCombineDiffsEdgeCases(t *testing.T) {
	if got := combineDiffs("", "cached"); got != "cached" {
		t.Fatalf("combineDiffs empty+cached = %q, want cached", got)
	}
	if got := combineDiffs("head", ""); got != "head" {
		t.Fatalf("combineDiffs head+empty = %q, want head", got)
	}
	if got := combineDiffs("", ""); got != "" {
		t.Fatalf("combineDiffs empty+empty = %q, want empty", got)
	}
}

func TestIsDefaultBranchNameEdgeCases(t *testing.T) {
	tests := []struct {
		branch  string
		dflt    string
		want    bool
	}{
		{"", "main", false},
		{"main", "", true},
		{"master", "", true},
		{"origin/main", "", true},
		{"origin/master", "", true},
		{"develop", "", false},
		{"develop", "develop", true},
		{"origin/develop", "develop", true},
		{"feature/develop", "develop", true}, // HasSuffix("/develop") matches remote-like patterns
	}

	for _, tt := range tests {
		t.Run(tt.branch+"/"+tt.dflt, func(t *testing.T) {
			got := isDefaultBranchName(tt.branch, tt.dflt)
			if got != tt.want {
				t.Fatalf("isDefaultBranchName(%q, %q) = %v, want %v", tt.branch, tt.dflt, got, tt.want)
			}
		})
	}
}

func TestParseAheadBehindMalformed(t *testing.T) {
	ahead, behind := parseAheadBehind("invalid")
	if ahead != 0 || behind != 0 {
		t.Fatalf("parseAheadBehind(invalid) = %d/%d, want 0/0", ahead, behind)
	}
}

func TestParseNumstatCountMalformed(t *testing.T) {
	if got := parseNumstatCount("abc"); got != 0 {
		t.Fatalf("parseNumstatCount(abc) = %d, want 0", got)
	}
}

func TestParsePorcelainPathIgnoredPrefix(t *testing.T) {
	if got := parsePorcelainPath("! ignored.txt"); got != "ignored.txt" {
		t.Fatalf("parsePorcelainPath for ignored = %q, want ignored.txt", got)
	}
}

func TestParsePorcelainPathEmptyFields(t *testing.T) {
	if got := parsePorcelainPath(""); got != "" {
		t.Fatalf("parsePorcelainPath for empty = %q, want empty", got)
	}
}

func TestParseStatusOutputDetachedHead(t *testing.T) {
	status := parseStatusOutput(
		"# branch.oid abcdef\n# branch.head (HEAD detached)\n",
		"",
		"",
	)
	if status.Branch != "" {
		t.Fatalf("Branch = %q, want empty for detached HEAD", status.Branch)
	}
}

func TestParseNumstatSkipsShortLines(t *testing.T) {
	entries := parseNumstat("incomplete\ttwo\n")
	if len(entries) != 0 {
		t.Fatalf("expected no entries for incomplete numstat line, got %d", len(entries))
	}
}

func TestStatusOnCleanRepository(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	status, err := core.Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if !status.IsRepo {
		t.Fatal("expected IsRepo=true")
	}
	if status.HasChanges {
		t.Fatal("expected HasChanges=false for clean repo")
	}
	if status.FileCount != 0 {
		t.Fatalf("FileCount = %d, want 0", status.FileCount)
	}
}

func TestHelperFunctions(t *testing.T) {
	if got := normalizeNumstatPath("old/name.txt => new/name.txt"); got != "new/name.txt" {
		t.Fatalf("normalizeNumstatPath returned %q", got)
	}
	if got := parsePorcelainPath("? notes.txt"); got != "notes.txt" {
		t.Fatalf("parsePorcelainPath for untracked returned %q", got)
	}
	if got := parsePorcelainPath("2 R. N... 100644 100644 100644 abc abc R100\tnew.txt\told.txt"); got != "new.txt" {
		t.Fatalf("parsePorcelainPath for rename returned %q", got)
	}
	if got := combineDiffs("one", "two"); got != "one\ntwo" {
		t.Fatalf("combineDiffs returned %q", got)
	}
	if got := parseNumstatCount("-"); got != 0 {
		t.Fatalf("parseNumstatCount('-') = %d, want 0", got)
	}
	if !isDefaultBranchName("origin/main", "") {
		t.Fatal("expected origin/main to be treated as default when origin HEAD is unavailable")
	}
}

