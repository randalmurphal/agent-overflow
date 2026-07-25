package aocli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// `ao notes …` and `ao schedule` — the automation half of the execution surface
// (spec §11). Notes are the continuity a scheduled run leaves for its next
// occurrence; a schedule is a standing instruction to start one.

// scheduleInput is the request body of `ao schedule`.
type scheduleInput struct {
	WorkflowID string          `json:"workflowId"`
	Scope      string          `json:"scope,omitempty"`
	Name       string          `json:"name,omitempty"`
	Cron       string          `json:"cron"`
	Seeds      json.RawMessage `json:"seeds,omitempty"`
}

func notesCommand(args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_ = writeOutput(stderr, notesUsage)
		return exitError
	}
	switch args[0] {
	case "help", "--help", "-h":
		if err := writeOutput(stdout, notesUsage); err != nil {
			return operationalError(stderr, err)
		}
		return exitOK
	case "get":
		return notesGetCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "set":
		return notesSetCommand.run(args[1:], lookupEnv, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "ao notes: unknown command %q\n", args[0])
		_ = writeOutput(stderr, notesUsage)
		return exitError
	}
}

var notesGetCommand = execCommand{
	name:  "ao notes get",
	usage: notesGetUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("ao notes get", args, 1, "exactly one automation id"); err != nil {
				return exitError, err
			}
			var notes string
			raw, err := c.callInto(&notes, "WorkflowAgentGetNotes", args[0])
			if err != nil {
				return exitError, err
			}
			// Notes are prose the previous run wrote. Human output is the text
			// itself, unlabelled, so it can be piped straight into a file.
			return exitOK, render(stdout, *jsonOutput, raw, notes)
		}
	},
}

var notesSetCommand = execCommand{
	name:  "ao notes set",
	usage: notesSetUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		file := flags.String("file", "", "read the notes from this file instead of stdin")
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("ao notes set", args, 1, "exactly one automation id"); err != nil {
				return exitError, err
			}
			notes, err := readNotes(*file)
			if err != nil {
				return exitError, err
			}
			var result struct {
				AutomationID string `json:"automationId"`
				Skipped      bool   `json:"skipped"`
			}
			raw, err := c.callInto(&result, "WorkflowAgentSetNotes", args[0], notes)
			if err != nil {
				return exitError, err
			}
			return exitOK, render(stdout, *jsonOutput, raw,
				fields("automation="+result.AutomationID, skippedField(result.Skipped)))
		}
	},
}

// readNotes takes the new notes from a file or from stdin. Empty is legal — it
// is how a run clears notes that no longer apply — but a missing file is not,
// so a typo'd path never silently erases the continuity note it meant to set.
func readNotes(file string) (string, error) {
	source := io.Reader(os.Stdin)
	if strings.TrimSpace(file) != "" {
		handle, err := os.Open(file)
		if err != nil {
			return "", fmt.Errorf("read notes: %w", err)
		}
		defer handle.Close()
		source = handle
	}
	content, err := io.ReadAll(source)
	if err != nil {
		return "", fmt.Errorf("read notes: %w", err)
	}
	return string(content), nil
}

var scheduleCommand = execCommand{
	name:  "ao schedule",
	usage: scheduleUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		cron := flags.String("cron", "", "five-field cron expression naming when to start the workflow")
		name := flags.String("name", "", "name shown in the automations list")
		scope := flags.String("scope", "", "resolve the workflow in this scope (shared|project)")
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		seeds := &seedFlag{}
		flags.Var(seeds, "seed", "seed one declared input as key=value (repeatable; JSON values are parsed)")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("ao schedule", args, 1, "exactly one workflow id"); err != nil {
				return exitError, err
			}
			if strings.TrimSpace(*cron) == "" {
				return exitError, usageError("ao schedule", "--cron is required")
			}
			encodedSeeds, err := seeds.encode()
			if err != nil {
				return exitError, err
			}
			var result struct {
				AutomationID string `json:"automationId"`
				Name         string `json:"name"`
				Cron         string `json:"cron"`
				Skipped      bool   `json:"skipped"`
			}
			raw, err := c.callInto(&result, "WorkflowAgentSchedule", scheduleInput{
				WorkflowID: args[0], Scope: *scope, Name: *name, Cron: *cron, Seeds: encodedSeeds,
			})
			if err != nil {
				return exitError, err
			}
			return exitOK, render(stdout, *jsonOutput, raw, fields(
				"automation="+result.AutomationID,
				fmt.Sprintf("name=%q", result.Name),
				fmt.Sprintf("cron=%q", result.Cron),
				skippedField(result.Skipped),
			))
		}
	},
}
