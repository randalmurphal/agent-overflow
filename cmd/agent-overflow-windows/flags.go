//go:build windows

// flags.go owns the launcher's CLI surface. The launcher is GUI-only
// in production (double-click from Start Menu), but the make dev-wsl
// path inside WSL invokes it with --distro $WSL_DISTRO_NAME so the
// dev round-trip skips the picker the developer already implicitly
// answered by their choice of WSL shell.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"agent-overflow/internal/appidentity"
)

// launcherFlags carries the parsed CLI state. Today only --distro is
// surfaced; new flags get a field here and a Lookup in
// parseLauncherFlags so the existing caller in main() doesn't grow a
// switch per option.
type launcherFlags struct {
	// Distro, if non-empty, is the name of a WSL distro to launch in
	// directly. When set, the launcher skips both the saved-config
	// short-circuit and the picker, and does NOT persist the choice
	// to wsl.json — the override stays scoped to the current run so a
	// dev-mode invocation doesn't clobber the user's saved pick.
	Distro string
	// Embedding is the internal COM-server launch switch Windows appends when
	// a toast is activated while the launcher is not already running. The
	// normal boot still runs so Wails can register the toast callback while
	// the WSL backend and bridge come online.
	Embedding bool
	// Profile selects a launch profile: "" (the normal instance),
	// "harness" (appidentity.ProfileHarness — the isolated mocked
	// instance an agent or a developer drives), or "soak"
	// (appidentity.ProfileSoak — that same instance with the soak
	// autopilot armed; docs/architecture/soak-rig.md). It is the ONE axis
	// behind every piece of per-instance state the launcher owns:
	// single-instance id, window title, WebView2 user-data dir, CDP port,
	// launcher log, window placement, and the backend's own isolation
	// flags. A single axis is deliberate — three ad-hoc flags would let an
	// isolated run share one of them and quietly reach into the
	// developer's real instance.
	Profile string
}

// profileEnv is the environment fallback for --profile, so the axis can
// be set by whatever launches the .exe (make soak forwards it across the
// WSL→Windows interop hop) without editing an argv.
const profileEnv = "AGENT_OVERFLOW_PROFILE"

// parseLauncherFlags parses the CLI args and returns the launcher's
// flag state. We use flag.ContinueOnError so callers see typed errors
// (and flag.ErrHelp for -h/-help) instead of the flag package calling
// os.Exit out from under us.
func parseLauncherFlags(args []string) (launcherFlags, error) {
	fs := flag.NewFlagSet("agent-overflow", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	distro := fs.String(
		"distro",
		"",
		"skip the picker and launch directly in this WSL distro (used by `make dev-wsl`)",
	)
	embedding := fs.Bool("Embedding", false, "internal Windows toast activation mode")
	profile := fs.String(
		"profile",
		os.Getenv(profileEnv),
		"launch profile: empty for the normal instance, `harness` for the isolated mocked instance, or soak for that instance with the soak autopilot armed (docs/architecture/soak-rig.md)",
	)
	if err := fs.Parse(args); err != nil {
		return launcherFlags{}, fmt.Errorf("parse flags: %w", err)
	}
	// An unknown profile is an error, never a silent fall-back to the
	// default instance: a typo that resolved to "" would run the isolated
	// instance against the developer's own launcher.log, WebView2
	// profile, and single-instance identity.
	normalizedProfile, err := appidentity.NormalizeProfile(*profile)
	if err != nil {
		return launcherFlags{}, err
	}
	return launcherFlags{
		Distro:    strings.TrimSpace(*distro),
		Embedding: *embedding,
		Profile:   normalizedProfile,
	}, nil
}
