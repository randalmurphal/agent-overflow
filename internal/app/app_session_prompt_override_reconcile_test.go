package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/promptoverride"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// The reconcile half of the settings-owned prompt axis on a HEADLESS Claude
// thread — the one transport that can swap a system prompt without a restart
// (`set_model.system_prompt`). The classification these tests pin:
//
//	stored override unchanged      → pin, render nothing, plan nothing
//	stored override edited         → re-render, plan a live prompt swap
//	override turned on             → render, plan a live prompt swap
//	override turned off            → the prompt axis empties, PlanLiveUpdate
//	                                 refuses it (no revert-to-built-in wire
//	                                 form) and the restart converges
//
// "Unchanged" is decided on the STORED text, never the rendered one: a
// rendered comparison would report a diff every time the workspace's git
// state moved under a {{GIT_BLOCK}} and reconcile forever.

// reconcileOptionsFor rebuilds a thread's options the way liveApplySessionConfig
// does and runs the reconcile-path resolution against a launch bundle.
func reconcileOptionsFor(
	t *testing.T, app *App, threadID string, launch provider.SessionOptions,
) provider.SessionOptions {
	t.Helper()
	stored, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	sanitized := app.sanitizeThreadModelSettings(stored)
	opts, err := app.buildSessionOptions(sanitized)
	if err != nil {
		t.Fatalf("buildSessionOptions() error = %v", err)
	}
	app.reconcileSettingsOwnedAxes(sanitized, "session-token", &opts, launch)
	return opts
}

func setClaudePromptOverride(t *testing.T, app *App, model, prompt string) {
	t.Helper()
	entries := []map[string]any{}
	if prompt != "" {
		entries = append(entries, map[string]any{
			"enabled": true, "models": []string{model}, "prompt": prompt,
		})
	}
	if _, err := app.settings.Update(map[string]any{"claudePromptOverrides": entries}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
}

// An unchanged stored override pins. This is the hot path — the reconciler
// runs on every model, effort, and runtime-mode change — so it must produce
// the live session's own prompt back without rendering anything. The launch
// prompt here is a sentinel no render could produce, which is what proves the
// pin rather than a coincidentally equal re-render.
func TestReconcileSettingsOwnedAxesPinsAnUnchangedClaudeOverride(t *testing.T) {
	app := newTestAppWithStore(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-reconcile-pin", string(provider.Claude), "claude-opus-5")
	setClaudePromptOverride(t, app, "claude-opus-5", "stored prompt")

	launch := promptOverrideOptions(t, app, id)
	// A value no render could reproduce, so an equal result proves the pin
	// rather than a coincidentally identical re-render.
	launch.SystemPrompt = "<<rendered at spawn, unreproducible>>"
	opts := reconcileOptionsFor(t, app, id, launch)

	if opts.SystemPrompt != launch.SystemPrompt {
		t.Fatalf("SystemPrompt = %q, want the live session's %q", opts.SystemPrompt, launch.SystemPrompt)
	}
	if opts.SystemPromptOverrideSource != "stored prompt" {
		t.Fatalf("SystemPromptOverrideSource = %q, want the pinned source", opts.SystemPromptOverrideSource)
	}
	update, ok := claude.PlanLiveUpdate(launch, opts)
	if !ok || update.SystemPrompt != "" {
		t.Fatalf("PlanLiveUpdate = (%+v, %v), want no prompt change", update, ok)
	}
}

// An EDITED override converges live: the reconcile path re-renders (the only
// place it pays for that) and the plan carries the new prompt on set_model.
func TestReconcileSettingsOwnedAxesConvergesAnEditedClaudeOverride(t *testing.T) {
	app := newTestAppWithStore(t)
	id, workDir := seedPromptOverrideThread(t, app, "thread-reconcile-edit", string(provider.Claude), "claude-opus-5")
	setClaudePromptOverride(t, app, "claude-opus-5", "launched prompt for {{WORKDIR}}")
	launch := promptOverrideOptions(t, app, id)

	// The user edits the stored override while the session is live.
	setClaudePromptOverride(t, app, "claude-opus-5", "edited prompt for {{WORKDIR}}")
	opts := reconcileOptionsFor(t, app, id, launch)

	want := "edited prompt for " + workDir
	if opts.SystemPrompt != want {
		t.Fatalf("SystemPrompt = %q, want the re-rendered %q", opts.SystemPrompt, want)
	}
	if opts.SystemPromptOverrideSource != "edited prompt for {{WORKDIR}}" {
		t.Fatalf("SystemPromptOverrideSource = %q, want the edited stored text", opts.SystemPromptOverrideSource)
	}
	update, ok := claude.PlanLiveUpdate(launch, opts)
	if !ok || update.SystemPrompt != want {
		t.Fatalf("PlanLiveUpdate = (%+v, %v), want a live prompt swap to %q", update, ok, want)
	}
}

// Turning the override OFF empties the prompt axis, which PlanLiveUpdate
// refuses — `set_model.system_prompt` must be non-empty and has no
// revert-to-built-in form, so only a respawn without --system-prompt-file
// restores the CLI's own prompt.
func TestReconcileSettingsOwnedAxesDisabledOverrideNeedsRestart(t *testing.T) {
	app := newTestAppWithStore(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-reconcile-off", string(provider.Claude), "claude-opus-5")
	setClaudePromptOverride(t, app, "claude-opus-5", "launched prompt for {{WORKDIR}}")
	launch := promptOverrideOptions(t, app, id)

	// The user turns the override off while the session is live.
	setClaudePromptOverride(t, app, "claude-opus-5", "")
	opts := reconcileOptionsFor(t, app, id, launch)

	if opts.SystemPrompt != "" {
		t.Fatalf("SystemPrompt = %q, want empty once the override is off", opts.SystemPrompt)
	}
	if _, ok := claude.PlanLiveUpdate(launch, opts); ok {
		t.Fatal("PlanLiveUpdate accepted a revert to the CLI built-in, which has no wire form")
	}
}

// Turning an override ON is NOT the mirror image of turning it off. The CLI's
// setter assigns unconditionally onto the same slot --system-prompt-file
// fills at spawn, so an empty prior value is not a special case: the render
// happens here and the swap goes live.
func TestReconcileSettingsOwnedAxesConvergesAnEnabledOverride(t *testing.T) {
	app := newTestAppWithStore(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-reconcile-on", string(provider.Claude), "claude-opus-5")
	launch := promptOverrideOptions(t, app, id) // no override was in play

	setClaudePromptOverride(t, app, "claude-opus-5", "brand new prompt")
	opts := reconcileOptionsFor(t, app, id, launch)

	if opts.SystemPrompt != "brand new prompt" {
		t.Fatalf("SystemPrompt = %q, want the newly rendered override", opts.SystemPrompt)
	}
	update, ok := claude.PlanLiveUpdate(launch, opts)
	if !ok {
		t.Fatal("PlanLiveUpdate refused an override turned on; set_model.system_prompt fills an empty slot")
	}
	if update.SystemPrompt != "brand new prompt" {
		t.Fatalf("update.SystemPrompt = %q, want the newly rendered override", update.SystemPrompt)
	}
}

// A feature-owned prompt (currently discussions) is untouched by any of
// this: buildSessionOptions has already written it, and the settings override
// stands down exactly as it does on the spawn path.
func TestReconcileSettingsOwnedAxesLeavesFeatureOwnedPromptsAlone(t *testing.T) {
	app := newTestAppWithStore(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-reconcile-feature", string(provider.Claude), "claude-opus-5")
	app.setThreadSystemPrompt(id, "deliberation prompt")
	launch := promptOverrideOptions(t, app, id)

	// A settings override appears; a feature already owns the prompt, so it
	// must stand down exactly as it does on the spawn path.
	setClaudePromptOverride(t, app, "claude-opus-5", "settings override")
	opts := reconcileOptionsFor(t, app, id, launch)

	if opts.SystemPrompt != "deliberation prompt" {
		t.Fatalf("SystemPrompt = %q, want the feature-owned prompt untouched", opts.SystemPrompt)
	}
	if opts.SystemPromptOverrideSource != "" {
		t.Fatalf("SystemPromptOverrideSource = %q, want empty — no settings override is in play",
			opts.SystemPromptOverrideSource)
	}
}

// The render memo: the deferred-restart watcher re-runs the whole reconcile
// once a second while a thread is busy, and a prompt change it cannot apply
// live would otherwise re-render — up to two git subprocesses — on every
// poll. Rendering twice for the same (source, workspace, model) must hit the
// memo, which this pins by moving the settings underneath a cached entry and
// asserting the CACHED value comes back.
func TestReconcilePromptOverrideRenderIsMemoized(t *testing.T) {
	app := newTestAppWithStore(t)
	id, workDir := seedPromptOverrideThread(t, app, "thread-reconcile-memo", string(provider.Claude), "claude-opus-5")
	setClaudePromptOverride(t, app, "claude-opus-5", "memo prompt for {{WORKDIR}}")

	stored, err := app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	sanitized := app.sanitizeThreadModelSettings(stored)
	entry, matched := claudePromptOverrideEntryForTest(t, app, sanitized)
	if !matched {
		t.Fatal("no override matched the seeded thread")
	}

	first, ok := app.reconcilePromptOverrideRender(sanitized, "tok", entry, workDir)
	if !ok || first != "memo prompt for "+workDir {
		t.Fatalf("first render = (%q, %v), want the rendered prompt", first, ok)
	}

	cached, _ := app.sessionManager().runtime.PromptRender("tok")
	cached.Rendered = "<<from the memo>>"
	app.sessionManager().runtime.PutPromptRender("tok", cached)

	second, ok := app.reconcilePromptOverrideRender(sanitized, "tok", entry, workDir)
	if !ok || second != "<<from the memo>>" {
		t.Fatalf("second render = (%q, %v), want the memoized value — the render ran again", second, ok)
	}

	// A different workspace is a different render and must miss the memo.
	other := t.TempDir()
	third, ok := app.reconcilePromptOverrideRender(sanitized, "tok", entry, other)
	if !ok || third != "memo prompt for "+other {
		t.Fatalf("third render = (%q, %v), want a fresh render for the new workspace", third, ok)
	}
}

// claudePromptOverrideEntryForTest resolves the settings entry the reconcile
// path would match for a thread, so a memo test can call the renderer
// directly.
func claudePromptOverrideEntryForTest(t *testing.T, app *App, thread store.Thread) (settings.PromptOverride, bool) {
	t.Helper()
	return promptoverride.Match(
		app.settings.Get().PromptOverridesForProvider(thread.Provider),
		thread.Provider,
		thread.Model,
	)
}

// An over-limit render on the reconcile path pins the live session's prompt
// rather than killing the session, and must SAY so. Silence here left the
// session running the prompt it started with while Settings showed the
// edited one, with nothing anywhere stating that the save had not taken —
// and, because the oversize verdict was the one render result that was never
// memoized, it re-paid the git subprocesses on every reconcile.
func TestOversizeReconcileRenderIsSurfacedOnceAndPinsThePrompt(t *testing.T) {
	app := newTestAppWithStore(t)
	errors := collectErrorItemUpserts(t, app, 4)
	repo := testutil.InitGitRepo(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-reconcile-huge", string(provider.Claude), "claude-opus-5")
	if err := app.store.UpdateWorkspacePath(id, repo); err != nil {
		t.Fatalf("UpdateWorkspacePath() error = %v", err)
	}
	// A dirty tree the git block will actually carry, so each token expands
	// to real bytes rather than an empty section.
	for i := range 200 {
		name := filepath.Join(repo, fmt.Sprintf("%s-%d.txt", strings.Repeat("path", 12), i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setClaudePromptOverride(t, app, "claude-opus-5", strings.Repeat("{{GIT_BLOCK}}\n", 200))

	launch := provider.SessionOptions{
		Model:                      "claude-opus-5",
		SystemPrompt:               "the prompt this session started with",
		SystemPromptOverrideSource: "the override it started with",
	}
	opts := reconcileOptionsFor(t, app, id, launch)

	if opts.SystemPrompt != launch.SystemPrompt {
		t.Fatalf("SystemPrompt = %q, want the live session's prompt pinned", opts.SystemPrompt)
	}
	if opts.SystemPromptOverrideSource != launch.SystemPromptOverrideSource {
		t.Fatalf("SystemPromptOverrideSource = %q, want the launch source pinned — a rewritten source would make the next reconcile believe the oversize prompt is live",
			opts.SystemPromptOverrideSource)
	}

	select {
	case item := <-errors:
		if !strings.Contains(item.Summary, "over the") || !strings.Contains(item.Summary, "not applied") {
			t.Fatalf("error item = %q, want the refusal and both sizes named", item.Summary)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an override that could not be applied left no user-facing state; Settings shows one prompt and the session runs another")
	}

	cached, hit := app.sessionManager().runtime.PromptRender("session-token")
	if !hit || !cached.Oversize {
		t.Fatalf("oversize memo = (%+v, %v), want the verdict cached so the git render is not re-paid every poll", cached, hit)
	}

	// The deferred-restart watcher re-reconciles once a second. The second
	// pass must decide the same thing without rendering again and without
	// telling the user again.
	opts = reconcileOptionsFor(t, app, id, launch)
	if opts.SystemPrompt != launch.SystemPrompt {
		t.Fatalf("second reconcile SystemPrompt = %q, want the same pin", opts.SystemPrompt)
	}
	select {
	case item := <-errors:
		t.Fatalf("the same oversize verdict was surfaced twice: %q", item.Summary)
	case <-time.After(100 * time.Millisecond):
	}
}
