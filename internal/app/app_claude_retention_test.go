package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/providerupdate"
	"agent-overflow/internal/providerupdate/updatefake"
	"agent-overflow/internal/settings"
)

// setTestRetentionDays saves the retention window on app.
func setTestRetentionDays(t *testing.T, app *App, days int) {
	t.Helper()
	if _, err := app.settings.Update(map[string]any{"retention": map[string]any{"days": days}}); err != nil {
		t.Fatalf("set retention: %v", err)
	}
}

// writeClaudeSettingsFile writes content as the Claude settings file at path.
func writeClaudeSettingsFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write Claude settings: %v", err)
	}
}

// writeTestClaudeSettings writes the user-level Claude settings file in the
// App's own provider home.
func writeTestClaudeSettings(t *testing.T, app *App, content string) {
	t.Helper()
	writeClaudeSettingsFile(t, claude.UserSettingsPathForHome(testProviderHome(t, app)), content)
}

// writeTestWorkspaceClaudeSettings writes a project-level Claude settings
// file in a new workspace directory and returns the directory.
func writeTestWorkspaceClaudeSettings(t *testing.T, content string) string {
	t.Helper()
	workDir := t.TempDir()
	writeClaudeSettingsFile(t, filepath.Join(workDir, ".claude", "settings.json"), content)
	return workDir
}

func TestGetClaudeTranscriptRetentionResolvesTheWindowAgainstClaudeSettings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		claudeFile    string // empty writes no file
		retentionDays int
		want          ClaudeTranscriptRetention
	}{
		{
			name:          "longer than Claude's default",
			retentionDays: 90,
			want:          ClaudeTranscriptRetention{ClaudeDays: 30, SessionDays: 90},
		},
		{
			name:          "the default window sends nothing",
			retentionDays: 30,
			want:          ClaudeTranscriptRetention{ClaudeDays: 30},
		},
		{
			name:          "disabled cleanup keeps transcripts for the cap",
			retentionDays: 0,
			want:          ClaudeTranscriptRetention{ClaudeDays: 30, SessionDays: settings.MaxRetentionDays},
		},
		{
			name:          "the user's longer period stands",
			claudeFile:    `{"cleanupPeriodDays":365}`,
			retentionDays: 90,
			want:          ClaudeTranscriptRetention{ClaudeDays: 365, Explicit: true},
		},
		{
			name:          "the user's shorter period is lengthened",
			claudeFile:    `{"cleanupPeriodDays":7}`,
			retentionDays: 90,
			want:          ClaudeTranscriptRetention{ClaudeDays: 7, Explicit: true, SessionDays: 90},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, _ := setupE2EApp(t)
			if tc.claudeFile != "" {
				writeTestClaudeSettings(t, app, tc.claudeFile)
			}
			got, err := app.GetClaudeTranscriptRetention(tc.retentionDays)
			if err != nil {
				t.Fatalf("GetClaudeTranscriptRetention: %v", err)
			}
			tc.want.SettingsPath = claude.UserSettingsPathForHome(testProviderHome(t, app))
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A Claude settings file whose value cannot be known sends no override and
// says why, so the storage settings page can show it.
func TestGetClaudeTranscriptRetentionReportsAnUnreadableClaudeFile(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	writeTestClaudeSettings(t, app, `{"cleanupPeriodDays":`)

	got, err := app.GetClaudeTranscriptRetention(90)
	if err != nil {
		t.Fatalf("GetClaudeTranscriptRetention: %v", err)
	}
	if got.Error == "" || got.SessionDays != 0 || got.ClaudeDays != 0 {
		t.Fatalf("got %+v, want an error and no override", got)
	}
	if !strings.Contains(got.Error, got.SettingsPath) {
		t.Fatalf("error %q does not name the file %q", got.Error, got.SettingsPath)
	}
}

func TestGetClaudeTranscriptRetentionRejectsAnOutOfRangeWindow(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	for _, days := range []int{-1, settings.MaxRetentionDays + 1} {
		if got, err := app.GetClaudeTranscriptRetention(days); err == nil {
			t.Fatalf("GetClaudeTranscriptRetention(%d) = %+v, want an error", days, got)
		}
	}
}

// claudeCleanupPeriodDays follows the saved window and the Claude file as
// each changes: on, off when the user's own period is longer, off while the
// file is unreadable, and on again once it is repaired.
func TestClaudeCleanupPeriodDaysFollowsTheSavedWindowAndTheClaudeFile(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)

	if got := app.claudeCleanupPeriodDays(""); got != 0 {
		t.Fatalf("default window: got %d, want 0", got)
	}
	setTestRetentionDays(t, app, 90)
	if got := app.claudeCleanupPeriodDays(""); got != 90 {
		t.Fatalf("90-day window: got %d, want 90", got)
	}
	writeTestClaudeSettings(t, app, `{"cleanupPeriodDays":365}`)
	if got := app.claudeCleanupPeriodDays(""); got != 0 {
		t.Fatalf("user keeps 365 days: got %d, want 0", got)
	}

	writeTestClaudeSettings(t, app, `not json`)
	for range 2 {
		if got := app.claudeCleanupPeriodDays(""); got != 0 {
			t.Fatalf("unreadable file: got %d, want 0", got)
		}
	}
	if app.claudeRetentionLogged[""] == "" {
		t.Fatal("an unreadable Claude settings file left no logged failure")
	}

	writeTestClaudeSettings(t, app, `{}`)
	if got := app.claudeCleanupPeriodDays(""); got != 90 {
		t.Fatalf("repaired file: got %d, want 90", got)
	}
	if len(app.claudeRetentionLogged) != 0 {
		t.Fatalf("a repaired file kept the logged failure %v", app.claudeRetentionLogged)
	}
	setTestRetentionDays(t, app, 0)
	if got := app.claudeCleanupPeriodDays(""); got != settings.MaxRetentionDays {
		t.Fatalf("disabled cleanup: got %d, want %d", got, settings.MaxRetentionDays)
	}
}

// The period is measured against what a process in that directory resolves:
// a workspace's own Claude settings outrank the user file, in both
// directions.
func TestClaudeCleanupPeriodDaysResolvesPerWorkingDirectory(t *testing.T) {
	t.Parallel()

	t.Run("a workspace that keeps longer is left alone", func(t *testing.T) {
		t.Parallel()
		app, _ := setupE2EApp(t)
		setTestRetentionDays(t, app, 90)
		writeTestClaudeSettings(t, app, `{"cleanupPeriodDays":7}`)
		keepsLonger := writeTestWorkspaceClaudeSettings(t, `{"cleanupPeriodDays":365}`)

		if got := app.claudeCleanupPeriodDays(keepsLonger); got != 0 {
			t.Fatalf("workspace keeping 365 days: got %d, want 0", got)
		}
		if got := app.claudeCleanupPeriodDays(t.TempDir()); got != 90 {
			t.Fatalf("workspace with no Claude settings: got %d, want 90", got)
		}
	})

	t.Run("a workspace that keeps less is lengthened", func(t *testing.T) {
		t.Parallel()
		app, _ := setupE2EApp(t)
		setTestRetentionDays(t, app, 90)
		writeTestClaudeSettings(t, app, `{"cleanupPeriodDays":365}`)
		keepsLess := writeTestWorkspaceClaudeSettings(t, `{"cleanupPeriodDays":7}`)

		if got := app.claudeCleanupPeriodDays(keepsLess); got != 90 {
			t.Fatalf("workspace keeping 7 days: got %d, want 90", got)
		}
		if got := app.claudeCleanupPeriodDays(t.TempDir()); got != 0 {
			t.Fatalf("workspace under the user's 365 days: got %d, want 0", got)
		}
	})
}

// A HOME in the Claude environment moves the home the child sweeps and reads
// its settings from, so the period is resolved there and not in the provider
// home.
func TestClaudeCleanupPeriodDaysResolvesInTheHomeTheChildRunsUnder(t *testing.T) {
	t.Parallel()
	setClaudeHome := func(t *testing.T, app *App, home string) {
		t.Helper()
		if _, err := app.settings.SetProviderEnvVar(string(provider.Claude), "HOME", home, false); err != nil {
			t.Fatalf("set the Claude environment: %v", err)
		}
	}

	t.Run("the child's longer period stands", func(t *testing.T) {
		t.Parallel()
		app, _ := setupE2EApp(t)
		setTestRetentionDays(t, app, 90)
		childHome := t.TempDir()
		writeClaudeSettingsFile(t, claude.UserSettingsPathForHome(childHome), `{"cleanupPeriodDays":365}`)
		setClaudeHome(t, app, childHome)

		if got := app.claudeCleanupPeriodDays(""); got != 0 {
			t.Fatalf("child home keeps 365 days: got %d, want 0", got)
		}
		state, err := app.GetClaudeTranscriptRetention(90)
		if err != nil {
			t.Fatalf("GetClaudeTranscriptRetention: %v", err)
		}
		if want := claude.UserSettingsPathForHome(childHome); state.SettingsPath != want {
			t.Fatalf("SettingsPath = %q, want %q", state.SettingsPath, want)
		}
	})

	t.Run("the provider home's period does not answer for the child", func(t *testing.T) {
		t.Parallel()
		app, _ := setupE2EApp(t)
		setTestRetentionDays(t, app, 90)
		writeTestClaudeSettings(t, app, `{"cleanupPeriodDays":365}`)
		setClaudeHome(t, app, t.TempDir())

		if got := app.claudeCleanupPeriodDays(""); got != 90 {
			t.Fatalf("child home sets no period: got %d, want 90", got)
		}
	})

	t.Run("a home that cannot be located sends none and says so", func(t *testing.T) {
		t.Parallel()
		app, _ := setupE2EApp(t)
		setTestRetentionDays(t, app, 90)
		setClaudeHome(t, app, "relative/home")

		if got := app.claudeCleanupPeriodDays(""); got != 0 {
			t.Fatalf("relative HOME: got %d, want 0", got)
		}
		if _, err := app.GetClaudeTranscriptRetention(90); err == nil || !strings.Contains(err.Error(), "HOME") {
			t.Fatalf("GetClaudeTranscriptRetention error = %v, want one naming HOME", err)
		}
	})
}

// startClaudeAndReadSettingsBlock starts a Claude session for a new thread in
// workDir against an argv-recording binary and returns its `--settings`
// value, empty when the session was spawned with none.
func startClaudeAndReadSettingsBlock(t *testing.T, app *App, workDir string) string {
	t.Helper()
	t.Cleanup(func() { _ = app.threadMCPServer().Close() })
	thread, err := createTestThread(t, app, string(provider.Claude), workDir, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	binary := writeArgvRecordingClaude(t, argvPath)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	block, _ := argValue(spawnedClaudeArgv(t, argvPath), "--settings")
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	return block
}

// The whole chain for a thread's session: saved window, the App's value for
// the thread's workspace, claude.Config, buildArgs, argv.
func TestStartSession_ClaudeCarriesTheRetentionWindow(t *testing.T) {
	t.Parallel()

	t.Run("a longer window is carried", func(t *testing.T) {
		t.Parallel()
		app, _ := setupE2EApp(t)
		setTestRetentionDays(t, app, 90)
		if block := startClaudeAndReadSettingsBlock(t, app, t.TempDir()); !strings.Contains(block, `"cleanupPeriodDays":90`) {
			t.Fatalf("--settings = %q, want it to carry cleanupPeriodDays 90", block)
		}
	})

	t.Run("a workspace that keeps longer gets none", func(t *testing.T) {
		t.Parallel()
		app, _ := setupE2EApp(t)
		setTestRetentionDays(t, app, 90)
		workDir := writeTestWorkspaceClaudeSettings(t, `{"cleanupPeriodDays":365}`)
		if block := startClaudeAndReadSettingsBlock(t, app, workDir); strings.Contains(block, "cleanupPeriodDays") {
			t.Fatalf("--settings = %q, want no cleanupPeriodDays over the workspace's 365", block)
		}
	})
}

// Every other Claude launch against the user's home takes its value from the
// same resolver, for its own working directory: the interactive TUI, account
// probes, MCP sign-in and text generation. The update commands are covered
// by TestProviderUpdateClaudeCommandsCarryTheRetentionWindow. A launch given
// no answer at all is refused by claude.NewCommandLine.
func TestEveryClaudeLaunchCarriesTheRetentionWindow(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	setTestRetentionDays(t, app, 90)
	binary := writeArgvRecordingClaude(t, filepath.Join(t.TempDir(), "argv.txt"))
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath":       binary,
		"textGenerationProvider": string(provider.Claude),
	}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	plain := t.TempDir()
	keepsLonger := writeTestWorkspaceClaudeSettings(t, `{"cleanupPeriodDays":365}`)

	tuiDays := func(workDir string) int {
		cfg, err := app.claudeTUISessionConfig("thread", provider.SessionOptions{
			Provider: string(provider.ClaudeTUI),
			WorkDir:  workDir,
		}, "", nil, sessionMemory{})
		if err != nil {
			t.Fatalf("claudeTUISessionConfig: %v", err)
		}
		return cfg.Retention.CleanupPeriodDays()
	}
	// A launch that was never given the resolver answers 0, which fails
	// below with that launch's name.
	resolve := func(resolver func(string) claude.Retention, workDir string) int {
		if resolver == nil {
			return 0
		}
		return resolver(workDir).CleanupPeriodDays()
	}
	claudeReportsModels(app)
	textgenAlternate, ok := app.resolveTextGenerationConfigFor(context.Background(), string(provider.Claude))
	if !ok {
		t.Fatal("resolveTextGenerationConfigFor did not resolve the mock Claude binary")
	}
	textgenPreferred := textGenConfig(t, app)
	if textgenPreferred.Provider != string(provider.Claude) {
		t.Fatalf("text generation chose %q, want claude", textgenPreferred.Provider)
	}
	mcpSignIn := app.mcpDeps().ClaudeRetention

	for name, launch := range map[string]func(workDir string) int{
		"interactive TUI": tuiDays,
		"MCP sign-in":     func(workDir string) int { return resolve(mcpSignIn, workDir) },
		"text generation": func(workDir string) int {
			return resolve(textgenPreferred.ClaudeRetention, workDir)
		},
		"text generation (alternate)": func(workDir string) int {
			return resolve(textgenAlternate.ClaudeRetention, workDir)
		},
	} {
		if got := launch(plain); got != 90 {
			t.Errorf("%s carries cleanupPeriodDays %d, want 90", name, got)
		}
		if got := launch(keepsLonger); got != 0 {
			t.Errorf("%s carries cleanupPeriodDays %d in a workspace keeping 365 days, want 0", name, got)
		}
	}
	// A probe runs in the home directory, not in a workspace.
	if got := app.ensureProviderAccountManager().ClaudeProbeConfig(binary, nil).Retention.CleanupPeriodDays(); got != 90 {
		t.Errorf("account probe carries cleanupPeriodDays %d, want 90", got)
	}
}

// The update check's `claude doctor` and the `claude update` run start the
// CLI against the user's home like every other launch, so they carry the
// window ahead of the subcommand and clear the config-home variables.
func TestProviderUpdateClaudeCommandsCarryTheRetentionWindow(t *testing.T) {
	resetClaudeProbeCacheForTest(t)
	app := newTestAppWithStore(t)
	setTestRetentionDays(t, app, 90)
	frames := watchProviderUpdates(app)
	binary := filepath.Join(t.TempDir(), "claude")
	writeClaudeProbeMockAt(t, binary, "99.0.0")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	version := updatefake.Baseline
	stubProviderBinaryDetect(t, func(providerName string) string {
		if providerName != string(provider.Claude) {
			return "0.149.0"
		}
		mu.Lock()
		defer mu.Unlock()
		return version
	})
	if err := app.providerUpdateFeed.SetLatest("99.1.0"); err != nil {
		t.Fatal(err)
	}
	commands := map[string]providerupdate.Command{}
	app.providerUpdateRunner = func(_ context.Context, command providerupdate.Command) (providerupdate.Output, error) {
		if command.Binary != binary {
			return providerupdate.Output{}, errors.New("unexpected binary " + command.Binary)
		}
		subcommand := command.Args[len(command.Args)-1]
		mu.Lock()
		commands[subcommand] = command
		mu.Unlock()
		switch subcommand {
		case "doctor":
			return providerupdate.Output{Text: "Auto-update channel: latest\n"}, nil
		case "update":
			mu.Lock()
			version = "99.1.0"
			mu.Unlock()
			return providerupdate.Output{Text: "Successfully updated"}, nil
		}
		return providerupdate.Output{}, errors.New("unexpected command")
	}

	if _, err := app.CheckProviderUpdates(); err != nil {
		t.Fatal(err)
	}
	if err := app.RunProviderUpdate(callSteppedUp("owner"), string(provider.Claude)); err != nil {
		t.Fatalf("RunProviderUpdate: %v", err)
	}
	frames.wait(t, func(s providerupdate.Status) bool {
		return s.Run.Phase == providerupdate.PhaseSucceeded || s.Run.Phase == providerupdate.PhaseFailed
	}, string(provider.Claude))

	mu.Lock()
	defer mu.Unlock()
	for _, subcommand := range []string{"doctor", "update"} {
		command, ran := commands[subcommand]
		if !ran {
			t.Errorf("claude %s never ran", subcommand)
			continue
		}
		want := []string{"--settings", `{"cleanupPeriodDays":90}`, subcommand}
		if !slices.Equal(command.Args, want) {
			t.Errorf("claude %s args = %q, want %q", subcommand, command.Args, want)
		}
		for _, name := range claude.ConfigHomeUnsetEnv() {
			if !slices.Contains(command.UnsetEnv, name) {
				t.Errorf("claude %s keeps an inherited %s (unset %q)", subcommand, name, command.UnsetEnv)
			}
		}
	}
}

// The update runner removes what a command's UnsetEnv names from the
// environment the command runs with.
func TestRunProviderUpdateGroupDropsUnsetEnv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/alternate")
	t.Setenv("AO_PROVIDER_UPDATE_KEPT", "kept")
	out, err := runProviderUpdateGroup(context.Background(), (&App{}).groupReaper("provider update"), providerupdate.Command{
		Binary:   "/bin/sh",
		Args:     []string{"-c", `printf '%s|%s' "${CLAUDE_CONFIG_DIR-unset}" "${AO_PROVIDER_UPDATE_KEPT-unset}"`},
		UnsetEnv: []string{"CLAUDE_CONFIG_DIR"},
		Dir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("runProviderUpdateGroup: %v", err)
	}
	if out.Text != "unset|kept" {
		t.Fatalf("child saw %q, want unset|kept", out.Text)
	}
}
