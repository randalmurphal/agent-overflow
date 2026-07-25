package aocli

import (
	"fmt"
	"sort"
	"strings"
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
	// SharedDir and ProjectDir are the two directories `ao workflow` resolves
	// definitions from. ProjectDir is empty when the project has no workflow
	// directory yet, which is worth saying: it is where a new one goes.
	SharedDir  string `json:"sharedDir"`
	ProjectDir string `json:"projectDir"`
	// SessionReady reports whether this thread's session actually carries the
	// AO_* credentials. It is false for a thread with no live session; saying so
	// beats an agent discovering it by running a command that cannot work.
	SessionReady bool               `json:"sessionReady"`
	Workflows    []ComposerWorkflow `json:"workflows"`
	// WorkflowOverflow and RunOverflow are how many entries were left out.
	WorkflowOverflow int           `json:"workflowOverflow"`
	Runs             []ComposerRun `json:"runs"`
	RunOverflow      int           `json:"runOverflow"`
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
	block.WriteString("Agent Overflow workflows are available in this session through the `ao` command.\n\n")

	if context.SessionReady {
		fmt.Fprintf(&block, "Your environment already carries %s, %s, and %s, so `ao` needs no configuration.\n",
			EnvEndpoint, EnvToken, EnvThreadID)
	} else {
		fmt.Fprintf(&block,
			"This thread has no live session yet, so %s and %s are not set. Send a message first; `ao` works from the session that starts.\n",
			EnvEndpoint, EnvToken)
	}
	block.WriteString("Your credential is scoped to this session and this project, and it is revoked when the session ends.\n\n")

	block.WriteString("Commands:\n")
	block.WriteString("  ao run start <workflow-id> [--goal <text>] [--seed k=v] [--wait]   start a run\n")
	block.WriteString("  ao run status|output|wait <run-id>                                 check on one\n")
	block.WriteString("  ao run list [--active]                                             see this project's runs\n")
	block.WriteString("  ao run pause|resume|cancel|rerun <run-id>                          control one\n")
	block.WriteString("  ao workflow list | ao workflow validate <path> | ao workflow schema  author one\n")
	block.WriteString("  ao <command> --help                                                full options\n")
	block.WriteString("Every command takes --json. `ao run wait` exits 1 if the run ends anywhere but done.\n\n")

	fmt.Fprintf(&block, "Workflow definitions for %s live in:\n", displayName(context.ProjectName))
	fmt.Fprintf(&block, "  %s   (shared with every project)\n", displayPath(context.SharedDir))
	fmt.Fprintf(&block, "  %s   (this project only, and shadows a shared id)\n", displayPath(context.ProjectDir))

	block.WriteString("\n")
	if len(context.Workflows) == 0 {
		block.WriteString("No workflows are configured here yet. `ao workflow new --help` scaffolds one.\n")
	} else {
		block.WriteString("Configured workflows:\n")
		for _, workflow := range context.Workflows {
			fmt.Fprintf(&block, "  %s (%s) — %s\n", workflow.ID, workflow.Scope, displayName(workflow.Name))
		}
		if context.WorkflowOverflow > 0 {
			fmt.Fprintf(&block, "  ... and %d more; `ao workflow list` shows all of them.\n", context.WorkflowOverflow)
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
			fmt.Fprintf(&block, "  ... and %d more; `ao run list --active` shows all of them.\n", context.RunOverflow)
		}
	}
	return block.String()
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
