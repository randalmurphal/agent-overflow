package git

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

// These tests target the gitlab forge implementation directly via
// ForgeByID("gitlab") so they isolate the glab-wrapper behaviour from
// the Core.forgeFor dispatch logic.

func TestGitLabForgeIDAndBinary(t *testing.T) {
	core := NewCore()
	f := core.ForgeByID("gitlab")
	if f.ID() != "gitlab" {
		t.Errorf("ID() = %q, want gitlab", f.ID())
	}
	if f.BinaryName() != "glab" {
		t.Errorf("BinaryName() = %q, want glab", f.BinaryName())
	}
}

func TestGitLabListOpenPRsParsesJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	script := `#!/bin/sh
cat <<'JSON'
[{"web_url": "https://gitlab.com/group/repo/-/merge_requests/3", "iid": 3, "title": "Feature MR", "state": "opened"}]
JSON
`
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	prs, err := core.ForgeByID("gitlab").ListOpenPRs(t.TempDir(), "feature/demo")
	if err != nil {
		t.Fatalf("ListOpenPRs returned error: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("len(prs) = %d, want 1", len(prs))
	}
	if prs[0].URL != "https://gitlab.com/group/repo/-/merge_requests/3" {
		t.Errorf("URL = %q", prs[0].URL)
	}
	if prs[0].Number != 3 {
		t.Errorf("Number = %d, want 3", prs[0].Number)
	}
	if prs[0].Title != "Feature MR" {
		t.Errorf("Title = %q", prs[0].Title)
	}
	// State is normalized: glab's "opened" → "open".
	if prs[0].State != "open" {
		t.Errorf("State = %q, want open (normalized from glab's opened)", prs[0].State)
	}
}

func TestGitLabListOpenPRsUsesAPIEndpointWithEncodedSourceBranch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	binDir := t.TempDir()
	argLog := filepath.Join(binDir, "args.log")
	glabPath := filepath.Join(binDir, "glab")
	// Record argv to verify the API endpoint and encoded source_branch query.
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" > %q
echo '[]'
`, argLog)
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	if _, err := core.ForgeByID("gitlab").ListOpenPRs(t.TempDir(), "feature/demo"); err != nil {
		t.Fatalf("ListOpenPRs returned error: %v", err)
	}

	args, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("read arg log: %v", err)
	}
	got := strings.TrimSpace(string(args))
	if !strings.Contains(got, "api projects/:fullpath/merge_requests?") {
		t.Errorf("argv = %q, want glab api project merge requests endpoint", got)
	}
	if !strings.Contains(got, "source_branch=feature%2Fdemo") {
		t.Errorf("argv = %q, want URL-encoded source branch", got)
	}
	if !strings.Contains(got, "state=opened") {
		t.Errorf("argv = %q, want state=opened filter", got)
	}
	if strings.Contains(got, "--output") {
		t.Errorf("argv = %q, must not use newer glab --output flag", got)
	}
}

func TestGitLabListOpenPRsDoesNotRequireMROutputFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	script := `#!/bin/sh
if [ "$1" = "mr" ] && [ "$2" = "list" ]; then
  echo "unknown flag: --output" 1>&2
  exit 1
fi
cat <<'JSON'
[{"web_url": "https://gitlab.com/group/repo/-/merge_requests/8", "iid": 8, "title": "Old glab compatible", "state": "opened"}]
JSON
`
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	prs, err := core.ForgeByID("gitlab").ListOpenPRs(t.TempDir(), "feature/demo")
	if err != nil {
		t.Fatalf("ListOpenPRs returned error: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 8 {
		t.Fatalf("prs = %+v, want one MR !8", prs)
	}
}

func TestGitLabListOpenPRsHandlesEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	script := "#!/bin/sh\necho '[]'\n"
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	prs, err := core.ForgeByID("gitlab").ListOpenPRs(t.TempDir(), "main")
	if err != nil {
		t.Fatalf("ListOpenPRs returned error: %v", err)
	}
	if prs != nil {
		t.Fatalf("expected nil prs for empty array, got %v", prs)
	}
}

func TestGitLabListOpenPRsRequiresHead(t *testing.T) {
	core := NewCore()
	_, err := core.ForgeByID("gitlab").ListOpenPRs(t.TempDir(), "  ")
	if err == nil {
		t.Fatal("expected error for empty source branch")
	}
	if !strings.Contains(err.Error(), "source branch is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitLabListOpenPRsHandlesNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	script := "#!/bin/sh\necho 'auth required' 1>&2\nexit 1\n"
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	_, err := core.ForgeByID("gitlab").ListOpenPRs(t.TempDir(), "main")
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "glab api merge request list failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitLabListOpenPRsHandlesMissingGlab(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	core := NewCore()
	_, err := core.ForgeByID("gitlab").ListOpenPRs(t.TempDir(), "main")
	if err == nil {
		t.Fatal("expected missing glab error")
	}
	if !strings.Contains(err.Error(), "GitLab CLI (`glab`)") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitLabCreatePRReturnsURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	repo := testutil.InitGitRepo(t)
	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	script := "#!/bin/sh\necho 'https://gitlab.com/group/repo/-/merge_requests/12'\n"
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	url, err := core.ForgeByID("gitlab").CreatePR(repo, "Demo MR", "Body", false)
	if err != nil {
		t.Fatalf("CreatePR returned error: %v", err)
	}
	if url != "https://gitlab.com/group/repo/-/merge_requests/12" {
		t.Fatalf("url = %q", url)
	}
}

func TestGitLabCreatePRPassesExpectedFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	repo := testutil.InitGitRepo(t)
	binDir := t.TempDir()
	argLog := filepath.Join(binDir, "args.log")
	glabPath := filepath.Join(binDir, "glab")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" > %q
echo "https://gitlab.com/x/y/-/merge_requests/1"
`, argLog)
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	if _, err := core.ForgeByID("gitlab").CreatePR(repo, "Demo MR", "Body", true); err != nil {
		t.Fatalf("CreatePR returned error: %v", err)
	}

	args, _ := os.ReadFile(argLog)
	argv := strings.TrimSpace(string(args))
	wantContains := []string{
		"mr create",
		"--title Demo MR",
		"--description Body",
		"--yes",
		"--no-editor",
		"--draft",
	}
	for _, want := range wantContains {
		if !strings.Contains(argv, want) {
			t.Errorf("argv = %q, missing %q", argv, want)
		}
	}
	// glab is allowed to default --source-branch to the current branch,
	// matching gh's behaviour.
	if strings.Contains(argv, "--source-branch") {
		t.Errorf("argv = %q, must NOT include --source-branch (rely on glab default)", argv)
	}
}

func TestGitLabCreatePRRequiresTitle(t *testing.T) {
	core := NewCore()
	_, err := core.ForgeByID("gitlab").CreatePR(t.TempDir(), "  ", "body", false)
	if err == nil {
		t.Fatal("expected error for empty title")
	}
	if !strings.Contains(err.Error(), "title is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitLabCreatePRHandlesNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	repo := testutil.InitGitRepo(t)
	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	script := "#!/bin/sh\necho 'auth failed' 1>&2\nexit 1\n"
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	_, err := core.ForgeByID("gitlab").CreatePR(repo, "Demo", "body", false)
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "glab mr create failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitLabCreatePRHandlesMissingGlab(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	t.Setenv("PATH", t.TempDir())

	core := NewCore()
	_, err := core.ForgeByID("gitlab").CreatePR(repo, "Demo", "body", false)
	if err == nil {
		t.Fatal("expected missing glab error")
	}
	if !strings.Contains(err.Error(), "GitLab CLI (`glab`)") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitLabViewPRParsesJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	script := `#!/bin/sh
cat <<'JSON'
{
  "title": "Add feature",
  "description": "Adds a thing",
  "source_branch": "feature",
  "target_branch": "main",
  "web_url": "https://gitlab.com/group/repo/-/merge_requests/9",
  "state": "opened",
  "author": {"username": "alice"}
}
JSON
`
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	meta, err := core.ForgeByID("gitlab").ViewPR(t.TempDir(), "group/repo", 9)
	if err != nil {
		t.Fatalf("ViewPR returned error: %v", err)
	}
	if meta.Title != "Add feature" {
		t.Errorf("Title = %q", meta.Title)
	}
	if meta.Body != "Adds a thing" {
		t.Errorf("Body = %q (want from description field)", meta.Body)
	}
	if meta.HeadRefName != "feature" {
		t.Errorf("HeadRefName = %q (want from source_branch)", meta.HeadRefName)
	}
	if meta.BaseRefName != "main" {
		t.Errorf("BaseRefName = %q (want from target_branch)", meta.BaseRefName)
	}
	if meta.URL != "https://gitlab.com/group/repo/-/merge_requests/9" {
		t.Errorf("URL = %q", meta.URL)
	}
	if meta.AuthorLogin != "alice" {
		t.Errorf("AuthorLogin = %q (want from author.username)", meta.AuthorLogin)
	}
	if meta.State != "open" {
		t.Errorf("State = %q, want open (normalized from glab's opened)", meta.State)
	}
}

func TestGitLabViewPRUsesRESTEndpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	binDir := t.TempDir()
	argLog := filepath.Join(binDir, "args.log")
	glabPath := filepath.Join(binDir, "glab")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" > %q
echo '{"title": "T"}'
`, argLog)
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	if _, err := core.ForgeByID("gitlab").ViewPR(t.TempDir(), "group/sub/repo", 7); err != nil {
		t.Fatalf("ViewPR returned error: %v", err)
	}

	args, _ := os.ReadFile(argLog)
	argv := strings.TrimSpace(string(args))
	if !strings.Contains(argv, "api projects/group%2Fsub%2Frepo/merge_requests/7") {
		t.Errorf("argv = %q, missing encoded MR REST endpoint", argv)
	}
	if strings.Contains(argv, "--output") || strings.Contains(argv, "mr view") {
		t.Errorf("argv = %q, must not use glab mr view JSON flags", argv)
	}
}

func TestGitLabViewPRRequiresProject(t *testing.T) {
	core := NewCore()
	_, err := core.ForgeByID("gitlab").ViewPR(t.TempDir(), "  ", 1)
	if err == nil {
		t.Fatal("expected error for empty project")
	}
	if !strings.Contains(err.Error(), "project") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitLabDiffReturnsStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	script := "#!/bin/sh\nprintf 'diff --git a/x b/x\\n+a\\n'\n"
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	diff, err := core.ForgeByID("gitlab").Diff(t.TempDir(), "group/repo", 9)
	if err != nil {
		t.Fatalf("Diff returned error: %v", err)
	}
	if !strings.Contains(diff, "diff --git a/x b/x") {
		t.Fatalf("diff missing header: %q", diff)
	}
}

func TestGitLabCreatePRHandlesEmptyURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}
	repo := testutil.InitGitRepo(t)

	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	script := "#!/bin/sh\necho ''\n"
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	_, err := core.ForgeByID("gitlab").CreatePR(repo, "Test MR", "body", false)
	if err == nil {
		t.Fatal("expected error for empty URL output")
	}
	if !strings.Contains(err.Error(), "empty URL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitLabViewPRRejectsZeroNumber(t *testing.T) {
	core := NewCore()
	_, err := core.ForgeByID("gitlab").ViewPR(t.TempDir(), "group/repo", 0)
	if err == nil {
		t.Fatal("expected error for zero number")
	}
	if !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitLabDiffRequiresProject(t *testing.T) {
	core := NewCore()
	_, err := core.ForgeByID("gitlab").Diff(t.TempDir(), "  ", 1)
	if err == nil {
		t.Fatal("expected error for empty project")
	}
	if !strings.Contains(err.Error(), "project") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitLabDiffRejectsZeroNumber(t *testing.T) {
	core := NewCore()
	_, err := core.ForgeByID("gitlab").Diff(t.TempDir(), "group/repo", 0)
	if err == nil {
		t.Fatal("expected error for zero number")
	}
	if !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitLabListOpenPRsHandlesNullStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}
	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	// glab can emit `null` (not `[]`) for some queries.
	script := "#!/bin/sh\necho 'null'\n"
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	prs, err := core.ForgeByID("gitlab").ListOpenPRs(t.TempDir(), "main")
	if err != nil {
		t.Fatalf("ListOpenPRs returned error: %v", err)
	}
	if prs != nil {
		t.Fatalf("expected nil prs for null stdout, got %v", prs)
	}
}

func TestExtractMRCreateURL(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"https://gitlab.com/foo/bar/-/merge_requests/1", "https://gitlab.com/foo/bar/-/merge_requests/1"},
		{"http://gitlab.com/foo/bar/-/merge_requests/1", "http://gitlab.com/foo/bar/-/merge_requests/1"},
		{"  https://gitlab.com/foo/bar/-/merge_requests/1  ", "https://gitlab.com/foo/bar/-/merge_requests/1"},
		{"banner\nhttps://gitlab.com/foo/bar/-/merge_requests/1\n", "https://gitlab.com/foo/bar/-/merge_requests/1"},
		// "Last URL wins" semantics — glab is allowed to emit progress
		// before the final URL; we pick the URL closest to stdout's tail.
		{"https://gitlab.com/x/y/-/merge_requests/1\nhttps://gitlab.com/a/b/-/merge_requests/2\n",
			"https://gitlab.com/a/b/-/merge_requests/2"},
		{"", ""},
		{"\n\n", ""},
		{"banner only no url", ""},
	}
	for _, tc := range cases {
		if got := extractMRCreateURL(tc.input); got != tc.want {
			t.Errorf("extractMRCreateURL(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
