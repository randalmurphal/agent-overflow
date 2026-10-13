package aocli

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// The session surface: commands that talk to a running Agent Overflow over the
// scoped RPC route with the AO_* credential the app injected into the provider
// session. Exit codes follow the binary-wide scheme: 0 success, 2 usage and
// operational errors.

// execCommand is the shared skeleton of an execution subcommand: parse flags,
// resolve the session, run. Splitting it out is what keeps every subcommand's
// error handling and exit codes identical instead of nearly identical.
type execCommand struct {
	name  string
	usage string
	// bind registers the command's flags and returns the run function. The run
	// function is called only after flags parse and the session resolves.
	bind func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error)
}

func (c execCommand) run(args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(c.name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	execute := c.bind(flags)
	positionals, err := parsePermuted(flags, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if writeErr := writeOutput(stdout, c.usage); writeErr != nil {
				return operationalError(stderr, writeErr)
			}
			return exitOK
		}
		fmt.Fprintf(stderr, "%s: %v\n", c.name, err)
		_ = writeOutput(stderr, c.usage)
		return exitError
	}
	session, err := SessionFromEnv(lookupEnv)
	if err != nil {
		return operationalError(stderr, err)
	}
	code, err := execute(newClient(session), positionals, stdout)
	if err != nil {
		return operationalError(stderr, err)
	}
	return code
}

// parsePermuted parses flags that may appear before, after, or between
// positional arguments. Go's flag package stops at the first non-flag token, so
// `agent-overflow remote status <uuid> --computer <id>` would otherwise read
// `--computer` as a second request id, an ordering rule no other CLI has and no
// caller would guess.
//
// The idiom is to parse repeatedly, peeling one positional off each time. A
// literal `--` still terminates flag parsing: everything after it is positional,
// which is how a caller passes a value that begins with a dash.
func parsePermuted(flags *flag.FlagSet, args []string) ([]string, error) {
	var trailing []string
	for i, arg := range args {
		if arg == "--" {
			trailing = append(trailing, args[i+1:]...)
			args = args[:i]
			break
		}
	}
	positionals := make([]string, 0, len(args))
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		rest := flags.Args()
		if len(rest) == 0 {
			return append(positionals, trailing...), nil
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
}
