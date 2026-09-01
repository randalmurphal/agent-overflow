package app

import (
	"context"
	"fmt"
	"strings"

	"agent-overflow/internal/editor"
	"agent-overflow/internal/wsldistro"
	"agent-overflow/internal/wsllauncher"
)

// Test seams. editor.IsWSL caches its /proc read via sync.Once, so
// tests must inject through these vars rather than mutate the cache.
var (
	wslIsWSL       = editor.IsWSL
	wslListDistros = wsllauncher.ListDistros
	wslConfigDir   = wsldistro.WSLConfigDir
	wslLoadConfig  = wsldistro.Load
	wslSaveConfig  = wsldistro.Save
)

// IsWSL reports whether the backend is running inside a WSL
// distribution.
//
//ao:scope threads:read
//ao:route home
func (a *App) IsWSL() (bool, error) {
	return wslIsWSL(), nil
}

// ListWSLDistros returns the WSL distros reported by `wsl.exe -l -v`
// over WSL interop, or nil + nil on non-WSL hosts. Re-queries each
// call so a distro installed mid-session shows up without a restart.
//
//ao:scope host
//ao:route home
func (a *App) ListWSLDistros() ([]wsllauncher.Distro, error) {
	if !wslIsWSL() {
		return nil, nil
	}
	distros, err := wslListDistros(context.Background())
	if err != nil {
		return nil, fmt.Errorf("list WSL distros: %w", err)
	}
	return distros, nil
}

// GetWSLDistroPreference returns the distro the Windows launcher is
// configured to boot into on next launch, or "" when the preference
// isn't readable: not running under WSL, the launcher didn't inject
// AGENT_OVERFLOW_WIN_APPDATA, or the wsl.json file doesn't exist yet.
//
//ao:scope host
//ao:route home
func (a *App) GetWSLDistroPreference() (string, error) {
	if !wslIsWSL() {
		return "", nil
	}
	dir, ok := wslConfigDir()
	if !ok {
		return "", nil
	}
	cfg, err := wslLoadConfig(dir)
	if err != nil {
		return "", fmt.Errorf("read launcher config: %w", err)
	}
	if cfg == nil {
		return "", nil
	}
	return cfg.Distro, nil
}

// SetWSLDistroPreference persists the new distro pick to the
// launcher's wsl.json. The change applies on the next Windows-side
// launch; the running backend stays in its current distro for the
// remainder of this session.
//
// Validates `name` against the live wsl.exe distro list so a typo or
// stale reference can't trap the user with an unbootable saved pick.
// InstalledVer / InstalledDistro are preserved by load-mutate-save —
// those fields are owned by the launcher's install path.
//
//ao:scope host
//ao:route home
//ao:stepup
func (a *App) SetWSLDistroPreference(name string) (string, error) {
	if !wslIsWSL() {
		return "", fmt.Errorf("WSL distro preference is only writable from a WSL backend")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("distro name is required")
	}
	distros, err := wslListDistros(context.Background())
	if err != nil {
		return "", fmt.Errorf("list WSL distros: %w", err)
	}
	found := false
	for _, d := range distros {
		if d.Name == name {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("distro %q not found among installed distros", name)
	}
	dir, ok := wslConfigDir()
	if !ok {
		return "", fmt.Errorf("launcher config directory unavailable; was this backend started via the Windows launcher?")
	}
	cfg, err := wslLoadConfig(dir)
	if err != nil {
		return "", fmt.Errorf("read launcher config: %w", err)
	}
	if cfg == nil {
		cfg = &wsldistro.Config{}
	}
	cfg.Distro = name
	if err := wslSaveConfig(dir, cfg); err != nil {
		return "", fmt.Errorf("write launcher config: %w", err)
	}
	return name, nil
}
