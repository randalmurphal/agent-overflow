package main

import (
	"context"
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
	"agent-overflow/internal/textgen"
)

// Unit-level coverage for the pure prompt builder, decoder, and
// sanitisers lives in internal/commitmsg. This file owns the
// App-coupled integration tests for GenerateCommitMessage.

// ---- GenerateCommitMessage top-level handler ----

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

func TestGenerateCommitMessage_EmptyStagedReturnsNothingToDescribe(t *testing.T) {
	app := newCommitMsgTestApp(t)
	workspace := initCommitMsgRepo(t)
	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID: "t-empty", ProjectID: defaultTestProjectID, Title: "x", Provider: string(provider.Claude),
		WorkspacePath: workspace, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Repo is clean — StageAll is a no-op, nothing to describe.
	_, err := app.GenerateCommitMessage("t-empty")
	if err == nil {
		t.Fatal("expected error when working tree is clean")
	}
	if !strings.Contains(err.Error(), "no uncommitted changes") {
		t.Errorf("error should mention empty diff; got: %v", err)
	}
}

func TestGenerateCommitMessage_CodexPathHappy(t *testing.T) {
	app := newCommitMsgTestApp(t)
	workspace := initCommitMsgRepo(t)
	// Dirty the repo so there's something to stage.
	if err := os.WriteFile(filepath.Join(workspace, "README"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("dirty: %v", err)
	}

	// Settings default to codex; we don't need to patch them.
	// Install the commit executor stub: write a realistic output.json
	// to the --output-last-message path so the real file-read path is
	// exercised end-to-end.
	var gotSpec textgen.CLISpec
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		gotSpec = spec
		payload := []byte(`{"subject":"Add world to README","body":"Mention the new world line."}`)
		outputPath := extractCodexOutputPath(spec.Args)
		if outputPath == "" {
			t.Fatalf("textgen.CLISpec has no --output-last-message; args=%v", spec.Args)
		}
		if err := os.WriteFile(outputPath, payload, 0o600); err != nil {
			return textgen.CLIResult{}, err
		}
		return textgen.CLIResult{ExitCode: 0}, nil
	}

	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID: "t-codex", ProjectID: defaultTestProjectID, Title: "x", Provider: string(provider.Codex),
		WorkspacePath: workspace, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	got, err := app.GenerateCommitMessage("t-codex")
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if got.Subject != "Add world to README" {
		t.Errorf("subject: got %q", got.Subject)
	}
	if got.Body != "Mention the new world line." {
		t.Errorf("body: got %q", got.Body)
	}

	// The CLI got invoked with the t3-code-style args.
	if !argsContain(gotSpec.Args, "exec") || !argsContain(gotSpec.Args, "--ephemeral") {
		t.Errorf("codex args missing 'exec --ephemeral'; got %v", gotSpec.Args)
	}
	if !argsContain(gotSpec.Args, "--model") {
		t.Errorf("codex args missing '--model'; got %v", gotSpec.Args)
	}
	// Default model for codex text generation is gpt-5.4-mini.
	if modelArg := nextArgAfter(gotSpec.Args, "--model"); modelArg != textgen.DefaultCodexModel {
		t.Errorf("codex model arg = %q, want %q", modelArg, textgen.DefaultCodexModel)
	}
	// Default reasoning effort is "low".
	if !argsContainSubstring(gotSpec.Args, `model_reasoning_effort="low"`) {
		t.Errorf("codex reasoning-effort config missing 'low'; got %v", gotSpec.Args)
	}
}

func TestGenerateCommitMessage_ClaudePathHappy(t *testing.T) {
	app := newCommitMsgTestApp(t)
	workspace := initCommitMsgRepo(t)
	if err := os.WriteFile(filepath.Join(workspace, "README"), []byte("bonjour\n"), 0o644); err != nil {
		t.Fatalf("dirty: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider": "claude",
	}); err != nil {
		t.Fatalf("select claude: %v", err)
	}

	// Claude returns the envelope as the last stdout line.
	var gotSpec textgen.CLISpec
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		gotSpec = spec
		return textgen.CLIResult{
			Stdout: `{"session_id":"abc"}
{"structured_output":{"subject":"Translate README greeting","body":""}}`,
			ExitCode: 0,
		}, nil
	}

	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID: "t-claude", ProjectID: defaultTestProjectID, Title: "x", Provider: string(provider.Claude),
		WorkspacePath: workspace, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	got, err := app.GenerateCommitMessage("t-claude")
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if got.Subject != "Translate README greeting" {
		t.Errorf("subject: got %q", got.Subject)
	}

	if !argsContain(gotSpec.Args, "-p") || !argsContain(gotSpec.Args, "--json-schema") {
		t.Errorf("claude args missing '-p --json-schema'; got %v", gotSpec.Args)
	}
	if !argsContain(gotSpec.Args, "--dangerously-skip-permissions") {
		t.Errorf("claude args missing '--dangerously-skip-permissions'; got %v", gotSpec.Args)
	}
	if modelArg := nextArgAfter(gotSpec.Args, "--model"); modelArg != textgen.DefaultClaudeModel {
		t.Errorf("claude model arg = %q, want %q", modelArg, textgen.DefaultClaudeModel)
	}
	if effortArg := nextArgAfter(gotSpec.Args, "--effort"); effortArg != "low" {
		t.Errorf("claude effort arg = %q, want low", effortArg)
	}
}

func TestGenerateCommitMessage_RoutingRespectsSettings(t *testing.T) {
	app := newCommitMsgTestApp(t)
	workspace := initCommitMsgRepo(t)
	if err := os.WriteFile(filepath.Join(workspace, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("dirty: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID: "t-route", ProjectID: defaultTestProjectID, Title: "x", Provider: string(provider.Claude),
		WorkspacePath: workspace, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Test both routes with a per-provider recognisable arg.
	tests := []struct {
		name     string
		provider string
		sentinel string
	}{
		{"codex", "codex", "exec"},
		{"claude", "claude", "-p"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := app.settings.Update(map[string]any{
				"textGenerationProvider": tc.provider,
			}); err != nil {
				t.Fatalf("set provider: %v", err)
			}

			var gotSpec textgen.CLISpec
			app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
				gotSpec = spec
				if tc.provider == "codex" {
					outputPath := extractCodexOutputPath(spec.Args)
					_ = os.WriteFile(outputPath, []byte(`{"subject":"ok","body":""}`), 0o600)
					return textgen.CLIResult{ExitCode: 0}, nil
				}
				return textgen.CLIResult{
					Stdout:   `{"structured_output":{"subject":"ok","body":""}}`,
					ExitCode: 0,
				}, nil
			}

			if _, err := app.GenerateCommitMessage("t-route"); err != nil {
				t.Fatalf("generate: %v", err)
			}
			if !argsContain(gotSpec.Args, tc.sentinel) {
				t.Errorf("%s route didn't pass its sentinel arg %q; got %v",
					tc.provider, tc.sentinel, gotSpec.Args)
			}
		})
	}
}

func TestGenerateCommitMessage_RoutingCustomEffortAndModel(t *testing.T) {
	app := newCommitMsgTestApp(t)
	workspace := initCommitMsgRepo(t)
	if err := os.WriteFile(filepath.Join(workspace, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("dirty: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider":        "codex",
		"textGenerationModel":           "gpt-5.4",
		"textGenerationReasoningEffort": "high",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID: "t-custom", ProjectID: defaultTestProjectID, Title: "x", Provider: string(provider.Claude),
		WorkspacePath: workspace, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	var gotSpec textgen.CLISpec
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		gotSpec = spec
		outputPath := extractCodexOutputPath(spec.Args)
		_ = os.WriteFile(outputPath, []byte(`{"subject":"x","body":""}`), 0o600)
		return textgen.CLIResult{ExitCode: 0}, nil
	}

	if _, err := app.GenerateCommitMessage("t-custom"); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got := nextArgAfter(gotSpec.Args, "--model"); got != "gpt-5.4" {
		t.Errorf("model = %q, want gpt-5.4", got)
	}
	if !argsContainSubstring(gotSpec.Args, `model_reasoning_effort="high"`) {
		t.Errorf("expected effort 'high'; got %v", gotSpec.Args)
	}
}

func TestGenerateCommitMessage_CLIMissingReturnsFriendlyError(t *testing.T) {
	app := newCommitMsgTestApp(t)
	workspace := initCommitMsgRepo(t)
	if err := os.WriteFile(filepath.Join(workspace, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("dirty: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID: "t-missing", ProjectID: defaultTestProjectID, Title: "x", Provider: string(provider.Claude),
		WorkspacePath: workspace, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	app.textGenerationExecutor = func(_ context.Context, _ textgen.CLISpec) (textgen.CLIResult, error) {
		// Emulate exec.LookPath failure shape — a PathError wrapping ENOENT.
		return textgen.CLIResult{}, exec.ErrNotFound
	}
	_, err := app.GenerateCommitMessage("t-missing")
	if err == nil {
		t.Fatal("expected error when CLI isn't available")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error; got: %v", err)
	}
}

func TestGenerateCommitMessage_CLIFailureSurfacesStderr(t *testing.T) {
	app := newCommitMsgTestApp(t)
	workspace := initCommitMsgRepo(t)
	if err := os.WriteFile(filepath.Join(workspace, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("dirty: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID: "t-fail", ProjectID: defaultTestProjectID, Title: "x", Provider: string(provider.Claude),
		WorkspacePath: workspace, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	app.textGenerationExecutor = func(_ context.Context, _ textgen.CLISpec) (textgen.CLIResult, error) {
		return textgen.CLIResult{
			Stderr:   "boom",
			ExitCode: 1,
		}, nil
	}
	_, err := app.GenerateCommitMessage("t-fail")
	if err == nil {
		t.Fatal("expected error when CLI exits non-zero")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected stderr 'boom' in error: %v", err)
	}
}

// TestGenerateCommitMessage_InvalidProviderCoercesToDefault — Guard that
// a malformed settings.json (provider = "bogus") does NOT crash. The
// sanitizeLoadedSettings layer coerces invalid values back to the
// default "codex", so routing still lands on a valid CLI path.
func TestGenerateCommitMessage_InvalidProviderCoercesToDefault(t *testing.T) {
	app := newCommitMsgTestApp(t)
	workspace := initCommitMsgRepo(t)
	if err := os.WriteFile(filepath.Join(workspace, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("dirty: %v", err)
	}
	// Seed a bogus provider directly on disk, bypassing the Update()
	// validation layer. sanitizeLoadedSettings on the read path will
	// coerce it back to the default.
	settingsPath := app.settings.Path()
	if err := os.WriteFile(settingsPath, []byte(`{"textGenerationProvider":"bogus"}`), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	// Wait until the file's modtime is strictly later than the cached
	// state so Get() reloads it. We explicitly bump modtime so there's
	// no wall-clock race.
	if err := os.Chtimes(settingsPath, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Confirm the coercion before we invoke the handler.
	if got := app.currentSettings().TextGenerationProvider; got != "codex" {
		t.Fatalf("expected bogus provider to be coerced to codex; got %q", got)
	}

	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID: "t-coerce", ProjectID: defaultTestProjectID, Title: "x", Provider: string(provider.Claude),
		WorkspacePath: workspace, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		// Confirm we landed on the codex path — if the default coercion
		// broke, we'd see claude's '-p' here instead.
		if !argsContain(spec.Args, "exec") {
			t.Errorf("expected codex route after coercion; got args %v", spec.Args)
		}
		outputPath := extractCodexOutputPath(spec.Args)
		_ = os.WriteFile(outputPath, []byte(`{"subject":"ok","body":""}`), 0o600)
		return textgen.CLIResult{ExitCode: 0}, nil
	}
	if _, err := app.GenerateCommitMessage("t-coerce"); err != nil {
		t.Fatalf("handler should succeed after provider coercion; got: %v", err)
	}
}

// initCommitMsgRepo creates a clean git repo with one committed file.
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

// ---- helper argument inspectors for test readability ----

func argsContain(args []string, needle string) bool {
	for _, a := range args {
		if a == needle {
			return true
		}
	}
	return false
}

func argsContainSubstring(args []string, needle string) bool {
	for _, a := range args {
		if strings.Contains(a, needle) {
			return true
		}
	}
	return false
}

func nextArgAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// extractCodexOutputPath pulls the --output-last-message path out of a
// Codex textgen.CLISpec so test stubs can populate the file the real CLI
// would write. Returns "" if the flag isn't present.
func extractCodexOutputPath(args []string) string {
	return nextArgAfter(args, "--output-last-message")
}

