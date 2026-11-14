// app_claude_retention.go applies this backend's retention window to Claude
// Code's own transcript cleanup. The mechanism and the list of launches that
// carry the value are in internal/provider/claude/retention.go.
package app

import (
	"fmt"
	"log"
	"path/filepath"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/settings"
)

// ClaudeTranscriptRetention reports how Claude Code's transcript cleanup on
// this backend relates to a retention window.
type ClaudeTranscriptRetention struct {
	// SettingsPath is the user-level Claude Code settings file of the home
	// Claude processes this backend starts run under.
	SettingsPath string `json:"settingsPath"`
	// ClaudeDays is the `cleanupPeriodDays` that file resolves to: its
	// value, or Claude Code's default when it sets none. Zero when Error
	// is set.
	ClaudeDays int `json:"claudeDays"`
	// Explicit reports whether the file sets the key.
	Explicit bool `json:"explicit"`
	// SessionDays is the `cleanupPeriodDays` a Claude process this backend
	// starts carries where no workspace settings file sets the key. Zero
	// means it carries none: the window is no longer than ClaudeDays, or
	// Error is set.
	SessionDays int `json:"sessionDays"`
	// Error says why the file's value is unknown.
	Error string `json:"error,omitempty"`
}

// claudeProcessHome is the home directory a Claude process this backend
// starts runs under, which holds the config home it sweeps and the user
// settings file it reads: a HOME the Claude environment sets, else the
// provider home.
func (a *App) claudeProcessHome() (string, error) {
	home, set := a.sessionProcessEnv(string(provider.Claude), nil, aoSessionCredential{})["HOME"]
	if !set {
		return a.providerHome()
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("the Claude environment sets HOME to %q, which is not an absolute path", home)
	}
	return home, nil
}

// claudeTranscriptRetention resolves retention against the Claude settings
// files a process started in workDir reads; an empty workDir is the user
// file alone. The returned error is a failure to locate the home Claude
// processes run under; a file that cannot be read is reported in the result.
func (a *App) claudeTranscriptRetention(retention settings.RetentionSettings, workDir string) (ClaudeTranscriptRetention, error) {
	home, err := a.claudeProcessHome()
	if err != nil {
		return ClaudeTranscriptRetention{}, err
	}
	state := ClaudeTranscriptRetention{SettingsPath: claude.UserSettingsPathForHome(home)}
	resolved, err := claude.ReadCleanupPeriod(home, workDir)
	if err != nil {
		state.Error = err.Error()
		return state, nil
	}
	state.ClaudeDays = resolved.Days
	state.Explicit = resolved.Explicit
	state.SessionDays = claude.CleanupPeriodOverride(retention.KeepDays(), resolved)
	return state, nil
}

// claudeCleanupPeriodDays is the `cleanupPeriodDays` a Claude process started
// now in workDir carries, or 0 for none. Every Claude launch against the
// user's home takes its value from here.
//
// A failure sends none, because a value sent without knowing the period the
// process resolves itself could shorten it. It is logged when it starts or
// changes for that directory, and GetClaudeTranscriptRetention shows the
// user-level one on the storage settings page.
func (a *App) claudeCleanupPeriodDays(workDir string) int {
	state, err := a.claudeTranscriptRetention(a.currentSettings().Retention, workDir)
	problem := state.Error
	if err != nil {
		problem = err.Error()
	}
	a.claudeRetentionLogMu.Lock()
	changed := problem != a.claudeRetentionLogged[workDir]
	switch {
	case problem == "":
		delete(a.claudeRetentionLogged, workDir)
	case changed:
		if a.claudeRetentionLogged == nil {
			a.claudeRetentionLogged = make(map[string]string)
		}
		a.claudeRetentionLogged[workDir] = problem
	}
	a.claudeRetentionLogMu.Unlock()
	if changed && problem != "" {
		log.Printf("claude transcript retention: %s; Claude processes in %q start without a %s override",
			problem, workDir, claude.CleanupPeriodDaysSetting)
	}
	return state.SessionDays
}

// GetClaudeTranscriptRetention reports how Claude Code's transcript cleanup
// on this backend relates to a retention window of retentionDays, in the
// units of settings.RetentionSettings.Days. The storage settings page passes
// the value it shows, so the answer does not wait for a save.
//
//ao:scope settings:read
//ao:route home
func (a *App) GetClaudeTranscriptRetention(retentionDays int) (ClaudeTranscriptRetention, error) {
	if err := settings.ValidateRetentionDays(retentionDays); err != nil {
		return ClaudeTranscriptRetention{}, err
	}
	return a.claudeTranscriptRetention(settings.RetentionSettings{Days: retentionDays}, "")
}
