package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/wsllauncher"
)

// Argv handling for the app binary. One executable wears three hats: the GUI /
// headless backend, the internal re-exec sidecars (handled in main before
// anything here), and the workflow CLI (D30 — there is no separate `ao`
// binary). This file owns the rule that decides which one an argv asked for,
// and the flag set the boot modes are parsed from.

// entryMode is what an argv asked this process to be.
type entryMode int

const (
	// entryBoot continues into flag parsing and the mode switch: desktop
	// window, headless backend, remote client, or harness.
	entryBoot entryMode = iota
	// entryCLI hands the whole argv to the workflow CLI.
	entryCLI
	// entryRefuse prints CLI help and exits non-zero. Reached only from
	// inside a provider session, where a boot would mean a second app.
	entryRefuse
)

// decideEntry classifies an argv. Pure: args plus one environment reader in,
// a decision out, no process state touched, so every branch is table-testable.
//
// The rules, in the order they are applied:
//
//  1. A top-level CLI verb is a CLI invocation anywhere, session or not. The
//     offline `workflow` commands are useful from a plain terminal and always
//     have been.
//  2. Outside a session (no AO_ENDPOINT) nothing else changes: this is the
//     binary a user double-clicks, and an unrecognised argument has always
//     landed in the desktop boot.
//  3. Inside a session, a leading flag this binary defines (--harness,
//     --connect, --data-dir, --listen, --print-url-fd, --mock-provider) is a
//     deliberate operator invocation and still boots — `make e2e` run from an
//     agent session inherits AO_ENDPOINT and must keep working.
//  4. Inside a session, anything else — no arguments at all, an unknown verb,
//     an unknown flag — is refused. An agent that typed a command we do not
//     have gets CLI help, never a second GUI process fighting the first one
//     for the same SQLite file.
func decideEntry(args []string, lookupEnv func(string) (string, bool)) entryMode {
	if len(args) > 0 && aocli.IsCommand(args[0]) {
		return entryCLI
	}
	if endpoint, _ := lookupEnv(aocli.EnvEndpoint); strings.TrimSpace(endpoint) == "" {
		return entryBoot
	}
	if len(args) == 0 {
		return entryRefuse
	}
	if isBootFlag(args[0]) {
		return entryBoot
	}
	return entryRefuse
}

// isBootFlag reports whether an argument names one of this binary's own boot
// flags. The answer comes from the same flag set parseFlags uses, so a flag
// added there is recognised here without a second list to keep in step.
func isBootFlag(arg string) bool {
	name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	if name == arg {
		return false
	}
	name, _, _ = strings.Cut(name, "=")
	flagSet, _ := newBootFlagSet()
	return flagSet.Lookup(name) != nil
}

// refuseInSessionBoot is the entryRefuse action: say why, print the CLI's own
// help, and exit with the CLI's usage-error code so a scripted caller reads it
// the same way it reads any other bad invocation.
func refuseInSessionBoot(args []string) {
	complaint := "it needs a command"
	if len(args) > 0 {
		complaint = strconv.Quote(args[0]) + " is not one of its commands"
	}
	fmt.Fprintf(os.Stderr,
		"agent-overflow: %s is set, so this is an Agent Overflow session and the app is already running; %s.\n",
		aocli.EnvEndpoint, complaint)
	fmt.Fprint(os.Stderr, aocli.Usage())
	os.Exit(2)
}

// bootFlags holds the pointers newBootFlagSet's flags write into. It exists so
// the flag set has exactly one definition: parseFlags reads the values out of
// it, and isBootFlag only asks the set what names it knows.
type bootFlags struct {
	listen             *string
	printURLFD         *string
	connect            *string
	dataDir            *string
	harness            *bool
	soak               *bool
	mockProvider       *string
	resetTransportPort *bool
}

// newBootFlagSet declares every flag this binary's boot modes take. The flag
// set is independent of the Wails CLI's argument parsing — Wails' alpha builds
// shell out to subprocesses with custom flags and we don't want our flags to
// leak into the wails3 dev/build argv.
func newBootFlagSet() (*flag.FlagSet, bootFlags) {
	flagSet := flag.NewFlagSet("agent-overflow", flag.ContinueOnError)
	flagSet.SetOutput(os.Stderr)
	return flagSet, bootFlags{
		listen:       flagSet.String("listen", "", "transport bind address (e.g. 127.0.0.1:0). Empty means use the default loopback + ephemeral port."),
		printURLFD:   flagSet.String("print-url-fd", "", "run headless and write {port,token} to this file descriptor as JSON. Falls back to a stdout sentinel when the fd isn't open."),
		connect:      flagSet.String("connect", "", "Phase F remote client mode: attach the desktop window to a remote backend at ws://host:port/?token=<value>. Skips local transport boot."),
		dataDir:      flagSet.String("data-dir", "", "data directory root override; app data lives in <data-dir>/agent-overflow. Required by --harness."),
		harness:      flagSet.Bool("harness", false, "agent test harness mode: headless boot on an isolated --data-dir with mock providers and the Harness RPC surface. See docs/architecture/agent-harness.md."),
		soak:         flagSet.Bool("soak", false, "soak rig backend: harness-grade isolation (mock providers, isolated data dir + HOME) behind the ORDINARY headless bootstrap, so the Windows launcher can host it in a real WebView2 window. Defaults --data-dir to ~/.agent-overflow-soak. See docs/architecture/soak-rig.md."),
		mockProvider: flagSet.String("mock-provider", "", "harness/soak mode only: path to the ao-mockprovider binary (default: alongside this executable)."),
		resetTransportPort: flagSet.Bool(resetTransportPortFlag, false,
			"discard this install's pinned transport port before binding and adopt whatever the OS hands out. The Windows launcher passes it on its one retry when the pinned port turned out to be unreachable from the host (see main_transport_port.go)."),
	}
}

// resetTransportPortFlag is the flag name, taken from the package that
// owns the launcher↔backend argv contract rather than re-spelled here:
// the Windows launcher passes it to this binary across the WSL boundary
// (cmd/agent-overflow-windows), so one definition is what keeps a
// rename from leaving the launcher passing a flag we reject.
const resetTransportPortFlag = wsllauncher.ResetTransportPortFlag

// cliFlags carries the parsed command-line state. Four modes are
// mutually exclusive: --connect (Phase F remote-client), --print-url-fd
// (Phase D headless), --harness (agent test harness), and the default
// desktop boot. parseFlags enforces the pairwise conflicts so mode
// selection is unambiguous.
type cliFlags struct {
	listenAddr string
	printURLFD int
	headless   bool
	connect    string
	// dataDir overrides the data directory root (app data lives in
	// <dataDir>/agent-overflow). Usable with any local-backend mode;
	// required by --harness so a harness can never touch real data.
	dataDir string
	// harness boots the agent test harness: headless transport, isolated
	// data dir + HOME, mock providers, and the Harness RPC surface.
	harness bool
	// soak boots the soak rig backend: the same isolation as harness,
	// but speaking the headless {port, token} bootstrap so the Windows
	// launcher can point a real WebView2 window at it. dataDir defaults
	// to soakDefaultDataRoot() when the operator (or the launcher, which
	// cannot know a Linux path) leaves it off.
	soak bool
	// mockProvider optionally overrides where --harness finds the
	// ao-mockprovider binary (default: next to this executable).
	mockProvider string
	// resetTransportPort discards the persisted transport port before
	// binding, so this boot adopts a fresh one. See
	// main_transport_port.go for the pin it clears and
	// cmd/agent-overflow-windows for the retry that passes it.
	resetTransportPort bool
}

// parseFlags pulls the command-line flags for a boot.
//
// Returns a typed error rather than calling fatalf so the conflict
// branches are unit-testable. main() converts errors into the
// stderr-and-exit shape callers expect.
func parseFlags(args []string) (cliFlags, error) {
	flagSet, values := newBootFlagSet()
	if err := flagSet.Parse(args); err != nil {
		return cliFlags{}, fmt.Errorf("parse flags: %w", err)
	}

	out := cliFlags{
		listenAddr:         *values.listen,
		connect:            *values.connect,
		dataDir:            *values.dataDir,
		harness:            *values.harness,
		soak:               *values.soak,
		mockProvider:       *values.mockProvider,
		resetTransportPort: *values.resetTransportPort,
	}

	if out.connect != "" && *values.printURLFD != "" {
		return cliFlags{}, errors.New("cannot combine --connect with --print-url-fd")
	}
	if out.harness {
		if out.connect != "" {
			return cliFlags{}, errors.New("cannot combine --harness with --connect")
		}
		if *values.printURLFD != "" {
			// Harness mode prints its own bootstrap line (the harness JSON
			// includes strictly more than {port,token}); a second bootstrap
			// channel would just be a divergence risk.
			return cliFlags{}, errors.New("cannot combine --harness with --print-url-fd")
		}
		if out.dataDir == "" {
			return cliFlags{}, errors.New("--harness requires --data-dir (the harness refuses to run against real app data)")
		}
	}
	if out.soak {
		if out.harness {
			// Both mock providers on an isolated data dir, but they are
			// different shells with different bootstrap contracts. Picking
			// one silently would hand the Windows launcher a bootstrap line
			// it cannot parse.
			return cliFlags{}, errors.New("cannot combine --soak with --harness (--soak is the launcher-hosted variant)")
		}
		if out.connect != "" {
			return cliFlags{}, errors.New("cannot combine --soak with --connect")
		}
		if out.dataDir == "" {
			// Unlike --harness, an omitted --data-dir is normal here: the
			// Windows launcher spells only `--soak` on the WSL child's argv.
			// The default is still an isolated, non-config-root path, and
			// prepareHarness re-checks it.
			out.dataDir = soakDefaultDataRoot()
		}
	}
	if out.dataDir != "" && out.connect != "" {
		// --connect has no local backend, so a --data-dir would be
		// silently ignored. Reject so the operator notices.
		return cliFlags{}, errors.New("cannot combine --data-dir with --connect")
	}
	if out.mockProvider != "" && !out.harness && !out.soak {
		return cliFlags{}, errors.New("--mock-provider requires --harness or --soak")
	}
	if out.connect != "" && out.resetTransportPort {
		// --connect boots no local transport, so there is no pin to
		// reset. Refuse rather than no-op: an operator reaching for this
		// flag is trying to fix a bind, and a silent no-op would tell
		// them it didn't work without telling them why.
		return cliFlags{}, fmt.Errorf("cannot combine --connect with --%s", resetTransportPortFlag)
	}
	if out.connect != "" && out.listenAddr != "" {
		// --listen configures the *local* transport bind. In --connect
		// mode there is no local transport (we attach to a remote
		// backend instead), so a --listen value would be silently
		// dropped. Reject explicitly so the operator notices the
		// conflict before the desktop window opens against the wrong
		// origin.
		return cliFlags{}, errors.New("cannot combine --connect with --listen")
	}

	if *values.printURLFD != "" {
		n, err := strconv.Atoi(*values.printURLFD)
		if err != nil {
			return cliFlags{}, fmt.Errorf("parse --print-url-fd: %w", err)
		}
		out.printURLFD = n
		out.headless = true
	}
	return out, nil
}
