package main

import (
	"flag"
	"testing"

	"agent-overflow/internal/aocli"
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
