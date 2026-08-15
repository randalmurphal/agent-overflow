package aocli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/untrustedtext"
)

// `agent-overflow run inspect` and `agent-overflow run narrative` — the two
// reads that answer "what is this run actually doing" without leaving the CLI.
//
// `run status` answers where a run is; these answer what it is. Everything they
// print is already persisted — what was missing was a read that returns it
// together, without a supervising agent falling back to raw SQL against the
// database or to guessing at a file path.

// inspectInput / narrativeInput are the request bodies. Zero fields are omitted
// so "no phase named" reaches the app as absence rather than as an empty phase
// id it would have to special-case.
type inspectInput struct {
	ItemID  string `json:"itemId"`
	PhaseID string `json:"phaseId,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
}

type narrativeInput struct {
	ItemID  string `json:"itemId"`
	PhaseID string `json:"phaseId"`
	Attempt int    `json:"attempt,omitempty"`
	UnitID  string `json:"unitId,omitempty"`
}

// runInspection mirrors only what the human block prints; `--json` forwards the
// app's document verbatim, as everywhere else on this surface.
type runInspection struct {
	Run          runView         `json:"run"`
	WorktreePath string          `json:"worktreePath"`
	Branch       string          `json:"branch"`
	BaseBranch   string          `json:"baseBranch"`
	Children     []runChild      `json:"children"`
	Guidance     []runGuidance   `json:"guidance"`
	Phase        *runPhaseDetail `json:"phase"`
}

// runGuidance is one pending `run guide` entry. The app bounded the text and
// computed the age; the CLI only decides how it reads, and the text is quoted as
// untrusted data for the reason an envelope output is — a phase session can leave
// one, and the reader is usually another model.
type runGuidance struct {
	Text       string `json:"text"`
	AgeSeconds int64  `json:"ageSeconds"`
	By         string `json:"by"`
	ByRun      string `json:"byRun"`
}

type runChild struct {
	ItemID        string `json:"itemId"`
	WorkflowID    string `json:"workflowId"`
	State         string `json:"state"`
	Reason        string `json:"reason"`
	ParentPhaseID string `json:"parentPhaseId"`
	ParentUnitID  string `json:"parentUnitId"`
	ParentAttempt int    `json:"parentAttempt"`
}

type runPhaseDetail struct {
	PhaseID        string                     `json:"phaseId"`
	Attempt        int                        `json:"attempt"`
	Status         string                     `json:"status"`
	Provider       string                     `json:"provider"`
	Model          string                     `json:"model"`
	Effort         string                     `json:"effort"`
	Cause          string                     `json:"cause"`
	Outputs        map[string]json.RawMessage `json:"outputs"`
	Decision       string                     `json:"decision"`
	DecisionTarget string                     `json:"decisionTarget"`
	ExhaustedLoops []string                   `json:"exhaustedLoops"`
	Units          []runUnit                  `json:"units"`
}

type runUnit struct {
	UnitID      string `json:"unitId"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	UnitAttempt int    `json:"unitAttempt"`
	// Note is what the unit rests with: how it ended, or what a repair told its
	// next try. It prints because the status alone is ambiguous where it matters
	// most — a paused run's in-flight units rest `failed` with an interrupted
	// note, since there is no interrupted unit status.
	Note         string `json:"note"`
	Branch       string `json:"branch"`
	WorktreePath string `json:"worktreePath"`
}

// guidanceBy renders one entry's provenance. A phase names its run, because
// "another agent left this" and "a person left this" are different facts and the
// run id is what makes the first one checkable.
func guidanceBy(entry runGuidance) string {
	if entry.ByRun != "" {
		return entry.By + "(" + entry.ByRun + ")"
	}
	return entry.By
}

// formatAge renders how long an entry has waited, using Go's own duration
// spelling so a reader never has to divide anything. Seconds are the app's unit;
// rounding to them here would be rounding twice.
func formatAge(seconds int64) string {
	return (time.Duration(seconds) * time.Second).String()
}

var runInspectCommand = execCommand{
	name:  "agent-overflow run inspect",
	usage: runInspectUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		phase := flags.String("phase", "", "read this phase's latest attempt whole instead of digesting every phase")
		attempt := flags.Int("attempt", 0, "with --phase, read this attempt instead of the latest")
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("agent-overflow run inspect", args, 1, "exactly one run id"); err != nil {
				return exitError, err
			}
			// The app refuses this too — it is the one that knows the run — but
			// refusing it here saves a round trip for what is plainly a typo.
			if *attempt != 0 && strings.TrimSpace(*phase) == "" {
				return exitError, usageError("agent-overflow run inspect",
					"--attempt names an attempt of --phase; supply --phase too")
			}
			var inspection runInspection
			raw, err := c.callInto(&inspection, "WorkflowAgentInspectRun", inspectInput{
				ItemID: args[0], PhaseID: *phase, Attempt: *attempt,
			})
			if err != nil {
				return exitError, err
			}
			return exitOK, render(stdout, *jsonOutput, raw, inspection.block())
		}
	},
}

// block is the whole picture on one screen: where the run is, where its work
// lives, what it was seeded with, what it called, what each attempt ran with and
// produced — and, when one was named, that attempt read whole.
func (i runInspection) block() string {
	var block strings.Builder
	block.WriteString(i.Run.line())
	i.Run.writeBudgetLine(&block)
	i.Run.writeFailedUnitLines(&block)
	if line := fields(
		optionalField("worktree", i.WorktreePath),
		optionalField("branch", i.Branch),
		optionalField("base-branch", i.BaseBranch),
	); line != "" {
		block.WriteString("\n")
		block.WriteString(line)
	}
	writeSeedLines(&block, i.Run.Seeds)
	for _, child := range i.Children {
		fmt.Fprintf(&block, "\n%s", fields(
			"child="+child.ItemID,
			optionalField("workflow", child.WorkflowID),
			"state="+child.State,
			optionalField("reason", child.Reason),
			optionalField("called-by", childCoordinate(child)),
		))
	}
	for _, entry := range i.Guidance {
		fmt.Fprintf(&block, "\n%s", fields(
			"guidance="+untrustedtext.Quote(entry.Text, 0),
			"by="+guidanceBy(entry),
			"age="+formatAge(entry.AgeSeconds),
		))
	}
	i.Run.writeAttemptLines(&block)
	if i.Phase != nil {
		block.WriteString("\n")
		block.WriteString(i.Phase.block())
	}
	return block.String()
}

// childCoordinate names the invocation that made a child: the phase, the unit
// when a fan-out made it, and the attempt. A campaign's waves are all children
// of one phase, so the phase alone does not tell two of them apart.
func childCoordinate(child runChild) string {
	if child.ParentPhaseID == "" {
		return ""
	}
	coordinate := child.ParentPhaseID
	if child.ParentUnitID != "" {
		coordinate += "/" + child.ParentUnitID
	}
	if child.ParentAttempt > 0 {
		coordinate += fmt.Sprintf(".%d", child.ParentAttempt)
	}
	return coordinate
}

// block renders one attempt read whole. Output values are quoted as untrusted
// data: they came out of a model, this text usually lands in another one, and a
// findings blob that reads as an instruction is exactly the shape a gate
// decision must not take.
//
// A park cause prints on its own line, whole, for the same reason the outputs
// do: naming an attempt is how a caller says the bounded form on the status
// line was not enough. The engine already capped what it stored.
func (p runPhaseDetail) block() string {
	var block strings.Builder
	block.WriteString(fields(
		"attempt-of="+p.PhaseID,
		fmt.Sprintf("attempt=%d", p.Attempt),
		"status="+p.Status,
		optionalField("provider", p.Provider),
		optionalField("model", p.Model),
		optionalField("effort", p.Effort),
		optionalField("decision", decisionField(p.Decision, p.DecisionTarget)),
		optionalField("exhausted-loops", strings.Join(p.ExhaustedLoops, ",")),
	))
	if p.Cause != "" {
		fmt.Fprintf(&block, "\n  cause=%s", untrustedtext.Quote(p.Cause, 0))
	}
	names := make([]string, 0, len(p.Outputs))
	for name := range p.Outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&block, "\n  output %s=%s", name, untrustedtext.Quote(outputText(p.Outputs[name]), 0))
	}
	for _, unit := range p.Units {
		fmt.Fprintf(&block, "\n  %s", fields(
			"unit="+unit.UnitID,
			optionalField("kind", unit.Kind),
			"status="+unit.Status,
			fmt.Sprintf("try=%d", unit.UnitAttempt),
			// 0 runes = unbounded: naming an attempt is how a caller says the
			// bounded form on the status block was not enough.
			optionalField("note", unitNoteText(unit.Note, 0)),
			optionalField("branch", unit.Branch),
			optionalField("worktree", unit.WorktreePath),
		))
	}
	return block.String()
}

// outputText renders a full output value the way the app renders a digested
// one: a JSON string as its text, anything else as its JSON. Both then go
// through the same quoting, so the two forms of the same output read alike.
func outputText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

var runNarrativeCommand = execCommand{
	name:  "agent-overflow run narrative",
	usage: runNarrativeUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		phase := flags.String("phase", "", "the phase whose account to read (required)")
		attempt := flags.Int("attempt", 0, "read this attempt instead of the phase's latest")
		unit := flags.String("unit", "", "read one fan-out unit's account instead of the phase's")
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("agent-overflow run narrative", args, 1, "exactly one run id"); err != nil {
				return exitError, err
			}
			if strings.TrimSpace(*phase) == "" {
				return exitError, usageError("agent-overflow run narrative", "--phase <phase-id> is required")
			}
			var narrative runNarrative
			raw, err := c.callInto(&narrative, "WorkflowAgentRunNarrative", narrativeInput{
				ItemID: args[0], PhaseID: *phase, Attempt: *attempt, UnitID: *unit,
			})
			if err != nil {
				return exitError, err
			}
			// An attempt that wrote no account is an ANSWER — the phase ran, the
			// file was never created — so it exits 1 like a wait that rested
			// somewhere other than done, not 2 like a command that failed. A run,
			// phase, attempt, or unit that does not exist is refused by the app
			// above and exits 2, which is the distinction that keeps a typo from
			// reading as "there is nothing to read".
			if !narrative.Present {
				return exitFindings, render(stdout, *jsonOutput, raw, narrative.absentLine())
			}
			return exitOK, render(stdout, *jsonOutput, raw, narrative.block())
		}
	},
}

// runNarrative mirrors what the human output prints.
type runNarrative struct {
	ItemID      string `json:"itemId"`
	PhaseID     string `json:"phaseId"`
	Attempt     int    `json:"attempt"`
	UnitID      string `json:"unitId"`
	UnitAttempt int    `json:"unitAttempt"`
	Path        string `json:"path"`
	Present     bool   `json:"present"`
	Bytes       int64  `json:"bytes"`
	Truncated   bool   `json:"truncated"`
	Content     string `json:"content"`
}

// coordinate names which account this is, in the same field shape the rest of
// the surface uses, so the absent line and the header read identically.
func (n runNarrative) coordinate() string {
	return fields(
		"run="+n.ItemID,
		"phase="+n.PhaseID,
		fmt.Sprintf("attempt=%d", n.Attempt),
		optionalField("unit", n.UnitID),
		unitTryField(n.UnitAttempt),
	)
}

func unitTryField(try int) string {
	if try <= 0 {
		return ""
	}
	return fmt.Sprintf("try=%d", try)
}

// absentLine names the coordinate AND the path, because the two failures it has
// to tell apart look identical otherwise: an element that wrote no account, and
// a reader looking in the wrong place.
func (n runNarrative) absentLine() string {
	return fields(n.coordinate(), "narrative=absent") + "\nnothing wrote " + n.Path
}

// block prints the header line and then the account verbatim. The content is
// NOT quoted: it is the whole point of the command, a human reads it as prose,
// and the header above it already says it is a narrative.
func (n runNarrative) block() string {
	header := fields(n.coordinate(), "path="+n.Path, fmt.Sprintf("bytes=%d", n.Bytes))
	if n.Truncated {
		header += fmt.Sprintf(" truncated-at=%d", len(n.Content))
	}
	return header + "\n" + n.Content
}
