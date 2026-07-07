package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/prthread"
	"agent-overflow/internal/settings"
)

// PR-thread integration tests cover the end-to-end CreateThreadFromPR
// pipeline against PATH-prepended `gh` / `glab` shims. URL/short-form
// parser correctness lives in internal/git/forge_test.go.

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

const mrIntegrationViewJSON = `{
  "title": "Big refactor",
  "description": "A large refactor touching many files.",
  "source_branch": "feature/big-refactor",
  "target_branch": "main",
  "web_url": "https://gitlab.com/group/sub/repo/-/merge_requests/77",
  "state": "opened",
  "author": {"username": "contributor"}
}`

const mrIntegrationDiff = `diff --git a/cmd/main.go b/cmd/main.go
index 1111111..2222222 100644
--- a/cmd/main.go
+++ b/cmd/main.go
@@ -1,3 +1,4 @@
 package main
+
 func main() {}
`

// installMockGhExitError installs a `gh` shim that exits non-zero on
// every invocation, emitting the given stderr message. Used to
// exercise error-surface assertions.
func installMockGhExitError(t *testing.T, stderrMsg string) {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "gh")
	script := fmt.Sprintf(`#!/bin/sh
echo %q 1>&2
exit 1
`, stderrMsg)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPR_CreateThreadFromValidURL(t *testing.T) {
	argLog := installFakeGh(t, prIntegrationViewJSON, prIntegrationDiff)

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.CreateThreadFromPR("agent/overflow", 77, string(provider.Claude), "claude-sonnet-4-6", "github")
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
	if calls := readArgLog(t, argLog); len(calls) != 2 {
		t.Fatalf("gh calls = %d, want 2 (pr view + pr diff)", len(calls))
	}
}

func TestPR_MissingGhReturnsStructuredError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	app := newTestAppWithStore(t)
	_, err := app.CreateThreadFromPR("owner/repo", 1, string(provider.Claude), "claude-sonnet-4-6", "github")
	if err == nil {
		t.Fatal("CreateThreadFromPR() error = nil, want gh-missing error")
	}
	if !strings.Contains(err.Error(), "GitHub CLI") {
		t.Fatalf("err = %v, want 'GitHub CLI' hint", err)
	}
}

func TestPR_GhReturnsNonZero(t *testing.T) {
	installMockGhExitError(t, "could not resolve to a Repository with the name 'owner/missing'")

	app := newTestAppWithStore(t)
	_, err := app.CreateThreadFromPR("owner/missing", 1, string(provider.Claude), "claude-sonnet-4-6", "github")
	if err == nil {
		t.Fatal("CreateThreadFromPR() error = nil, want gh failure")
	}
	if !strings.Contains(err.Error(), "could not resolve") {
		t.Fatalf("err = %v, want gh stderr surfaced", err)
	}
}

func TestPR_GhReturnsMalformedJSON(t *testing.T) {
	installFakeGh(t, "not json at all", prIntegrationDiff)

	app := newTestAppWithStore(t)
	_, err := app.CreateThreadFromPR("owner/repo", 1, string(provider.Claude), "claude-sonnet-4-6", "github")
	if err == nil {
		t.Fatal("CreateThreadFromPR() error = nil, want malformed JSON error")
	}
	if !strings.Contains(err.Error(), "malformed JSON") {
		t.Fatalf("err = %v, want 'malformed JSON'", err)
	}
}

func TestPR_LargeDiffTruncatedOrCapped(t *testing.T) {
	largeDiff := buildLargeDiff(prthread.MaxInlinedDiffBytes * 2)
	installFakeGh(t, prIntegrationViewJSON, largeDiff)

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.CreateThreadFromPR("agent/overflow", 77, string(provider.Claude), "claude-sonnet-4-6", "github")
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
	if len(summary) >= len(largeDiff) {
		t.Fatalf("summary length %d >= diff length %d -- expected truncation", len(summary), len(largeDiff))
	}
	if !strings.Contains(summary, "diff truncated at") {
		t.Fatalf("summary missing truncation marker:\n%s", tailForLog(summary))
	}
	if !strings.Contains(summary, "bytes omitted") {
		t.Fatalf("summary missing omitted-bytes count:\n%s", tailForLog(summary))
	}
}

func tailForLog(s string) string {
	const keep = 500
	if len(s) <= keep {
		return s
	}
	return "..." + s[len(s)-keep:]
}

func TestPR_WorkspaceResolvedFromRecents(t *testing.T) {
	t.Run("match", func(t *testing.T) {
		installFakeGh(t, prIntegrationViewJSON, prIntegrationDiff)
		app := newTestAppWithStore(t)
		app.settings = settings.NewService(t.TempDir())
		matchingClone := filepath.Join(t.TempDir(), "overflow")
		if err := os.MkdirAll(matchingClone, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		app.settings.AddRecentWorkspace(matchingClone)

		thread, err := app.CreateThreadFromPR("agent/overflow", 77, string(provider.Claude), "claude-sonnet-4-6", "github")
		if err != nil {
			t.Fatalf("CreateThreadFromPR() error = %v", err)
		}
		if thread.WorkspacePath != matchingClone {
			t.Fatalf("workspace = %q, want %q", thread.WorkspacePath, matchingClone)
		}
	})

	t.Run("no-match", func(t *testing.T) {
		installFakeGh(t, prIntegrationViewJSON, prIntegrationDiff)
		app := newTestAppWithStore(t)
		app.settings = settings.NewService(t.TempDir())
		unrelated := filepath.Join(t.TempDir(), "some-other-project")
		if err := os.MkdirAll(unrelated, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		app.settings.AddRecentWorkspace(unrelated)

		thread, err := app.CreateThreadFromPR("agent/overflow", 77, string(provider.Claude), "claude-sonnet-4-6", "github")
		if err != nil {
			t.Fatalf("CreateThreadFromPR() error = %v", err)
		}
		if thread.WorkspacePath != "" {
			t.Fatalf("workspace = %q, want empty (no match)", thread.WorkspacePath)
		}
	})
}

func TestPR_EmptyDiffStillCreatesThread(t *testing.T) {
	installFakeGh(t, prIntegrationViewJSON, "")

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.CreateThreadFromPR("agent/overflow", 77, string(provider.Claude), "claude-sonnet-4-6", "github")
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

func TestPR_InvalidPRNumberZeroOrNegative(t *testing.T) {
	argLog := installFakeGh(t, prIntegrationViewJSON, prIntegrationDiff)

	app := newTestAppWithStore(t)
	for _, num := range []int{0, -1, -99} {
		num := num
		t.Run(fmt.Sprintf("num=%d", num), func(t *testing.T) {
			_, err := app.CreateThreadFromPR("owner/repo", num, string(provider.Claude), "claude-sonnet-4-6", "github")
			if err == nil {
				t.Fatalf("CreateThreadFromPR(%d) error = nil, want rejection", num)
			}
			if !strings.Contains(err.Error(), "PR number") || !strings.Contains(err.Error(), "positive") {
				t.Fatalf("err = %v, want positive-number hint", err)
			}
		})
	}
	if calls := readArgLog(t, argLog); len(calls) != 0 {
		t.Fatalf("gh invocations = %d, want 0 (reject before exec)", len(calls))
	}
}

// --- GitLab MR integration ---------------------------------------------

func TestMR_CreateThreadFromValidGitLabSubgroup(t *testing.T) {
	argLog := installFakeGlab(t, mrIntegrationViewJSON, mrIntegrationDiff)

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.CreateThreadFromPR("group/sub/repo", 77, string(provider.Claude), "claude-sonnet-4-6", "gitlab")
	if err != nil {
		t.Fatalf("CreateThreadFromPR(gitlab subgroup) error = %v", err)
	}
	if thread.Title != "MR !77: Big refactor" {
		t.Fatalf("title = %q, want 'MR !77: Big refactor'", thread.Title)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if !strings.Contains(items[0].Summary, "```diff") {
		t.Fatalf("first item missing diff fence")
	}
	if !strings.Contains(items[0].Summary, "@contributor") {
		t.Fatalf("first item missing author from glab username mapping")
	}

	calls := readArgLog(t, argLog)
	if len(calls) != 2 {
		t.Fatalf("glab calls = %d, want 2: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "api projects/group%2Fsub%2Frepo/merge_requests/77") {
		t.Errorf("glab view call missing encoded subgroup endpoint: %q", calls[0])
	}
	if !strings.Contains(calls[1], "-R group/sub/repo") {
		t.Errorf("glab diff call missing subgroup -R: %q", calls[1])
	}
}

func TestMR_GlabReturnsNonZero(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "glab")
	script := `#!/bin/sh
echo "merge request not found" 1>&2
exit 1
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	app := newTestAppWithStore(t)
	_, err := app.CreateThreadFromPR("group/repo", 1, string(provider.Claude), "claude-sonnet-4-6", "gitlab")
	if err == nil {
		t.Fatal("CreateThreadFromPR(gitlab) error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "merge request not found") {
		t.Fatalf("err = %v, want glab stderr surfaced", err)
	}
}

func TestMR_MissingGlabReturnsStructuredError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	app := newTestAppWithStore(t)
	_, err := app.CreateThreadFromPR("group/repo", 1, string(provider.Claude), "claude-sonnet-4-6", "gitlab")
	if err == nil {
		t.Fatal("CreateThreadFromPR(gitlab) error = nil, want glab-missing error")
	}
	if !strings.Contains(err.Error(), "GitLab CLI") {
		t.Fatalf("err = %v, want 'GitLab CLI' hint", err)
	}
}

// buildLargeDiff constructs a fake unified diff roughly `targetSize`
// bytes long so we can observe the app's behavior on oversized output.
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
