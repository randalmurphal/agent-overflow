package app

import (
	"os"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/codexconfig"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// NativeMemoryStatus is whether each provider's own memory is on in the
// user's configuration on this computer, before the app's settings turn it
// off for the sessions it starts (docs/specs/memory.md, "Settings"). The
// Memory settings page warns from it when shared memory is on beside a
// provider's own.
type NativeMemoryStatus struct {
	Claude bool `json:"claude"`
	Codex  bool `json:"codex"`
}

// GetNativeMemoryStatus reads whether Claude's auto-memory and Codex's
// memories are on as the user configured them: Claude's from the variable
// its sessions get and the user's settings.json, on by default; Codex's
// from config.toml's `[features] memories`, off by default. A file that
// exists and cannot be read or parsed is an error naming it.
//
//ao:scope settings:read
//ao:route selected
func (a *App) GetNativeMemoryStatus() (NativeMemoryStatus, error) {
	home, err := a.providerHome()
	if err != nil {
		return NativeMemoryStatus{}, err
	}
	// The variable as a Claude session gets it: what the app sets in the
	// session's environment (sessionProcessEnv), else what it inherits.
	env, set := a.sessionProcessEnv(string(provider.Claude), nil, aoSessionCredential{})[claude.AutoMemoryOffEnv]
	if !set {
		env, set = os.LookupEnv(claude.AutoMemoryOffEnv)
	}
	claudeOn, err := claudeconfig.New(claudeconfig.PathForHome(home)).AutoMemoryOn(env, set)
	if err != nil {
		return NativeMemoryStatus{}, err
	}
	codexPath, err := a.codexConfigTOMLPath()
	if err != nil {
		return NativeMemoryStatus{}, err
	}
	codexOn, err := codexconfig.New(codexPath).MemoriesOn()
	if err != nil {
		return NativeMemoryStatus{}, err
	}
	return NativeMemoryStatus{Claude: claudeOn, Codex: codexOn}, nil
}
