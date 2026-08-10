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
	// ProjectSlug is what `--project` takes. It is in the block because the
	// offline commands cannot infer it: a session carries it as AO_PROJECT, but a
	// reader who lost the environment (or is reading the block to write a command
	// for somewhere else) has no other place to find it.
	ProjectSlug string `json:"projectSlug"`
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
	{"agent-overflow run watch <run-id> [--tree] [--timeout <dur>]", "block; one line per state change until it rests — the verb that replaces a sleep loop"},
	{"agent-overflow run inspect <run-id> [--phase <id> [--attempt <n>]]", "read one whole: worktree, branch, seeds, children, outputs"},
	{"agent-overflow run narrative <run-id> --phase <id> [--unit <id>]", "read what an attempt says it did"},
	{"agent-overflow run list [--active]", "see this project's runs"},
	{"agent-overflow run pause|resume|cancel|rerun <run-id>", "control one"},
	{"agent-overflow run retry-unit <run-id> <unit-id>|retry-failed-units <run-id>", "repair a fan-out"},
	{"agent-overflow run resolve <run-id> --approve|--reject [--note <text>]", "decide a parked gate"},
	{"agent-overflow run answer <run-id> <text>", "answer a parked question"},
	{"agent-overflow run soft-stop <run-id> [--clear]", "stop after the current wave"},
	{"agent-overflow run amend <run-id> --seed k=v", "fix a parked run's seed; the result says when the run reads it"},
	{"agent-overflow run guide <run-id> \"<text>\"", "steer a working run; delivered at its next phase entry, never mid-turn"},
	{"agent-overflow memory add --kind <kind> \"<text>\" | memory list", "the campaign's shared lessons, injected into every element's prompt"},
	{"agent-overflow workflow list|validate <path>|schema", "author one"},
	{"agent-overflow <command> --help", "full options"},
}

// composerRepair is the reason→verb map. A cold agent reads a park reason and
// still has to work out which verb acts on it, so every reason a verb settles
// names that verb here and says what taking it does — the difference between
// `run resume` and `run resume --phase` is a decision, not a detail. A reason
// absent from the map is one whose own cause is the instruction, which is what
// the line under the table says, because an unexplained omission is what makes
// an agent invent a verb.
var composerRepair = []composerRow{
	{"paused|interrupted|checkpoint → run resume", "continues where the run parked"},
	{"gate (decision=human) → run resolve --approve|--reject [--note <text>]", "takes one of the two routes the gate declared"},
	{"gate (decision=park) → run resume", "a park: route declares no approve/reject; resume re-enters the phase once its cause is addressed"},
	{"question → run answer <run-id> <text>", "the answer reaches the phase that asked"},
	{"unit-failed → run retry-failed-units", "repairs every failed unit, the join included (retry-unit <run-id> <unit-id> repairs one); run resume continues the same attempt without a note"},
	{"stuck → run resume", "re-enters the parked phase fresh once the blocker the phase named is cleared; --refresh-def re-reads the definition when the fix was an edit to it"},
	{"retries-exhausted → run resume", "continues the parked attempt on the session its turn died in; --phase <id> goes back further and is the only thing that refills loop budgets; a park whose cause names a usage limit resumes itself at the stated time"},
	{"state failed → run rerun", "starts the failed run's last phase again"},
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
	block.WriteString("`run resume` continues and preserves finished work; `run resume --phase <id>` starts that phase over and re-runs everything in it, including runs it called.\n")
	block.WriteString("A run renders the definition it froze at start, so a prompt edited while it was parked is read only by a resume that carries --refresh-def — accepted wherever the verb enters a phase FRESH: bare `run resume --refresh-def` on a park that re-enters fresh (stuck and the like), `run resume --phase <id> --refresh-def` on a continuable one, or `run rerun --refresh-def` — a call already re-reads its target from disk every time it is made.\n")
	block.WriteString("`run status` names a run's failed units, its parent, and what each phase attempt ran with — its per-attempt decision= field is what tells a human: gate from a park: one; any other reason names its own cause, and `run inspect <run-id>` is where you read it: worktree, branch, seeds, called runs, and every attempt's outputs in one call.\n")
	block.WriteString("Deciding a park needs the `resolve` grant in a workflow phase; an interactive session like this one holds it already.\n\n")

	fmt.Fprintf(&block, "Workflow definitions for %s live in:\n", displayName(context.ProjectName))
	fmt.Fprintf(&block, "  %s   (shared with every project)\n", displayPath(context.SharedDir))
	fmt.Fprintf(&block, "  %s   (this project only — %s; shadows a shared id)\n",
		displayPath(context.ProjectDir), projectScopeFlag(context.ProjectSlug))

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

// projectScopeFlag renders the flag that reaches the project scope, with this
// project's slug filled in when there is one. Without a slug the placeholder is
// still worth printing: it says the flag exists and takes a value.
func projectScopeFlag(slug string) string {
	if strings.TrimSpace(slug) == "" {
		return "--project <slug>"
	}
	return "--project " + slug
}

func displayPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "(not created yet)"
	}
	return path
}
