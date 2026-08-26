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
