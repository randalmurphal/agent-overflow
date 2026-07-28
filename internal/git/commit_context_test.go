package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
