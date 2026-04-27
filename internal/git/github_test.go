package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests target the github forge implementation directly via
// ForgeByID("github") so they isolate the gh-wrapper behaviour from
// the Core.forgeFor dispatch logic (which depends on origin URL
// classification — covered separately in forge_detect_test.go).

func TestListOpenPRsParsesJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho '[{\"url\":\"https://example.com/pr/7\",\"number\":7,\"title\":\"Feature branch\",\"state\":\"OPEN\"}]'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	prs, err := core.ForgeByID("github").ListOpenPRs(t.TempDir(), "feature/demo")
	if err != nil {
		t.Fatalf("ListOpenPRs returned error: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("len(prs) = %d, want 1", len(prs))
	}
	if prs[0].URL != "https://example.com/pr/7" {
		t.Fatalf("prs[0].URL = %q, want https://example.com/pr/7", prs[0].URL)
	}
	if prs[0].Title != "Feature branch" {
		t.Fatalf("prs[0].Title = %q, want Feature branch", prs[0].Title)
	}
	// State is normalized to canonical lowercase: gh's "OPEN" → "open".
	if prs[0].State != "open" {
		t.Fatalf("prs[0].State = %q, want open (normalized from gh's OPEN)", prs[0].State)
	}
}

func TestCreatePRReturnsURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho 'https://example.com/pr/9'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	url, err := core.ForgeByID("github").CreatePR(t.TempDir(), "Demo PR", "Body", false)
	if err != nil {
		t.Fatalf("CreatePR returned error: %v", err)
	}
	if url != "https://example.com/pr/9" {
		t.Fatalf("url = %q, want https://example.com/pr/9", url)
	}
}

func TestCreatePRRequiresTitle(t *testing.T) {
	core := NewCore()

	_, err := core.ForgeByID("github").CreatePR(t.TempDir(), "  ", "body", false)
	if err == nil {
		t.Fatal("expected error for empty title")
	}
	if !strings.Contains(err.Error(), "title is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreatePRHandlesNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho 'auth required' 1>&2\nexit 1\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	_, err := core.ForgeByID("github").CreatePR(t.TempDir(), "Test PR", "body", false)
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "gh pr create failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreatePRHandlesEmptyURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho ''\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	_, err := core.ForgeByID("github").CreatePR(t.TempDir(), "Test PR", "body", false)
	if err == nil {
		t.Fatal("expected error for empty URL output")
	}
	if !strings.Contains(err.Error(), "empty URL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreatePRHandlesMissingGH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	core := NewCore()
	_, err := core.ForgeByID("github").CreatePR(t.TempDir(), "Test PR", "body", false)
	if err == nil {
		t.Fatal("expected missing gh error")
	}
	if !strings.Contains(err.Error(), "GitHub CLI (`gh`)") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListOpenPRsRequiresHead(t *testing.T) {
	core := NewCore()

	_, err := core.ForgeByID("github").ListOpenPRs(t.TempDir(), "  ")
	if err == nil {
		t.Fatal("expected error for empty head")
	}
	if !strings.Contains(err.Error(), "head branch is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListOpenPRsHandlesNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho 'no repo' 1>&2\nexit 1\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	_, err := core.ForgeByID("github").ListOpenPRs(t.TempDir(), "main")
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "gh pr list failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListOpenPRsReturnsNilForEmptyOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	prs, err := core.ForgeByID("github").ListOpenPRs(t.TempDir(), "main")
	if err != nil {
		t.Fatalf("ListOpenPRs returned error: %v", err)
	}
	if prs != nil {
		t.Fatalf("expected nil prs, got %v", prs)
	}
}

func TestCommandOutputMessage(t *testing.T) {
	if got := commandOutputMessage("", "stderr msg"); got != "stderr msg" {
		t.Fatalf("commandOutputMessage with stderr = %q, want stderr msg", got)
	}
	if got := commandOutputMessage("stdout msg", ""); got != "stdout msg" {
		t.Fatalf("commandOutputMessage with stdout = %q, want stdout msg", got)
	}
	if got := commandOutputMessage("", ""); got != "command failed" {
		t.Fatalf("commandOutputMessage with empty = %q, want command failed", got)
	}
}

func TestListOpenPRsHandlesMissingGH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	core := NewCore()
	_, err := core.ForgeByID("github").ListOpenPRs(t.TempDir(), "main")
	if err == nil {
		t.Fatal("expected missing gh error")
	}
	if !strings.Contains(err.Error(), "GitHub CLI (`gh`)") {
		t.Fatalf("expected missing gh message, got %v", err)
	}
}

func TestGitHubForgeIDAndBinary(t *testing.T) {
	core := NewCore()
	f := core.ForgeByID("github")
	if f.ID() != "github" {
		t.Errorf("ID() = %q, want github", f.ID())
	}
	if f.BinaryName() != "gh" {
		t.Errorf("BinaryName() = %q, want gh", f.BinaryName())
	}
}

func TestGitHubForgeViewPRParsesJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := `#!/bin/sh
cat <<'JSON'
{
  "title": "Demo",
  "body": "Description here",
  "headRefName": "feature",
  "baseRefName": "main",
  "url": "https://github.com/owner/repo/pull/9",
  "files": [{"path": "a.go", "additions": 3, "deletions": 1}],
  "author": {"login": "octocat"},
  "state": "OPEN"
}
JSON
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	meta, err := core.ForgeByID("github").ViewPR(t.TempDir(), "owner/repo", 9)
	if err != nil {
		t.Fatalf("ViewPR returned error: %v", err)
	}
	if meta.Title != "Demo" || meta.Body != "Description here" {
		t.Errorf("title/body mismatch: got %+v", meta)
	}
	if meta.HeadRefName != "feature" || meta.BaseRefName != "main" {
		t.Errorf("ref names mismatch: got %+v", meta)
	}
	if meta.URL != "https://github.com/owner/repo/pull/9" {
		t.Errorf("URL = %q", meta.URL)
	}
	if meta.AuthorLogin != "octocat" || meta.State != "open" {
		t.Errorf("author/state mismatch: got %+v", meta)
	}
	if len(meta.Files) != 1 || meta.Files[0].Path != "a.go" || meta.Files[0].Additions != 3 {
		t.Errorf("files mismatch: got %+v", meta.Files)
	}
}

func TestGitHubForgeDiffReturnsStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\ncat <<'EOF'\ndiff --git a/x b/x\n+a\nEOF\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	diff, err := core.ForgeByID("github").Diff(t.TempDir(), "owner/repo", 9)
	if err != nil {
		t.Fatalf("Diff returned error: %v", err)
	}
	if !strings.Contains(diff, "diff --git a/x b/x") {
		t.Fatalf("diff missing header: %q", diff)
	}
}

func TestGitHubForgeViewPRMissingProject(t *testing.T) {
	core := NewCore()
	_, err := core.ForgeByID("github").ViewPR(t.TempDir(), "  ", 1)
	if err == nil {
		t.Fatal("expected error for empty project")
	}
	if !strings.Contains(err.Error(), "project") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitHubForgeViewPRRejectsZeroNumber(t *testing.T) {
	core := NewCore()
	_, err := core.ForgeByID("github").ViewPR(t.TempDir(), "owner/repo", 0)
	if err == nil {
		t.Fatal("expected error for zero number")
	}
	if !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("unexpected error: %v", err)
	}
}
