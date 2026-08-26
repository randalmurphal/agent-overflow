// app_provider_home.go holds THE seam every app-layer resolution of a
// provider home goes through.
//
// Why one seam: `~/.claude` and `~/.codex` are the developer's real,
// authenticated provider state. An isolated boot (--harness / --soak) and
// every test binary must be structurally unable to write to them, and the
// only way to make that true is to resolve the home in exactly one place
// that the isolation can override. Before this existed the override
// (App.credentialHomeOverride) was honoured by two call sites and eight
// others called os.UserHomeDir() directly — which, under
// AO_HARNESS_KEEP_HOME (documented as "read-only widening only"), meant
// the MCP config writer, the Claude memory-directory mkdir, the
// session-fork writer and the authenticated rate-limit probe all resolved
// against the developer's real home.
//
// TestAppLayerResolvesProviderHomesThroughOneSeam (app_provider_home_test.go)
// is the enforcement: a bare os.UserHomeDir() in app_*.go,
// internal/provider/, internal/claudeconfig or internal/codexconfig fails
// the build's test run unless it is on the allowlist there.
package main

import (
	"fmt"
	"os"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/codexconfig"
	"agent-overflow/internal/provider/claude/sessionfork"
)

// providerHome returns the home directory every provider-owned path
// (`<home>/.claude`, `<home>/.claude.json`, `<home>/.codex`) is resolved
// beneath: the isolation override when one is pinned, else the OS home.
//
// The override is set once before Start by newIsolatedProviderApp (to the
// harness-owned `<dataRoot>/home`) and by session-import fixtures. It is
// deliberately consulted for READS as well as writes: a keep-home run that
// could read the real trees but not write them would still hand every
// downstream writer a path INTO them (sessionfork writes beside the
// transcript it located), so "read-only widening" is not a property the
// backend can hold one call at a time.
func (a *App) providerHome() (string, error) {
	if a.credentialHomeOverride != "" {
		return a.credentialHomeOverride, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate provider home directory: %w", err)
	}
	return home, nil
}

// claudeConfigJSONPath is `<providerHome>/.claude.json` — the file Claude
// Code and Agent Overflow share for MCP server scopes.
func (a *App) claudeConfigJSONPath() (string, error) {
	home, err := a.providerHome()
	if err != nil {
		return "", err
	}
	return claudeconfig.PathForHome(home), nil
}

// claudeProjectsDir is `<providerHome>/.claude/projects` — where Claude
// files session transcripts, and therefore where every fork this app
// writes lands.
func (a *App) claudeProjectsDir() (string, error) {
	home, err := a.providerHome()
	if err != nil {
		return "", err
	}
	return sessionfork.ProjectsDirForHome(home), nil
}

// codexConfigTOMLPath is `<providerHome>/.codex/config.toml`.
func (a *App) codexConfigTOMLPath() (string, error) {
	home, err := a.providerHome()
	if err != nil {
		return "", err
	}
	return codexconfig.PathForHome(home), nil
}
