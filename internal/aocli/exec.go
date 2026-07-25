package aocli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// The execution surface: the commands that talk to a running Agent Overflow
// over the scoped RPC route (spec §5, D15). Everything here needs the AO_*
// session environment; `ao workflow …` stays offline and needs none of it.
//
// Exit codes follow the binary-wide scheme established by the offline commands:
// 0 success (a surface-and-skip replay included — the effect exists, which is
// what was asked for), 1 the "not the answer you wanted" outcome (a wait whose
// run rested somewhere other than `done`), 2 usage and operational errors.

// seedFlag collects repeated `--seed k=v`. A value that parses as JSON is used
// as JSON, so `--seed count=3` seeds a number and `--seed name=alice` seeds a
// string, without the caller having to quote-escape a shell round trip.
type seedFlag struct {
	values map[string]any
}

func (s *seedFlag) String() string { return "" }

func (s *seedFlag) Set(raw string) error {
	name, value, found := strings.Cut(raw, "=")
	name = strings.TrimSpace(name)
	if !found || name == "" {
		return fmt.Errorf("seed %q must be in key=value form", raw)
	}
	if s.values == nil {
		s.values = map[string]any{}
	}
	if _, exists := s.values[name]; exists {
		return fmt.Errorf("seed %q was given more than once", name)
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err == nil {
		s.values[name] = decoded
		return nil
	}
	s.values[name] = value
	return nil
}

// encode renders the collected seeds as the JSON object the RPCs take. Nil when
// nothing was seeded, so the app can tell "no seeds" from "an empty object".
func (s *seedFlag) encode() (json.RawMessage, error) {
	if len(s.values) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(s.values)
	if err != nil {
		return nil, fmt.Errorf("encode seeds: %w", err)
	}
	return encoded, nil
}

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
// `ao run start flow --wait` would otherwise read `--wait` as a second workflow
// id — an ordering rule no other CLI has and no caller would guess.
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

// usageError is a wrong-arguments failure raised after flag parsing, where the
// command knows what it needed and the flag package does not.
func usageError(command, message string) error {
	return fmt.Errorf("%s: %s", command, message)
}

// requireArgs enforces a fixed positional arity.
func requireArgs(command string, args []string, want int, shape string) error {
	if len(args) != want {
		return usageError(command, "expected "+shape)
	}
	for _, arg := range args {
		if strings.TrimSpace(arg) == "" {
			return usageError(command, "expected "+shape)
		}
	}
	return nil
}

// render writes either the RPC's own JSON result or the terse human line. Every
// command routes through it so `--json` means exactly one thing: the app's
// result shape, unedited by the CLI.
func render(stdout io.Writer, asJSON bool, raw json.RawMessage, human string) error {
	if asJSON {
		return writeRawJSON(stdout, raw)
	}
	if !strings.HasSuffix(human, "\n") {
		human += "\n"
	}
	return writeOutput(stdout, human)
}

// writeRawJSON pretty-prints an already-encoded result. A method that returns
// nothing prints an explicit `null` rather than an empty line, so a `--json`
// consumer always has a document to parse.
func writeRawJSON(stdout io.Writer, raw json.RawMessage) error {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		return fmt.Errorf("format JSON output: %w", err)
	}
	return writeOutput(stdout, indented.String()+"\n")
}

func fields(pairs ...string) string {
	kept := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		if pair != "" {
			kept = append(kept, pair)
		}
	}
	return strings.Join(kept, " ")
}

// optionalField renders `name=value` only when there is a value, so a terse
// line does not carry a column of empty keys.
func optionalField(name, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return name + "=" + value
}
