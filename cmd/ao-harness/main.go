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
}

// commands is a function rather than a package var because `help` prints
// the table and is itself a row in it, which a var would make an
// initialization cycle.
func commands() []command {
	return []command{
		{"up", "start a harness instance (detached) and print how to reach it", runUp},
		{"down", "stop an instance (SIGTERM, then kill after 5s)", runDown},
		{"list", "list known instances, pruning rows whose process is gone", runList},
		{"info", "identity, evidence paths and URL for one instance", runInfo},
		{"open", "print the instance URL (--browser opens it)", runOpen},
		{"rpc", "call any App or Harness method by name with JSON arguments", runRPC},
		{"seed", "apply a HarnessSeed spec (-f file, or - for stdin)", runSeed},
		{"reset", "wipe app state without rebooting", runReset},
		{"threads", "list thread rows, drafts included", runThreads},
		{"items", "list a thread's items", runItems},
		{"send", "send a message to a thread", runSend},
		{"scenario", "set|list|clear mock scenario rules, or validate files offline", runScenario},
		{"mock", "list|advance|emit|exit against registered mock providers", runMock},
		{"events", "tail|await|count events on the wire", runEvents},
		{"record", "start|stop a replay bundle capture", runRecord},
		{"bundles", "list recorded replay bundles", runBundles},
		{"replay", "bundle|file|pause|resume|step|stop|status", runReplay},
		{"logs", "tail backend|frontend-errors|ui-trace", runLogs},
		{"db", "run one read-only SELECT against the instance database", runDB},
		{"ui", "snapshot|query|state|diff the attached frontend", runUI},
		{"perf", "start|stop|status|watch the perf meters", runPerf},
		{"bench", "run a bench workload and write a perf report", runBench},
		{"health", "roll up an instance's liveness, errors, memory and mocks", runHealth},
		{"help", "print this help", runHelp},
	}
}

func main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}

// Run executes one invocation and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	e := &env{stdout: stdout, stderr: stderr, format: "text"}
	// -h before a command name means the whole tool, not the globals the
	// root flag set happens to hold.
	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "-help" {
			fmt.Fprint(stdout, usage())
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
	b.WriteString("  --instance <id|dataRoot>  which instance to act on (default: the only live\n")
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
