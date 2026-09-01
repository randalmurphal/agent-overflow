package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/harness/instanceinfo"
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
	// entryServe is the `serve` verb: a BOOT with a name, not a command.
	// It continues into flag parsing exactly as entryBoot does, with the
	// verb stripped off the argv first (serveBootArgs).
	entryServe
)

// serveVerb names the windowless boot mode
// (docs/specs/remote-access.md §7, "Headless serve mode and remote
// update"). It is a verb rather than a flag because a person types it,
// and a boot mode rather than an aocli command because it needs the
// embedded asset FS and the whole transport/App boot graph — both of
// which live in package main by construction (internal/AGENTS.md:
// executable-only code stays at the root). Routing it through
// aocli.topLevelCommands would mean injecting a boot callback INTO the
// CLI package, which inverts the dependency to gain nothing: the verb
// set the CLI owns would still not own this one.
const serveVerb = "serve"

// decideEntry classifies an argv. Pure: args plus one environment reader in,
// a decision out, no process state touched, so every branch is table-testable.
//
// The rules, in the order they are applied:
//
//  1. `serve` is a BOOT, and it is checked first so the CLI's verb table can
//     never acquire that name and silently reclassify it. Inside a session it
//     is refused like every other boot: a server started from inside an agent
//     session is a second app fighting the first one for the same SQLite file,
//     which is the whole entryRefuse class.
//  2. A top-level CLI verb is a CLI invocation anywhere, session or not. The
//     offline `workflow` commands are useful from a plain terminal and always
//     have been.
//  3. Outside a session (no AO_ENDPOINT) nothing else changes: this is the
//     binary a user double-clicks, and an unrecognised argument has always
//     landed in the desktop boot.
//  4. Inside a session, a leading flag this binary defines (--harness,
//     --connect, --data-dir, --listen, --print-url-fd, --mock-provider) is a
//     deliberate operator invocation and still boots — `make e2e` run from an
//     agent session inherits AO_ENDPOINT and must keep working.
//  5. Inside a session, anything else — no arguments at all, an unknown verb,
//     an unknown flag — is refused. An agent that typed a command we do not
//     have gets CLI help, never a second GUI process fighting the first one
//     for the same SQLite file.
func decideEntry(args []string, lookupEnv func(string) (string, bool)) entryMode {
	inSession := func() bool {
		endpoint, _ := lookupEnv(aocli.EnvEndpoint)
		return strings.TrimSpace(endpoint) != ""
	}
	if len(args) > 0 && args[0] == serveVerb {
		if inSession() {
			return entryRefuse
		}
		return entryServe
	}
	if len(args) > 0 && aocli.IsCommand(args[0]) {
		return entryCLI
	}
	if !inSession() {
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

// serveBootArgs strips the `serve` verb so the boot flags after it reach
// parseFlags. Go's flag package stops at the first non-flag token, so an
// argv still carrying the verb would parse zero flags and silently boot
// on defaults — the exact silent-default failure splitListenAddr exists
// to prevent one layer down.
func serveBootArgs(args []string) []string {
	if len(args) > 0 && args[0] == serveVerb {
		return args[1:]
	}
	return args
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
	fmt.Fprintf(os.Stderr,
		"agent-overflow: %s is set, so this is an Agent Overflow session and the app is already running; %s.\n",
		aocli.EnvEndpoint, inSessionComplaint(args))
	fmt.Fprint(os.Stderr, aocli.Usage())
	os.Exit(2)
}

// inSessionComplaint says which of the three refusals this argv earned.
// `serve` gets its own sentence because it IS one of this binary's verbs
// — telling an operator it is not a command would send them looking for a
// spelling mistake instead of at the second backend they just asked for.
func inSessionComplaint(args []string) string {
	switch {
	case len(args) == 0:
		return "it needs a command"
	case args[0] == serveVerb:
		return strconv.Quote(serveVerb) + " starts a backend, and this session is already talking to one"
	default:
		return strconv.Quote(args[0]) + " is not one of its commands"
	}
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
	autopilot          *bool
	isolatedProfile    *string
	launcherPID        *int
	launcherStartTime  *string
	launcherExecutable *string
	launcherProfile    *string
	launcherWebview    *string
	window             *bool
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
		listen:             flagSet.String("listen", "", "transport bind address (e.g. 127.0.0.1:0). Empty means use the default loopback + ephemeral port."),
		printURLFD:         flagSet.String("print-url-fd", "", "run headless and write {port,token} to this file descriptor as JSON. Falls back to a stdout sentinel when the fd isn't open."),
		connect:            flagSet.String("connect", "", "remote client mode: attach the desktop window to a backend instead of booting a local one. Takes a pairing link (pairs this device, then attaches), a backend this device is already paired with (its id, its endpoint, or host:port), or ws://host:port/?token=<value> for a backend on this machine. Skips local transport boot."),
		dataDir:            flagSet.String("data-dir", "", "data directory root override; app data lives in <data-dir>/agent-overflow. Required by --harness."),
		harness:            flagSet.Bool("harness", false, "agent test harness mode: headless boot on an isolated --data-dir with mock providers and the Harness RPC surface. See docs/architecture/agent-harness.md."),
		soak:               flagSet.Bool("soak", false, "launcher-shell isolated backend: harness-grade isolation (mock providers, isolated data dir + HOME) behind the ORDINARY headless bootstrap, so the Windows launcher can host it in a real WebView2 window. Launcher-owned wire flag; the historical name is why it says soak. Defaults --data-dir to ~/.agent-overflow-harness, or ~/.agent-overflow-soak with --autopilot."),
		autopilot:          flagSet.Bool("autopilot", false, "--soak only: arm the soak autopilot — seed two threads and start a never-ending streaming background-agent turn. This is what makes an isolated instance a SOAK rather than a harness. See docs/architecture/soak-rig.md."),
		isolatedProfile:    flagSet.String("isolated-profile", "", "internal --soak identity: `perf` selects the dedicated renderer-benchmark data root and discovery mode. Launcher-owned; empty is the Windows harness."),
		launcherPID:        flagSet.Int("launcher-pid", 0, "--soak only: the Windows launcher process that spawned this backend, so a deliberate teardown can close its window too. Set by the launcher; 0 means nobody hosts a window for us."),
		launcherStartTime:  flagSet.String("launcher-start-time", "", "--soak only: immutable launcher process birth marker; set by the Windows launcher."),
		launcherExecutable: flagSet.String("launcher-executable", "", "--soak only: immutable launcher executable path; set by the Windows launcher."),
		launcherProfile:    flagSet.String("launcher-profile", "", "--soak only: launcher profile identity; set by the Windows launcher."),
		launcherWebview:    flagSet.String("launcher-webview-profile", "", "--soak only: launcher WebView2 profile path; set by the Windows launcher."),
		window:             flagSet.Bool("window", false, "harness/soak mode only: open the real Wails webview window on the isolated backend instead of running headless. GUI builds only. See docs/specs/testing-harness.md."),
		mockProvider:       flagSet.String("mock-provider", "", "harness/soak mode only: path to the ao-mockprovider binary (default: alongside this executable)."),
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
	// soak boots the launcher-shell isolated backend: the same isolation
	// as harness, but speaking the headless {port, token} bootstrap so the
	// Windows launcher can point a real WebView2 window at it. dataDir
	// defaults to a fixed home-dir root when the operator (or the
	// launcher, which cannot know a Linux path) leaves it off.
	//
	// The flag name is historical and launcher-owned: this shell was built
	// for the soak rig, and the SOAK part of a soak — the autopilot — is
	// now its own flag. --soak alone is the Windows harness instance.
	soak bool
	// autopilot arms the soak preset on a --soak boot: seed two threads
	// and start a never-ending streaming background-agent turn
	// (docs/architecture/soak-rig.md). Without it the instance boots and
	// waits for whoever is driving it.
	autopilot bool
	// isolatedProfile distinguishes launcher-hosted harnesses that must not
	// share discovery identity or data. Empty is the ordinary Windows
	// harness. "perf" is the destructive renderer-benchmark instance.
	isolatedProfile string
	// launcherPID is the Windows launcher process hosting this backend's
	// window, or 0 when nobody does. Published in the instance discovery
	// files so `ao-harness down` can close the window the launcher
	// deliberately keeps open after its child dies.
	launcherPID            int
	launcherStartTime      string
	launcherExecutable     string
	launcherProfile        string
	launcherWebviewProfile string
	// window opens the real Wails webview window over an isolated
	// (--harness / --soak) backend instead of leaving it headless. Only
	// those two modes accept it, and only a GUI (!nogui) build can honor
	// it — see main_harness_window.go.
	window bool
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
		listenAddr:             *values.listen,
		connect:                *values.connect,
		dataDir:                *values.dataDir,
		harness:                *values.harness,
		soak:                   *values.soak,
		autopilot:              *values.autopilot,
		isolatedProfile:        strings.ToLower(strings.TrimSpace(*values.isolatedProfile)),
		launcherPID:            *values.launcherPID,
		launcherStartTime:      strings.TrimSpace(*values.launcherStartTime),
		launcherExecutable:     strings.TrimSpace(*values.launcherExecutable),
		launcherProfile:        strings.TrimSpace(*values.launcherProfile),
		launcherWebviewProfile: strings.TrimSpace(*values.launcherWebview),
		window:                 *values.window,
		mockProvider:           *values.mockProvider,
		resetTransportPort:     *values.resetTransportPort,
	}
	if out.isolatedProfile != "" && out.isolatedProfile != string(instanceinfo.ModePerf) {
		return cliFlags{}, fmt.Errorf("unknown --isolated-profile %q (valid: %q)", out.isolatedProfile, instanceinfo.ModePerf)
	}
	if out.isolatedProfile != "" && !out.soak {
		return cliFlags{}, errors.New("--isolated-profile requires --soak (only the Windows launcher assigns this identity)")
	}
	if out.isolatedProfile == string(instanceinfo.ModePerf) && out.autopilot {
		return cliFlags{}, errors.New("--isolated-profile perf cannot arm --autopilot")
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
			// Windows launcher spells only the profile's flags on the WSL
			// child's argv. The default is still an isolated,
			// non-config-root path, and prepareHarness re-checks it.
			//
			// Which default depends on who is booting us, on two axes. WHO:
			// a launcher-spawned boot has no meaningful cwd, so it takes a
			// fixed home-dir root the operator (and `make soak-check`) can
			// name; a native `--window` boot is started per checkout by
			// hand, so it takes the per-worktree root — two worktrees
			// running at once must not share a database. WHICH INSTANCE: the
			// autopilot refuses a data dir holding threads it did not seed,
			// so a soak can never share a root with a harness. The perf
			// identity is destructive by design and gets a third root.
			switch {
			case out.isolatedProfile == string(instanceinfo.ModePerf):
				out.dataDir = perfDefaultDataRoot()
			case out.window && out.autopilot:
				out.dataDir = instanceinfo.DefaultSoakDataRoot()
			case out.window:
				out.dataDir = instanceinfo.DefaultDataRoot()
			case out.autopilot:
				out.dataDir = soakDefaultDataRoot()
			default:
				out.dataDir = harnessDefaultDataRoot()
			}
		}
	}
	if out.autopilot && !out.soak {
		// The autopilot drives mock providers on an isolated data root
		// through the harness receiver. Outside --soak there is neither, so
		// this would be a flag that silently did nothing.
		return cliFlags{}, errors.New("--autopilot requires --soak (it arms the soak preset on the isolated backend)")
	}
	if out.launcherPID < 0 {
		// The value is published for another process to signal. A negative
		// pid is a process GROUP on POSIX and nonsense on Windows; refuse it
		// here rather than write it into a discovery file.
		return cliFlags{}, fmt.Errorf("--launcher-pid must be a process id, got %d", out.launcherPID)
	}
	if out.launcherPID != 0 && !out.soak {
		// Only the Windows launcher passes this, and it only ever spawns
		// the --soak shell. A value anywhere else would publish a pid that
		// `ao-harness down` might later signal.
		return cliFlags{}, errors.New("--launcher-pid requires --soak (only the Windows launcher hosts a window for this backend)")
	}
	launcherIdentityPresent := out.launcherStartTime != "" || out.launcherExecutable != "" || out.launcherProfile != "" || out.launcherWebviewProfile != ""
	if launcherIdentityPresent && !out.soak {
		return cliFlags{}, errors.New("launcher identity flags require --soak")
	}
	if out.soak && out.launcherPID > 0 && (!launcherIdentityPresent || out.launcherProfile == "" || out.launcherWebviewProfile == "") {
		return cliFlags{}, errors.New("--soak launcher identity is incomplete")
	}
	if out.soak && out.launcherPID == 0 && launcherIdentityPresent {
		return cliFlags{}, errors.New("launcher identity requires --launcher-pid")
	}
	if out.soak && out.launcherPID > 0 {
		if !instanceinfo.IsAbsolutePath(out.launcherExecutable) || !instanceinfo.IsAbsolutePath(out.launcherWebviewProfile) {
			return cliFlags{}, errors.New("launcher identity paths must be absolute")
		}
	}
	if out.window {
		// --window is a SHELL choice for an isolated backend, not a mode of
		// its own: it decides whether the harness/soak boot ends in a
		// webview window or in the headless signal wait.
		if !out.harness && !out.soak {
			return cliFlags{}, errors.New("--window requires --harness or --soak (the ordinary desktop boot is already windowed)")
		}
		if out.connect != "" {
			return cliFlags{}, errors.New("cannot combine --window with --connect")
		}
		if *values.printURLFD != "" {
			// --print-url-fd means "somebody else hosts the window" (the
			// Windows launcher). Honoring both would open a second window
			// onto the same backend.
			return cliFlags{}, errors.New("cannot combine --window with --print-url-fd (that bootstrap channel exists for a launcher-hosted window)")
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
