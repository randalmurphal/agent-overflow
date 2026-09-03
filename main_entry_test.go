package main

import (
	"flag"
	"strings"
	"testing"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/serviceinstall"
)

// noEnv and inSession are the two environments decideEntry distinguishes:
// outside a provider session, and inside one.
func noEnv(string) (string, bool) { return "", false }

func inSession(name string) (string, bool) {
	if name == aocli.EnvEndpoint {
		return "http://127.0.0.1:41234", true
	}
	return "", false
}

func TestDecideEntry(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		lookupEnv func(string) (string, bool)
		want      entryMode
	}{
		{name: "bare invocation outside a session boots", args: nil, lookupEnv: noEnv, want: entryBoot},
		{name: "boot flags outside a session boot", args: []string{"--harness", "--data-dir", "/tmp/x"}, lookupEnv: noEnv, want: entryBoot},
		{name: "an unknown argument outside a session still boots", args: []string{"frobnicate"}, lookupEnv: noEnv, want: entryBoot},
		{name: "an offline verb outside a session is the CLI", args: []string{"workflow", "list"}, lookupEnv: noEnv, want: entryCLI},
		{name: "a session verb inside a session is the CLI", args: []string{"run", "start", "flow"}, lookupEnv: inSession, want: entryCLI},
		{name: "bare invocation inside a session is refused", args: nil, lookupEnv: inSession, want: entryRefuse},
		{name: "an unknown verb inside a session is refused", args: []string{"frobnicate"}, lookupEnv: inSession, want: entryRefuse},
		{name: "an unknown flag inside a session is refused", args: []string{"--help"}, lookupEnv: inSession, want: entryRefuse},
		// A developer running `make e2e` from inside an agent session inherits
		// AO_ENDPOINT. The harness must still boot, or the suite becomes
		// unrunnable from the place it is most often run.
		{name: "harness mode inside a session still boots", args: []string{"--harness", "--data-dir", "/tmp/x"}, lookupEnv: inSession, want: entryBoot},
		{name: "connect mode inside a session still boots", args: []string{"--connect", "ws://host:1/?token=t"}, lookupEnv: inSession, want: entryBoot},
		{name: "single-dash boot flags inside a session boot", args: []string{"-listen", "127.0.0.1:0"}, lookupEnv: inSession, want: entryBoot},
		{name: "a boot flag with an inline value inside a session boots", args: []string{"--data-dir=/tmp/x"}, lookupEnv: inSession, want: entryBoot},
		// `serve` is a boot with a name. It is matched BEFORE the CLI check,
		// so it can never be shadowed by a future aocli row of the same name,
		// and it keeps every boot flag that follows it.
		{name: "the serve verb outside a session is a serve boot", args: []string{"serve"}, lookupEnv: noEnv, want: entryServe},
		{name: "serve keeps its boot flags", args: []string{"serve", "--listen", "0.0.0.0:7777"}, lookupEnv: noEnv, want: entryServe},
		// Booting a second backend from inside an agent session talking to the
		// first one is the entryRefuse class, exactly like a bare invocation.
		{name: "the serve verb inside a session is refused", args: []string{"serve"}, lookupEnv: inSession, want: entryRefuse},
		{name: "serve with flags inside a session is refused", args: []string{"serve", "--listen", "0.0.0.0:7777"}, lookupEnv: inSession, want: entryRefuse},
		// The verb is matched exactly. A word that merely starts with it is an
		// unknown argument and takes the ordinary path.
		{name: "a word starting with serve is not the verb", args: []string{"serveall"}, lookupEnv: noEnv, want: entryBoot},
		{name: "serve is only the verb in first position", args: []string{"--listen", "0.0.0.0:1", "serve"}, lookupEnv: noEnv, want: entryBoot},
		// An empty AO_ENDPOINT is not a session: a broken injection must not
		// make the app unlaunchable.
		{
			name: "an empty endpoint is not a session",
			args: nil,
			lookupEnv: func(name string) (string, bool) {
				if name == aocli.EnvEndpoint {
					return "  ", true
				}
				return "", false
			},
			want: entryBoot,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := decideEntry(test.args, test.lookupEnv); got != test.want {
				t.Fatalf("decideEntry(%q) = %d, want %d", test.args, got, test.want)
			}
		})
	}
}

// The verb set main dispatches on is aocli's own — there is no second copy —
// so every command that package declares must reach the CLI branch, in a
// session and out of it alike.
func TestEveryCLICommandReachesTheCLIBranch(t *testing.T) {
	commands := aocli.Commands()
	if len(commands) == 0 {
		t.Fatal("aocli.Commands() is empty; main would dispatch nothing to the CLI")
	}
	for _, command := range commands {
		for _, env := range []struct {
			name   string
			lookup func(string) (string, bool)
		}{{"outside a session", noEnv}, {"inside a session", inSession}} {
			if got := decideEntry([]string{command}, env.lookup); got != entryCLI {
				t.Fatalf("decideEntry([%q]) %s = %d, want entryCLI", command, env.name, got)
			}
		}
	}
}

// The serve verb is deliberately NOT an aocli row (the argument is at
// serveVerb). If somebody adds one, decideEntry would still route it to the
// boot — the serve check runs first — and `agent-overflow serve` would then
// mean two different things depending on which file you read. Fail here
// instead, at the seam, rather than leave a shadowed CLI command behind.
func TestServeVerbIsNotACLICommand(t *testing.T) {
	if aocli.IsCommand(serveVerb) {
		t.Fatalf("aocli declares %q as a command, but main routes it to the serve boot before the CLI check", serveVerb)
	}
}

// The supervise verb is the same seam as serve: routed here, refused as a CLI
// command, so it cannot come to mean two things depending on which file you
// read. Unlike serve it is deliberately NOT in the root usage — nobody types
// it, a service manager does, and documenting it would invite an operator to
// start a supervisor by hand beside the one their unit already runs.
func TestSuperviseVerbIsNotACLICommand(t *testing.T) {
	if aocli.IsCommand(superviseVerb) {
		t.Fatalf("aocli declares %q as a command, but main routes it to the supervise boot before the CLI check", superviseVerb)
	}
	if superviseVerb == serveVerb {
		t.Fatal("the two boot verbs are the same string")
	}
}

// internal/serviceinstall writes `ExecStart=<binary> supervise` into the unit
// file and this package is what answers to it. Neither imports the other's
// spelling by accident, so the two are pinned here: a rename on one side alone
// installs a unit whose command this binary rejects, which shows up as a
// service that will not start and nothing else.
func TestTheInstalledUnitStartsTheVerbThisBinaryRoutes(t *testing.T) {
	if superviseVerb != serviceinstall.SuperviseVerb {
		t.Fatalf("main routes %q and serviceinstall installs %q",
			superviseVerb, serviceinstall.SuperviseVerb)
	}
}

// The root help text is the one place a person reads the verb set, and serve
// is the one verb in it that this package routes rather than aocli. A usage
// string that stopped naming it would leave the mode undiscoverable.
func TestRootUsageNamesTheServeVerb(t *testing.T) {
	if !strings.Contains(aocli.Usage(), "  "+serveVerb+" ") {
		t.Fatalf("aocli.Usage() does not document the %q verb:\n%s", serveVerb, aocli.Usage())
	}
}

// bootArgsAfterVerb strips the verb so the flags after it reach parseFlags. Go's
// flag package stops at the first non-flag token, so an argv that still
// carried the verb would parse ZERO flags and boot on defaults — the silent
// default this repo refuses everywhere else in the bind path.
func TestServeBootArgs(t *testing.T) {
	tests := []struct {
		name string
		verb string
		args []string
		want []string
	}{
		{name: "the verb is stripped", verb: serveVerb, args: []string{"serve"}, want: []string{}},
		{name: "flags after the verb survive", verb: serveVerb, args: []string{"serve", "--listen", "0.0.0.0:7777"}, want: []string{"--listen", "0.0.0.0:7777"}},
		{name: "an argv without the verb is untouched", verb: serveVerb, args: []string{"--listen", "0.0.0.0:7777"}, want: []string{"--listen", "0.0.0.0:7777"}},
		{name: "an empty argv is untouched", verb: serveVerb, args: nil, want: nil},
		{name: "the supervise verb is stripped by the same helper", verb: superviseVerb, args: []string{"supervise", "--data-dir", "/srv/ao"}, want: []string{"--data-dir", "/srv/ao"}},
		{name: "one verb does not strip the other", verb: superviseVerb, args: []string{"serve", "--data-dir", "/srv/ao"}, want: []string{"serve", "--data-dir", "/srv/ao"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := bootArgsAfterVerb(test.args, test.verb)
			if len(got) != len(test.want) {
				t.Fatalf("bootArgsAfterVerb(%q, %q) = %q, want %q", test.args, test.verb, got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("bootArgsAfterVerb(%q, %q) = %q, want %q", test.args, test.verb, got, test.want)
				}
			}
		})
	}

	// The whole point: after stripping, the ordinary boot flag set parses.
	flags, err := parseFlags(bootArgsAfterVerb([]string{"serve", "--listen", "0.0.0.0:7777", "--reset-transport-port"}, serveVerb))
	if err != nil {
		t.Fatalf("parseFlags after bootArgsAfterVerb: %v", err)
	}
	if flags.listenAddr != "0.0.0.0:7777" {
		t.Fatalf("listenAddr = %q, want %q", flags.listenAddr, "0.0.0.0:7777")
	}
	if !flags.resetTransportPort {
		t.Fatal("--reset-transport-port did not reach cliFlags through the serve argv")
	}
}

// The refusals a person can actually earn, plus the three flags that must
// keep working after the verb. Both verbs share one list on purpose: a
// supervisor hands its flags straight to the serve child it spawns, so a flag
// only one of them refused would be a unit that starts and immediately fails.
func TestCheckServeFlags(t *testing.T) {
	refused := []struct {
		name string
		args []string
	}{
		{name: "connect", args: []string{"--connect", "ws://host:1/?token=t"}},
		{name: "print-url-fd", args: []string{"--print-url-fd", "3"}},
		{name: "harness", args: []string{"--harness", "--data-dir", "/tmp/x"}},
		{name: "soak", args: []string{"--soak", "--data-dir", "/tmp/x"}},
		{name: "window", args: []string{"--soak", "--window", "--data-dir", "/tmp/x"}},
		{name: "mock-provider", args: []string{"--harness", "--data-dir", "/tmp/x", "--mock-provider", "/tmp/p"}},
	}
	for _, test := range refused {
		t.Run("refuses "+test.name, func(t *testing.T) {
			flags, err := parseFlags(test.args)
			if err != nil {
				t.Fatalf("parseFlags(%q): %v", test.args, err)
			}
			for _, verb := range []string{serveVerb, superviseVerb} {
				err := checkBackendVerbFlags(verb, flags)
				if err == nil {
					t.Fatalf("checkBackendVerbFlags(%q) accepted %q", verb, test.args)
				}
				// The refusal names the verb the operator typed. A supervisor
				// that reported a serve refusal would send them to the wrong
				// command line.
				if !strings.Contains(err.Error(), verb) {
					t.Fatalf("checkBackendVerbFlags(%q) = %v, which does not name the verb", verb, err)
				}
			}
		})
	}

	kept := [][]string{
		nil,
		{"--listen", "0.0.0.0:7777"},
		{"--data-dir", "/tmp/serve-root"},
		{"--reset-transport-port"},
		{"--listen", "127.0.0.1:0", "--data-dir", "/tmp/serve-root", "--reset-transport-port"},
	}
	for _, args := range kept {
		flags, err := parseFlags(args)
		if err != nil {
			t.Fatalf("parseFlags(%q): %v", args, err)
		}
		for _, verb := range []string{serveVerb, superviseVerb} {
			if err := checkBackendVerbFlags(verb, flags); err != nil {
				t.Fatalf("checkBackendVerbFlags(%q, %q) = %v, want nil", verb, args, err)
			}
		}
	}
}

// The launcher-identity flags are refused transitively: each one already
// requires --soak, and serve refuses --soak. This pins that reasoning rather
// than a second copy of the rule inside checkBackendVerbFlags.
func TestServeRefusesLauncherIdentityThroughSoak(t *testing.T) {
	for _, args := range [][]string{
		{"--autopilot"},
		{"--isolated-profile", "perf"},
		{"--launcher-pid", "42"},
	} {
		if _, err := parseFlags(args); err == nil {
			t.Fatalf("parseFlags(%q) succeeded; serve relies on --soak being required for it", args)
		}
	}
}

// isBootFlag asks the real flag set rather than a hand-kept list, so a flag
// added to newBootFlagSet is recognised here for free. This walks the set to
// prove that, and pins that a bare word is never mistaken for a flag.
func TestIsBootFlagTracksTheBootFlagSet(t *testing.T) {
	flagSet, _ := newBootFlagSet()
	var declared int
	flagSet.VisitAll(func(declaredFlag *flag.Flag) {
		declared++
		for _, form := range []string{"-" + declaredFlag.Name, "--" + declaredFlag.Name, "--" + declaredFlag.Name + "=value"} {
			if !isBootFlag(form) {
				t.Fatalf("isBootFlag(%q) = false for a declared boot flag", form)
			}
		}
	})
	if declared == 0 {
		t.Fatal("the boot flag set declares no flags")
	}
	for _, notAFlag := range []string{"run", "--nonesuch", "-nonesuch", "", "-"} {
		if isBootFlag(notAFlag) {
			t.Fatalf("isBootFlag(%q) = true", notAFlag)
		}
	}
}

// TestParseFlagsResetTransportPort pins the launcher-facing signal: the
// flag reaches cliFlags, defaults off, and is refused in --connect mode
// (no local transport means no pin to reset — a silent no-op there would
// tell an operator the fix failed without saying why).
func TestParseFlagsResetTransportPort(t *testing.T) {
	// The Windows launcher spells this on the child's argv; an already
	// installed backend from an older payload only understands this
	// spelling, so the wire word is pinned on both sides.
	if resetTransportPortFlag != "reset-transport-port" {
		t.Fatalf("resetTransportPortFlag = %q, want %q", resetTransportPortFlag, "reset-transport-port")
	}

	got, err := parseFlags([]string{"--listen", "127.0.0.1:0", "--print-url-fd", "0", "--" + resetTransportPortFlag})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !got.resetTransportPort {
		t.Error("--" + resetTransportPortFlag + " did not reach cliFlags")
	}

	got, err = parseFlags([]string{"--listen", "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if got.resetTransportPort {
		t.Error("resetTransportPort defaulted to true")
	}

	if _, err := parseFlags([]string{"--connect", "ws://host:1/", "--" + resetTransportPortFlag}); err == nil {
		t.Error("parseFlags accepted --connect with --" + resetTransportPortFlag)
	}
}
