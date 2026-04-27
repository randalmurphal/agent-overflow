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
}

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
	if err := fs.Parse(args); err != nil {
		return launcherFlags{}, fmt.Errorf("parse flags: %w", err)
	}
	return launcherFlags{Distro: strings.TrimSpace(*distro)}, nil
}
