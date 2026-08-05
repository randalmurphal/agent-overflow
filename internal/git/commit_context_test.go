package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

// seedRepo creates a git repo with one committed file so tests have a
// stable baseline. Mirrors the helper in root-package commit tests.
func seedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	run("add", "-A")
	run("-c", "user.email=t@t", "-c", "user.name=Tester", "commit", "-q", "-m", "init")
	return dir
}

func TestStagedSummaryReportsStagedChanges(t *testing.T) {
	dir := seedRepo(t)
	core := NewCore()

	// Add a new file and stage it.
	if err := os.WriteFile(filepath.Join(dir, "NEW"), []byte("new file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := core.StageAll(dir); err != nil {
		t.Fatalf("StageAll: %v", err)
	}

	summary, err := core.StagedSummary(dir)
	if err != nil {
		t.Fatalf("StagedSummary: %v", err)
	}
	if !strings.Contains(summary, "NEW") {
		t.Errorf("expected NEW in summary; got %q", summary)
	}
	// name-status format prefixes new files with 'A'.
	if !strings.HasPrefix(strings.TrimSpace(summary), "A") {
		t.Errorf("expected 'A\\tNEW' style status; got %q", summary)
	}
}

func TestStagedPatchIncludesDiffLines(t *testing.T) {
	dir := seedRepo(t)
	core := NewCore()

	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := core.StageAll(dir); err != nil {
		t.Fatalf("StageAll: %v", err)
	}

	patch, err := core.StagedPatch(dir)
	if err != nil {
		t.Fatalf("StagedPatch: %v", err)
	}
	if !strings.Contains(patch, "+world") {
		t.Errorf("expected added line '+world'; got %q", patch)
	}
	if !strings.Contains(patch, "diff --git") {
		t.Errorf("expected git diff header; got %q", patch)
	}
}

func TestStagedSummaryEmptyOnCleanRepo(t *testing.T) {
	dir := seedRepo(t)
	core := NewCore()

	summary, err := core.StagedSummary(dir)
	if err != nil {
		t.Fatalf("StagedSummary: %v", err)
	}
	if strings.TrimSpace(summary) != "" {
		t.Errorf("expected empty summary on clean repo; got %q", summary)
	}
}

// argvLogGit is a mock `git` that records the argv of every invocation.
const argvLogGit = `#!/bin/sh
printf '%s\n' "$*" >> "$AO_GIT_LOG"
exit 0
`

// TestStagedContextNeutralizesUserDiffConfig pins the flags that keep the
// user's git config out of the commit-message context. Every staged read
// must carry all three: without --no-ext-diff a `diff.external` differ's
// output (or a GUI that launches and never returns) reaches the prompt,
// without --no-textconv a filter's rendering replaces the file's content,
// and without --no-color `color.ui = always` splices ANSI escapes through
// the patch.
func TestStagedContextNeutralizesUserDiffConfig(t *testing.T) {
	logPath := installMockGit(t, argvLogGit)

	core := NewCore()
	cwd := t.TempDir()
	if _, err := core.StagedSummary(cwd); err != nil {
		t.Fatalf("StagedSummary: %v", err)
	}
	if _, err := core.StagedPatch(cwd); err != nil {
		t.Fatalf("StagedPatch: %v", err)
	}

	argvs := strings.Split(strings.TrimSpace(readFile(t, logPath)), "\n")
	if len(argvs) != 2 {
		t.Fatalf("mock git saw %d invocations, want 2:\n%s", len(argvs), strings.Join(argvs, "\n"))
	}
	for _, argv := range argvs {
		for _, flag := range []string{"--no-color", "--no-ext-diff", "--no-textconv"} {
			if !strings.Contains(argv, flag) {
				t.Errorf("`git %s` is missing %s", argv, flag)
			}
		}
	}
}

// TestStagedPatchIgnoresExternalDifferAndColor is the same guarantee against
// a real git: a repo configured with an external differ and forced colour
// must still yield git's own uncoloured patch, because that patch is what
// the commit-message model reads.
func TestStagedPatchIgnoresExternalDifferAndColor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script external differ is unix-only")
	}
	dir := seedRepo(t)

	differ := filepath.Join(t.TempDir(), "differ.sh")
	if err := os.WriteFile(differ, []byte("#!/bin/sh\necho EXTERNAL-DIFF-RAN\n"), 0o755); err != nil {
		t.Fatalf("write external differ: %v", err)
	}
	core := NewCore()
	if _, _, err := core.Execute(dir, "config", "diff.external", differ); err != nil {
		t.Fatalf("set diff.external: %v", err)
	}
	if _, _, err := core.Execute(dir, "config", "color.ui", "always"); err != nil {
		t.Fatalf("set color.ui: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := core.StageAll(dir); err != nil {
		t.Fatalf("StageAll: %v", err)
	}

	patch, err := core.StagedPatch(dir)
	if err != nil {
		t.Fatalf("StagedPatch: %v", err)
	}
	if strings.Contains(patch, "EXTERNAL-DIFF-RAN") {
		t.Errorf("external differ output reached the commit-message context: %q", patch)
	}
	if strings.Contains(patch, "\x1b[") {
		t.Errorf("ANSI colour escapes reached the commit-message context: %q", patch)
	}
	if !strings.Contains(patch, "+world") {
		t.Errorf("expected git's own patch with '+world'; got %q", patch)
	}
}

func TestLimitSectionTruncationMarker(t *testing.T) {
	in := strings.Repeat("x", 100)
	out := limitSection(in, 50)
	if !strings.HasSuffix(out, "[truncated]") {
		t.Errorf("expected truncation marker; got %q", out)
	}
	if len(out) == len(in) {
		t.Error("expected truncation to shorten input")
	}
}

func TestLimitSectionNoopBelowBudget(t *testing.T) {
	in := "short"
	if got := limitSection(in, 100); got != in {
		t.Errorf("expected no-op; got %q", got)
	}
}

func TestRecentCommitSubjectsNewestFirstSkippingMerges(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	commit := func(name, subject string) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		testutil.RunGit(t, repo, "add", "-A")
		testutil.RunGit(t, repo, "commit", "-q", "-m", subject)
	}
	commit("a.txt", "feat: add a")
	testutil.RunGit(t, repo, "checkout", "-q", "-b", "side")
	commit("b.txt", "fix: add b")
	testutil.RunGit(t, repo, "checkout", "-q", "main")
	commit("c.txt", "chore: add c")
	testutil.RunGit(t, repo, "merge", "-q", "--no-ff", "-m", "merge side", "side")

	core := NewCore()
	got := core.RecentCommitSubjects(repo, 3)
	want := []string{"chore: add c", "fix: add b", "feat: add a"}
	if len(got) != len(want) {
		t.Fatalf("subjects = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("subjects = %q, want %q", got, want)
		}
	}
}

func TestRecentCommitSubjectsBestEffortOnEmptyOrInvalid(t *testing.T) {
	core := NewCore()
	if got := core.RecentCommitSubjects(t.TempDir(), 20); got != nil {
		t.Fatalf("non-repo: got %q, want nil", got)
	}
	repo := t.TempDir()
	testutil.RunGit(t, repo, "init", "-q")
	if got := core.RecentCommitSubjects(repo, 20); got != nil {
		t.Fatalf("repo without commits: got %q, want nil", got)
	}
	if got := core.RecentCommitSubjects(repo, 0); got != nil {
		t.Fatalf("n=0: got %q, want nil", got)
	}
}
