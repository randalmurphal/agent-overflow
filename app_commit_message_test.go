package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

// ---- sanitizeCommitSubject ----

func TestSanitizeCommitSubject_StripsQuotesAndPunctuation(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`"Add login flow."`, `Add login flow`},
		{"'Fix typo'", `Fix typo`},
		{"`Refactor auth`", `Refactor auth`},
		{`Add login flow.`, `Add login flow`},
		{`  Leading and trailing  `, `Leading and trailing`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := sanitizeCommitSubject(tc.in); got != tc.want {
				t.Errorf("sanitizeCommitSubject(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeCommitSubject_KeepsOnlyFirstLine(t *testing.T) {
	in := "Add login flow\nExtra body line that should not leak into subject"
	want := "Add login flow"
	if got := sanitizeCommitSubject(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeCommitSubject_CollapsesInternalWhitespace(t *testing.T) {
	in := "Add    login\t flow"
	want := "Add login flow"
	if got := sanitizeCommitSubject(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeCommitSubject_TruncatesWithEllipsis(t *testing.T) {
	in := strings.Repeat("x", 100)
	got := sanitizeCommitSubject(in)
	// 69 chars + "..." = 72 total.
	if len([]rune(got)) > 72 {
		t.Errorf("expected <= 72 runes, got %d", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected ellipsis suffix, got %q", got)
	}
}

func TestSanitizeCommitSubject_EmptyInputYieldsEmpty(t *testing.T) {
	for _, s := range []string{"", "   ", "\n\n", `""`} {
		if got := sanitizeCommitSubject(s); got != "" {
			t.Errorf("sanitizeCommitSubject(%q) = %q, want empty", s, got)
		}
	}
}

func TestSanitizeCommitSubject_UnicodeAtBoundary(t *testing.T) {
	// 80 runes of unicode → should still truncate to <=72 runes with
	// ellipsis; multi-byte code points count as one rune each.
	in := strings.Repeat("日", 80)
	got := sanitizeCommitSubject(in)
	if len([]rune(got)) > 72 {
		t.Errorf("rune length: got %d, want <=72", len([]rune(got)))
	}
}

// ---- sanitizeCommitBody ----

func TestSanitizeCommitBody_CollapsesBlankRuns(t *testing.T) {
	in := "First paragraph.\n\n\n\nSecond paragraph."
	want := "First paragraph.\n\nSecond paragraph."
	if got := sanitizeCommitBody(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeCommitBody_EmptyIsEmpty(t *testing.T) {
	if got := sanitizeCommitBody("   \n\n "); got != "" {
		t.Errorf("expected empty for whitespace-only body, got %q", got)
	}
}

func TestSanitizeCommitBody_PreservesSingleNewlines(t *testing.T) {
	// Single newlines inside a paragraph aren't blank runs — leave them.
	in := "Line one.\nLine two.\n\nParagraph break."
	if got := sanitizeCommitBody(in); got != in {
		t.Errorf("expected no change, got %q", got)
	}
}

// ---- truncateDiffForPrompt ----

func TestTruncateDiffForPrompt_NoopBelowBudget(t *testing.T) {
	in := "short diff"
	if got := truncateDiffForPrompt(in, 1000); got != in {
		t.Errorf("expected no truncation, got %q", got)
	}
}

func TestTruncateDiffForPrompt_PreservesHeadAndTail(t *testing.T) {
	// Build a diff with distinct head / tail markers so we can verify both
	// survived the truncation.
	head := strings.Repeat("HEAD_", 2_000) // 10k bytes
	tail := strings.Repeat("TAIL_", 2_000) // 10k bytes
	middle := strings.Repeat("x", 100_000)
	diff := head + middle + tail

	got := truncateDiffForPrompt(diff, 20_000)
	if len(got) > 20_000 {
		t.Errorf("got %d bytes, expected <=20000", len(got))
	}
	if !strings.Contains(got, "HEAD_") {
		t.Errorf("expected head marker to survive")
	}
	if !strings.Contains(got, "TAIL_") {
		t.Errorf("expected tail marker to survive")
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncation marker in output")
	}
}

func TestTruncateDiffForPrompt_HandlesTinyBudget(t *testing.T) {
	// Degenerate case — caller passes a budget smaller than the marker.
	// Should still return something no larger than the budget.
	in := strings.Repeat("x", 500)
	got := truncateDiffForPrompt(in, 100)
	if len(got) > 100 {
		t.Errorf("got %d bytes, expected <=100", len(got))
	}
}

// ---- decodeClaudeCommitMessage ----

func TestDecodeClaudeCommitMessage_ExtractsStructuredOutput(t *testing.T) {
	stdout := []byte(`{"session_id":"abc"}
{"structured_output":{"subject":"Add login flow","body":"Supports SSO."}}`)
	subject, body, err := decodeClaudeCommitMessage(stdout)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if subject != "Add login flow" {
		t.Errorf("subject: got %q", subject)
	}
	if body != "Supports SSO." {
		t.Errorf("body: got %q", body)
	}
}

func TestDecodeClaudeCommitMessage_EmptyOutputErrors(t *testing.T) {
	if _, _, err := decodeClaudeCommitMessage(nil); err == nil {
		t.Error("expected error for empty output")
	}
	if _, _, err := decodeClaudeCommitMessage([]byte("\n\n\n")); err == nil {
		t.Error("expected error for whitespace-only output")
	}
}

func TestDecodeClaudeCommitMessage_MissingSubjectErrors(t *testing.T) {
	stdout := []byte(`{"structured_output":{"body":"orphaned body"}}`)
	if _, _, err := decodeClaudeCommitMessage(stdout); err == nil {
		t.Error("expected error when subject is missing")
	}
}

func TestDecodeClaudeCommitMessage_MalformedJSONErrors(t *testing.T) {
	if _, _, err := decodeClaudeCommitMessage([]byte("not json")); err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestDecodeClaudeCommitMessage_HandlesMultilineEnvelope(t *testing.T) {
	// Claude's -p JSON output can include multiple lines; we take the last
	// non-empty line as the envelope.
	stdout := []byte(`first log line
{"structured_output":{"subject":"Fix race in readLoop","body":""}}
`)
	subject, body, err := decodeClaudeCommitMessage(stdout)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if subject != "Fix race in readLoop" {
		t.Errorf("subject: got %q", subject)
	}
	if body != "" {
		t.Errorf("body: got %q, want empty", body)
	}
}

// ---- buildCommitMessagePrompt ----

func TestBuildCommitMessagePrompt_IncludesDiffAndRules(t *testing.T) {
	diff := "diff --git a/x b/x\n+added line"
	prompt := buildCommitMessagePrompt(diff)
	if !strings.Contains(prompt, diff) {
		t.Errorf("prompt should include the diff")
	}
	// Key rules that keep the output usable.
	for _, needle := range []string{"subject", "body", "Imperative", "72 characters"} {
		if !strings.Contains(prompt, needle) {
			t.Errorf("prompt missing %q", needle)
		}
	}
}

// ---- GenerateCommitMessage top-level handler ----
//
// The handler has four paths: missing thread, unsupported provider,
// empty diff, and happy path. The happy + failure paths with a real
// CLI call use a bash mock so we don't shell out to a real Claude
// binary in the test suite.

func newCommitMsgTestApp(t *testing.T) *App {
	t.Helper()
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	app.checkpoints = checkpoint.NewStore()
	return app
}

func TestGenerateCommitMessage_UnknownThreadErrors(t *testing.T) {
	app := newCommitMsgTestApp(t)
	_, err := app.GenerateCommitMessage("nope")
	if err == nil {
		t.Fatal("expected error for unknown thread id")
	}
	if !strings.Contains(err.Error(), "generate commit message") {
		t.Errorf("error should be prefixed with the operation name: %v", err)
	}
}

func TestGenerateCommitMessage_CodexThreadRejected(t *testing.T) {
	app := newCommitMsgTestApp(t)
	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID: "t-codex", Title: "x", Provider: string(provider.Codex),
		WorkspacePath: t.TempDir(), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	_, err := app.GenerateCommitMessage("t-codex")
	if err == nil {
		t.Fatal("expected error: non-Claude provider must be rejected")
	}
	if !strings.Contains(err.Error(), "codex") && !strings.Contains(err.Error(), "manually") {
		t.Errorf("error should name the unsupported provider + suggest manual entry; got: %v", err)
	}
}

func TestGenerateCommitMessage_EmptyDiffReturnsNothingToDescribe(t *testing.T) {
	app := newCommitMsgTestApp(t)
	workspace := initCommitMsgRepo(t)
	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID: "t-claude", Title: "x", Provider: string(provider.Claude),
		WorkspacePath: workspace, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Repo is clean — no uncommitted changes. The handler should
	// short-circuit before invoking the CLI.
	_, err := app.GenerateCommitMessage("t-claude")
	if err == nil {
		t.Fatal("expected error when working tree is clean")
	}
	if !strings.Contains(err.Error(), "no uncommitted changes") {
		t.Errorf("error should mention empty diff; got: %v", err)
	}
}

func TestGenerateCommitMessage_HappyPathWithMockClaude(t *testing.T) {
	app := newCommitMsgTestApp(t)
	workspace := initCommitMsgRepo(t)
	if err := os.WriteFile(filepath.Join(workspace, "README"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("dirty repo: %v", err)
	}

	// Mock `claude -p`: drains stdin then emits the tail-line envelope
	// decodeClaudeCommitMessage expects.
	mockPath := filepath.Join(t.TempDir(), "claude-mock.sh")
	mock := `#!/bin/bash
cat > /dev/null
echo '{"session_id":"mock"}'
echo '{"structured_output":{"subject":"Refine README greeting","body":"Mention the workspace layout."}}'
`
	if err := os.WriteFile(mockPath, []byte(mock), 0o755); err != nil {
		t.Fatalf("write mock: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": mockPath}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID: "t-claude", Title: "x", Provider: string(provider.Claude),
		WorkspacePath: workspace, CreatedAt: now, UpdatedAt: now,
		Model: "claude-sonnet-4-6",
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	got, err := app.GenerateCommitMessage("t-claude")
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if got.Subject != "Refine README greeting" {
		t.Errorf("subject: got %q", got.Subject)
	}
	if got.Body != "Mention the workspace layout." {
		t.Errorf("body: got %q", got.Body)
	}
}

func TestGenerateCommitMessage_SurfacesClaudeFailureAsError(t *testing.T) {
	app := newCommitMsgTestApp(t)
	workspace := initCommitMsgRepo(t)
	if err := os.WriteFile(filepath.Join(workspace, "README"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("dirty repo: %v", err)
	}

	mockPath := filepath.Join(t.TempDir(), "claude-fail.sh")
	if err := os.WriteFile(mockPath, []byte("#!/bin/bash\ncat > /dev/null\necho 'boom' 1>&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write mock: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": mockPath}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID: "t-claude", Title: "x", Provider: string(provider.Claude),
		WorkspacePath: workspace, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	_, err := app.GenerateCommitMessage("t-claude")
	if err == nil {
		t.Fatal("expected error when mock CLI exits non-zero")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should include stderr message from the mock; got: %v", err)
	}
}

// initCommitMsgRepo creates a clean git repo with one committed file so
// GetWorkingTreeDiff has a stable baseline.
func initCommitMsgRepo(t *testing.T) string {
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
