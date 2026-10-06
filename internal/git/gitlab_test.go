package git

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/mockexec"
)

// These tests target the gitlab forge implementation directly via
// ForgeByID("gitlab") so they isolate the glab-wrapper behaviour from
// the Core.forgeFor dispatch logic.

func TestGitLabForgeIDAndBinary(t *testing.T) {
	t.Parallel()
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
	mockexec.Write(t, glabPath, script)
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
	mockexec.Write(t, glabPath, script)
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
	mockexec.Write(t, glabPath, script)
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
	mockexec.Write(t, glabPath, script)
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
	t.Parallel()
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
	mockexec.Write(t, glabPath, script)
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

	repo := initGitRepo(t)
	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	script := "#!/bin/sh\necho 'https://gitlab.com/group/repo/-/merge_requests/12'\n"
	mockexec.Write(t, glabPath, script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	url, err := core.ForgeByID("gitlab").CreatePR(repo, "Demo MR", "Body", "", false)
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

	repo := initGitRepo(t)
	binDir := t.TempDir()
	argLog := filepath.Join(binDir, "args.log")
	glabPath := filepath.Join(binDir, "glab")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" > %q
echo "https://gitlab.com/x/y/-/merge_requests/1"
`, argLog)
	mockexec.Write(t, glabPath, script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	if _, err := core.ForgeByID("gitlab").CreatePR(repo, "Demo MR", "Body", "release", true); err != nil {
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
		"--target-branch release",
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
	if _, err := core.ForgeByID("gitlab").CreatePR(repo, "Default target", "Body", "", false); err != nil {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(argLog)
	if argv := string(args); strings.Contains(argv, "--target-branch") {
		t.Fatalf("argv = %q, unexpected target branch", argv)
	}
}

func TestGitLabCreatePRRequiresTitle(t *testing.T) {
	t.Parallel()
	core := NewCore()
	_, err := core.ForgeByID("gitlab").CreatePR(t.TempDir(), "  ", "body", "", false)
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

	repo := initGitRepo(t)
	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	script := "#!/bin/sh\necho 'auth failed' 1>&2\nexit 1\n"
	mockexec.Write(t, glabPath, script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	_, err := core.ForgeByID("gitlab").CreatePR(repo, "Demo", "body", "", false)
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "glab mr create failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitLabCreatePRHandlesMissingGlab(t *testing.T) {
	repo := initGitRepo(t)
	t.Setenv("PATH", t.TempDir())

	core := NewCore()
	_, err := core.ForgeByID("gitlab").CreatePR(repo, "Demo", "body", "", false)
	if err == nil {
		t.Fatal("expected missing glab error")
	}
	if !strings.Contains(err.Error(), "GitLab CLI (`glab`)") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitLabCreatePRHandlesEmptyURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}
	repo := initGitRepo(t)

	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	script := "#!/bin/sh\necho ''\n"
	mockexec.Write(t, glabPath, script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	_, err := core.ForgeByID("gitlab").CreatePR(repo, "Test MR", "body", "", false)
	if err == nil {
		t.Fatal("expected error for empty URL output")
	}
	if !strings.Contains(err.Error(), "empty URL") {
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
	mockexec.Write(t, glabPath, script)
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
	t.Parallel()
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

func TestGitLabListMergedPRHeadsPagesPast100(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	binDir := t.TempDir()
	argLog := filepath.Join(binDir, "args.log")
	glabPath := filepath.Join(binDir, "glab")
	// Page 1 returns a full 100 rows, page 2 a short 50 — the pager must
	// request page 2 and stop on the short page.
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
case "$*" in
*"&page=1&"*)
  printf '['
  i=1
  while [ "$i" -le 100 ]; do
    [ "$i" -gt 1 ] && printf ','
    printf '{"source_branch":"p1-%%d","sha":"a%%d","web_url":"https://gitlab.example/mr/p1-%%d"}' "$i" "$i" "$i"
    i=$((i + 1))
  done
  printf ']\n'
  ;;
*"&page=2&"*)
  printf '['
  i=1
  while [ "$i" -le 50 ]; do
    [ "$i" -gt 1 ] && printf ','
    printf '{"source_branch":"p2-%%d","sha":"b%%d","web_url":"https://gitlab.example/mr/p2-%%d"}' "$i" "$i" "$i"
    i=$((i + 1))
  done
  printf ']\n'
  ;;
*)
  echo '[]'
  ;;
esac
`, argLog)
	mockexec.Write(t, glabPath, script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	heads, err := core.ForgeByID("gitlab").ListMergedPRHeads(t.TempDir(), 200)
	if err != nil {
		t.Fatalf("ListMergedPRHeads returned error: %v", err)
	}
	if len(heads) != 150 {
		t.Fatalf("len(heads) = %d, want 150 (full page 1 + short page 2)", len(heads))
	}
	if heads[0].HeadRefName != "p1-1" || heads[100].HeadRefName != "p2-1" {
		t.Fatalf("pages must concatenate in order, got first=%q, 101st=%q", heads[0].HeadRefName, heads[100].HeadRefName)
	}
	if heads[149].HeadOid != "b50" || heads[149].URL != "https://gitlab.example/mr/p2-50" {
		t.Fatalf("last head = %+v, want page-2 row 50", heads[149])
	}

	args, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("read arg log: %v", err)
	}
	calls := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(calls) != 2 {
		t.Fatalf("expected exactly 2 glab calls (short page stops paging), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "per_page=100") || !strings.Contains(calls[0], "&page=1&") {
		t.Errorf("first call = %q, want per_page=100&page=1", calls[0])
	}
	if !strings.Contains(calls[1], "per_page=100") || !strings.Contains(calls[1], "&page=2&") {
		t.Errorf("second call = %q, want per_page=100&page=2", calls[1])
	}
	if !strings.Contains(calls[0], "state=merged") {
		t.Errorf("first call = %q, want state=merged filter", calls[0])
	}
}
