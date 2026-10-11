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
// ForgeByID("gitlab") so they isolate it from the Core.forgeFor dispatch
// logic. CreatePR runs glab; every other operation is a request to an
// httptest GitLab through the forge API transport (gitlab_api_test.go).

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
	url, err := core.ForgeByID("gitlab").CreatePR(t.Context(), repo, "Demo MR", "Body", "", false)
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
	if _, err := core.ForgeByID("gitlab").CreatePR(t.Context(), repo, "Demo MR", "Body", "release", true); err != nil {
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
	if _, err := core.ForgeByID("gitlab").CreatePR(t.Context(), repo, "Default target", "Body", "", false); err != nil {
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
	_, err := core.ForgeByID("gitlab").CreatePR(t.Context(), t.TempDir(), "  ", "body", "", false)
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
	_, err := core.ForgeByID("gitlab").CreatePR(t.Context(), repo, "Demo", "body", "", false)
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
	_, err := core.ForgeByID("gitlab").CreatePR(t.Context(), repo, "Demo", "body", "", false)
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
	_, err := core.ForgeByID("gitlab").CreatePR(t.Context(), repo, "Test MR", "body", "", false)
	if err == nil {
		t.Fatal("expected error for empty URL output")
	}
	if !strings.Contains(err.Error(), "empty URL") {
		t.Fatalf("unexpected error: %v", err)
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
