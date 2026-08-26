package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// env is one invocation: where output goes and what the global flags
// resolved to. Every command takes it rather than reading process state,
// so the whole router is testable without a filesystem or a backend.
type env struct {
	stdout io.Writer
	stderr io.Writer

	// instance selects which harness to talk to: an instance id or a
	// data root. Empty means "resolve it" (see instance.go).
	instance string
	// registryDir overrides where discovery rows are read and written.
	// Tests set it; a human never needs to.
	registryDir string
	// format is "text" (default) or "json".
	format string
}

func (e *env) jsonOutput() bool { return e.format == "json" }

// bindGlobals declares the global flags on a subcommand's flag set,
// defaulting to whatever the root parse already resolved. That is what
// makes both `ao-harness -o json info` and `ao-harness info -o json`
// work — an agent will type either.
func (e *env) bindGlobals(flags *flag.FlagSet) {
	flags.StringVar(&e.instance, "instance", e.instance, "instance id or data root to act on")
	flags.StringVar(&e.registryDir, "registry-dir", e.registryDir, "override the harness instance registry directory")
	flags.StringVar(&e.format, "o", e.format, "output format: text or json")
}

// usageErr marks a wrong-arguments failure, which exits 2 and prints
// usage. Everything else is an operational failure and exits 1: a script
// can tell "I typed it wrong" from "the harness said no".
type usageErr struct{ err error }

func (u usageErr) Error() string { return u.err.Error() }
func (u usageErr) Unwrap() error { return u.err }

func usagef(format string, args ...any) error {
	return usageErr{fmt.Errorf(format, args...)}
}

// parsePermuted parses flags that may appear before, between, or after
// positional arguments, and returns the positionals. `--` ends flag
// parsing, which is how `send` passes a message that starts with a dash.
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

// newFlagSet builds a subcommand flag set with the globals bound and
// flag-package output suppressed: this CLI prints its own usage.
func (e *env) newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet("ao-harness "+name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	e.bindGlobals(flags)
	return flags
}

// parse is newFlagSet's companion: parse permuted, translating the flag
// package's own complaints into usage errors and answering -h with the
// subcommand's own flag list. Asking for help is not a failure, so it
// exits 0 (errHelp), which matters because an agent that greps a
// command's flags should not have to ignore an exit code to do it.
func (e *env) parse(flags *flag.FlagSet, args []string) ([]string, error) {
	rest, err := parsePermuted(flags, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(e.stdout, "usage: %s [flags]\n\nflags:\n", flags.Name())
			flags.SetOutput(e.stdout)
			flags.PrintDefaults()
			return nil, errHelp
		}
		return nil, usagef("%v", err)
	}
	return rest, nil
}

// errHelp is -h: printed already, nothing failed.
var errHelp = errors.New("help requested")

// writeJSON prints a value as indented JSON — the -o json shape.
func (e *env) writeJSON(value any) error {
	encoder := json.NewEncoder(e.stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write JSON output: %w", err)
	}
	return nil
}

// writeRawJSON prints a server result verbatim, re-indented. A CLI that
// re-marshalled through map[string]any would reorder every object and
// turn integers into floats; the bytes the backend sent are the answer.
func (e *env) writeRawJSON(raw json.RawMessage) error {
	if len(raw) == 0 {
		_, err := fmt.Fprintln(e.stdout, "null")
		return err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		// Not valid JSON is a contract break, but printing what arrived
		// beats hiding it behind an encoder error.
		_, printErr := fmt.Fprintln(e.stdout, string(raw))
		return printErr
	}
	_, err := fmt.Fprintln(e.stdout, buf.String())
	return err
}

func (e *env) printf(format string, args ...any) {
	fmt.Fprintf(e.stdout, format, args...)
}

// table writes aligned rows. Terse by design: a terminal reader wants
// the columns that identify a row, and -o json carries everything else.
func (e *env) table(header []string, rows [][]string) error {
	w := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	if len(header) > 0 {
		fmt.Fprintln(w, strings.Join(header, "\t"))
	}
	for _, row := range rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	return w.Flush()
}

// truncate keeps a table cell readable. Runes, not bytes: a title is
// user text and cutting a multi-byte rune in half would print a replacement
// character in the middle of a column.
func truncate(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\t", " ")
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}
