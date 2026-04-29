//go:build windows

package main

import (
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/wsldistro"
	"agent-overflow/internal/wsllauncher"
)

// TestExportAppDataToWSL_NoAPPDATA covers the defensive branch: if
// %APPDATA% is unset, the function is a no-op (sentinel logs once).
// The WSL backend's WSLConfigDir() returns ok=false in that case and
// the Settings UI hides the WSL distro switcher.
func TestExportAppDataToWSL_NoAPPDATA(t *testing.T) {
	t.Setenv("APPDATA", "")
	t.Setenv(wsldistro.AppDataEnv, "leftover")
	t.Setenv("WSLENV", "")

	exportAppDataToWSL()

	if got := os.Getenv(wsldistro.AppDataEnv); got != "leftover" {
		t.Errorf("AppDataEnv was clobbered: got %q, want %q (function should no-op)", got, "leftover")
	}
	if got := os.Getenv("WSLENV"); got != "" {
		t.Errorf("WSLENV was clobbered: got %q, want empty", got)
	}
}

// TestExportAppDataToWSL_SetsBoth_NoPriorWSLENV is the cold-start
// path: APPDATA exported to AGENT_OVERFLOW_WIN_APPDATA, WSLENV set to
// the /p translation rule alone.
func TestExportAppDataToWSL_SetsBoth_NoPriorWSLENV(t *testing.T) {
	const want = `C:\Users\dev\AppData\Roaming`
	t.Setenv("APPDATA", want)
	t.Setenv(wsldistro.AppDataEnv, "")
	t.Setenv("WSLENV", "")

	exportAppDataToWSL()

	if got := os.Getenv(wsldistro.AppDataEnv); got != want {
		t.Errorf("AppDataEnv = %q, want %q", got, want)
	}
	wantWSLENV := wsldistro.AppDataEnv + "/p"
	if got := os.Getenv("WSLENV"); got != wantWSLENV {
		t.Errorf("WSLENV = %q, want %q", got, wantWSLENV)
	}
}

// TestExportAppDataToWSL_PrependsToExistingWSLENV pins the merge
// behavior: a developer with an existing WSLENV (e.g. PYTHONPATH/p)
// must keep their rules — the function prepends our entry so we don't
// silently drop another tool's translation.
func TestExportAppDataToWSL_PrependsToExistingWSLENV(t *testing.T) {
	const appdata = `C:\Users\dev\AppData\Roaming`
	const prior = "PYTHONPATH/p:GOPATH/p"
	t.Setenv("APPDATA", appdata)
	t.Setenv(wsldistro.AppDataEnv, "")
	t.Setenv("WSLENV", prior)

	exportAppDataToWSL()

	got := os.Getenv("WSLENV")
	wantPrefix := wsldistro.AppDataEnv + "/p:"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("WSLENV = %q, want prefix %q", got, wantPrefix)
	}
	if !strings.Contains(got, prior) {
		t.Errorf("WSLENV = %q, lost prior rules %q", got, prior)
	}
}

// TestForwardDebugEnvToWSL_Unset_NoOp covers the production default:
// AGENT_OVERFLOW_DEBUG unset, the function must not touch WSLENV.
func TestForwardDebugEnvToWSL_Unset_NoOp(t *testing.T) {
	t.Setenv("AGENT_OVERFLOW_DEBUG", "")
	t.Setenv("WSLENV", "")

	forwardDebugEnvToWSL()

	if got := os.Getenv("WSLENV"); got != "" {
		t.Errorf("WSLENV = %q, want empty (function should no-op when var unset)", got)
	}
}

// TestForwardDebugEnvToWSL_Set_PrependsRule covers the dev-wsl path:
// AGENT_OVERFLOW_DEBUG=provider lands as a string rule in WSLENV so
// wsl.exe forwards it to the Linux backend.
func TestForwardDebugEnvToWSL_Set_PrependsRule(t *testing.T) {
	t.Setenv("AGENT_OVERFLOW_DEBUG", "provider")
	t.Setenv("WSLENV", "")

	forwardDebugEnvToWSL()

	if got := os.Getenv("WSLENV"); got != "AGENT_OVERFLOW_DEBUG" {
		t.Errorf("WSLENV = %q, want %q", got, "AGENT_OVERFLOW_DEBUG")
	}
}

// TestForwardDebugEnvToWSL_PrependsToExistingWSLENV pins co-existence
// with rules already added by exportAppDataToWSL (and any user rules).
// In production main() runs both functions in sequence; this verifies
// the second call doesn't drop the first's rule.
func TestForwardDebugEnvToWSL_PrependsToExistingWSLENV(t *testing.T) {
	const prior = "AGENT_OVERFLOW_WIN_APPDATA/p:PYTHONPATH/p"
	t.Setenv("AGENT_OVERFLOW_DEBUG", "all")
	t.Setenv("WSLENV", prior)

	forwardDebugEnvToWSL()

	got := os.Getenv("WSLENV")
	if !strings.HasPrefix(got, "AGENT_OVERFLOW_DEBUG:") {
		t.Errorf("WSLENV = %q, want prefix %q", got, "AGENT_OVERFLOW_DEBUG:")
	}
	if !strings.Contains(got, prior) {
		t.Errorf("WSLENV = %q, lost prior rules %q", got, prior)
	}
}

func TestResolveChosenDistro(t *testing.T) {
	distros := []wsllauncher.Distro{
		{Name: "Ubuntu-24.04", Default: true, Version: 2, State: "Running"},
		{Name: "Debian", Version: 2, State: "Stopped"},
	}
	soloDistros := []wsllauncher.Distro{
		{Name: "Ubuntu-24.04", Default: true, Version: 2, State: "Running"},
	}

	cases := []struct {
		name          string
		flags         launcherFlags
		cfg           *wsldistro.Config
		distros       []wsllauncher.Distro
		wantChosen    string
		wantTransient bool
	}{
		{
			name:          "override matches installed distro",
			flags:         launcherFlags{Distro: "Debian"},
			cfg:           &wsldistro.Config{Distro: "Ubuntu-24.04"},
			distros:       distros,
			wantChosen:    "Debian",
			wantTransient: true,
		},
		{
			name:          "override beats saved config when both match",
			flags:         launcherFlags{Distro: "Debian"},
			cfg:           &wsldistro.Config{Distro: "Debian"},
			distros:       distros,
			wantChosen:    "Debian",
			wantTransient: true,
		},
		{
			name:          "override pointing at unknown distro falls through to picker",
			flags:         launcherFlags{Distro: "Fedora"},
			cfg:           &wsldistro.Config{Distro: "Ubuntu-24.04"},
			distros:       distros,
			wantChosen:    "",
			wantTransient: false,
		},
		{
			name:          "saved config used when no override",
			flags:         launcherFlags{},
			cfg:           &wsldistro.Config{Distro: "Ubuntu-24.04"},
			distros:       distros,
			wantChosen:    "Ubuntu-24.04",
			wantTransient: false,
		},
		{
			name:          "stale saved config falls through to picker",
			flags:         launcherFlags{},
			cfg:           &wsldistro.Config{Distro: "Removed-Distro"},
			distros:       distros,
			wantChosen:    "",
			wantTransient: false,
		},
		{
			name:          "no override no saved config returns empty",
			flags:         launcherFlags{},
			cfg:           nil,
			distros:       distros,
			wantChosen:    "",
			wantTransient: false,
		},
		{
			name:          "no override and empty cfg.Distro returns empty",
			flags:         launcherFlags{},
			cfg:           &wsldistro.Config{Distro: ""},
			distros:       distros,
			wantChosen:    "",
			wantTransient: false,
		},
		{
			name:          "no distros installed, override fails through to picker",
			flags:         launcherFlags{Distro: "Ubuntu"},
			cfg:           nil,
			distros:       nil,
			wantChosen:    "",
			wantTransient: false,
		},
		{
			name:          "single distro auto-picks when no override and no saved config",
			flags:         launcherFlags{},
			cfg:           nil,
			distros:       soloDistros,
			wantChosen:    "Ubuntu-24.04",
			wantTransient: false,
		},
		{
			name:          "single distro auto-picks when no override and empty saved config",
			flags:         launcherFlags{},
			cfg:           &wsldistro.Config{Distro: ""},
			distros:       soloDistros,
			wantChosen:    "Ubuntu-24.04",
			wantTransient: false,
		},
		{
			name:          "single distro auto-picks even when saved config is stale",
			flags:         launcherFlags{},
			cfg:           &wsldistro.Config{Distro: "Removed-Distro"},
			distros:       soloDistros,
			wantChosen:    "Ubuntu-24.04",
			wantTransient: false,
		},
		{
			name:          "saved config still wins over auto-pick when it matches",
			flags:         launcherFlags{},
			cfg:           &wsldistro.Config{Distro: "Ubuntu-24.04"},
			distros:       soloDistros,
			wantChosen:    "Ubuntu-24.04",
			wantTransient: false,
		},
		{
			name:          "override beats single-distro auto-pick and stays transient",
			flags:         launcherFlags{Distro: "Ubuntu-24.04"},
			cfg:           nil,
			distros:       soloDistros,
			wantChosen:    "Ubuntu-24.04",
			wantTransient: true,
		},
		{
			name:          "stale override does not fall through to single-distro auto-pick",
			flags:         launcherFlags{Distro: "Fedora"},
			cfg:           nil,
			distros:       soloDistros,
			wantChosen:    "",
			wantTransient: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotChosen, gotTransient := resolveChosenDistro(tc.flags, tc.cfg, tc.distros)
			if gotChosen != tc.wantChosen {
				t.Errorf("chosen = %q, want %q", gotChosen, tc.wantChosen)
			}
			if gotTransient != tc.wantTransient {
				t.Errorf("transient = %v, want %v", gotTransient, tc.wantTransient)
			}
		})
	}
}
