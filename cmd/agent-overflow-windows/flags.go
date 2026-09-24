//go:build windows

// flags.go owns the launcher's CLI surface. The launcher is GUI-only
// in production (double-click from Start Menu), but the make dev-wsl
// path inside WSL invokes it with --distro $WSL_DISTRO_NAME so the
// dev round-trip skips the picker the developer already implicitly
// answered by their choice of WSL shell.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/supervise"
	"agent-overflow/internal/wsllauncher"
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
	// autopilot armed; docs/architecture/soak-rig.md), or "perf"
	// (appidentity.ProfilePerf — a third isolated mocked instance reserved
	// for destructive renderer benchmarks). It is the ONE axis
	// behind every piece of per-instance state the launcher owns:
	// single-instance id, window title, WebView2 user-data dir, CDP port,
	// launcher log, window placement, and the backend's own isolation
	// flags. A single axis is deliberate — three ad-hoc flags would let an
	// isolated run share one of them and quietly reach into the
	// developer's real instance.
	Profile string

	// The in-app update's internal modes (update_trial.go). UpdatePreflight
	// is the answer file a new launcher writes after staging and checking
	// its payload beside UpdateStable in Distro, for update UpdateID.
	// UpdateApply runs the update with that id. Wait is a launcher that
	// must exit before this one claims the single-instance identity, named
	// by --wait-pid and --wait-start so a reused process id never matches.
	UpdatePreflight string
	UpdateID        string
	UpdateStable    string
	UpdateApply     string
	Wait            supervise.ProcessRef
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
		"launch profile: empty for normal, `harness` for the driveable mock instance, `soak` for its autopilot, or `perf` for isolated renderer benchmarks",
	)
	updatePreflight := fs.String("update-preflight", "", "internal in-app update mode")
	updateID := fs.String("update-id", "", "internal in-app update mode")
	updateStable := fs.String("update-stable", "", "internal in-app update mode")
	updateApply := fs.String("update-apply", "", "internal in-app update mode")
	waitPID := fs.Int("wait-pid", 0, "internal: wait for this launcher to exit first")
	waitStart := fs.String("wait-start", "", "internal: the start time of the --wait-pid launcher")
	if err := fs.Parse(args); err != nil {
		return launcherFlags{}, fmt.Errorf("parse flags: %w", err)
	}
	if *updateApply != "" && !wsllauncher.ValidUpdateID(*updateApply) {
		return launcherFlags{}, fmt.Errorf("--update-apply %q is not an update id", *updateApply)
	}
	if *updatePreflight != "" && !wsllauncher.ValidUpdateID(*updateID) {
		return launcherFlags{}, fmt.Errorf("--update-id %q is not an update id", *updateID)
	}
	if *waitPID < 0 {
		return launcherFlags{}, fmt.Errorf("--wait-pid %d is not a process id", *waitPID)
	}
	if (*waitPID == 0) != (*waitStart == "") {
		return launcherFlags{}, errors.New("--wait-pid and --wait-start go together")
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
		Distro:          strings.TrimSpace(*distro),
		Embedding:       *embedding,
		Profile:         normalizedProfile,
		UpdatePreflight: *updatePreflight,
		UpdateID:        *updateID,
		UpdateStable:    *updateStable,
		UpdateApply:     *updateApply,
		Wait:            supervise.ProcessRef{PID: *waitPID, Start: *waitStart},
	}, nil
}
