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
	case "retry-failed-units":
		return runRetryFailedUnitsCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "soft-stop":
		return runSoftStopCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "resolve":
		return runResolveCommand.run(args[1:], lookupEnv, stdout, stderr)
	case "answer":
		return runAnswerCommand.run(args[1:], lookupEnv, stdout, stderr)
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
			return exitOK, render(stdout, *jsonOutput, raw, view.statusBlock())
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
			// An empty human list would print one blank line, which reads as a
			// command that did not work rather than as an answer. --json keeps
			// the app's own document: `[]` is already unambiguous there.
			if len(views) == 0 {
				empty := "No runs in this project."
				if *active {
					empty = "No active runs in this project."
				}
				return exitOK, render(stdout, *jsonOutput, raw, empty)
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
				return reportRunState(c, args[0], *jsonOutput, stdout)
			}
		},
	}
}

// reportRunState re-reads a run after a command changed it, because "what state
// is it in now" is the only useful answer to "I acted on it". Every acting verb
// shares it so none of them can drift into reporting something else. The status
// read also carries per-attempt provenance; this deliberately prints only the
// run line, which is what `run status` is for.
func reportRunState(c *client, itemID string, asJSON bool, stdout io.Writer, extra ...string) (int, error) {
	var view runView
	raw, err := c.callInto(&view, "WorkflowAgentRunStatus", itemID)
	if err != nil {
		return exitError, err
	}
	return exitOK, render(stdout, asJSON, raw, fields(append([]string{view.line()}, extra...)...))
}

var runPauseCommand = runControl("agent-overflow run pause", runPauseUsage, "WorkflowPauseItem", nil)

var runCancelCommand = runControl("agent-overflow run cancel", runCancelUsage, "WorkflowCancelItem", nil)

// The definition a run froze at start is what it keeps running, so --refresh-def
// is how an operator who edited a parked phase's prompt gets the edit rendered.
// The app refuses it where the entry is a continuation rather than a fresh one.
const refreshDefUsage = "re-read the workflow definition and its prompt files from disk for this entry"

var runResumeCommand = runControl("agent-overflow run resume", runResumeUsage, "WorkflowResumeItem",
	func(flags *flag.FlagSet) func() []any {
		phase := flags.String("phase", "", "re-enter this phase instead of continuing where the run parked")
		refreshDef := flags.Bool("refresh-def", false, refreshDefUsage)
		return func() []any { return []any{*phase, *refreshDef} }
	})

var runRerunCommand = runControl("agent-overflow run rerun", runRerunUsage, "WorkflowRerunItem",
	func(flags *flag.FlagSet) func() []any {
		guidance := flags.String("guidance", "", "text carried into the new attempt alongside the failure diagnosis")
		refreshDef := flags.Bool("refresh-def", false, refreshDefUsage)
		return func() []any { return []any{*guidance, *refreshDef} }
	})

// retry-failed-units is its own verb rather than a --all on retry-unit: the two
// differ in arity, not in options. `retry-unit` needs a unit id and this one
// refuses to be given one, so a flag would have made the second positional
// conditionally required — the shape where a typo silently repairs the wrong
// thing. It joins the pause/resume/cancel/rerun family for the same reason they
// share it: one run id, an optional note, and "what state is it in now" as the
// only useful answer.
var runRetryFailedUnitsCommand = runControl(
	"agent-overflow run retry-failed-units", runRetryFailedUnitsUsage, "WorkflowRetryFailedUnits",
	func(flags *flag.FlagSet) func() []any {
		note := flags.String("note", "", "text carried into every repaired unit's next try")
		return func() []any { return []any{*note} }
	})

// soft-stop is the deferred half of pause: it interrupts nothing and starts
// nothing, it just decides whether the run takes its next call. `--clear`
// withdraws the request rather than being a second verb, because arming and
// withdrawing are one piece of state and a caller that could only arm would have
// no way to change its mind.
var runSoftStopCommand = runControl(
	"agent-overflow run soft-stop", runSoftStopUsage, "WorkflowRequestSoftStop",
	func(flags *flag.FlagSet) func() []any {
		clear := flags.Bool("clear", false, "withdraw a pending stop instead of asking for one")
		return func() []any { return []any{!*clear} }
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
			return reportRunState(c, args[0], *jsonOutput, stdout, "unit="+args[1])
		}
	},
}

// The gate decisions the engine accepts. Mirrored rather than imported, for the
// same reason stateDone is: the CLI puts them on the wire, and pulling the
// engine package in for two strings would drag the whole FSM with it.
const (
	decisionApprove = "approve"
	decisionReject  = "reject"
)

// resolve settles a gate a workflow author routed to a human. It stays out of
// the runControl family because its two directions are a DECISION rather than a
// state: `--approve` and `--reject` take the routes the gate itself declared, so
// neither can be the default and a caller who supplies both has not decided
// anything. Making one of them a flag on the other would have hidden that.
var runResolveCommand = execCommand{
	name:  "agent-overflow run resolve",
	usage: runResolveUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		approve := flags.Bool("approve", false, "take the gate's approve route")
		reject := flags.Bool("reject", false, "take the gate's reject route")
		note := flags.String("note", "", "text recorded with the decision and carried into a reject loop")
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("agent-overflow run resolve", args, 1, "exactly one run id"); err != nil {
				return exitError, err
			}
			if *approve == *reject {
				return exitError, usageError("agent-overflow run resolve",
					"expected exactly one of --approve or --reject")
			}
			decision := decisionApprove
			if *reject {
				decision = decisionReject
			}
			if _, err := c.call("WorkflowResolveGate", args[0], decision, *note); err != nil {
				return exitError, err
			}
			return reportRunState(c, args[0], *jsonOutput, stdout)
		}
	},
}

// answer settles the other human-routed park: the run asked a question and is
// waiting on the text. The answer is a positional rather than a flag because it
// is the point of the command, and requireArgs refuses a blank one — an empty
// answer is a question still unanswered.
var runAnswerCommand = execCommand{
	name:  "agent-overflow run answer",
	usage: runAnswerUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		jsonOutput := flags.Bool("json", false, "write the app's result as JSON")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("agent-overflow run answer", args, 2, "a run id and the answer text"); err != nil {
				return exitError, err
			}
			if _, err := c.call("WorkflowAnswerQuestion", args[0], args[1]); err != nil {
				return exitError, err
			}
			return reportRunState(c, args[0], *jsonOutput, stdout)
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
