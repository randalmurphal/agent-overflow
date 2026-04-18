package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// -- Agent 3 (Wave 5D) -- integration tests for CreateThreadFromPR and the PR
// URL / shorthand parser.
//
// The existing unit tests in app_bindings_test.go cover the happy path and a
// handful of edge cases. These integration tests harden the parser against
// trailing-path / query-string / host variations and exercise the end-to-end
// CreateThreadFromPR pipeline against a richer mock gh shim so we catch
// regressions in URL normalization, workspace resolution, and first-item
// persistence.
//
// Agent 1's testutil.WriteMockGhCLI was not available at the time this file
// was written; we ship an inline mock gh helper instead.

// -- Parser integration --

func TestPRParse_HTTPSFull(t *testing.T) {
	got, err := ParsePRReference("https://github.com/owner/repo/pull/123")
	if err != nil {
		t.Fatalf("ParsePRReference() error = %v", err)
	}
	if got.Owner != "owner" || got.Repo != "repo" || got.Number != 123 {
		t.Fatalf("got = %+v, want owner/repo/123", got)
	}
}

func TestPRParse_BareHost(t *testing.T) {
	got, err := ParsePRReference("github.com/owner/repo/pull/123")
	if err != nil {
		t.Fatalf("ParsePRReference() error = %v", err)
	}
	if got.Owner != "owner" || got.Repo != "repo" || got.Number != 123 {
		t.Fatalf("got = %+v, want owner/repo/123", got)
	}
}

func TestPRParse_Shorthand(t *testing.T) {
	got, err := ParsePRReference("owner/repo#123")
	if err != nil {
		t.Fatalf("ParsePRReference() error = %v", err)
	}
	if got.Owner != "owner" || got.Repo != "repo" || got.Number != 123 {
		t.Fatalf("got = %+v", got)
	}
}

// TestPRParse_MalformedNoNumber: inputs that look like a PR URL but are
// missing the number (or have a non-numeric placeholder) must be rejected.
func TestPRParse_MalformedNoNumber(t *testing.T) {
	cases := []string{
		"https://github.com/owner/repo/pull/",
		"https://github.com/owner/repo/pull/abc",
		"github.com/owner/repo/pull/",
		"owner/repo#",
		"owner/repo#notanumber",
	}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			if _, err := ParsePRReference(in); err == nil {
				t.Fatalf("ParsePRReference(%q) = nil, want error", in)
			}
		})
	}
}

// TestPRParse_MalformedWrongHost: we only accept github.com. Everything else
// (GitLab, Bitbucket, self-hosted) must be rejected so the `gh` CLI isn't
// pointed at a non-GitHub host.
func TestPRParse_MalformedWrongHost(t *testing.T) {
	cases := []string{
		"https://gitlab.com/owner/repo/pull/1",
		"https://bitbucket.org/owner/repo/pull-requests/1",
		"https://git.example.com/owner/repo/pull/1",
		"http://github.com.attacker.com/owner/repo/pull/1",
		"githubxcom/owner/repo/pull/1",
	}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			if _, err := ParsePRReference(in); err == nil {
				t.Fatalf("ParsePRReference(%q) = nil, want rejection", in)
			}
		})
	}
}

// TestPRParse_TrailingSlashOrQuery: GitHub's UI emits URLs with /files,
// ?diff=split, #comment anchors etc. The parser must strip those.
func TestPRParse_TrailingSlashOrQuery(t *testing.T) {
	cases := []struct {
		in       string
		wantNum  int
		wantRepo string
	}{
		{"https://github.com/owner/repo/pull/123/files?foo=bar", 123, "repo"},
		{"https://github.com/owner/repo/pull/42/", 42, "repo"},
		{"https://github.com/owner/repo/pull/99#issuecomment-1", 99, "repo"},
		{"https://github.com/owner/repo/pull/7?diff=split&w=1", 7, "repo"},
		{"https://github.com/owner/repo/pull/15/commits", 15, "repo"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParsePRReference(tc.in)
			if err != nil {
				t.Fatalf("ParsePRReference(%q) error = %v", tc.in, err)
			}
			if got.Number != tc.wantNum || got.Repo != tc.wantRepo {
				t.Fatalf("got = %+v, want num=%d repo=%s", got, tc.wantNum, tc.wantRepo)
			}
		})
	}
}

// TestPRParse_RepoWithDots: repos named with dots like "owner/repo.name" are
// legal GitHub names and must round-trip.
func TestPRParse_RepoWithDots(t *testing.T) {
	got, err := ParsePRReference("owner/repo.name#1")
	if err != nil {
		t.Fatalf("ParsePRReference(dots) error = %v", err)
	}
	if got.Owner != "owner" || got.Repo != "repo.name" || got.Number != 1 {
		t.Fatalf("got = %+v, want owner/repo.name/1", got)
	}

	// Same in the URL form.
	got, err = ParsePRReference("https://github.com/owner/repo.name/pull/1")
	if err != nil {
		t.Fatalf("URL ParsePRReference(dots) error = %v", err)
	}
	if got.Repo != "repo.name" {
		t.Fatalf("URL repo = %q, want repo.name", got.Repo)
	}
}

// TestPRParse_NumericOwner: GitHub org slugs may be numeric / start with digits
// (e.g. "123-org") and must be accepted.
func TestPRParse_NumericOwner(t *testing.T) {
	got, err := ParsePRReference("123-org/repo#1")
	if err != nil {
		t.Fatalf("ParsePRReference(numeric owner) error = %v", err)
	}
	if got.Owner != "123-org" || got.Repo != "repo" || got.Number != 1 {
		t.Fatalf("got = %+v, want 123-org/repo/1", got)
	}

	// And via URL.
	got, err = ParsePRReference("https://github.com/123-org/repo/pull/1")
	if err != nil {
		t.Fatalf("URL ParsePRReference(numeric owner) error = %v", err)
	}
	if got.Owner != "123-org" {
		t.Fatalf("URL owner = %q, want 123-org", got.Owner)
	}
}

// -- gh CLI interaction integration --

const prIntegrationViewJSON = `{
  "title": "Big refactor",
  "body": "A large refactor touching many files.",
  "headRefName": "feature/big-refactor",
  "baseRefName": "main",
  "url": "https://github.com/agent/overflow/pull/77",
  "files": [
    {"path": "cmd/main.go", "additions": 20, "deletions": 5},
    {"path": "internal/core.go", "additions": 3, "deletions": 1}
  ],
  "author": {"login": "contributor"},
  "state": "OPEN"
}`

const prIntegrationDiff = `diff --git a/cmd/main.go b/cmd/main.go
index 1111111..2222222 100644
--- a/cmd/main.go
+++ b/cmd/main.go
@@ -1,3 +1,4 @@
 package main
+
 func main() {}
`

// installMockGhPR is a self-contained mock gh shim tailored for the PR
// integration tests. It replaces the global lookPath + ghCommand hooks for the
// lifetime of the test. Unlike the thinner installFakeGh used elsewhere, this
// variant supports injecting different scripts per-test more ergonomically
// (by accepting pre-built JSON / diff blobs), and records every invocation.
func installMockGhPR(t *testing.T, prViewJSON, prDiff string) *ghCallRecorder {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mock gh shim assumes POSIX shell")
	}

	dir := t.TempDir()
	rec := &ghCallRecorder{}
	script := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
  pr)
    shift
    case "$1" in
      view)
        cat <<'END_OF_VIEW_JSON'
%s
END_OF_VIEW_JSON
        ;;
      diff)
        cat <<'END_OF_DIFF'
%s
END_OF_DIFF
        ;;
      *)
        echo "unknown pr subcommand: $1" 1>&2
        exit 2
        ;;
    esac
    ;;
  *)
    echo "unknown gh command: $1" 1>&2
    exit 2
    ;;
esac
`, prViewJSON, prDiff)

	binPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	prevLookPath := lookPath
	prevGhCommand := ghCommand
	lookPath = func(name string) (string, error) {
		if name == "gh" {
			return binPath, nil
		}
		return exec.LookPath(name)
	}
	ghCommand = func(args ...string) *exec.Cmd {
		rec.record(args)
		return exec.Command(binPath, args...)
	}
	t.Cleanup(func() {
		lookPath = prevLookPath
		ghCommand = prevGhCommand
	})
	return rec
}

// installMockGhExitError installs a gh shim that exits non-zero on every
// invocation, emitting the given stderr message. Used to exercise error
// surfaces.
func installMockGhExitError(t *testing.T, stderrMsg string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mock gh shim assumes POSIX shell")
	}

	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo %q 1>&2
exit 1
`, stderrMsg)
	binPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	prevLookPath := lookPath
	prevGhCommand := ghCommand
	lookPath = func(name string) (string, error) {
		if name == "gh" {
			return binPath, nil
		}
		return exec.LookPath(name)
	}
	ghCommand = func(args ...string) *exec.Cmd {
		return exec.Command(binPath, args...)
	}
	t.Cleanup(func() {
		lookPath = prevLookPath
		ghCommand = prevGhCommand
	})
}

type ghCallRecorder struct {
	mu    struct{}
	calls [][]string
}

func (r *ghCallRecorder) record(args []string) {
	copied := make([]string, len(args))
	copy(copied, args)
	r.calls = append(r.calls, copied)
}

// TestPR_CreateThreadFromValidURL: full end-to-end. The mock gh returns a real
// PR payload + diff; the app must:
//   - persist a new thread with title "PR #77: Big refactor"
//   - persist a user-role "text" item carrying the diff in a ```diff code fence
func TestPR_CreateThreadFromValidURL(t *testing.T) {
	rec := installMockGhPR(t, prIntegrationViewJSON, prIntegrationDiff)

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.CreateThreadFromPR("agent/overflow", 77, string(provider.Claude), "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("CreateThreadFromPR() error = %v", err)
	}
	if thread.Title != "PR #77: Big refactor" {
		t.Fatalf("title = %q, want 'PR #77: Big refactor'", thread.Title)
	}
	if thread.Provider != string(provider.Claude) {
		t.Fatalf("provider = %q, want claude", thread.Provider)
	}
	if thread.Mode != "chat" {
		t.Fatalf("mode = %q, want chat", thread.Mode)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	item := items[0]
	if item.Role != "user" {
		t.Fatalf("item.Role = %q, want user", item.Role)
	}
	if !strings.Contains(item.Summary, "```diff") {
		t.Fatalf("first item missing diff fence")
	}
	if !strings.Contains(item.Summary, "cmd/main.go") {
		t.Fatalf("first item missing diff body; got: %q", item.Summary[:clamp(len(item.Summary), 300)])
	}
	if !strings.Contains(item.Summary, "Big refactor") {
		t.Fatalf("first item missing PR title; got: %q", item.Summary[:clamp(len(item.Summary), 300)])
	}
	if !strings.Contains(item.Summary, "@contributor") {
		t.Fatalf("first item missing author mention; got: %q", item.Summary[:clamp(len(item.Summary), 300)])
	}
	if len(rec.calls) != 2 {
		t.Fatalf("gh calls = %d, want 2 (pr view + pr diff)", len(rec.calls))
	}
}

// TestPR_MissingGhReturnsStructuredError: when gh isn't on PATH, the error must
// include the install hint so the UI can render it verbatim.
func TestPR_MissingGhReturnsStructuredError(t *testing.T) {
	// Force lookPath to always report gh as missing.
	prevLookPath := lookPath
	lookPath = func(name string) (string, error) {
		if name == "gh" {
			return "", fmt.Errorf("exec: %q executable file not found in $PATH", name)
		}
		return exec.LookPath(name)
	}
	t.Cleanup(func() { lookPath = prevLookPath })

	app := newTestAppWithStore(t)
	_, err := app.CreateThreadFromPR("owner/repo", 1, string(provider.Claude), "claude-sonnet-4-6")
	if err == nil {
		t.Fatal("CreateThreadFromPR() error = nil, want gh-missing error")
	}
	if !strings.Contains(err.Error(), "GitHub CLI") {
		t.Fatalf("err = %v, want 'GitHub CLI' hint", err)
	}
	if !strings.Contains(err.Error(), "cli.github.com") {
		t.Fatalf("err = %v, want install URL hint", err)
	}
	if !strings.Contains(err.Error(), "gh auth login") {
		t.Fatalf("err = %v, want auth login hint", err)
	}
}

// TestPR_GhReturnsNonZero: a failing gh invocation must surface stderr output
// verbatim so users see GitHub's actual error (e.g. "could not resolve ...").
func TestPR_GhReturnsNonZero(t *testing.T) {
	installMockGhExitError(t, "could not resolve to a Repository with the name 'owner/missing'")

	app := newTestAppWithStore(t)
	_, err := app.CreateThreadFromPR("owner/missing", 1, string(provider.Claude), "claude-sonnet-4-6")
	if err == nil {
		t.Fatal("CreateThreadFromPR() error = nil, want gh failure")
	}
	if !strings.Contains(err.Error(), "could not resolve") {
		t.Fatalf("err = %v, want gh stderr surfaced", err)
	}
}

// TestPR_GhReturnsMalformedJSON: if gh's output is mangled (updated gh CLI,
// broken pipe, etc) the app must report a clear parsing error.
func TestPR_GhReturnsMalformedJSON(t *testing.T) {
	installMockGhPR(t, "not json at all", prIntegrationDiff)

	app := newTestAppWithStore(t)
	_, err := app.CreateThreadFromPR("owner/repo", 1, string(provider.Claude), "claude-sonnet-4-6")
	if err == nil {
		t.Fatal("CreateThreadFromPR() error = nil, want malformed JSON error")
	}
	if !strings.Contains(err.Error(), "malformed JSON") {
		t.Fatalf("err = %v, want 'malformed JSON'", err)
	}
}

// TestPR_LargeDiffTruncatedOrCapped: gh can return very large diffs (e.g.
// vendored dependency bumps). The app caps the inlined diff at
// MaxInlinedPRDiffBytes and appends a visible truncation marker so oversized
// PRs don't blow up the SQLite row or the frontend render.
func TestPR_LargeDiffTruncatedOrCapped(t *testing.T) {
	largeDiff := buildLargeDiff(MaxInlinedPRDiffBytes * 2)
	installMockGhPR(t, prIntegrationViewJSON, largeDiff)

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.CreateThreadFromPR("agent/overflow", 77, string(provider.Claude), "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("CreateThreadFromPR(largeDiff) error = %v", err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	summary := items[0].Summary
	// The body must stay well below the raw diff length — truncation happened.
	if len(summary) >= len(largeDiff) {
		t.Fatalf("summary length %d >= diff length %d -- expected truncation", len(summary), len(largeDiff))
	}
	// A clear truncation marker must be present so the agent (and reviewers)
	// see explicit evidence of the omission.
	if !strings.Contains(summary, "diff truncated at") {
		t.Fatalf("summary missing truncation marker:\n%s", tailForLog(summary))
	}
	if !strings.Contains(summary, "bytes omitted") {
		t.Fatalf("summary missing omitted-bytes count:\n%s", tailForLog(summary))
	}
}

// tailForLog returns the last few hundred bytes of a string so we can include
// the truncation marker in a failure message without dumping the full body.
func tailForLog(s string) string {
	const keep = 500
	if len(s) <= keep {
		return s
	}
	return "..." + s[len(s)-keep:]
}

// TestPR_WorkspaceResolvedFromRecents: an entry in settings.RecentWorkspaces
// whose basename matches the PR repo should be auto-selected as the workspace.
// When nothing matches, WorkspacePath must remain empty so the UI can prompt.
func TestPR_WorkspaceResolvedFromRecents(t *testing.T) {
	installMockGhPR(t, prIntegrationViewJSON, prIntegrationDiff)

	t.Run("match", func(t *testing.T) {
		app := newTestAppWithStore(t)
		app.settings = settings.NewService(t.TempDir())
		matchingClone := filepath.Join(t.TempDir(), "overflow")
		if err := os.MkdirAll(matchingClone, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		app.settings.AddRecentWorkspace(matchingClone)

		thread, err := app.CreateThreadFromPR("agent/overflow", 77, string(provider.Claude), "claude-sonnet-4-6")
		if err != nil {
			t.Fatalf("CreateThreadFromPR() error = %v", err)
		}
		if thread.WorkspacePath != matchingClone {
			t.Fatalf("workspace = %q, want %q", thread.WorkspacePath, matchingClone)
		}
	})

	t.Run("no-match", func(t *testing.T) {
		app := newTestAppWithStore(t)
		app.settings = settings.NewService(t.TempDir())
		unrelated := filepath.Join(t.TempDir(), "some-other-project")
		if err := os.MkdirAll(unrelated, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		app.settings.AddRecentWorkspace(unrelated)

		thread, err := app.CreateThreadFromPR("agent/overflow", 77, string(provider.Claude), "claude-sonnet-4-6")
		if err != nil {
			t.Fatalf("CreateThreadFromPR() error = %v", err)
		}
		if thread.WorkspacePath != "" {
			t.Fatalf("workspace = %q, want empty (no match)", thread.WorkspacePath)
		}
	})
}

// TestPR_EmptyDiffStillCreatesThread: some PRs (docs-only, empty branches) have
// an empty diff. The thread must still be created with the metadata, only the
// patch section will be empty. We observe the code block is still emitted
// (a zero-byte fenced block) -- that's fine for the model to read.
func TestPR_EmptyDiffStillCreatesThread(t *testing.T) {
	installMockGhPR(t, prIntegrationViewJSON, "")

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.CreateThreadFromPR("agent/overflow", 77, string(provider.Claude), "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("CreateThreadFromPR(emptyDiff) error = %v", err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	summary := items[0].Summary
	if !strings.Contains(summary, "Big refactor") {
		t.Fatalf("summary missing PR title; got: %q", summary[:clamp(len(summary), 300)])
	}
	if !strings.Contains(summary, "```diff") {
		t.Fatalf("summary missing diff fence marker even for empty diff; got: %q", summary[:clamp(len(summary), 300)])
	}
}

// TestPR_InvalidPRNumberZeroOrNegative: CreateThreadFromPR must reject
// non-positive PR numbers without even attempting to invoke gh.
func TestPR_InvalidPRNumberZeroOrNegative(t *testing.T) {
	rec := installMockGhPR(t, prIntegrationViewJSON, prIntegrationDiff)

	app := newTestAppWithStore(t)
	for _, num := range []int{0, -1, -99} {
		num := num
		t.Run(fmt.Sprintf("num=%d", num), func(t *testing.T) {
			_, err := app.CreateThreadFromPR("owner/repo", num, string(provider.Claude), "claude-sonnet-4-6")
			if err == nil {
				t.Fatalf("CreateThreadFromPR(%d) error = nil, want rejection", num)
			}
			if !strings.Contains(err.Error(), "PR number") || !strings.Contains(err.Error(), "positive") {
				t.Fatalf("err = %v, want positive-number hint", err)
			}
		})
	}
	// No gh invocations should have happened -- the reject path runs before
	// the binary lookup.
	if len(rec.calls) != 0 {
		t.Fatalf("gh invocations = %d, want 0 (reject before exec)", len(rec.calls))
	}
}

// buildLargeDiff constructs a fake unified diff roughly `targetSize` bytes long
// so we can observe the app's behavior on oversized gh output.
func buildLargeDiff(targetSize int) string {
	var b strings.Builder
	b.WriteString("diff --git a/big.txt b/big.txt\nindex 1..2 100644\n--- a/big.txt\n+++ b/big.txt\n@@ -1,1 +1,2 @@\n existing line\n")
	for b.Len() < targetSize {
		b.WriteString("+A line of content added to simulate a very large patch body.\n")
	}
	return b.String()
}

func clamp(n, max int) int {
	if n < max {
		return n
	}
	return max
}
