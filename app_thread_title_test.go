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

	updates := make(chan store.Thread, 1)
	app.emitEventFn = func(name string, data any) {
		if name != "thread:updated" {
			return
		}
		updated, ok := data.(store.Thread)
		if !ok {
			t.Fatalf("thread:updated payload type = %T, want store.Thread", data)
		}
		updates <- updated
	}

	app.maybeGenerateThreadTitle(thread, "fix the reconnect bug", false)

	select {
	case updated := <-updates:
		if updated.ID != thread.ID {
			t.Fatalf("updated threadID = %q, want %q", updated.ID, thread.ID)
		}
		if updated.Title != "Reconnect spinner fix" {
			t.Fatalf("updated title = %q", updated.Title)
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

	updates := make(chan store.Thread, 1)
	app.emitEventFn = func(name string, data any) {
		if name == "thread:updated" {
			updates <- data.(store.Thread)
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
		if updated.Title != "Codex generated title" {
			t.Fatalf("updated title = %q", updated.Title)
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

	updates := make(chan store.Thread, 1)
	app.emitEventFn = func(name string, data any) {
		if name == "thread:updated" {
			updates <- data.(store.Thread)
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

	updates := make(chan store.Thread, 1)
	app.emitEventFn = func(name string, data any) {
		if name == "thread:updated" {
			updates <- data.(store.Thread)
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

	updates := make(chan store.Thread, 1)
	app.emitEventFn = func(name string, data any) {
		if name == "thread:updated" {
			updates <- data.(store.Thread)
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

