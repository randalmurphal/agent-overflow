package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// installFakeGh installs a PATH-prepended `gh` shim that responds to
// `pr view` with onView and `pr diff` with onDiff. The recorded-args
// log file is returned so callers can verify the invocations.
func installFakeGh(t *testing.T, onView, onDiff string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake gh shim assumes a POSIX shell")
	}
	dir := t.TempDir()
	argLog := filepath.Join(dir, "calls.log")
	binPath := filepath.Join(dir, "gh")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
echo "$@" >> %q
case "$1" in
  pr)
    shift
    case "$1" in
      view)
        cat <<'END_OF_VIEW'
%s
END_OF_VIEW
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
`, argLog, onView, onDiff)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argLog
}

// installFakeGlab installs a PATH-prepended `glab` shim that responds
// to `mr view` with onView and `mr diff` with onDiff.
func installFakeGlab(t *testing.T, onView, onDiff string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake glab shim assumes a POSIX shell")
	}
	dir := t.TempDir()
	argLog := filepath.Join(dir, "calls.log")
	binPath := filepath.Join(dir, "glab")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
echo "$@" >> %q
case "$1" in
  mr)
    shift
    case "$1" in
      view)
        cat <<'END_OF_VIEW'
%s
END_OF_VIEW
        ;;
      diff)
        cat <<'END_OF_DIFF'
%s
END_OF_DIFF
        ;;
      *)
        echo "unknown mr subcommand: $1" 1>&2
        exit 2
        ;;
    esac
    ;;
  *)
    echo "unknown glab command: $1" 1>&2
    exit 2
    ;;
esac
`, argLog, onView, onDiff)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake glab: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argLog
}

func readArgLog(t *testing.T, argLog string) []string {
	t.Helper()
	data, err := os.ReadFile(argLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read arg log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

const sampleViewJSON = `{
  "title": "Add PR import",
  "body": "Lets users start threads from PR URLs.",
  "headRefName": "feature/pr-import",
  "baseRefName": "main",
  "url": "https://github.com/owner/repo/pull/42",
  "files": [{"path": "app.go", "additions": 10, "deletions": 2}],
  "author": {"login": "octocat"},
  "state": "OPEN"
}`

const samplePRDiff = `diff --git a/app.go b/app.go
index abc..def 100644
--- a/app.go
+++ b/app.go
@@ -1,3 +1,6 @@
 func foo() {}
+
+func bar() {}
+
`

const sampleMRViewJSON = `{
  "title": "Add MR import",
  "description": "Imports MRs from GitLab into a thread.",
  "source_branch": "feature/mr-import",
  "target_branch": "main",
  "web_url": "https://gitlab.com/group/repo/-/merge_requests/9",
  "state": "opened",
  "author": {"username": "alice"}
}`

const sampleMRDiff = `diff --git a/app.go b/app.go
index 111..222 100644
--- a/app.go
+++ b/app.go
@@ -1,3 +1,4 @@
 func foo() {}
+func mr() {}
`

func TestCreateThreadFromPRCreatesThreadWithFirstItem(t *testing.T) {
	installFakeGh(t, sampleViewJSON, samplePRDiff)

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.CreateThreadFromPR("owner/repo", 42, string(provider.Claude), "claude-sonnet-4-6", "github")
	if err != nil {
		t.Fatalf("CreateThreadFromPR() error = %v", err)
	}
	if !strings.HasPrefix(thread.Title, "PR #42: ") {
		t.Fatalf("Title = %q, want PR #42 prefix", thread.Title)
	}
	if thread.Provider != string(provider.Claude) {
		t.Fatalf("Provider = %q, want claude", thread.Provider)
	}
	if thread.Model != "claude-sonnet-4-6" {
		t.Fatalf("Model = %q", thread.Model)
	}
	if thread.Mode != "chat" {
		t.Fatalf("Mode = %q, want chat", thread.Mode)
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
	if item.Kind != "user_text" {
		t.Fatalf("item.Kind = %q, want user_text", item.Kind)
	}
	if item.TurnIndex != 1 {
		t.Fatalf("item.TurnIndex = %d, want 1", item.TurnIndex)
	}
	if !strings.Contains(item.Summary, "```diff") {
		t.Fatalf("item.Summary missing diff fence; got first 200 chars: %q", item.Summary[:min(200, len(item.Summary))])
	}
	if !strings.Contains(item.Summary, "Add PR import") {
		t.Fatalf("item.Summary missing PR title; got first 200: %q", item.Summary[:min(200, len(item.Summary))])
	}
	if !strings.Contains(item.Summary, "bar()") {
		t.Fatalf("item.Summary missing diff content; got: %q", item.Summary[:min(300, len(item.Summary))])
	}
}

func TestCreateThreadFromPRDefaultsToGithubWhenForgeEmpty(t *testing.T) {
	installFakeGh(t, sampleViewJSON, samplePRDiff)
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.CreateThreadFromPR("owner/repo", 42, string(provider.Claude), "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("CreateThreadFromPR() with empty forge error = %v", err)
	}
	if !strings.HasPrefix(thread.Title, "PR #42:") {
		t.Errorf("Title = %q, want github 'PR #' prefix", thread.Title)
	}
}

func TestCreateThreadFromPRMissingGh(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	app := newTestAppWithStore(t)
	_, err := app.CreateThreadFromPR("owner/repo", 42, string(provider.Claude), "claude-sonnet-4-6", "github")
	if err == nil {
		t.Fatal("CreateThreadFromPR() error = nil, want gh-missing error")
	}
	if !strings.Contains(err.Error(), "GitHub CLI") {
		t.Fatalf("error = %v, want gh-missing hint", err)
	}
}

func TestCreateThreadFromPRGhReturnsMalformedJSON(t *testing.T) {
	installFakeGh(t, `{not json`, samplePRDiff)

	app := newTestAppWithStore(t)
	_, err := app.CreateThreadFromPR("owner/repo", 42, string(provider.Claude), "claude-sonnet-4-6", "github")
	if err == nil {
		t.Fatal("CreateThreadFromPR() error = nil, want malformed JSON error")
	}
	if !strings.Contains(err.Error(), "malformed JSON") {
		t.Fatalf("error = %v, want malformed JSON", err)
	}
}

func TestCreateThreadFromPRGhReturnsError(t *testing.T) {
	// Install a shim that exits non-zero for every gh subcommand.
	dir := t.TempDir()
	binPath := filepath.Join(dir, "gh")
	script := `#!/bin/sh
echo "could not resolve to a Repository with the name" 1>&2
exit 1
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	app := newTestAppWithStore(t)
	_, err := app.CreateThreadFromPR("owner/missing-repo", 99, string(provider.Claude), "claude-sonnet-4-6", "github")
	if err == nil {
		t.Fatal("CreateThreadFromPR() error = nil, want gh failure")
	}
	if !strings.Contains(err.Error(), "could not resolve") {
		t.Fatalf("error = %v, want gh stderr", err)
	}
}

func TestCreateThreadFromPRRejectsInvalidProject(t *testing.T) {
	installFakeGh(t, sampleViewJSON, samplePRDiff)
	app := newTestAppWithStore(t)

	for _, bad := range []string{"", "nofoo", "foo/", "/bar"} {
		if _, err := app.CreateThreadFromPR(bad, 1, string(provider.Claude), "claude-sonnet-4-6", "github"); err == nil {
			t.Fatalf("CreateThreadFromPR(%q) error = nil, want validation error", bad)
		}
	}
	// github rejects 3-segment paths; gitlab would accept them.
	if _, err := app.CreateThreadFromPR("foo/bar/baz", 1, string(provider.Claude), "claude-sonnet-4-6", "github"); err == nil {
		t.Fatal("CreateThreadFromPR with 3-segment github project error = nil, want validation error")
	}
}

// TestCreateThreadFromPRRejectsUnsafeProjectSegments pins the
// defense-in-depth segment validation. Even though the CLI argv path
// is shell-safe (we never interpolate via a shell), pathological
// segment values are rejected at the boundary so they don't reach
// SQLite rows or workspace-suffix matching.
func TestCreateThreadFromPRRejectsUnsafeProjectSegments(t *testing.T) {
	app := newTestAppWithStore(t)

	cases := []struct {
		name    string
		project string
		forge   string
	}{
		{"github leading dash owner", "-flag/repo", "github"},
		{"github leading dash repo", "owner/-flag", "github"},
		{"github double dot", "owner/..", "github"},
		{"github embedded null", "owner/repo\x00", "github"},
		{"github internal newline", "own\ner/repo", "github"},
		{"gitlab dot segment", "group/./repo", "gitlab"},
		{"gitlab traversal", "group/../repo", "gitlab"},
		{"gitlab leading dash subgroup", "-flag/sub/repo", "gitlab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.CreateThreadFromPR(tc.project, 1, string(provider.Claude), "claude-sonnet-4-6", tc.forge)
			if err == nil {
				t.Fatalf("CreateThreadFromPR(%q, forge=%q) error = nil, want rejection", tc.project, tc.forge)
			}
		})
	}
}

func TestCreateThreadFromPRRejectsInvalidInputs(t *testing.T) {
	installFakeGh(t, sampleViewJSON, samplePRDiff)
	app := newTestAppWithStore(t)

	if _, err := app.CreateThreadFromPR("owner/repo", 0, string(provider.Claude), "claude-sonnet-4-6", "github"); err == nil {
		t.Fatal("number=0 should fail")
	}
	if _, err := app.CreateThreadFromPR("owner/repo", -3, string(provider.Claude), "claude-sonnet-4-6", "github"); err == nil {
		t.Fatal("number<0 should fail")
	}
	if _, err := app.CreateThreadFromPR("owner/repo", 1, "", "claude-sonnet-4-6", "github"); err == nil {
		t.Fatal("empty provider should fail")
	}
	thread, err := app.CreateThreadFromPR("owner/repo", 1, string(provider.Claude), "", "github")
	if err != nil {
		t.Fatalf("empty model should seed provider fallback: %v", err)
	}
	if thread.Model == "" {
		t.Fatal("empty model produced blank thread model")
	}
}

func TestCreateThreadFromPRRejectsUnsupportedForge(t *testing.T) {
	app := newTestAppWithStore(t)
	_, err := app.CreateThreadFromPR("owner/repo", 1, string(provider.Claude), "claude-sonnet-4-6", "bitbucket")
	if err == nil {
		t.Fatal("expected error for unsupported forge")
	}
	if !strings.Contains(err.Error(), "unsupported forge") {
		t.Fatalf("error = %v, want unsupported-forge hint", err)
	}
}

func TestCreateThreadFromPRResolvesRecentWorkspace(t *testing.T) {
	installFakeGh(t, sampleViewJSON, samplePRDiff)

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	// Seed a local clone path whose basename matches the repo.
	workspace := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	app.settings.AddRecentWorkspace(workspace)

	thread, err := app.CreateThreadFromPR("owner/repo", 42, string(provider.Claude), "claude-sonnet-4-6", "github")
	if err != nil {
		t.Fatalf("CreateThreadFromPR() error = %v", err)
	}
	if thread.WorkspacePath != workspace {
		t.Fatalf("WorkspacePath = %q, want %q", thread.WorkspacePath, workspace)
	}
}

func TestCreateThreadFromPRInvokesExpectedGhCommands(t *testing.T) {
	argLog := installFakeGh(t, sampleViewJSON, samplePRDiff)

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	if _, err := app.CreateThreadFromPR("owner/repo", 42, string(provider.Claude), "claude-sonnet-4-6", "github"); err != nil {
		t.Fatalf("CreateThreadFromPR() error = %v", err)
	}
	calls := readArgLog(t, argLog)
	if len(calls) != 2 {
		t.Fatalf("got %d gh invocations, want 2: %v", len(calls), calls)
	}
	view := calls[0]
	diff := calls[1]
	if !strings.HasPrefix(view, "pr view") {
		t.Fatalf("first call = %q, want 'pr view ...'", view)
	}
	wantFlags := []string{"--repo", "owner/repo", "42", "--json"}
	for _, flag := range wantFlags {
		if !strings.Contains(view, flag) {
			t.Fatalf("pr view args missing %q: %q", flag, view)
		}
	}
	if !strings.HasPrefix(diff, "pr diff") {
		t.Fatalf("second call = %q, want 'pr diff ...'", diff)
	}
}

// --- GitLab MR parallel tests -------------------------------------------

func TestCreateThreadFromMRCreatesThreadWithFirstItem(t *testing.T) {
	installFakeGlab(t, sampleMRViewJSON, sampleMRDiff)

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.CreateThreadFromPR("group/repo", 9, string(provider.Claude), "claude-sonnet-4-6", "gitlab")
	if err != nil {
		t.Fatalf("CreateThreadFromPR(gitlab) error = %v", err)
	}
	if !strings.HasPrefix(thread.Title, "MR !9: ") {
		t.Fatalf("Title = %q, want 'MR !9:' prefix", thread.Title)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	item := items[0]
	if !strings.Contains(item.Summary, "# MR !9:") {
		t.Fatalf("item.Summary missing MR header; got: %q", item.Summary[:min(200, len(item.Summary))])
	}
	if !strings.Contains(item.Summary, "Add MR import") {
		t.Fatalf("item.Summary missing MR title; got: %q", item.Summary[:min(200, len(item.Summary))])
	}
	if !strings.Contains(item.Summary, "@alice") {
		t.Fatalf("item.Summary missing author; got: %q", item.Summary[:min(300, len(item.Summary))])
	}
}

func TestCreateThreadFromMRAcceptsSubgroupNamespace(t *testing.T) {
	argLog := installFakeGlab(t, sampleMRViewJSON, sampleMRDiff)

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.CreateThreadFromPR("group/sub1/sub2/repo", 9, string(provider.Claude), "claude-sonnet-4-6", "gitlab")
	if err != nil {
		t.Fatalf("CreateThreadFromPR(subgroup) error = %v", err)
	}
	// The anchor should preserve the full namespace.
	if !strings.Contains(thread.ProjectPath, "group/sub1/sub2/repo") {
		t.Errorf("ProjectPath = %q, want full subgroup chain", thread.ProjectPath)
	}

	// glab must have been invoked with the full namespace via -R.
	calls := readArgLog(t, argLog)
	if len(calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2: %v", len(calls), calls)
	}
	for _, call := range calls {
		if !strings.Contains(call, "-R group/sub1/sub2/repo") {
			t.Errorf("call missing -R flag with subgroup: %q", call)
		}
	}
}

func TestCreateThreadFromMRMissingGlab(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	app := newTestAppWithStore(t)
	_, err := app.CreateThreadFromPR("group/repo", 9, string(provider.Claude), "claude-sonnet-4-6", "gitlab")
	if err == nil {
		t.Fatal("CreateThreadFromPR(gitlab) error = nil, want glab-missing error")
	}
	if !strings.Contains(err.Error(), "GitLab CLI") {
		t.Fatalf("error = %v, want glab-missing hint", err)
	}
}

func TestCreateThreadFromMRRejectsSingleSegmentProject(t *testing.T) {
	installFakeGlab(t, sampleMRViewJSON, sampleMRDiff)
	app := newTestAppWithStore(t)

	if _, err := app.CreateThreadFromPR("single", 1, string(provider.Claude), "claude-sonnet-4-6", "gitlab"); err == nil {
		t.Fatal("expected error for single-segment gitlab project")
	}
}

func TestCreateThreadFromMRAnchorIncludesForge(t *testing.T) {
	installFakeGlab(t, sampleMRViewJSON, sampleMRDiff)
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.CreateThreadFromPR("group/repo", 9, string(provider.Claude), "claude-sonnet-4-6", "gitlab")
	if err != nil {
		t.Fatalf("CreateThreadFromPR() error = %v", err)
	}
	// No matching local clone — anchor should be pr://gitlab/group/repo.
	if !strings.HasPrefix(thread.ProjectPath, "pr://gitlab/") {
		t.Errorf("ProjectPath = %q, want pr://gitlab/ prefix", thread.ProjectPath)
	}
}

// --- Helpers ------------------------------------------------------------

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Bug C4 regression: when a PR diff contains a literal triple-backtick run
// (e.g. a patch that modifies a README's own fenced block), the default
// triple-backtick fence used by buildPRUserMessage would close prematurely
// and the rest of the diff would escape the code block, confusing the
// provider. The fix picks a fence length one longer than the longest
// backtick run found in the content.

func TestBuildPRUserMessageFenceBeatsTripleBacktick(t *testing.T) {
	ref := gitops.PRReference{Forge: "github", Namespace: "acme", Repo: "tool", Number: 1}
	meta := gitops.PRMetadata{Title: "Demo"}
	diff := "diff --git a/README.md b/README.md\n" +
		"+ Example:\n" +
		"+ ```go\n" +
		"+ fmt.Println(\"hello\")\n" +
		"+ ```\n"

	msg := buildPRUserMessage(ref, meta, diff)
	fence := patchFenceFrom(t, msg)
	// Markdown closes a fence at the first run that matches the opener.
	// If the fence is only 3 backticks long, the ```go line inside the
	// diff closes the block prematurely and the rest of the diff escapes
	// the code block. The fence must be strictly longer than any inner
	// run so that doesn't happen.
	if len(fence) <= 3 {
		t.Fatalf("fence %q is too short to contain inner ``` runs", fence)
	}
	patch := extractPatchBlock(t, msg)
	if !strings.Contains(patch, "fmt.Println(\"hello\")") {
		t.Fatalf("patch content lost; got %q", patch)
	}
	if !strings.Contains(patch, "```go") {
		t.Fatalf("inner triple-backtick line not preserved; got %q", patch)
	}
}

func TestBuildPRUserMessageFenceOutlivesLongestRun(t *testing.T) {
	ref := gitops.PRReference{Forge: "github", Namespace: "acme", Repo: "tool", Number: 2}
	meta := gitops.PRMetadata{Title: "Huge fences"}
	// A run of ten backticks on its own line — the fence must be at least
	// 11 characters long so the patch block survives.
	diff := "+ " + strings.Repeat("`", 10) + "\n+ not closed yet\n"

	msg := buildPRUserMessage(ref, meta, diff)
	fence := patchFenceFrom(t, msg)
	if len(fence) < 11 {
		t.Fatalf("expected fence >= 11 backticks, got %d: %q", len(fence), fence)
	}
	patch := extractPatchBlock(t, msg)
	if !strings.Contains(patch, strings.Repeat("`", 10)) {
		t.Fatalf("10-backtick run dropped from patch; got %q", patch)
	}
	if !strings.Contains(patch, "not closed yet") {
		t.Fatalf("line after the backtick run lost; got %q", patch)
	}
}

func TestBuildPRUserMessageFenceFallsBackToThree(t *testing.T) {
	ref := gitops.PRReference{Forge: "github", Namespace: "acme", Repo: "tool", Number: 3}
	meta := gitops.PRMetadata{Title: "Plain diff"}
	diff := "diff --git a/foo.go b/foo.go\n+ println(\"hi\")\n"

	msg := buildPRUserMessage(ref, meta, diff)
	fence := patchFenceFrom(t, msg)
	if fence != "```" {
		t.Fatalf("expected triple-backtick fence for backtick-free diff, got %q", fence)
	}
}

func TestBuildPRUserMessageGitLabHeaderUsesMRSigil(t *testing.T) {
	ref := gitops.PRReference{Forge: "gitlab", Namespace: "group", Repo: "tool", Number: 7}
	meta := gitops.PRMetadata{Title: "Demo MR"}

	msg := buildPRUserMessage(ref, meta, "diff --git a/x b/x\n")
	if !strings.HasPrefix(msg, "# MR !7: Demo MR") {
		t.Fatalf("gitlab header missing; got prefix %q", msg[:min(60, len(msg))])
	}
}

// extractPatchBlock locates the "## Patch" section and returns the content
// between the opening and closing fence. Fails the test if the structure
// doesn't match so assertions stay readable.
func extractPatchBlock(t *testing.T, msg string) string {
	t.Helper()
	patchHeader := "## Patch\n\n"
	idx := strings.Index(msg, patchHeader)
	if idx < 0 {
		t.Fatalf("message missing Patch section: %q", msg)
	}
	rest := msg[idx+len(patchHeader):]

	// The first line after the header is the opening fence + language tag.
	newline := strings.IndexByte(rest, '\n')
	if newline < 0 {
		t.Fatalf("message ends abruptly after Patch header: %q", msg)
	}
	fenceLine := rest[:newline]
	fence := strings.TrimSuffix(fenceLine, "diff")
	body := rest[newline+1:]

	// Find the matching closing fence on its own line.
	closing := "\n" + fence + "\n"
	end := strings.LastIndex(body, closing)
	if end < 0 {
		t.Fatalf("no closing fence %q found in message %q", fence, msg)
	}
	return body[:end]
}

func patchFenceFrom(t *testing.T, msg string) string {
	t.Helper()
	patchHeader := "## Patch\n\n"
	idx := strings.Index(msg, patchHeader)
	if idx < 0 {
		t.Fatalf("message missing Patch section: %q", msg)
	}
	rest := msg[idx+len(patchHeader):]
	newline := strings.IndexByte(rest, '\n')
	if newline < 0 {
		t.Fatalf("message ends abruptly after Patch header: %q", msg)
	}
	fenceLine := rest[:newline]
	return strings.TrimSuffix(fenceLine, "diff")
}

// Integration-level Bug C6 regression: exercise the full CreateThreadFromPR
// path with a multibyte title so the byte-boundary slicing bug manifests.
// Without the fix, the sliced title is invalid UTF-8 and the SQLite row
// downstream either rejects or renders a replacement character.
func TestCreateThreadFromPRPreservesMultibyteTitle(t *testing.T) {
	// A 150-CJK-rune title — well past the 120-rune cap, so it must be
	// truncated. The rune-boundary truncation keeps the result valid.
	longTitle := strings.Repeat("你", 150)
	viewJSON := fmt.Sprintf(`{
	  "title": %q,
	  "body": "",
	  "headRefName": "feature",
	  "baseRefName": "main",
	  "url": "https://github.com/owner/repo/pull/1",
	  "files": [],
	  "author": {"login": "u"},
	  "state": "OPEN"
	}`, longTitle)
	installFakeGh(t, viewJSON, "diff --git a/x b/x\n")

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.CreateThreadFromPR("owner/repo", 1, string(provider.Claude), "claude-sonnet-4-6", "github")
	if err != nil {
		t.Fatalf("CreateThreadFromPR() error = %v", err)
	}
	if !utf8.ValidString(thread.Title) {
		t.Fatalf("Title is not valid UTF-8: %q", thread.Title)
	}
	// Title must not be longer than our 120-rune ceiling.
	if utf8.RuneCountInString(thread.Title) > 120 {
		t.Fatalf("rune count = %d, want <= 120", utf8.RuneCountInString(thread.Title))
	}
}

// Bug C6 regression: titles used to be sliced at byte 117, which would
// split a UTF-8 codepoint mid-sequence and produce an invalid-UTF-8 string
// (rendered as a question-mark replacement character downstream). The fix
// truncates on rune boundaries.

func TestTruncatePRTitleASCIIShort(t *testing.T) {
	title := "PR #1: tiny"
	got := truncatePRTitle(title)
	if got != title {
		t.Fatalf("got %q, want %q (unchanged)", got, title)
	}
}

func TestTruncatePRTitleASCIIAtBoundary(t *testing.T) {
	// Exactly 120 runes; must pass through unchanged.
	title := strings.Repeat("a", 120)
	got := truncatePRTitle(title)
	if got != title {
		t.Fatalf("120-rune title mutated: got %q", got)
	}
}

func TestTruncatePRTitleASCIIOverflow(t *testing.T) {
	// 200 ASCII runes — must cut to 117 + "...".
	title := strings.Repeat("a", 200)
	got := truncatePRTitle(title)
	if utf8.RuneCountInString(got) != 120 {
		t.Fatalf("rune count = %d, want 120", utf8.RuneCountInString(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("missing ellipsis: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("output not valid UTF-8: %q", got)
	}
}

func TestTruncatePRTitleMultibyteRunes(t *testing.T) {
	// 200 ASCII + 20 three-byte CJK runes. Byte-based slicing at 117 would
	// split a CJK rune; rune-based truncation must keep it whole. The
	// result must always be valid UTF-8 regardless of where the cut falls.
	title := strings.Repeat("a", 200) + strings.Repeat("你", 20)
	got := truncatePRTitle(title)
	if !utf8.ValidString(got) {
		t.Fatalf("output not valid UTF-8: %q", got)
	}
	if utf8.RuneCountInString(got) != 120 {
		t.Fatalf("rune count = %d, want 120", utf8.RuneCountInString(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("missing ellipsis: %q", got)
	}
}

func TestTruncatePRTitleLeadingMultibyte(t *testing.T) {
	// The cut lands squarely inside the multibyte run — must still be
	// valid and keep the expected rune count. Uses 150 CJK runes so the
	// whole string is multibyte.
	title := strings.Repeat("你", 150)
	got := truncatePRTitle(title)
	if !utf8.ValidString(got) {
		t.Fatalf("output not valid UTF-8: %q", got)
	}
	if utf8.RuneCountInString(got) != 120 {
		t.Fatalf("rune count = %d, want 120", utf8.RuneCountInString(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("missing ellipsis: %q", got)
	}
	// The body must be exactly 117 '你' runes followed by the ellipsis.
	body := strings.TrimSuffix(got, "...")
	if utf8.RuneCountInString(body) != 117 {
		t.Fatalf("body rune count = %d, want 117", utf8.RuneCountInString(body))
	}
	if strings.Contains(body, "a") {
		t.Fatalf("unexpected ASCII in body: %q", body)
	}
}

func TestTruncatePRTitleCombiningMarkPreservesValidity(t *testing.T) {
	// "é" as a two-rune NFD sequence (e + U+0301 combining acute) — the
	// combiner *can* be separated from its base, but the output must
	// still be valid UTF-8 (the combining mark alone is a real rune).
	// We don't promise NFC/NFD integrity, just that we never emit
	// invalid UTF-8.
	baseWithCombiner := "é"
	title := strings.Repeat(baseWithCombiner, 100) // 200 runes total
	got := truncatePRTitle(title)
	if !utf8.ValidString(got) {
		t.Fatalf("output not valid UTF-8: %q", got)
	}
	if utf8.RuneCountInString(got) != 120 {
		t.Fatalf("rune count = %d, want 120", utf8.RuneCountInString(got))
	}
}

func TestFenceForContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"empty", "", "```"},
		{"no backticks", "hello world", "```"},
		{"single backtick", "`x`", "```"},
		{"double backtick", "``x``", "```"},
		{"triple backtick", "```", "````"},
		{"four backticks", "````", "`````"},
		{"backtick runs split by content", "``` foo ```", "````"},
		{"longest run wins", "``\nhello\n````\nworld\n```", "`````"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fenceForContent(tt.content)
			if got != tt.want {
				t.Fatalf("fenceForContent(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}
