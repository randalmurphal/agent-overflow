// Command ao-harness drives a running agent test harness (or soak)
// instance from a shell: boot one, seed it, script its mock providers,
// wait on its event wire, read its database and its evidence logs, and
// stop it again.
//
// It is a pure client — a WS/HTTP peer plus a process supervisor. It
// links no App code, so it cannot fabricate app state; every capability
// here is an RPC the backend already exposes, and everything it reads on
// disk is a file the backend already writes.
//
// Full guide: cmd/ao-harness/AGENTS.md.
// Contract: docs/specs/testing-harness.md §3.
//
//go:generate go run . --generate-docs ../../docs/references/ao-harness.md
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// command is one row of the dispatch table. A table rather than a switch
// so `help` can print exactly what exists, and adding a verb is one
// edit.
type command struct {
	name string
	// summary is the one-line description in `ao-harness help`.
	summary string
	run     func(e *env, args []string) error
	// children describes command families. The same tree drives group help
	// and the generated reference, so adding a subcommand has one source.
	children []command
}

// commands is a function rather than a package var because `help` prints
// the table and is itself a row in it, which a var would make an
// initialization cycle.
func commands() []command {
	return []command{
		{name: "up", summary: "start a harness instance (detached) and print how to reach it", run: runUp},
		{name: "down", summary: "stop an instance (SIGTERM, then kill after 5s; --force for an orphaned pid)", run: runDown},
		{name: "list", summary: "list known instances, pruning rows whose process is gone", run: runList},
		{name: "info", summary: "identity, evidence paths and URL for one instance", run: runInfo},
		{name: "open", summary: "print the instance URL (--browser opens it)", run: runOpen},
		{name: "attach", summary: "host the instance page in a headless browser so ui/perf/bench work unattended", run: runAttach},
		{name: "rpc", summary: "call any App or Harness method by name with JSON arguments", run: runRPC},
		{name: "seed", summary: "apply a HarnessSeed spec (-f file, or - for stdin)", run: runSeed},
		{name: "reset", summary: "wipe app state without rebooting", run: runReset},
		{name: "threads", summary: "list thread rows, drafts included", run: runThreads},
		{name: "items", summary: "list a thread's items", run: runItems},
		{name: "send", summary: "send a message to a thread", run: runSend},
		{name: "scenario", summary: "set|list|clear mock scenario rules, rebuild one from a real thread, or validate files offline", run: runScenario, children: scenarioCommandDescriptors()},
		{name: "clone", summary: "build a harness data root from a copy of a real app data dir", run: runClone},
		{name: "mock", summary: "list|advance|emit|exit against registered mock providers", run: runMock, children: mockCommandDescriptors()},
		{name: "events", summary: "tail|await|count events on the wire", run: runEvents, children: eventsCommandDescriptors()},
		{name: "record", summary: "start|stop a replay bundle capture", run: runRecord, children: recordCommandDescriptors()},
		{name: "bundles", summary: "list recorded replay bundles", run: runBundles},
		{name: "replay", summary: "bundle|file|pause|resume|step|stop|status", run: runReplay, children: replayCommandDescriptors()},
		{name: "logs", summary: "tail backend|frontend-errors|ui-trace", run: runLogs},
		{name: "db", summary: "run one read-only SELECT against the instance database", run: runDB},
		{name: "ui", summary: "snapshot|query|state|diff the attached frontend", run: runUI, children: uiCommandDescriptors()},
		{name: "perf", summary: "start|stop|status|watch the perf meters", run: runPerf, children: perfCommandDescriptors()},
		{name: "monitor", summary: "list or operate typed app-feel monitor specifications", run: runMonitor, children: monitorCommandDescriptors()},
		{name: "bench", summary: "run a bench workload and write a perf report", run: runBench},
		{name: "run", summary: "run one strict managed workload plan", run: runManaged},
		{name: "profile", summary: "record a CPU profile of one scripted turn (needs a Chromium devtools endpoint)", run: runProfile},
		{name: "compare", summary: "prepare or run an offline A/B comparison capsule", run: runCompare, children: compareCommandDescriptors()},
		{name: "postmortem", summary: "read-only offline inspection of a stopped harness evidence root", run: runPostmortem},
		{name: "artifacts", summary: "list, pin, unpin, or clean failed-run artifacts", run: runArtifacts, children: artifactsCommandDescriptors()},
		{name: "health", summary: "roll up an instance's liveness, errors, memory and mocks", run: runHealth},
		{name: "version", summary: "print this CLI's build stamp", run: runVersion},
		{name: "help", summary: "print this help", run: runHelp},
	}
}

func main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}

// newEnv is one invocation's starting state: text output, and the
// instance $AO_HARNESS_INSTANCE names, which --instance then overrides.
// A function rather than a literal so the default is written once —
// a test that built its own env would not be testing the real default.
func newEnv(stdout, stderr io.Writer) *env {
	return &env{stdout: stdout, stderr: stderr, format: "text", instance: os.Getenv(instanceEnv)}
}

// Run executes one invocation and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	e := newEnv(stdout, stderr)
	if len(args) > 0 && args[0] == "--watchdog" {
		if err := runDetachedWatchdog(args[1:]); err != nil {
			fmt.Fprintf(stderr, "ao-harness watchdog: %v\n", err)
			return exitError
		}
		return exitOK
	}
	if len(args) >= 1 && args[0] == "--generate-docs" {
		if len(args) != 2 {
			fmt.Fprintln(stderr, "ao-harness: --generate-docs needs exactly one output path")
			return exitUsage
		}
		if err := writeReferenceDoc(args[1]); err != nil {
			fmt.Fprintf(stderr, "ao-harness: generate docs: %v\n", err)
			return exitError
		}
		return exitOK
	}
	// -h before a command name means the whole tool, not the globals the
	// root flag set happens to hold. --version is answered the same way:
	// it is a question about the binary, not about an instance.
	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "-help" {
			fmt.Fprint(stdout, usage())
			return exitOK
		}
		if arg == "--version" || arg == "-version" || arg == "-v" {
			fmt.Fprintf(stdout, "%s\n", version)
			return exitOK
		}
		if !strings.HasPrefix(arg, "-") {
			break
		}
	}
	// The root parse is NOT permuted: it takes the globals written before
	// the command name and stops at the command, leaving everything after
	// it (including that command's own -h) to the subcommand's flag set,
	// which binds the same globals again.
	root := e.newFlagSet("")
	if err := root.Parse(args); err != nil {
		return fail(e, usagef("%v", err))
	}
	rest := root.Args()
	if len(rest) == 0 {
		fmt.Fprint(stderr, usage())
		return exitUsage
	}
	if e.format != "text" && e.format != "json" {
		return fail(e, usagef("unknown output format %q (want text or json)", e.format))
	}
	for _, c := range commands() {
		if c.name != rest[0] {
			continue
		}
		if err := c.run(e, rest[1:]); err != nil {
			return fail(e, err)
		}
		return exitOK
	}
	return fail(e, usagef("unknown command %q", rest[0]))
}

// fail prints an error the way a script wants to read it: prose on
// stderr, exit 2 for a bad invocation and 1 for anything the harness or
// the filesystem refused.
func fail(e *env, err error) int {
	if errors.Is(err, errHelp) {
		return exitOK
	}
	var usage usageErr
	if errors.As(err, &usage) {
		fmt.Fprintf(e.stderr, "ao-harness: %v\n", err)
		fmt.Fprint(e.stderr, usageHint())
		return exitUsage
	}
	fmt.Fprintf(e.stderr, "ao-harness: %v\n", err)
	var coded exitCodeError
	if errors.As(err, &coded) {
		return coded.code
	}
	return exitError
}

func runHelp(e *env, _ []string) error {
	_, err := io.WriteString(e.stdout, usage())
	return err
}

func usage() string {
	var b strings.Builder
	b.WriteString("ao-harness: drive an agent test harness instance\n\n")
	b.WriteString("usage: ao-harness [global flags] <command> [args]\n\n")
	b.WriteString("global flags:\n")
	b.WriteString("  --instance <id|idPrefix|dataRoot>  which instance to act on (default: the only live\n")
	b.WriteString("                            one, else this worktree's default data root)\n")
	b.WriteString("  --registry-dir <dir>      override the instance registry directory\n")
	b.WriteString("  -o <text|json>            output format for read commands\n\n")
	b.WriteString("commands:\n")
	for _, c := range commands() {
		fmt.Fprintf(&b, "  %-9s %s\n", c.name, c.summary)
	}
	b.WriteString("\nRun `ao-harness <command> -h` for a command's own flags.\n")
	return b.String()
}

func usageHint() string {
	return "run `ao-harness help` for the command list\n"
}
