package workflowtools

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// The `/workflow` composer context (D31). Typing `/workflow` in a chat composer
// inserts this block, which is the whole onboarding an agent gets: which tools
// reach the app, which workflows exist here, and what is already running.
//
// The renderer is pure so its shape is testable without a database. The RPC that
// resolves the live data lives in `internal/app`.

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
	// SharedDir and ProjectDir are the two directories definitions resolve
	// from. ProjectDir is empty when the thread has no project.
	SharedDir  string `json:"sharedDir"`
	ProjectDir string `json:"projectDir"`
	// SessionReady reports whether this thread's live session carries the
	// workflow tools. It is false for a thread with no live session; saying so
	// beats an agent discovering it by calling a tool it does not have.
	SessionReady bool               `json:"sessionReady"`
	Workflows    []ComposerWorkflow `json:"workflows"`
	// WorkflowOverflow and RunOverflow are how many entries were left out.
	WorkflowOverflow int           `json:"workflowOverflow"`
	Runs             []ComposerRun `json:"runs"`
	RunOverflow      int           `json:"runOverflow"`
}

// composerRow is one row of a two-column table in the block. The left cell is
// padded into one column at render time rather than by hand, so editing a row
// cannot leave the table misaligned in an agent's context window.
type composerRow struct{ left, right string }

// composerTools is the tool table the block prints: the call, its purpose.
var composerTools = []composerRow{
	{"run_start(workflow_id, goal?, seeds?, wait_seconds?)", "start a run; bound to this thread, so its rest arrives here"},
	{"run_status(run_id, wait_seconds?)", "check on one, or wait for it to come to rest"},
	{"run_watch(run_id, cursor?, tree?)", "wait for the next state change; the call that replaces a sleep loop"},
	{"run_output(run_id)", "read its declared outputs and artifacts"},
	{"run_inspect(run_id, phase_id?, attempt?)", "read one whole: worktree, branch, seeds, called runs, outputs"},
	{"run_narrative(run_id, phase_id, unit_id?)", "read what an attempt says it did"},
	{"run_list(active_only?)", "see this project's runs"},
	{"run_control(run_id, action)", "pause, resume, cancel, rerun, soft_stop, retry_unit, retry_failed_units"},
	{"run_resolve(run_id, decision)", "approve or reject a parked gate, or answer a parked question"},
	{"run_amend(run_id, seeds)", "fix a parked run's seeds; the result says when the run reads them"},
	{"run_guide(run_id, text)", "steer a working run; delivered at its next phase entry, never mid-turn"},
	{"memory_add(kind, text) / memory_list()", "the campaign's shared lessons, injected into every element's prompt"},
	{"workflow_list / workflow_validate / workflow_new / workflow_schema", "author one"},
}

// composerRepair is the reason-to-call map. A cold agent reads a park reason
// and still has to work out which call acts on it, so every reason a call
// settles names that call here and says what taking it does: the difference
// between resume and resume with phase_id is a decision, not a detail. A reason
// absent from the map is one whose own cause is the instruction, which is what
// the line under the table says, because an unexplained omission is what makes
// an agent invent a call.
var composerRepair = []composerRow{
	{"paused|interrupted|checkpoint → run_control resume", "continues where the run parked"},
	{"gate (decision=human) → run_resolve approve|reject", "takes one of the two routes the gate declared"},
	{"gate (decision=park) → run_control resume", "a park: route declares no approve or reject; resume re-enters the phase once its cause is addressed"},
	{"question → run_resolve answer", "the answer reaches the phase that asked"},
	{"unit-failed → run_control retry_failed_units", "repairs every failed unit, the join included (retry_unit with unit_id repairs one); resume continues the same attempt without a note"},
	{"stuck → run_control resume", "re-enters the parked phase fresh once the blocker the phase named is cleared; refresh_def re-reads the definition when the fix was an edit to it"},
	{"provider-retries-exhausted → run_control resume", "continues the parked attempt on the session its turn died in once the transient provider failure clears"},
	{"provider-usage-limited → run_control resume", "tries the parked attempt immediately; use after usage resets or after switching accounts, and no recorded limit blocks the attempt"},
	{"loop-limit-exhausted → run_control resume with phase_id", "naming an earlier phase re-enters the cycle from outside and refills its bound"},
	{"retries-exhausted (legacy) → run_inspect", "resume preserves the old continuation behavior; resume with phase_id for a spent loop bound"},
	{"state failed → run_control rerun", "starts the failed run's last phase again"},
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
	block.WriteString("Agent Overflow workflows are available in this session through the " + MCPServerName + " tools.\n\n")

	if context.SessionReady {
		block.WriteString("This session already has them; they act in this project, with this thread's authority, for as long as the session lives.\n")
	} else {
		block.WriteString("This thread has no live session yet, so the tools are not attached. Send a message first; the session that starts carries them.\n")
	}
	block.WriteString("\n")

	block.WriteString("Tools:\n")
	writeComposerRows(&block, composerTools)
	block.WriteString("A wait never runs past " + fmt.Sprint(MaxWaitSeconds) + " seconds; a run this thread started reports its rest here as a message, so a wait that ends early costs nothing.\n")

	block.WriteString("When a run needs a human, the reason picks the fix:\n")
	writeComposerRows(&block, composerRepair)
	block.WriteString("resume continues and preserves finished work; resume with phase_id starts that phase over and re-runs everything in it, including runs it called.\n")
	block.WriteString("A run renders the definition it froze at start, so a prompt edited while it was parked is read only by an entry that carries refresh_def, which is accepted wherever the call enters a phase fresh: a bare resume on a park that re-enters fresh (stuck and the like), resume with phase_id on a continuable one, or rerun. A call phase already re-reads its target from disk every time it is made.\n")
	block.WriteString("run_status names a run's failed units, its parent, and what each phase attempt ran with; its per-attempt decision field is what tells a human: gate from a park: one. Any other reason names its own cause, and run_inspect is where you read it: worktree, branch, seeds, called runs, and every attempt's outputs in one call.\n")
	block.WriteString("Deciding a park needs the `resolve` grant in a workflow phase; an interactive session like this one holds it already.\n\n")

	fmt.Fprintf(&block, "Workflow definitions for %s live in:\n", displayName(context.ProjectName))
	fmt.Fprintf(&block, "  %s   (shared with every project)\n", displayPath(context.SharedDir))
	fmt.Fprintf(&block, "  %s   (this project only; shadows a shared id)\n", displayPath(context.ProjectDir))

	block.WriteString("\n")
	if len(context.Workflows) == 0 {
		block.WriteString("No workflows are configured here yet. workflow_new scaffolds one.\n")
	} else {
		block.WriteString("Configured workflows:\n")
		for _, workflow := range context.Workflows {
			fmt.Fprintf(&block, "  %s (%s): %s\n", workflow.ID, workflow.Scope, displayName(workflow.Name))
		}
		if context.WorkflowOverflow > 0 {
			fmt.Fprintf(&block, "  ... and %d more; workflow_list shows all of them.\n", context.WorkflowOverflow)
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
			fmt.Fprintf(&block, "  ... and %d more; run_list with active_only shows all of them.\n", context.RunOverflow)
		}
	}
	return block.String()
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
