package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"strings"
	"testing"
)

// run invokes the router the way main does and returns (exit, stdout,
// stderr). Every router test goes through this rather than calling a
// command function, so flag wiring is covered too.
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestNoArgumentsPrintsUsageAndExitsTwo(t *testing.T) {
	code, _, stderr := run(t)
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "usage: ao-harness") {
		t.Fatalf("stderr did not carry usage:\n%s", stderr)
	}
}

func TestUnknownCommandExitsTwoAndNamesIt(t *testing.T) {
	code, _, stderr := run(t, "frobnicate")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Fatalf("stderr did not name the command:\n%s", stderr)
	}
}

func TestUnknownOutputFormatIsRefusedBeforeAnythingIsOpened(t *testing.T) {
	code, _, stderr := run(t, "-o", "yaml", "list")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "unknown output format") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestHelpListsEveryCommand(t *testing.T) {
	code, stdout, _ := run(t, "help")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, c := range commands() {
		if !strings.Contains(stdout, c.name) {
			t.Errorf("help output omits %q:\n%s", c.name, stdout)
		}
	}
}

func TestTopLevelDashHPrintsTheWholeToolNotJustGlobals(t *testing.T) {
	code, stdout, _ := run(t, "-h")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "commands:") || !strings.Contains(stdout, "scenario") {
		t.Fatalf("-h did not print the command list:\n%s", stdout)
	}
}

// A subcommand's -h is a question, not a mistake: an agent reading a
// command's flags should not have to ignore a nonzero exit.
func TestSubcommandDashHPrintsItsFlagsAndExitsZero(t *testing.T) {
	code, stdout, _ := run(t, "db", "-h")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "-file") || !strings.Contains(stdout, "-limit") {
		t.Fatalf("db -h did not print its own flags:\n%s", stdout)
	}
}

func TestBenchDashHDoesNotResolveAnInstance(t *testing.T) {
	code, stdout, stderr := run(t, "bench", "-h", "--registry-dir", t.TempDir())
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "-repeat") || strings.Contains(stderr, "instances are running") {
		t.Fatalf("bench help was not answered locally\nstdout: %s\nstderr: %s", stdout, stderr)
	}
}

func TestMonitorListDashHDoesNotResolveAnInstance(t *testing.T) {
	code, stdout, stderr := run(t, "monitor", "list", "-h", "--registry-dir", t.TempDir())
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "-page-id") || strings.Contains(stderr, "no live instance") {
		t.Fatalf("monitor list help was not answered locally\nstdout: %s\nstderr: %s", stdout, stderr)
	}
}

func TestMonitorListParsesJSONAfterSubcommand(t *testing.T) {
	code, _, stderr := run(t, "monitor", "list", "-o", "json", "--registry-dir", t.TempDir())
	if code == exitUsage || !strings.Contains(stderr, "no live instance") {
		t.Fatalf("monitor list rejected its output/global flags before attach: exit=%d stderr=%s", code, stderr)
	}
}

func TestMonitorSubcommandHelpListsTypedOperationsWithoutAttaching(t *testing.T) {
	code, stdout, stderr := run(t, "monitor", "-h", "--registry-dir", t.TempDir())
	if code != exitOK {
		t.Fatalf("monitor help exit = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, name := range []string{"start", "heartbeat", "status", "collect", "stop", "cleanup", "last"} {
		if !strings.Contains(stdout, name) {
			t.Errorf("monitor help omits %q:\n%s", name, stdout)
		}
	}
}

func TestGlobalFlagsWorkBeforeAndAfterTheCommand(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"-o", "json", "--registry-dir", dir, "list"},
		{"list", "-o", "json", "--registry-dir", dir},
	} {
		code, stdout, stderr := run(t, args...)
		if code != exitOK {
			t.Fatalf("%v: exit = %d (%s)", args, code, stderr)
		}
		var payload struct {
			Instances []any `json:"instances"`
		}
		if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
			t.Fatalf("%v: stdout was not JSON: %v\n%s", args, err, stdout)
		}
	}
}

func TestParsePermutedTakesFlagsAfterPositionals(t *testing.T) {
	flags := flag.NewFlagSet("t", flag.ContinueOnError)
	name := flags.String("name", "", "")
	rest, err := parsePermuted(flags, []string{"alpha", "--name", "x", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if *name != "x" {
		t.Fatalf("name = %q, want x", *name)
	}
	if strings.Join(rest, ",") != "alpha,beta" {
		t.Fatalf("positionals = %v", rest)
	}
}

// `--` is what lets `send <thread> -- -starts-with-a-dash` work.
func TestParsePermutedStopsFlagParsingAtDoubleDash(t *testing.T) {
	flags := flag.NewFlagSet("t", flag.ContinueOnError)
	name := flags.String("name", "", "")
	rest, err := parsePermuted(flags, []string{"--name", "x", "--", "--name", "y"})
	if err != nil {
		t.Fatal(err)
	}
	if *name != "x" {
		t.Fatalf("name = %q, want x (the value after -- is text)", *name)
	}
	if strings.Join(rest, " ") != "--name y" {
		t.Fatalf("positionals = %v", rest)
	}
}

func TestUnknownSubcommandsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"scenario", "frobnicate"},
		{"mock", "frobnicate"},
		{"events", "frobnicate"},
		{"replay", "frobnicate"},
		{"record", "frobnicate"},
		{"logs", "frobnicate"},
		// The bridge-backed families route the same way, and their
		// subcommand names are the ones an agent is most likely to guess
		// wrong (`perf report`, `ui dump`, `bench everything`).
		{"ui", "frobnicate"},
		{"perf", "frobnicate"},
		{"bench", "frobnicate"},
	} {
		code, _, stderr := run(t, args...)
		if code != exitUsage {
			t.Errorf("%v: exit = %d, want %d (%s)", args, code, exitUsage, stderr)
		}
	}
}

// `events -h` asking what the family holds used to be answered as a
// mistake — `unknown events subcommand "-h"`, exit 2 — because every
// family routes on args[0].
func TestGroupDashHPrintsItsSubcommandsAndExitsZero(t *testing.T) {
	for group, want := range map[string]string{
		"events":   "channels",
		"ui":       "reload",
		"mock":     "advance",
		"scenario": "show",
		"perf":     "watch",
		"record":   "start",
		"replay":   "bundle",
		"monitor":  "list",
	} {
		for _, flag := range []string{"-h", "--help"} {
			code, stdout, stderr := run(t, group, flag)
			if code != exitOK {
				t.Errorf("%s %s: exit = %d (%s)", group, flag, code, stderr)
				continue
			}
			if !strings.Contains(stdout, want) {
				t.Errorf("%s %s did not list %q:\n%s", group, flag, want, stdout)
			}
		}
	}
}

// A rejected flag is answered with THIS command's flag list. The generic
// "run `ao-harness help`" sends a caller to the command table, which is
// never where the answer is: they already picked the command and got one
// flag wrong.
func TestAnUndefinedFlagPrintsThatCommandsFlags(t *testing.T) {
	code, _, stderr := run(t, "events", "tail", "--chanel", "provider:usage")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "-channel") || !strings.Contains(stderr, "-where") {
		t.Fatalf("stderr does not carry `events tail`'s own flags:\n%s", stderr)
	}
}

// flag.PrintDefaults reads the FIRST backquoted word in a usage string as
// the value placeholder, so a help string that backquoted prose renamed
// the flag's argument to that word.
func TestFlagHelpDoesNotRenameAnArgumentFromProse(t *testing.T) {
	code, stdout, _ := run(t, "ui", "snapshot", "-h")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	found := false
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "-save") {
			continue
		}
		found = true
		// A boolean flag's declaration line is the flag and nothing else.
		// Anything after it is a value placeholder PrintDefaults lifted out
		// of the first backquoted word in the usage prose.
		if strings.TrimSpace(line) != "-save" {
			t.Fatalf("--save picked up a value placeholder from its help prose: %q", line)
		}
	}
	if !found {
		t.Fatalf("ui snapshot -h did not list --save:\n%s", stdout)
	}
}

func TestVersionPrintsTheBuildStamp(t *testing.T) {
	code, stdout, _ := run(t, "version")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.HasPrefix(stdout, "ao-harness ") {
		t.Fatalf("version output = %q", stdout)
	}

	// --version is a question about the binary, answered before any
	// instance is resolved.
	code, flagOut, _ := run(t, "--version")
	if code != exitOK {
		t.Fatalf("--version exit = %d", code)
	}
	if strings.TrimSpace(flagOut) != version {
		t.Fatalf("--version printed %q, want %q", flagOut, version)
	}
}

// Piping a spec is the only shape `seed` takes; a shell that expanded the
// JSON onto argv got "seed takes no positional arguments", which names
// the symptom and not the fix.
func TestSeedNamesThePipeWhenHandedInlineJSON(t *testing.T) {
	code, _, stderr := run(t, "seed", `{"threads":[]}`)
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "pipe it") {
		t.Fatalf("stderr does not name the fix:\n%s", stderr)
	}
}

// A bundle listing that printed a raw epoch made the reader paste it into
// another tool before it said anything.
func TestBundleTimestampsMatchTheOtherListings(t *testing.T) {
	if got := bundleTime(0); got != "-" {
		t.Fatalf("an absent stamp rendered %q", got)
	}
	got := bundleTime(1756166400000)
	if !strings.HasPrefix(got, "2025-08-26") && !strings.HasPrefix(got, "2025-08-25") {
		t.Fatalf("bundleTime = %q, want an RFC3339 date", got)
	}
	if !strings.Contains(got, "T") {
		t.Fatalf("bundleTime = %q, want RFC3339", got)
	}
}
