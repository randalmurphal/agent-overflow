package aocli

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"agent-overflow/internal/untrustedtext"
	"agent-overflow/internal/workflow/memory"
)

// `agent-overflow memory …` — the campaign-memory half of the execution surface.
//
// This is NOT `agent-overflow notes`. Notes are one automation's continuity
// message to its own next occurrence: one mutable string, replaced wholesale.
// Memory is a run TREE's accumulated lessons: append-only, many-authored, keyed
// by the root run, and injected into every element's prompt. Two shapes, two
// lifetimes, two verbs.

// memoryInput is the request body of `agent-overflow memory add`. It carries no
// provenance field, which is the point: the app stamps who wrote a note and
// when, from the session's own coordinates.
type memoryInput struct {
	ItemID string   `json:"itemId,omitempty"`
	Kind   string   `json:"kind"`
	Text   string   `json:"text"`
	Files  []string `json:"files,omitempty"`
}

type memoryListInput struct {
	ItemID string `json:"itemId,omitempty"`
	Kind   string `json:"kind,omitempty"`
}

func memoryCommand(args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_ = writeOutput(stderr, memoryUsage)
		return exitError
	}
	switch args[0] {
	case "help", "--help", "-h":
		if err := writeOutput(stdout, memoryUsage); err != nil {
			return operationalError(stderr, err)
		}
		return exitOK
	case "add":
		return memoryAddCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "list":
		return memoryListCommand.run(args[1:], lookupEnv, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "agent-overflow memory: unknown command %q\n", args[0])
		_ = writeOutput(stderr, memoryUsage)
		return exitError
	}
}

// pathFlag collects repeated `--file`. A note cites the evidence it is about;
// repeating the flag is how a note names more than one file without the caller
// inventing a separator the app would have to guess at.
type pathFlag struct {
	values []string
}

func (p *pathFlag) String() string { return strings.Join(p.values, ", ") }

func (p *pathFlag) Set(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("--file needs a path")
	}
	p.values = append(p.values, raw)
	return nil
}

var memoryAddCommand = execCommand{
	name:  "agent-overflow memory add",
	usage: memoryAddUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		kind := flags.String("kind", "", "one of "+memory.KindList())
		run := flags.String("run", "", "record against this run instead of the calling phase's own")
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		files := &pathFlag{}
		flags.Var(files, "file", "cite one path as evidence (repeatable)")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("agent-overflow memory add", args, 1, "exactly one note"); err != nil {
				return exitError, err
			}
			// The kind is checked here as well as in the app because a typo is a
			// usage error the caller fixes on the spot, and a round trip to learn
			// the vocabulary is a round trip the usage string already answered.
			if !memory.KnownKind(strings.TrimSpace(*kind)) {
				return exitError, usageError("agent-overflow memory add",
					"--kind must be one of "+memory.KindList())
			}
			var result struct {
				RootID string `json:"rootId"`
				Kind   string `json:"kind"`
				Wave   int    `json:"wave"`
				Path   string `json:"path"`
			}
			raw, err := c.callInto(&result, "WorkflowAgentAddMemory", memoryInput{
				ItemID: strings.TrimSpace(*run), Kind: strings.TrimSpace(*kind),
				Text: args[0], Files: files.values,
			})
			if err != nil {
				return exitError, err
			}
			return exitOK, render(stdout, *jsonOutput, raw, fields(
				"recorded="+result.Kind,
				fmt.Sprintf("wave=%d", result.Wave),
				"campaign="+result.RootID,
				"log="+result.Path,
			))
		}
	},
}

var memoryListCommand = execCommand{
	name:  "agent-overflow memory list",
	usage: memoryListUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		kind := flags.String("kind", "", "show only this kind ("+memory.KindList()+")")
		run := flags.String("run", "", "read this run's campaign instead of the calling phase's own")
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if len(args) != 0 {
				return exitError, usageError("agent-overflow memory list", "expected no positional arguments")
			}
			// Checked here for the same reason `memory add` checks it: a typo is a
			// usage error the caller fixes on the spot, and shipping it to learn the
			// vocabulary is a round trip the usage string already answered. An EMPTY
			// kind stays legal — it is the unfiltered read, not a kind.
			filter := strings.TrimSpace(*kind)
			if filter != "" && !memory.KnownKind(filter) {
				return exitError, usageError("agent-overflow memory list",
					"--kind must be one of "+memory.KindList())
			}
			var result memoryLog
			raw, err := c.callInto(&result, "WorkflowAgentListMemory", memoryListInput{
				ItemID: strings.TrimSpace(*run), Kind: filter,
			})
			if err != nil {
				return exitError, err
			}
			return exitOK, render(stdout, *jsonOutput, raw, result.block(filter))
		}
	},
}

// memoryLog decodes only what the human rendering prints, so the CLI never
// becomes a second definition of what a note looks like.
type memoryLog struct {
	RootID string `json:"rootId"`
	Path   string `json:"path"`
	Total  int    `json:"total"`
	// Skipped is lines the log holds that are not readable notes — a torn final
	// line from a crash. It is printed rather than hidden: a reader deciding
	// whether the memory is complete has to know one was lost.
	Skipped int `json:"skipped"`
	Notes   []struct {
		Kind       string   `json:"kind"`
		Text       string   `json:"text"`
		Files      []string `json:"files,omitempty"`
		At         int64    `json:"at"`
		Provenance struct {
			RunID   string `json:"runId"`
			PhaseID string `json:"phaseId"`
			Attempt int    `json:"attempt"`
			UnitID  string `json:"unitId"`
			Wave    int    `json:"wave"`
		} `json:"provenance"`
	} `json:"notes"`
}

// maxMemoryTextRunes bounds one note on a CLI line. It is generous next to the
// prompt digest's budget — a reader who ran the verb asked for the notes, and
// paying for them is the point — and still bounded, because the answer lands in
// an agent's context window and the log has no ceiling of its own.
const maxMemoryTextRunes = 1_000

// block renders the log newest LAST, which is the order the file holds and the
// order a reader scrolling a terminal wants: the most recent lesson is the one
// still on screen. That is the opposite of the injected digest, which is
// newest-first because it is bounded and the reader may never reach the end.
func (l memoryLog) block(kind string) string {
	var out strings.Builder
	header := fmt.Sprintf("campaign=%s notes=%d", l.RootID, len(l.Notes))
	if kind != "" {
		header += fmt.Sprintf(" of=%d kind=%s", l.Total, kind)
	}
	if l.Skipped > 0 {
		header += fmt.Sprintf(" unreadable-lines=%d", l.Skipped)
	}
	out.WriteString(header + " log=" + l.Path + "\n")
	if len(l.Notes) == 0 {
		out.WriteString("No notes recorded yet.\n")
		return out.String()
	}
	for _, note := range l.Notes {
		element := note.Provenance.PhaseID
		if note.Provenance.UnitID != "" {
			element += "/" + note.Provenance.UnitID
		}
		if element == "" {
			element = "human"
		} else if note.Provenance.Attempt > 0 {
			element += fmt.Sprintf(".%d", note.Provenance.Attempt)
		}
		out.WriteString(fmt.Sprintf("%-8s [wave %d %s] %s",
			note.Kind, note.Provenance.Wave, untrustedtext.Field(element),
			untrustedtext.Quote(note.Text, maxMemoryTextRunes)))
		for _, path := range note.Files {
			out.WriteString(" " + untrustedtext.Field(path))
		}
		out.WriteString("\n")
	}
	return out.String()
}
