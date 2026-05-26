package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	attachmentstore "agent-overflow/internal/attachment"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/threadtitle"
	"agent-overflow/internal/triage"
)

// TestMaybeGenerateThreadTitleAppliesGeneratedTitleAndEmits covers the happy
// path of maybeGenerateThreadTitle → generatedThreadTitle →
// applyGeneratedThreadTitle. The thread title advances from the default to
// the generated value, and a thread:updated event is emitted so the
// frontend sidebar refreshes.
func TestMaybeGenerateThreadTitleAppliesGeneratedTitleAndEmits(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-happy")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.generateThreadTitleFn = func(got store.Thread, message string, _ []store.Attachment) (string, error) {
		if got.ID != thread.ID {
			t.Fatalf("generate called with thread %q, want %q", got.ID, thread.ID)
		}
		if message != "fix the reconnect bug" {
			t.Fatalf("generate message = %q, want first user turn", message)
		}
		return "Reconnect spinner fix", nil
	}

	updates := make(chan triage.ThreadUpdateEvent, 1)
	app.emitEventFn = func(name string, data any) {
		if name != "thread:updated" {
			return
		}
		evt, ok := data.(triage.ThreadUpdateEvent)
		if !ok {
			t.Fatalf("thread:updated payload type = %T, want triage.ThreadUpdateEvent", data)
		}
		updates <- evt
	}

	app.maybeGenerateThreadTitle(thread, "fix the reconnect bug", false)

	select {
	case evt := <-updates:
		if evt.Action != "patch" {
			t.Fatalf("expected patch action, got %q", evt.Action)
		}
		if evt.ID != thread.ID {
			t.Fatalf("updated threadID = %q, want %q", evt.ID, thread.ID)
		}
		if evt.Title == nil || *evt.Title != "Reconnect spinner fix" {
			t.Fatalf("updated title = %v", evt.Title)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for thread:updated event")
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "Reconnect spinner fix" {
		t.Fatalf("stored title = %q", stored.Title)
	}
}

// TestMaybeGenerateThreadTitleRunsForCodexThread enforces t3-code parity:
// automatic thread titles use the configured text-generation provider and
// must not be gated by the chat thread's provider.
func TestMaybeGenerateThreadTitleRunsForCodexThread(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-codex")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	calls := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		calls <- struct{}{}
		return "Codex generated title", nil
	}

	updates := make(chan triage.ThreadUpdateEvent, 1)
	app.emitEventFn = func(name string, data any) {
		if name == "thread:updated" {
			updates <- data.(triage.ThreadUpdateEvent)
		}
	}

	app.maybeGenerateThreadTitle(thread, "fix the reconnect bug", false)

	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("generateThreadTitleFn not called for Codex thread")
	}

	select {
	case updated := <-updates:
		if updated.Title == nil || *updated.Title != "Codex generated title" {
			t.Fatalf("updated title = %v", updated.Title)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for thread:updated")
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "Codex generated title" {
		t.Fatalf("stored title = %q", stored.Title)
	}
}

// TestMaybeGenerateThreadTitleSkipsWhenPriorItemsExist ensures we only
// generate titles on the first turn, not on every subsequent send.
func TestMaybeGenerateThreadTitleSkipsWhenPriorItemsExist(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-prior")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	calls := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		calls <- struct{}{}
		return "Late generated", nil
	}

	app.maybeGenerateThreadTitle(thread, "another user message", true)

	select {
	case <-calls:
		t.Fatal("generateThreadTitleFn called when prior items exist")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestMaybeGenerateThreadTitleSkipsWhenTitleCustom ensures a thread that has
// already been renamed (title is NOT the default) is left alone.
func TestMaybeGenerateThreadTitleSkipsWhenTitleCustom(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-custom")
	thread.Title = "Custom user title"
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	calls := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		calls <- struct{}{}
		return "Regenerated", nil
	}

	app.maybeGenerateThreadTitle(thread, "pick me", false)

	select {
	case <-calls:
		t.Fatal("generateThreadTitleFn called when title is not default")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestMaybeGenerateThreadTitleSkipsOnBlankContent covers the empty-message
// guard that prevents the configured text-generation provider from being asked
// to title a no-op message.
func TestMaybeGenerateThreadTitleSkipsOnBlankContent(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-blank")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	calls := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		calls <- struct{}{}
		return "Unexpected", nil
	}

	app.maybeGenerateThreadTitle(thread, "   \n\t", false)

	select {
	case <-calls:
		t.Fatal("generateThreadTitleFn called on blank content")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestMaybeGenerateThreadTitleSwallowsSubprocessError ensures a title-gen
// failure does NOT panic, does NOT update the title, and does NOT emit a
// rename event — it logs and moves on.
func TestMaybeGenerateThreadTitleSwallowsSubprocessError(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-error")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	done := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		defer func() { done <- struct{}{} }()
		return "", errors.New("subprocess boom")
	}

	updates := make(chan triage.ThreadUpdateEvent, 1)
	app.emitEventFn = func(name string, data any) {
		if name == "thread:updated" {
			updates <- data.(triage.ThreadUpdateEvent)
		}
	}

	app.maybeGenerateThreadTitle(thread, "fix the reconnect bug", false)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("generateThreadTitleFn never called")
	}

	select {
	case <-updates:
		t.Fatal("thread:updated emitted despite subprocess error")
	case <-time.After(150 * time.Millisecond):
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != threadtitle.Default {
		t.Fatalf("stored title = %q, want unchanged", stored.Title)
	}
}

// TestMaybeGenerateThreadTitleIgnoresEmptyResponse enforces the behavior when
// the title generator returns an empty string: no rename event, no store
// update. (Sanitization converts empty input into the default title, which
// is also treated as "skip" by the callsite.)
func TestMaybeGenerateThreadTitleIgnoresEmptyResponse(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-empty-response")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	done := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		defer func() { done <- struct{}{} }()
		return "", nil
	}

	updates := make(chan triage.ThreadUpdateEvent, 1)
	app.emitEventFn = func(name string, data any) {
		if name == "thread:updated" {
			updates <- data.(triage.ThreadUpdateEvent)
		}
	}

	app.maybeGenerateThreadTitle(thread, "something", false)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("generateThreadTitleFn never called")
	}

	select {
	case <-updates:
		t.Fatal("thread:updated emitted despite empty title response")
	case <-time.After(150 * time.Millisecond):
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != threadtitle.Default {
		t.Fatalf("stored title = %q, want unchanged default", stored.Title)
	}
}

func TestGeneratedThreadTitle_CodexPathHappy(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-title-codex-cli")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	attachmentsRoot := t.TempDir()
	attachments, err := attachmentstore.NewStore(attachmentstore.Config{RootDir: attachmentsRoot}, app.store)
	if err != nil {
		t.Fatalf("attachment store: %v", err)
	}
	app.attachments = attachments
	record, err := app.attachments.Upload(
		thread.ID,
		"screenshot.png",
		"image/png",
		base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}),
		time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	var gotSpec textgen.CLISpec
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		gotSpec = spec
		outputPath := extractCodexOutputPath(spec.Args)
		if outputPath == "" {
			t.Fatalf("codex title args missing --output-last-message: %v", spec.Args)
		}
		if err := os.WriteFile(outputPath, []byte(`{"title":"  \"Reconnect title generation\"  "}`), 0o600); err != nil {
			return textgen.CLIResult{}, err
		}
		return textgen.CLIResult{ExitCode: 0}, nil
	}

	got, err := app.generatedThreadTitle(thread, "Fix title generation for Codex threads.", []store.Attachment{record})
	if err != nil {
		t.Fatalf("generatedThreadTitle() error = %v", err)
	}
	if got != "Reconnect title generation" {
		t.Fatalf("title = %q", got)
	}
	if !argsContain(gotSpec.Args, "exec") || !argsContain(gotSpec.Args, "--ephemeral") {
		t.Fatalf("codex args missing exec --ephemeral: %v", gotSpec.Args)
	}
	if modelArg := nextArgAfter(gotSpec.Args, "--model"); modelArg != textgen.DefaultCodexModel {
		t.Fatalf("codex model = %q, want %q", modelArg, textgen.DefaultCodexModel)
	}
	imagePath := nextArgAfter(gotSpec.Args, "--image")
	if imagePath == "" {
		t.Fatalf("codex args missing --image: %v", gotSpec.Args)
	}
	if !filepath.IsAbs(imagePath) {
		t.Fatalf("image path = %q, want absolute path", imagePath)
	}
	if !strings.Contains(gotSpec.Stdin, "Attachment metadata:") {
		t.Fatalf("prompt missing attachment metadata: %q", gotSpec.Stdin)
	}
}

func TestThreadTitleImagePathsRequireOwnershipAndExistingFile(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-title-image-owner")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	attachments, err := attachmentstore.NewStore(attachmentstore.Config{RootDir: t.TempDir()}, app.store)
	if err != nil {
		t.Fatalf("attachment store: %v", err)
	}
	app.attachments = attachments
	record, err := app.attachments.Upload(
		thread.ID,
		"screenshot.png",
		"image/png",
		base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}),
		time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	paths, err := app.threadTitleImagePaths("other-thread", []store.Attachment{record})
	if err != nil {
		t.Fatalf("threadTitleImagePaths(other) error = %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("cross-thread attachment leaked path: %v", paths)
	}

	_, path, ok, err := app.attachments.Get(record.ID)
	if err != nil || !ok {
		t.Fatalf("Get() = ok %v err %v", ok, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove attachment file: %v", err)
	}
	paths, err = app.threadTitleImagePaths(thread.ID, []store.Attachment{record})
	if err != nil {
		t.Fatalf("threadTitleImagePaths(stale) error = %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("stale attachment file should be skipped: %v", paths)
	}
}

func TestGeneratedThreadTitle_RoutesToClaudeWhenConfigured(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider": "claude",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	thread := testThread("thread-title-claude-cli")
	thread.WorkspacePath = t.TempDir()

	var gotSpec textgen.CLISpec
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		gotSpec = spec
		return textgen.CLIResult{
			Stdout:   `{"structured_output":{"title":"Claude generated title"}}`,
			ExitCode: 0,
		}, nil
	}

	got, err := app.generatedThreadTitle(thread, "Fix title generation.", nil)
	if err != nil {
		t.Fatalf("generatedThreadTitle() error = %v", err)
	}
	if got != "Claude generated title" {
		t.Fatalf("title = %q", got)
	}
	if !argsContain(gotSpec.Args, "-p") || !argsContain(gotSpec.Args, "--json-schema") {
		t.Fatalf("claude args missing structured output flags: %v", gotSpec.Args)
	}
	if argsContain(gotSpec.Args, "--dangerously-skip-permissions") {
		t.Fatalf("claude title args must not bypass permissions: %v", gotSpec.Args)
	}
	if modelArg := nextArgAfter(gotSpec.Args, "--model"); modelArg != textgen.DefaultClaudeModel {
		t.Fatalf("claude model = %q, want %q", modelArg, textgen.DefaultClaudeModel)
	}
}

// ---- Layer 2 (runtime retry) tests for generatedThreadTitle ----

// TestGeneratedThreadTitle_Layer2PrimaryFailsAlternateSucceeds covers the
// canonical Layer 2 case: Codex is on PATH but its CLI fails (auth, rate
// limit, OpenAI down, etc.). The orchestrator must retry with Claude when
// Claude is also installed, and surface Claude's title.
func TestGeneratedThreadTitle_Layer2PrimaryFailsAlternateSucceeds(t *testing.T) {
	app := newTestAppWithStore(t)
	app.lookPathFn = fakeLookPath("claude", "codex") // both installed.

	thread := testThread("thread-title-l2-fallback")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	var specs []textgen.CLISpec
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		specs = append(specs, spec)
		if spec.Binary == "codex" {
			// Simulate auth/rate-limit failure.
			return textgen.CLIResult{
				Stderr:   "Error: unauthorized — token expired",
				ExitCode: 1,
			}, nil
		}
		// Claude succeeds.
		return textgen.CLIResult{
			Stdout:   `{"structured_output":{"title":"Fallback title via Claude"}}`,
			ExitCode: 0,
		}, nil
	}

	got, err := app.generatedThreadTitle(thread, "Fix title gen.", nil)
	if err != nil {
		t.Fatalf("generatedThreadTitle: %v", err)
	}
	if got != "Fallback title via Claude" {
		t.Fatalf("title = %q, want fallback from Claude", got)
	}
	if len(specs) != 2 {
		t.Fatalf("executor called %d times, want 2 (primary + alternate)", len(specs))
	}
	if specs[0].Binary != "codex" {
		t.Fatalf("first invocation binary = %q, want codex", specs[0].Binary)
	}
	if specs[1].Binary != "claude" {
		t.Fatalf("retry invocation binary = %q, want claude", specs[1].Binary)
	}
	// The retry must use Claude's default model, not whatever was set for Codex.
	if model := nextArgAfter(specs[1].Args, "--model"); model != textgen.DefaultClaudeModel {
		t.Fatalf("retry model = %q, want %q (always default for alternate)", model, textgen.DefaultClaudeModel)
	}
}

func TestGeneratedThreadTitle_Layer2BothFailReturnsPrimaryError(t *testing.T) {
	app := newTestAppWithStore(t)
	app.lookPathFn = fakeLookPath("claude", "codex")

	thread := testThread("thread-title-l2-both-fail")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		stderr := "codex went boom"
		if spec.Binary == "claude" {
			stderr = "claude also went boom"
		}
		return textgen.CLIResult{Stderr: stderr, ExitCode: 1}, nil
	}

	_, err := app.generatedThreadTitle(thread, "anything", nil)
	if err == nil {
		t.Fatal("expected error when both providers fail")
	}
	// Primary error surfaces — not the alternate's.
	if !strings.Contains(err.Error(), "codex went boom") {
		t.Fatalf("error should carry primary (codex) failure: %v", err)
	}
	if strings.Contains(err.Error(), "claude also went boom") {
		t.Fatalf("alternate error leaked: %v", err)
	}
}

func TestGeneratedThreadTitle_Layer2AlternateMissingNoRetry(t *testing.T) {
	app := newTestAppWithStore(t)
	// Configured codex; ONLY codex on PATH. Codex CLI fails. There's no
	// claude to fall back to, so the orchestrator must NOT call the
	// executor a second time.
	app.lookPathFn = fakeLookPath("codex")

	thread := testThread("thread-title-l2-no-alt")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	calls := 0
	app.textGenerationExecutor = func(_ context.Context, _ textgen.CLISpec) (textgen.CLIResult, error) {
		calls++
		return textgen.CLIResult{Stderr: "codex boom", ExitCode: 1}, nil
	}

	_, err := app.generatedThreadTitle(thread, "anything", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("executor called %d times, want 1 (no alternate available)", calls)
	}
}

func TestGeneratedThreadTitle_Layer2ContextCanceledNoRetry(t *testing.T) {
	app := newTestAppWithStore(t)
	app.lookPathFn = fakeLookPath("claude", "codex")

	thread := testThread("thread-title-l2-canceled")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	calls := 0
	app.textGenerationExecutor = func(_ context.Context, _ textgen.CLISpec) (textgen.CLIResult, error) {
		calls++
		// Simulate the app shutting down mid-call: surface ctx.Canceled
		// from the executor. The orchestrator must NOT retry.
		return textgen.CLIResult{}, context.Canceled
	}

	_, err := app.generatedThreadTitle(thread, "anything", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("executor called %d times, want 1 (ctx canceled = no retry)", calls)
	}
}

// TestGeneratedThreadTitle_Layer1SubstitutesThenLayer2NoAlternate is the
// composed-fallback case: configured Codex is missing (Layer 1 substitutes
// Claude), Claude runs and fails, the Layer 2 orchestrator asks for the
// alternate (Codex) and finds its binary still missing → no retry. This
// guards against a regression where the substitution mutates Layer 2's
// "alternate" search and accidentally points back at Claude (self-retry)
// or where the alternate-missing branch loses its `ok=false` guard.
func TestGeneratedThreadTitle_Layer1SubstitutesThenLayer2NoAlternate(t *testing.T) {
	app := newTestAppWithStore(t)
	// Default settings prefer Codex; only Claude is on PATH.
	app.lookPathFn = fakeLookPath("claude")

	thread := testThread("thread-title-l1l2-no-alt")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	calls := 0
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		calls++
		if spec.Binary != "claude" {
			t.Fatalf("unexpected binary %q — Layer 1 should have substituted to claude", spec.Binary)
		}
		return textgen.CLIResult{Stderr: "claude boom", ExitCode: 1}, nil
	}

	_, err := app.generatedThreadTitle(thread, "anything", nil)
	if err == nil {
		t.Fatal("expected error from claude failing")
	}
	if calls != 1 {
		t.Fatalf("executor called %d times, want 1 (Layer 1 substituted to Claude, Layer 2 alternate Codex unavailable)", calls)
	}
}

// TestApplyGeneratedThreadTitleCompareAndSwapSkipsWhenTitleChanged exercises
// the race guard: if the user renamed the thread between the generation call
// and the apply call, UpdateTitleIfCurrent reports 0 rows affected and the
// rename event must NOT be emitted.
func TestApplyGeneratedThreadTitleCompareAndSwapSkipsWhenTitleChanged(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-cas-lost")
	thread.Title = "User picked this"
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	updates := make(chan triage.ThreadUpdateEvent, 1)
	app.emitEventFn = func(name string, data any) {
		if name == "thread:updated" {
			updates <- data.(triage.ThreadUpdateEvent)
		}
	}

	if err := app.applyGeneratedThreadTitle(thread.ID, "Generated fallback"); err != nil {
		t.Fatalf("applyGeneratedThreadTitle() error = %v", err)
	}

	select {
	case <-updates:
		t.Fatal("thread:updated emitted when current title != default (CAS should fail)")
	case <-time.After(150 * time.Millisecond):
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "User picked this" {
		t.Fatalf("stored title = %q, want user-picked preserved", stored.Title)
	}
}
