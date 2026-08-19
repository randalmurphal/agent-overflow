package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/promptoverride"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/testutil"
)

// seedPromptOverrideThread creates a thread on the given provider/model whose
// workspace exists on disk (a mock provider process spawns with cwd =
// workspace) and returns the row as stored, so callers diff against the same
// coerced view a spawn reads.
func seedPromptOverrideThread(t *testing.T, app *App, id, providerName, model string) (string, string) {
	t.Helper()
	workDir := t.TempDir()
	thread := testThread(id)
	thread.Provider = providerName
	thread.Model = model
	thread.WorkspacePath = workDir
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	return id, workDir
}

// promptOverrideOptions builds the options bundle exactly the way the SPAWN
// path does: buildSessionOptions followed by applySettingsOwnedAxes. The two
// are deliberately separate (the reconcile path pins instead of applying), so
// a test that called only the first would assert on a bundle no session is
// ever started from.
func promptOverrideOptions(t *testing.T, app *App, threadID string) provider.SessionOptions {
	t.Helper()
	opts, _ := promptOverrideOptionsAndResolution(t, app, threadID)
	return opts
}

func promptOverrideOptionsAndResolution(t *testing.T, app *App, threadID string) (provider.SessionOptions, promptOverrideResolution) {
	t.Helper()
	stored, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	opts, _, err := app.buildSessionOptions(app.sanitizeThreadModelSettings(stored))
	if err != nil {
		t.Fatalf("buildSessionOptions() error = %v", err)
	}
	res, err := app.applySettingsOwnedAxes(app.sanitizeThreadModelSettings(stored), &opts)
	if err != nil {
		t.Fatalf("applySettingsOwnedAxes() error = %v", err)
	}
	return opts, res
}

// The composition rule end to end: the matching entry for the thread's
// provider+model becomes the session's system prompt with its placeholders
// resolved against the workspace the session will actually run in, and the
// tool toggles are stamped from the same provider's list. The other
// provider's settings must be inert here — one Settings file feeds both.
func TestBuildSessionOptionsRendersThePromptOverrideForTheThreadProvider(t *testing.T) {
	app := newTestAppWithStore(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-prompt-override-compose", string(provider.Codex), "gpt-5.4")

	if _, err := app.settings.Update(map[string]any{
		"claudePromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"gpt-5.4"}, "prompt": "claude prompt"},
		},
		"claudeDisabledTools": []string{"Workflow"},
		"codexPromptOverrides": []map[string]any{
			{"enabled": false, "models": []string{"gpt-5.4"}, "prompt": "disabled entry"},
			{"enabled": true, "models": []string{"gpt-5.4"}, "prompt": "cwd={{WORKDIR}} os={{PLATFORM}} id={{MODEL_ID}}"},
		},
		"codexDisabledTools": []string{"web_search", "view_image"},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	opts := promptOverrideOptions(t, app, id)
	want := fmt.Sprintf("cwd=%s os=%s id=%s", opts.WorkDir, runtime.GOOS, opts.Model)
	if opts.SystemPrompt != want {
		t.Fatalf("SystemPrompt = %q, want %q", opts.SystemPrompt, want)
	}
	if !slices.Equal(opts.DisabledTools, []string{"web_search", "view_image"}) {
		t.Fatalf("DisabledTools = %v, want the codex list", opts.DisabledTools)
	}
}

// The Claude leg of the same rule, carried all the way to the launch config:
// the override becomes cfg.SystemPrompt (which the session materializes into
// the `--system-prompt-file` temp file), and the settings tool list UNIONS
// with the read-only mode strip rather than replacing it.
func TestBuildSessionOptionsRendersTheClaudeOverrideIntoTheLaunchConfig(t *testing.T) {
	app := newTestAppWithStore(t)
	id, workDir := seedPromptOverrideThread(t, app, "thread-prompt-override-claude", string(provider.Claude), "claude-opus-5")
	if err := app.store.UpdateRuntimeMode(id, string(provider.RuntimeReadOnly)); err != nil {
		t.Fatalf("UpdateRuntimeMode() error = %v", err)
	}

	if _, err := app.settings.Update(map[string]any{
		"claudePromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"claude-opus-5"}, "prompt": "You work in {{WORKDIR}} as {{MODEL_NAME}}."},
		},
		"claudeDisabledTools": []string{"Workflow", "WebSearch"},
		"codexPromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"claude-opus-5"}, "prompt": "codex prompt"},
		},
		"codexDisabledTools": []string{"web_search"},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	opts := promptOverrideOptions(t, app, id)
	want := fmt.Sprintf("You work in %s as %s.", workDir, "Claude Opus 5")
	if opts.SystemPrompt != want {
		t.Fatalf("SystemPrompt = %q, want %q", opts.SystemPrompt, want)
	}

	cfg := claude.ConfigFromOptions(opts)
	if cfg.SystemPrompt != want {
		t.Fatalf("claude cfg.SystemPrompt = %q, want %q", cfg.SystemPrompt, want)
	}
	// Mode strips first, then the user's list in its configured order.
	wantTools := []string{"Write", "Edit", "NotebookEdit", "Workflow", "WebSearch"}
	if !slices.Equal(cfg.DisallowedTools, wantTools) {
		t.Fatalf("claude cfg.DisallowedTools = %v, want %v", cfg.DisallowedTools, wantTools)
	}
}

// claude-tui shares Claude's lists, mirroring claudeHiddenModels: it is the
// same binary, and the interactive TUI honors `--system-prompt-file` and
// `--disallowedTools` exactly as headless does (spike-verified 2.1.234 via a
// PTY + wire capture — user decision 2026-08-18,
// docs/specs/prompt-tool-overrides.md).
//
// Carried all the way to the launch config, because that is the half that
// could silently drop: claudetui.ConfigFromOptions takes the tool list RAW
// rather than off claude.ConfigFromOptions's merged field (the read-only mode
// strip must stay inert on a provider whose approvals AO does not drive), so
// a regression there would leave settings applied on the options bundle and
// absent from the launch.
func TestBuildSessionOptionsAppliesBothAxesToClaudeTUI(t *testing.T) {
	app := newTestAppWithStore(t)
	id, workDir := seedPromptOverrideThread(t, app, "thread-prompt-override-tui", string(provider.ClaudeTUI), "claude-opus-5")

	if _, err := app.settings.Update(map[string]any{
		"claudePromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"claude-opus-5"}, "prompt": "You work in {{WORKDIR}} as {{MODEL_NAME}}."},
		},
		"claudeDisabledTools":         []string{"Workflow", "WebSearch"},
		"claudeTodoRemindersDisabled": true,
		"codexPromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"claude-opus-5"}, "prompt": "codex prompt"},
		},
		"codexDisabledTools": []string{"web_search"},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	opts, res := promptOverrideOptionsAndResolution(t, app, id)
	want := fmt.Sprintf("You work in %s as %s.", workDir, "Claude Opus 5")
	if opts.SystemPrompt != want {
		t.Fatalf("SystemPrompt = %q, want %q", opts.SystemPrompt, want)
	}
	if !slices.Equal(opts.DisabledTools, []string{"Workflow", "WebSearch"}) {
		t.Fatalf("DisabledTools = %v, want the Claude list", opts.DisabledTools)
	}
	// The third settings-owned axis stamps alongside the tool list and
	// reaches the TUI launch config the same way.
	if !opts.DisableTodoReminders {
		t.Fatal("DisableTodoReminders = false, want the settings toggle stamped")
	}
	if !res.Applied {
		t.Fatal("resolution reports the override did not apply to a claude-tui thread")
	}

	cfg := claudetui.ConfigFromOptions(opts)
	if cfg.SystemPrompt != want {
		t.Fatalf("claudetui cfg.SystemPrompt = %q, want %q", cfg.SystemPrompt, want)
	}
	// Exactly the settings list — no runtime-mode strip unioned in, unlike
	// the headless leg above.
	if !slices.Equal(cfg.DisallowedTools, []string{"Workflow", "WebSearch"}) {
		t.Fatalf("claudetui cfg.DisallowedTools = %v, want the settings list alone", cfg.DisallowedTools)
	}
}

// The reconcile half of the pair on a claude-tui thread: claudetui has no
// live-update surface, so any diff the reconciler sees becomes a deferred
// RESTART. pinSettingsOwnedAxes is generic and runs before the diff, so a
// settings edit (or a workspace's git state moving under a rendered
// {{GIT_BLOCK}}) must leave the freshly built options equal to what the live
// session launched with. Without the pin, including claude-tui in the feature
// would queue a restart on every running TUI session the next time anything
// reconciled.
func TestPinSettingsOwnedAxesKeepsAClaudeTUISessionOffTheRestartPath(t *testing.T) {
	app := newTestAppWithStore(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-prompt-override-tui-pin", string(provider.ClaudeTUI), "claude-opus-5")

	if _, err := app.settings.Update(map[string]any{
		"claudePromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"claude-opus-5"}, "prompt": "launched prompt"},
		},
		"claudeDisabledTools": []string{"Workflow"},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	launch := promptOverrideOptions(t, app, id)

	// The user edits all three settings-owned axes while the session is live.
	if _, err := app.settings.Update(map[string]any{
		"claudePromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"claude-opus-5"}, "prompt": "edited prompt"},
		},
		"claudeDisabledTools":         []string{"Workflow", "WebSearch"},
		"claudeTodoRemindersDisabled": true,
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	stored, err := app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	rebuilt, _, err := app.buildSessionOptions(app.sanitizeThreadModelSettings(stored))
	if err != nil {
		t.Fatalf("buildSessionOptions() error = %v", err)
	}
	pinSettingsOwnedAxes(&rebuilt, launch)

	if rebuilt.SystemPrompt != launch.SystemPrompt {
		t.Fatalf("SystemPrompt = %q, want the launched %q", rebuilt.SystemPrompt, launch.SystemPrompt)
	}
	if !slices.Equal(rebuilt.DisabledTools, launch.DisabledTools) {
		t.Fatalf("DisabledTools = %v, want the launched %v", rebuilt.DisabledTools, launch.DisabledTools)
	}
	if !reflect.DeepEqual(claudetui.ConfigFromOptions(rebuilt), claudetui.ConfigFromOptions(launch)) {
		t.Fatal("the rebuilt launch config differs from the live one — a reconcile would queue a restart")
	}
}

// {{MEMORY_DIR}} resolves for BOTH Claude transports: headless and the
// interactive TUI are the same binary reading the same
// `<claudeHome>/projects/<slug>/memory`, and the CLI stops creating it under a
// replaced system prompt either way. Codex has no such directory, so the
// placeholder renders empty there rather than pointing a Codex session at a
// Claude path.
func TestClaudeMemoryDirForThreadCoversBothClaudeTransports(t *testing.T) {
	app := newTestAppWithStore(t)
	workDir := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}
	want, ok, err := promptoverride.ClaudeMemoryDir(home, workDir)
	if err != nil || !ok {
		t.Fatalf("ClaudeMemoryDir() = %q, %v, %v — want a resolvable directory", want, ok, err)
	}

	for _, tc := range []struct {
		provider provider.ProviderKind
		want     string
	}{
		{provider: provider.Claude, want: want},
		{provider: provider.ClaudeTUI, want: want},
		{provider: provider.Codex, want: ""},
	} {
		t.Run(string(tc.provider), func(t *testing.T) {
			thread := testThread("thread-memory-dir-" + string(tc.provider))
			thread.Provider = string(tc.provider)
			dir, resolved, err := app.claudeMemoryDirForThread(thread, workDir)
			if err != nil {
				t.Fatalf("claudeMemoryDirForThread() error = %v", err)
			}
			if dir != tc.want || resolved != (tc.want != "") {
				t.Fatalf("claudeMemoryDirForThread() = %q, %v, want %q, %v", dir, resolved, tc.want, tc.want != "")
			}
		})
	}
}

// Design mode and discussions already run a fully custom system prompt.
// Replacing it would break them, so the settings override stands down — but
// the tool toggles are a separate axis and still apply.
func TestBuildSessionOptionsKeepsAFeatureOwnedSystemPrompt(t *testing.T) {
	app := newTestAppWithStore(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-prompt-override-feature", string(provider.Codex), "gpt-5.4")
	app.setThreadSystemPrompt(id, "deliberation prompt")

	if _, err := app.settings.Update(map[string]any{
		"codexPromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"gpt-5.4"}, "prompt": "settings prompt"},
		},
		"codexDisabledTools": []string{"web_search"},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	opts, res := promptOverrideOptionsAndResolution(t, app, id)
	if opts.SystemPrompt != "deliberation prompt" {
		t.Fatalf("SystemPrompt = %q, want the feature-owned prompt", opts.SystemPrompt)
	}
	if !slices.Equal(opts.DisabledTools, []string{"web_search"}) {
		t.Fatalf("DisabledTools = %v, want the toggles to apply regardless", opts.DisabledTools)
	}
	if res.Applied {
		t.Fatal("resolution reports the override applied over a feature-owned prompt")
	}
}

// Rendering is multiplicative: {{GIT_BLOCK}} expands to a repository
// snapshot, so a stored prompt within the settings length limit can still
// render to tens of megabytes. That must fail the spawn with both sizes
// named — silently truncating would hand the model a prompt cut mid-sentence,
// and silently sending it would put megabytes into every turn's context.
func TestApplySettingsOwnedAxesRefusesAnOversizedRenderedPrompt(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-prompt-override-huge", string(provider.Codex), "gpt-5.4")
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

	if _, err := app.settings.Update(map[string]any{
		"codexPromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"gpt-5.4"}, "prompt": strings.Repeat("{{GIT_BLOCK}}\n", 200)},
		},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	stored, err := app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	opts, _, err := app.buildSessionOptions(app.sanitizeThreadModelSettings(stored))
	if err != nil {
		t.Fatalf("buildSessionOptions() error = %v", err)
	}
	if _, err := app.applySettingsOwnedAxes(app.sanitizeThreadModelSettings(stored), &opts); err == nil {
		t.Fatalf("applySettingsOwnedAxes() error = nil, want a refusal (rendered %d bytes)", len(opts.SystemPrompt))
	} else if !strings.Contains(err.Error(), "over the") {
		t.Fatalf("applySettingsOwnedAxes() error = %v, want the size limit named", err)
	}
}

// The git placeholders, both directions plus the fast path. {{IS_GIT_REPO}}
// on its own must not pay for PromptSnapshot's two subprocesses — it is
// answerable from git's on-disk layout — while {{GIT_BLOCK}} carries the real
// branch/status/commits shape.
func TestPromptOverrideGitFacts(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	t.Run("git block inside a repository", func(t *testing.T) {
		id, _ := seedPromptOverrideThread(t, app, "thread-prompt-override-git-block", string(provider.Codex), "gpt-5.4")
		if err := app.store.UpdateWorkspacePath(id, repo); err != nil {
			t.Fatalf("UpdateWorkspacePath() error = %v", err)
		}
		if _, err := app.settings.Update(map[string]any{
			"codexPromptOverrides": []map[string]any{
				{"enabled": true, "models": []string{"gpt-5.4"}, "prompt": "repo={{IS_GIT_REPO}}\n{{GIT_BLOCK}}"},
			},
		}); err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		prompt := promptOverrideOptions(t, app, id).SystemPrompt
		if !strings.HasPrefix(prompt, "repo=Yes\n") {
			t.Fatalf("SystemPrompt = %q, want IS_GIT_REPO=Yes", prompt)
		}
		if !strings.Contains(prompt, "Current branch: ") || !strings.Contains(prompt, "Recent commits:") {
			t.Fatalf("SystemPrompt = %q, want the PromptBlock shape", prompt)
		}
	})

	t.Run("outside a repository", func(t *testing.T) {
		id, _ := seedPromptOverrideThread(t, app, "thread-prompt-override-git-none", string(provider.Codex), "gpt-5.4")
		if _, err := app.settings.Update(map[string]any{
			"codexPromptOverrides": []map[string]any{
				{"enabled": true, "models": []string{"gpt-5.4"}, "prompt": "repo={{IS_GIT_REPO}}|block={{GIT_BLOCK}}|"},
			},
		}); err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if got := promptOverrideOptions(t, app, id).SystemPrompt; got != "repo=No|block=|" {
			t.Fatalf("SystemPrompt = %q, want the non-repo answer", got)
		}
	})

	// The fast path is behavioural, not just an optimization: it must give
	// the same Yes/No answer PromptSnapshot would, from a repository AND
	// from a linked worktree (which gitroot resolves to its main repo).
	t.Run("is-git-repo only", func(t *testing.T) {
		if _, err := app.settings.Update(map[string]any{
			"codexPromptOverrides": []map[string]any{
				{"enabled": true, "models": []string{"gpt-5.4"}, "prompt": "repo={{IS_GIT_REPO}}"},
			},
		}); err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		worktree := filepath.Join(t.TempDir(), "wt")
		testutil.RunGit(t, repo, "worktree", "add", "-b", "wt-branch", worktree)
		for _, tc := range []struct {
			name    string
			workDir string
			want    string
		}{
			{name: "repository", workDir: repo, want: "repo=Yes"},
			{name: "linked worktree", workDir: worktree, want: "repo=Yes"},
			{name: "plain directory", workDir: t.TempDir(), want: "repo=No"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id, _ := seedPromptOverrideThread(t, app, "thread-prompt-override-isrepo-"+tc.name, string(provider.Codex), "gpt-5.4")
				if err := app.store.UpdateWorkspacePath(id, tc.workDir); err != nil {
					t.Fatalf("UpdateWorkspacePath() error = %v", err)
				}
				if got := promptOverrideOptions(t, app, id).SystemPrompt; got != tc.want {
					t.Fatalf("SystemPrompt = %q, want %q", got, tc.want)
				}
			})
		}
	})
}

// The memory directory is the one side effect a rendered override carries,
// and it runs on the spawn path only. Deleting ensureClaudeMemoryDir's body
// must fail here.
func TestSpawnCreatesTheClaudeMemoryDirectoryOnlyWhenThePromptAsksForIt(t *testing.T) {
	cases := []struct {
		name       string
		provider   provider.ProviderKind
		model      string
		prompt     string
		wantMemory bool
	}{
		{
			name:       "claude thread whose prompt uses the placeholder",
			provider:   provider.Claude,
			model:      "claude-opus-5",
			prompt:     "Memory lives in {{MEMORY_DIR}}.",
			wantMemory: true,
		},
		{
			name:     "claude thread whose prompt does not",
			provider: provider.Claude,
			model:    "claude-opus-5",
			prompt:   "No placeholders here.",
		},
		{
			// Codex has no memory directory; the placeholder renders empty
			// rather than pointing a Codex session at a Claude path.
			name:     "codex thread",
			provider: provider.Codex,
			model:    "gpt-5.4",
			prompt:   "Memory lives in {{MEMORY_DIR}}.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := setupE2EApp(t)
			_, workDir := startOverrideSession(t, app, "thread-memory-"+string(tc.provider), tc.provider, tc.model, tc.prompt)

			home, err := os.UserHomeDir()
			if err != nil {
				t.Fatalf("UserHomeDir() error = %v", err)
			}
			dir, ok, err := promptoverride.ClaudeMemoryDir(home, workDir)
			if err != nil || !ok {
				t.Fatalf("ClaudeMemoryDir() = %q, %v, %v — want a resolvable directory", dir, ok, err)
			}
			_, statErr := os.Stat(dir)
			if tc.wantMemory && statErr != nil {
				t.Fatalf("memory directory %s missing after spawn: %v", dir, statErr)
			}
			if !tc.wantMemory && statErr == nil {
				t.Fatalf("memory directory %s created for a session that never promised it", dir)
			}
		})
	}
}

// A memory directory that cannot be created is a DEGRADED session, not a
// broken one: the failure surfaces as thread error state the user can act on
// and the session still starts.
func TestSpawnSurfacesAMemoryDirectoryFailureWithoutFailingTheSpawn(t *testing.T) {
	app, bus := setupE2EApp(t)
	workDir := t.TempDir()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}
	dir, ok, err := promptoverride.ClaudeMemoryDir(home, workDir)
	if err != nil || !ok {
		t.Fatalf("ClaudeMemoryDir() = %q, %v, %v — want a resolvable directory", dir, ok, err)
	}
	// A regular FILE where the project directory belongs: MkdirAll cannot
	// create anything underneath it.
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(dir), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	threadID := startOverrideSessionIn(t, app, "thread-memory-failure", provider.Claude, "claude-opus-5", "Memory lives in {{MEMORY_DIR}}.", workDir)

	evt := bus.nextProviderEventOfKind(t, provider.EventError, 5*time.Second)
	if evt.ThreadID != threadID {
		t.Fatalf("error event threadID = %q, want %q", evt.ThreadID, threadID)
	}
	if !strings.Contains(evt.Content, "memory directory") {
		t.Fatalf("error content = %q, want the memory-directory failure", evt.Content)
	}
	app.mu.Lock()
	_, live := app.sessions[threadID]
	app.mu.Unlock()
	if !live {
		t.Fatal("the spawn failed; a memory-directory failure must never be fatal")
	}
}

// startOverrideSession seeds a thread on providerName with the given override
// prompt configured, installs the matching mock provider binary over the
// poisoned default, and starts the session. Returns the thread id and its
// workspace.
func startOverrideSession(t *testing.T, app *App, id string, providerName provider.ProviderKind, model, prompt string) (string, string) {
	t.Helper()
	workDir := t.TempDir()
	return startOverrideSessionIn(t, app, id, providerName, model, prompt, workDir), workDir
}

func startOverrideSessionIn(t *testing.T, app *App, id string, providerName provider.ProviderKind, model, prompt string, workDir string) string {
	t.Helper()
	thread := testThread(id)
	thread.Provider = string(providerName)
	thread.Model = model
	thread.WorkspacePath = workDir
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	settingsKey := "codexPromptOverrides"
	binaryKey := "codexBinaryPath"
	binary := testutil.WriteMockCodexSession(t, t.TempDir(), map[string]string{
		`"method":"initialize"`:   `{"jsonrpc":"2.0","id":%s,"result":{}}`,
		`"method":"thread/start"`: `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"codex-` + id + `"}}}`,
	})
	if providerName == provider.Claude {
		settingsKey = "claudePromptOverrides"
		binaryKey = "claudeBinaryPath"
		binary = testutil.WriteMockClaudeScript(t, t.TempDir(), nil)
	}
	if _, err := app.settings.Update(map[string]any{
		settingsKey: []map[string]any{
			{"enabled": true, "models": []string{model}, "prompt": prompt},
		},
		binaryKey: binary,
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if err := app.StartSession(id); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	t.Cleanup(func() { _ = app.StopSession(id) })
	return id
}

// The "next sessions only" contract, as a transition rather than a state.
// Both override axes are spawn-only on the wire, so a running session that
// re-diffed against freshly-read Settings would report a spawn-only
// difference and queue a restart — killing the user's live session because
// they saved an unrelated Settings edit. The live session must keep what it
// launched with, an unrelated live-appliable change must still apply without
// a restart, and the next spawn must read the new values.
func TestPromptOverrideChangeDoesNotDisturbALiveSession(t *testing.T) {
	app := newTestAppWithStore(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-prompt-override-live", string(provider.Codex), "gpt-5.4")

	if _, err := app.settings.Update(map[string]any{
		"codexPromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"gpt-5.4"}, "prompt": "launched prompt"},
		},
		"codexDisabledTools": []string{"web_search"},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	launchOpts := promptOverrideOptions(t, app, id)
	if launchOpts.SystemPrompt != "launched prompt" {
		t.Fatalf("launch SystemPrompt = %q, want the configured override", launchOpts.SystemPrompt)
	}
	registerLiveCodexSession(t, app, id, launchOpts)

	app.startSessionFn = func(threadID string) error {
		return fmt.Errorf("unexpected restart of %s — a settings override edit must not restart a live session", threadID)
	}
	app.configReconnectPollIntervalOverride = 10 * time.Millisecond
	app.configReconnectQuietWindowOverride = time.Nanosecond

	if _, err := app.settings.Update(map[string]any{
		"codexPromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"gpt-5.4"}, "prompt": "edited prompt"},
		},
		"codexDisabledTools": []string{"update_plan"},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	// An unrelated live-appliable change is what drives a reconcile in
	// practice; it must succeed rather than being dragged into a restart by
	// the stale override axes.
	if _, err := app.UpdateThreadReasoningEffort(id, "low"); err != nil {
		t.Fatalf("UpdateThreadReasoningEffort() error = %v", err)
	}

	app.mu.Lock()
	live := app.sessions[id]
	pending := app.pendingConfigReconnects[id]
	app.mu.Unlock()

	if pending {
		t.Fatal("a settings override edit queued a deferred restart on a live session")
	}
	if live.launchOpts.ReasoningEffort != provider.EffortLow {
		t.Fatalf("session effort = %q, want low (the unrelated change must still live-apply)", live.launchOpts.ReasoningEffort)
	}
	if live.launchOpts.SystemPrompt != "launched prompt" {
		t.Fatalf("session SystemPrompt = %q, want the prompt it launched with", live.launchOpts.SystemPrompt)
	}
	if !slices.Equal(live.launchOpts.DisabledTools, []string{"web_search"}) {
		t.Fatalf("session DisabledTools = %v, want the list it launched with", live.launchOpts.DisabledTools)
	}

	// The next spawn reads Settings fresh: that is where the edit lands.
	next := promptOverrideOptions(t, app, id)
	if next.SystemPrompt != "edited prompt" {
		t.Fatalf("next-spawn SystemPrompt = %q, want the edited override", next.SystemPrompt)
	}
	if !slices.Equal(next.DisabledTools, []string{"update_plan"}) {
		t.Fatalf("next-spawn DisabledTools = %v, want the edited list", next.DisabledTools)
	}
}

// The other half of the pin: SystemPrompt is a SHARED axis. Design mode and
// discussions own it for their threads, and an edit to a feature-owned prompt
// must keep converging exactly as it did before the settings override
// existed. A pin that swallowed that would silently freeze design threads on
// their first prompt.
func TestFeatureOwnedPromptChangeStillConvergesThroughTheReconciler(t *testing.T) {
	app := newTestAppWithStore(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-prompt-override-feature-live", string(provider.Codex), "gpt-5.4")
	app.setThreadSystemPrompt(id, "deliberation prompt v1")

	launchOpts := promptOverrideOptions(t, app, id)
	if launchOpts.SystemPrompt != "deliberation prompt v1" {
		t.Fatalf("launch SystemPrompt = %q, want the feature-owned prompt", launchOpts.SystemPrompt)
	}
	registerLiveCodexSession(t, app, id, launchOpts)

	restarted := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		select {
		case restarted <- threadID:
		default:
		}
		return nil
	}
	app.configReconnectPollIntervalOverride = 10 * time.Millisecond
	app.configReconnectQuietWindowOverride = time.Nanosecond

	app.setThreadSystemPrompt(id, "deliberation prompt v2")

	// Only a restart can converge a system-prompt change on either provider.
	if app.liveApplySessionConfig(id) {
		t.Fatal("liveApplySessionConfig() = true; a feature-prompt edit must fall through to a restart")
	}
	app.reconcileSessionConfig(id)
	select {
	case got := <-restarted:
		if got != id {
			t.Fatalf("restarted thread = %q, want %q", got, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the feature-owned prompt edit never reached a restart")
	}
}

// A Codex fork does not carry the config: `thread/fork` names only the thread
// and the anchor turn, so the forked thread inherits its instructions and
// tool config from the SUBSEQUENT `thread/resume` the next spawn runs. That
// makes the fork's next launch config the thing worth pinning.
func TestNextSpawnAfterACodexForkStillCarriesTheOverrideAxes(t *testing.T) {
	app := newTestAppWithStore(t)
	id, _ := seedPromptOverrideThread(t, app, "thread-prompt-override-fork", string(provider.Codex), "gpt-5.4")

	if _, err := app.settings.Update(map[string]any{
		"codexPromptOverrides": []map[string]any{
			{"enabled": true, "models": []string{"gpt-5.4"}, "prompt": "forked instructions"},
		},
		"codexDisabledTools": []string{"web_search", "update_plan"},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	stored, err := app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	stored.PendingForkRef = "codex-thread-forked"
	if err := app.store.UpdateThread(stored); err != nil {
		t.Fatalf("UpdateThread() error = %v", err)
	}

	opts := promptOverrideOptions(t, app, id)
	if opts.Resume != "codex-thread-forked" || !opts.ForkSession {
		t.Fatalf("opts.Resume = %q, ForkSession = %v — want the pending fork consumed", opts.Resume, opts.ForkSession)
	}

	cfg := codex.ConfigFromOptions(opts)
	if cfg.SystemPrompt != "forked instructions" {
		t.Fatalf("codex cfg.SystemPrompt = %q, want the override as baseInstructions", cfg.SystemPrompt)
	}
	if !slices.Equal(cfg.DisabledTools, []string{"web_search", "update_plan"}) {
		t.Fatalf("codex cfg.DisabledTools = %v, want the configured toggles", cfg.DisabledTools)
	}
	// The toggle ids only matter as the config keys they expand into on the
	// thread/resume params.
	keys := codex.DisabledToolConfigOverrides(cfg.DisabledTools)
	if len(keys) == 0 {
		t.Fatalf("DisabledToolConfigOverrides(%v) = empty, want the config keys the resume carries", cfg.DisabledTools)
	}
}

// registerLiveCodexSession spawns a mock Codex session for the thread and
// registers it as the live session with the given launch options, so a
// reconcile has something real to diff against.
func registerLiveCodexSession(t *testing.T, app *App, threadID string, launchOpts provider.SessionOptions) {
	t.Helper()
	binary := testutil.WriteMockCodexSession(t, t.TempDir(), map[string]string{
		`"method":"initialize"`:    `{"jsonrpc":"2.0","id":%s,"result":{}}`,
		`"method":"thread/start"`:  `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"mock-thread-override"}}}`,
		`"method":"thread/resume"`: `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"mock-thread-override"}}}`,
	})
	cfg := codex.ConfigFromOptions(launchOpts)
	cfg.Binary = binary
	sess, err := codex.NewSession(context.Background(), threadID, cfg, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("codex.NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.mu.Lock()
	app.sessions[threadID] = session{
		provider:   string(provider.Codex),
		token:      "override-token",
		codex:      sess,
		launchOpts: launchOpts,
		liveness:   newSessionLiveness(time.Now()),
	}
	app.mu.Unlock()
}
