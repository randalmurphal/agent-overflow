package aocli

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// The `/workflow` composer context (spec §5, D15). Typing `/workflow` in a chat
// composer inserts this block, which is the whole onboarding an agent gets: how
// to reach the app, which workflows exist here, and what is already running.
//
// The renderer is pure so its shape is testable without a database. The RPC that
// resolves the live data lives in the main package.

// Bounds on what the block may contain. A composer block competes with the
// user's actual message for context; a project with two hundred workflows must
// still produce a block worth reading, so both lists truncate and say so.
const (
	MaxComposerWorkflows = 25
	MaxComposerRuns      = 10
)

// ComposerWorkflow is one workflow the project can start.
type ComposerWorkflow struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Scope string `json:"scope"`
}

// ComposerRun is one run already in flight.
type ComposerRun struct {
	ItemID     string `json:"itemId"`
	WorkflowID string `json:"workflowId"`
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	PhaseID    string `json:"phaseId,omitempty"`
}

// ComposerContext is everything the block renders. The caller resolves it; this
// package decides what it looks like.
type ComposerContext struct {
	ProjectName string `json:"projectName"`
	// SharedDir and ProjectDir are the two directories `agent-overflow workflow`
	// resolves definitions from. ProjectDir is empty when the project has no
	// workflow directory yet, which is worth saying: it is where a new one goes.
	SharedDir  string `json:"sharedDir"`
	ProjectDir string `json:"projectDir"`
	// SessionReady reports whether this thread's session actually carries the
	// AO_* credentials. It is false for a thread with no live session; saying so
	// beats an agent discovering it by running a command that cannot work.
	SessionReady bool `json:"sessionReady"`
	// CommandOnPath reports whether boot published this binary under its
	// canonical name on the session's PATH (D30). Everything else in the block
	// assumes the command resolves, so the one case where it does not has to be
	// stated rather than left for the agent to discover as "command not found".
	CommandOnPath bool               `json:"commandOnPath"`
	Workflows     []ComposerWorkflow `json:"workflows"`
	// WorkflowOverflow and RunOverflow are how many entries were left out.
	WorkflowOverflow int           `json:"workflowOverflow"`
	Runs             []ComposerRun `json:"runs"`
	RunOverflow      int           `json:"runOverflow"`
}

// composerRow is one row of a two-column table in the block. The left cell is
// padded into one column at render time rather than by hand, so editing a row
// cannot leave the table misaligned in an agent's context window.
type composerRow struct{ left, right string }

// composerCommands is the command table the block prints: invocation, purpose.
var composerCommands = []composerRow{
	{"agent-overflow run start <workflow-id> [--goal <text>] [--seed k=v] [--wait]", "start a run"},
	{"agent-overflow run status|output|wait <run-id>", "check on one"},
	{"agent-overflow run list [--active]", "see this project's runs"},
	{"agent-overflow run pause|resume|cancel|rerun <run-id>", "control one"},
	{"agent-overflow run retry-unit <run-id> <unit-id>|retry-failed-units <run-id>", "repair a fan-out"},
	{"agent-overflow run soft-stop <run-id> [--clear]", "stop after the current wave"},
	{"agent-overflow workflow list|validate <path>|schema", "author one"},
	{"agent-overflow <command> --help", "full options"},
}

// composerRepair is the reason→verb map. A cold agent reads a park reason and
// still has to work out which verb acts on it, and the CLI deliberately has no
// verb for the human-judgment reasons — so the map has to say which reasons it
// covers AND that the rest are not CLI-repairable, or an agent will invent one.
var composerRepair = []composerRow{
	{"paused|interrupted|checkpoint → run resume", "unit-failed → run retry-failed-units (retry-unit for one)"},
	{"state failed → run rerun", "gate|question → a human decides in the app; surface it, don't answer it"},
}

// TrimComposerLists applies the bounds and records the overflow. Workflows sort
// by id so the same project renders the same block twice running; runs arrive
// newest-relevant-first from the caller and keep that order.
func TrimComposerLists(context ComposerContext) ComposerContext {
	sort.Slice(context.Workflows, func(i, j int) bool {
		return context.Workflows[i].ID < context.Workflows[j].ID
	})
	if len(context.Workflows) > MaxComposerWorkflows {
		context.WorkflowOverflow = len(context.Workflows) - MaxComposerWorkflows
		context.Workflows = context.Workflows[:MaxComposerWorkflows]
	}
	if len(context.Runs) > MaxComposerRuns {
		context.RunOverflow = len(context.Runs) - MaxComposerRuns
		context.Runs = context.Runs[:MaxComposerRuns]
	}
	return context
}

// RenderComposerContext produces the text block inserted into the composer.
func RenderComposerContext(context ComposerContext) string {
	context = TrimComposerLists(context)
	var block strings.Builder
	block.WriteString("Agent Overflow workflows are available in this session through the `agent-overflow` command.\n\n")

	if context.SessionReady {
		fmt.Fprintf(&block, "Your environment already carries %s, %s, and %s, so `agent-overflow` needs no configuration.\n",
			EnvEndpoint, EnvToken, EnvThreadID)
	} else {
		fmt.Fprintf(&block,
			"This thread has no live session yet, so %s and %s are not set. Send a message first; `agent-overflow` works from the session that starts.\n",
			EnvEndpoint, EnvToken)
	}
	block.WriteString("Your credential is scoped to this session and this project, and it is revoked when the session ends.\n")
	if !context.CommandOnPath {
		block.WriteString("Note: this app could not publish `agent-overflow` on this session's PATH, so the command may not resolve. The app's own log says why.\n")
	}
	block.WriteString("\n")

	block.WriteString("Commands:\n")
	writeComposerRows(&block, composerCommands)
	block.WriteString("Every command takes --json. `agent-overflow run wait` exits 1 if the run ends anywhere but done.\n")

	block.WriteString("When a run needs a human, the reason picks the fix:\n")
	writeComposerRows(&block, composerRepair)
	block.WriteString("`run status` names a run's failed units and its parent; any other reason names its own cause — read that run in the app.\n\n")

	fmt.Fprintf(&block, "Workflow definitions for %s live in:\n", displayName(context.ProjectName))
	fmt.Fprintf(&block, "  %s   (shared with every project)\n", displayPath(context.SharedDir))
	fmt.Fprintf(&block, "  %s   (this project only, and shadows a shared id)\n", displayPath(context.ProjectDir))

	block.WriteString("\n")
	if len(context.Workflows) == 0 {
		block.WriteString("No workflows are configured here yet. `agent-overflow workflow new --help` scaffolds one.\n")
	} else {
		block.WriteString("Configured workflows:\n")
		for _, workflow := range context.Workflows {
			fmt.Fprintf(&block, "  %s (%s) — %s\n", workflow.ID, workflow.Scope, displayName(workflow.Name))
		}
		if context.WorkflowOverflow > 0 {
			fmt.Fprintf(&block, "  ... and %d more; `agent-overflow workflow list` shows all of them.\n", context.WorkflowOverflow)
		}
	}

	block.WriteString("\n")
	if len(context.Runs) == 0 {
		block.WriteString("No runs are active in this project.\n")
	} else {
		block.WriteString("Active runs:\n")
		for _, run := range context.Runs {
			fmt.Fprintf(&block, "  %s\n", fields(
				run.ItemID, optionalField("workflow", run.WorkflowID),
				"state="+run.State, optionalField("reason", run.Reason), optionalField("phase", run.PhaseID)))
		}
		if context.RunOverflow > 0 {
			fmt.Fprintf(&block, "  ... and %d more; `agent-overflow run list --active` shows all of them.\n", context.RunOverflow)
		}
	}
	return block.String()
}

// writeComposerRows renders a two-column table with the left column padded to a
// common width. The width is counted in RUNES because that is what fmt's `%-*s`
// pads to; a byte count would silently over-pad any row carrying a `→`.
func writeComposerRows(block *strings.Builder, rows []composerRow) {
	width := 0
	for _, row := range rows {
		width = max(width, utf8.RuneCountInString(row.left))
	}
	for _, row := range rows {
		fmt.Fprintf(block, "  %-*s   %s\n", width, row.left, row.right)
	}
}

func displayName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "(unnamed)"
	}
	return name
}

func displayPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "(not created yet)"
	}
	return path
}
