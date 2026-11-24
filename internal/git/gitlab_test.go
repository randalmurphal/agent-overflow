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

// CreatePR returns the URL glab printed, and an error when glab fails or
// prints no URL.
func TestGitLabCreatePRReturnsURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	repo := initGitRepo(t)
	for _, tc := range []struct{ name, script, wantURL, wantErr string }{
		{"url", "#!/bin/sh\necho 'https://gitlab.com/group/repo/-/merge_requests/12'\n", "https://gitlab.com/group/repo/-/merge_requests/12", ""},
		{"non-zero exit", "#!/bin/sh\necho 'auth failed' 1>&2\nexit 1\n", "", "glab mr create failed"},
		{"empty url", "#!/bin/sh\necho ''\n", "", "empty URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			mockexec.Write(t, filepath.Join(binDir, "glab"), tc.script)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			url, err := NewCore().ForgeByID("gitlab").CreatePR(t.Context(), repo, "Demo MR", "Body", "", false)
			if url != tc.wantURL || (err == nil) != (tc.wantErr == "") || err != nil && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("CreatePR = %q, %v; want %q and an error containing %q", url, err, tc.wantURL, tc.wantErr)
			}
		})
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

// The head pipeline's check bucket reads GitLab's own statuses: a failed
// pipeline is a failure, not a pending check.
func TestGitLabCheckSummaryBucketsThePipelineStatus(t *testing.T) {
	t.Parallel()
	for status, want := range map[string][5]int{
		// success, failure, canceled, skipped, pending
		"success":  {1, 0, 0, 0, 0},
		"failed":   {0, 1, 0, 0, 0},
		"canceled": {0, 0, 1, 0, 0},
		"skipped":  {0, 0, 0, 1, 0},
		"running":  {0, 0, 0, 0, 1},
		"manual":   {0, 0, 0, 0, 1},
	} {
		got := gitlabCheckSummary(&gitlabPipelineRaw{Status: status})
		if buckets := [5]int{got.Success, got.Failure, got.Canceled, got.Skipped, got.Pending}; got.Total != 1 || buckets != want {
			t.Errorf("%s: summary = %+v, want buckets %v", status, got, want)
		}
	}
}
