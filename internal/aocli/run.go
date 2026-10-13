package aocli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/buildvariant"
)

const (
	exitOK       = 0
	exitFindings = 1
	exitError    = 2
)

// topLevelCommand is one row of the root dispatch table. Every top-level verb
// carries the same signature whether or not it uses every argument, so the
// table — not a switch — is what Run walks.
type topLevelCommand struct {
	name string
	run  func(args []string, configRoot string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int
	// remote marks a command that exists only for remote access; a build
	// without it refuses the command (internal/buildvariant).
	remote bool
}

// topLevelCommands is the single source of truth for what a top-level command
// is. Run dispatches through it and Commands reports it, so the app binary's
// verb dispatch (main.go, D30) cannot drift from the set this package handles:
// adding a row here is what makes `agent-overflow <verb>` reach it.
//
// One verb the root usage documents is deliberately NOT a row: `serve`, which
// boots the windowless backend. It is a BOOT, and a boot needs the embedded
// asset filesystem and the whole transport/App graph, both of which live in
// package main by construction. A row here would mean injecting a boot
// callback into this package, which inverts the dependency to gain nothing —
// the verb set this package owns still would not own that one. The argument
// lives beside the code that routes it, at serveVerb in main_entry.go.
var topLevelCommands = []topLevelCommand{
	{name: "remote", run: func(args []string, _ string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
		return remoteCommand(args, lookupEnv, stdout, stderr)
	}, remote: true},
	{name: "pair", run: func(args []string, root string, _ func(string) (string, bool), stdout, stderr io.Writer) int {
		return pairCommand(args, root, os.Stdin, stdout, stderr)
	}, remote: true},
	{name: "thread", run: func(args []string, root string, _ func(string) (string, bool), stdout, stderr io.Writer) int {
		return threadCommand(args, root, stdout, stderr)
	}},
	{name: "help", run: func(_ []string, _ string, _ func(string) (string, bool), stdout, stderr io.Writer) int {
		if err := writeOutput(stdout, rootUsage); err != nil {
			return operationalError(stderr, err)
		}
		return exitOK
	}},
	{name: "service", run: func(args []string, _ string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
		return serviceCommand(args, hostServiceEnv(lookupEnv), stdout, stderr)
	}, remote: true},
}

// Commands names every top-level command, in the order the dispatch table
// declares them. The app binary uses it to decide whether an argv is a CLI
// invocation or a boot, which is the one place a second, hand-maintained copy
// of this list would silently rot.
func Commands() []string {
	names := make([]string, 0, len(topLevelCommands))
	for _, command := range topLevelCommands {
		names = append(names, command.name)
	}
	return names
}

// IsCommand reports whether name is a top-level command this package handles.
func IsCommand(name string) bool {
	for _, command := range topLevelCommands {
		if command.name == name {
			return true
		}
	}
	return false
}

// Usage is the root help text, exported so the app binary can print it when it
// refuses an in-session invocation that names no command.
func Usage() string { return rootUsage }

// Run executes one CLI command and returns its process exit code. Environment
// lookup is injected so the execution commands are testable without mutating
// process state; os.LookupEnv is the production reader.
func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithEnv(args, os.LookupEnv, stdout, stderr)
}

// RunWithEnv is Run against a supplied environment.
func RunWithEnv(args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	root := flag.NewFlagSet("agent-overflow", flag.ContinueOnError)
	root.SetOutput(io.Discard)
	configRoot := root.String("config-root", "", "override the Agent Overflow config root")
	if err := root.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if writeErr := writeOutput(stdout, rootUsage); writeErr != nil {
				return operationalError(stderr, writeErr)
			}
			return exitOK
		}
		fmt.Fprintf(stderr, "agent-overflow: %v\n", err)
		_ = writeOutput(stderr, rootUsage)
		return exitError
	}

	rest := root.Args()
	if len(rest) == 0 {
		_ = writeOutput(stderr, rootUsage)
		return exitError
	}
	for _, command := range topLevelCommands {
		if command.name != rest[0] {
			continue
		}
		if command.remote && !buildvariant.RemoteAccess {
			fmt.Fprintf(stderr, "agent-overflow %s: %v\n", command.name, buildvariant.ErrRemoteAccessUnavailable)
			return exitError
		}
		return command.run(rest[1:], *configRoot, lookupEnv, stdout, stderr)
	}
	fmt.Fprintf(stderr, "agent-overflow: unknown command %q\n", rest[0])
	_ = writeOutput(stderr, rootUsage)
	return exitError
}

// resolveConfigRoot answers the data root a local command reads: the
// --config-root override when given, else the platform default.
func resolveConfigRoot(override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return override, nil
	}
	return appdirs.Root()
}

func writeOutput(output io.Writer, value string) error {
	if _, err := io.WriteString(output, value); err != nil {
		return fmt.Errorf("write command output: %w", err)
	}
	return nil
}

func operationalError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "agent-overflow: %v\n", err)
	return exitError
}
