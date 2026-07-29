package aocli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// `agent-overflow run …` — start, observe, and control runs from inside an
// agent session.

// startInput is the request body of `agent-overflow run start`. Fields are
// omitted when
// unset so an unseeded start sends no seeds at all rather than a null the app
// would have to special-case.
type startInput struct {
	WorkflowID string          `json:"workflowId"`
	Scope      string          `json:"scope,omitempty"`
	Goal       string          `json:"goal,omitempty"`
	Seeds      json.RawMessage `json:"seeds,omitempty"`
	BaseBranch string          `json:"baseBranch,omitempty"`
	StepMode   bool            `json:"stepMode,omitempty"`
}

// runView mirrors only the fields the human lines print. The authoritative shape
// is the app's; `--json` forwards that one verbatim.
type runView struct {
	ItemID              string `json:"itemId"`
	WorkflowID          string `json:"workflowId"`
	WorkflowScope       string `json:"workflowScope"`
	Goal                string `json:"goal"`
	State               string `json:"state"`
	Reason              string `json:"reason"`
	CurrentPhaseID      string `json:"currentPhaseId"`
	CurrentPhaseOrdinal int    `json:"currentPhaseOrdinal"`
	PhaseCount          int    `json:"phaseCount"`
	Resting             bool   `json:"resting"`
	Skipped             bool   `json:"skipped"`
	BoundThreadID       string `json:"boundThreadId"`
	BindingWarning      string `json:"bindingWarning"`
}

func (v runView) line() string {
	phase := v.CurrentPhaseID
	if phase != "" && v.PhaseCount > 0 {
		phase = fmt.Sprintf("%s(%d/%d)", phase, v.CurrentPhaseOrdinal, v.PhaseCount)
	}
	return fields(
		"run="+v.ItemID,
		optionalField("workflow", v.WorkflowID),
		"state="+v.State,
		optionalField("reason", v.Reason),
		optionalField("phase", phase),
		skippedField(v.Skipped),
	)
}

func skippedField(skipped bool) string {
	if !skipped {
		return ""
	}
	return "skipped=true"
}

func runCommand(args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_ = writeOutput(stderr, runUsage)
		return exitError
	}
	switch args[0] {
	case "help", "--help", "-h":
		if err := writeOutput(stdout, runUsage); err != nil {
			return operationalError(stderr, err)
		}
		return exitOK
	case "start":
		return runStartCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "status":
		return runStatusCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "wait":
		return runWaitCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "output":
		return runOutputCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "list":
		return runListCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "pause":
		return runPauseCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "resume":
		return runResumeCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "cancel":
		return runCancelCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "rerun":
		return runRerunCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "retry-unit":
		return runRetryUnitCommand.run(args[1:], lookupEnv, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "agent-overflow run: unknown command %q\n", args[0])
		_ = writeOutput(stderr, runUsage)
		return exitError
	}
}

var runStartCommand = execCommand{
	name:  "agent-overflow run start",
	usage: runStartUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		scope := flags.String("scope", "", "resolve the workflow in this scope (shared|project)")
		goal := flags.String("goal", "", "one-line goal recorded on the run")
		baseBranch := flags.String("base-branch", "", "branch the run's worktree starts from")
		stepMode := flags.Bool("step", false, "pause at every gate decision")
		wait := flags.Bool("wait", false, "block until the run stops doing work")
		timeout := flags.Duration("timeout", defaultWaitTimeout, "give up waiting after this long")
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		seeds := &seedFlag{}
		flags.Var(seeds, "seed", "seed one declared input as key=value (repeatable; JSON values are parsed)")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("agent-overflow run start", args, 1, "exactly one workflow id"); err != nil {
				return exitError, err
			}
			encodedSeeds, err := seeds.encode()
			if err != nil {
				return exitError, err
			}
			var started runView
			raw, err := c.callInto(&started, "WorkflowAgentStartRun", startInput{
				WorkflowID: args[0], Scope: *scope, Goal: *goal, Seeds: encodedSeeds,
				BaseBranch: *baseBranch, StepMode: *stepMode,
			})
			if err != nil {
				return exitError, err
			}
			if !*wait {
				return exitOK, render(stdout, *jsonOutput, raw, started.line())
			}
			// The start line is printed before the wait so a caller that loses
			// patience (or the run) still knows the run id. Never on the --json
			// path: that promises exactly one document.
			if !*jsonOutput {
				if err := writeOutput(stdout, started.line()+"\n"); err != nil {
					return exitError, err
				}
			}
			return waitForRun(c, started.ItemID, *timeout, *jsonOutput, stdout)
		}
	},
}

var runStatusCommand = execCommand{
	name:  "agent-overflow run status",
	usage: runStatusUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("agent-overflow run status", args, 1, "exactly one run id"); err != nil {
				return exitError, err
			}
			var view runView
			raw, err := c.callInto(&view, "WorkflowAgentRunStatus", args[0])
			if err != nil {
				return exitError, err
			}
			return exitOK, render(stdout, *jsonOutput, raw, view.line())
		}
	},
}

var runWaitCommand = execCommand{
	name:  "agent-overflow run wait",
	usage: runWaitUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		timeout := flags.Duration("timeout", defaultWaitTimeout, "give up waiting after this long")
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("agent-overflow run wait", args, 1, "exactly one run id"); err != nil {
				return exitError, err
			}
			return waitForRun(c, args[0], *timeout, *jsonOutput, stdout)
		}
	},
}

var runOutputCommand = execCommand{
	name:  "agent-overflow run output",
	usage: runOutputUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("agent-overflow run output", args, 1, "exactly one run id"); err != nil {
				return exitError, err
			}
			var outputs struct {
				ItemID    string                     `json:"itemId"`
				State     string                     `json:"state"`
				Reason    string                     `json:"reason"`
				Outputs   map[string]json.RawMessage `json:"outputs"`
				Artifacts []string                   `json:"artifacts"`
			}
			raw, err := c.callInto(&outputs, "WorkflowAgentRunOutput", args[0])
			if err != nil {
				return exitError, err
			}
			var human strings.Builder
			fmt.Fprintln(&human, fields(
				"run="+outputs.ItemID, "state="+outputs.State, optionalField("reason", outputs.Reason)))
			names := make([]string, 0, len(outputs.Outputs))
			for name := range outputs.Outputs {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				fmt.Fprintf(&human, "output %s=%s\n", name, string(outputs.Outputs[name]))
			}
			for _, artifact := range outputs.Artifacts {
				fmt.Fprintf(&human, "artifact %s\n", artifact)
			}
			return exitOK, render(stdout, *jsonOutput, raw, human.String())
		}
	},
}

var runListCommand = execCommand{
	name:  "agent-overflow run list",
	usage: runListUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		active := flags.Bool("active", false, "list only runs that are running or need a human")
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if len(args) != 0 {
				return exitError, usageError("agent-overflow run list", "unexpected positional arguments")
			}
			var views []runView
			raw, err := c.callInto(&views, "WorkflowAgentListRuns", *active)
			if err != nil {
				return exitError, err
			}
			var human strings.Builder
			for _, view := range views {
				fmt.Fprintln(&human, view.line())
			}
			return exitOK, render(stdout, *jsonOutput, raw, human.String())
		}
	},
}

// runControl builds the pause/cancel/rerun family: one run id, an optional
// extra flag, no result body. They report by re-reading status, because "what
// state is it in now" is the only useful answer to "I stopped it".
func runControl(name, usage, method string, extra func(*flag.FlagSet) func() []any) execCommand {
	return execCommand{
		name:  name,
		usage: usage,
		bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
			jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
			var extraArgs func() []any
			if extra != nil {
				extraArgs = extra(flags)
			}
			return func(c *client, args []string, stdout io.Writer) (int, error) {
				if err := requireArgs(name, args, 1, "exactly one run id"); err != nil {
					return exitError, err
				}
				params := []any{args[0]}
				if extraArgs != nil {
					params = append(params, extraArgs()...)
				}
				if _, err := c.call(method, params...); err != nil {
					return exitError, err
				}
				var view runView
				raw, err := c.callInto(&view, "WorkflowAgentRunStatus", args[0])
				if err != nil {
					return exitError, err
				}
				return exitOK, render(stdout, *jsonOutput, raw, view.line())
			}
		},
	}
}

var runPauseCommand = runControl("agent-overflow run pause", runPauseUsage, "WorkflowPauseItem", nil)

var runCancelCommand = runControl("agent-overflow run cancel", runCancelUsage, "WorkflowCancelItem", nil)

var runResumeCommand = runControl("agent-overflow run resume", runResumeUsage, "WorkflowResumeItem",
	func(flags *flag.FlagSet) func() []any {
		phase := flags.String("phase", "", "re-enter this phase instead of continuing where the run parked")
		return func() []any { return []any{*phase} }
	})

var runRerunCommand = runControl("agent-overflow run rerun", runRerunUsage, "WorkflowRerunItem",
	func(flags *flag.FlagSet) func() []any {
		guidance := flags.String("guidance", "", "text carried into the new attempt alongside the failure diagnosis")
		return func() []any { return []any{*guidance} }
	})

var runRetryUnitCommand = execCommand{
	name:  "agent-overflow run retry-unit",
	usage: runRetryUnitUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		note := flags.String("note", "", "text carried into the unit's next try")
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("agent-overflow run retry-unit", args, 2, "a run id and a unit id"); err != nil {
				return exitError, err
			}
			if _, err := c.call("WorkflowRetryUnit", args[0], args[1], *note); err != nil {
				return exitError, err
			}
			var view runView
			raw, err := c.callInto(&view, "WorkflowAgentRunStatus", args[0])
			if err != nil {
				return exitError, err
			}
			return exitOK, render(stdout, *jsonOutput, raw, fields(view.line(), "unit="+args[1]))
		}
	},
}

// Waiting is polling, not subscribing. The event push lives on the WebSocket,
// and a CLI process that exits after one answer has no business owning a
// long-lived socket with a replay ring behind it; the backoff below costs a
// handful of one-row SQLite reads over loopback for a run that takes minutes.
const (
	defaultWaitTimeout  = 30 * time.Minute
	initialWaitInterval = 500 * time.Millisecond
	maxWaitInterval     = 5 * time.Second
	waitBackoffFactor   = 3
	waitBackoffDivisor  = 2
)

// waitForRun blocks until the run stops doing work. A run resting anywhere other
// than `done` exits 1: the command succeeded, the run did not, and a caller
// scripting `agent-overflow run wait && …` needs those to differ.
func waitForRun(c *client, itemID string, timeout time.Duration, asJSON bool, stdout io.Writer) (int, error) {
	if timeout <= 0 {
		return exitError, usageError("agent-overflow run wait", "--timeout must be positive")
	}
	deadline := time.Now().Add(timeout)
	interval := initialWaitInterval
	for {
		var view runView
		raw, err := c.callInto(&view, "WorkflowAgentRunStatus", itemID)
		if err != nil {
			return exitError, err
		}
		if view.Resting {
			if err := render(stdout, asJSON, raw, view.line()); err != nil {
				return exitError, err
			}
			if view.State == stateDone {
				return exitOK, nil
			}
			return exitFindings, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return exitError, fmt.Errorf(
				"agent-overflow run wait: run %s was still %s after %s; it is still going, so wait again or check `agent-overflow run status`",
				itemID, view.State, timeout)
		}
		if interval > remaining {
			interval = remaining
		}
		time.Sleep(interval)
		if interval < maxWaitInterval {
			interval = interval * waitBackoffFactor / waitBackoffDivisor
			if interval > maxWaitInterval {
				interval = maxWaitInterval
			}
		}
	}
}

// stateDone is the one run state that makes a wait a success. Mirrored rather
// than imported: the CLI reads it off the wire, and pulling the engine package
// into the CLI for one string would drag the whole FSM with it.
const stateDone = "done"
