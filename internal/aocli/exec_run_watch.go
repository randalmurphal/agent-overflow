package aocli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"
)

// `agent-overflow run watch` — the verb that replaces the sleep loop.
//
// `run wait` polls: it asks for a status, sleeps, and asks again, which is fine
// for a run that takes minutes and is what a supervising agent hand-rolled
// seven times over a multi-day campaign, sleeping four hours in aggregate and
// running 553 log-tail polls in one nine-hour stretch. One of those loops died
// without saying so and cost a night.
//
// This one blocks SERVER-side. Each call hands the app a cursor and is answered
// when the run tree moves, when the caller's remaining budget runs out, or
// immediately if the run has already stopped. The CLI never sleeps: between two
// calls it is printing. A call that comes back with nothing is a hold that
// expired, and the next one re-establishes from the same cursor, so nothing
// between them is lost.

// Watch-specific exit codes, on top of the binary-wide 0/1/2. Both name an
// outcome the caller has to tell apart from "the run rested", because a
// supervisor that cannot is a supervisor that waits forever on an answer that
// already came, or never came at all.
const (
	// exitWatchTimeout: --timeout expired and the run is still going.
	exitWatchTimeout = 3
	// exitWatchDisconnected: the app stopped answering. The watch ended without
	// a verdict, which is NOT the run failing and must never be read as one.
	exitWatchDisconnected = 4
)

// watchCall is the CLI's view of one long poll. Transitions stay raw: --json
// prints the app's own objects, exactly as every other verb forwards the app's
// own document.
type watchCall struct {
	Cursor      int64             `json:"cursor"`
	Gap         bool              `json:"gap"`
	Transitions []json.RawMessage `json:"transitions"`
	Run         json.RawMessage   `json:"run"`
}

// watchTransition mirrors only the fields the human line prints.
type watchTransition struct {
	At      int64  `json:"at"`
	ItemID  string `json:"itemId"`
	PhaseID string `json:"phaseId"`
	Attempt int    `json:"attempt"`
	From    string `json:"from"`
	To      string `json:"to"`
	Reason  string `json:"reason"`
	Cause   string `json:"cause"`
	Resting bool   `json:"resting"`
}

// watchRunState mirrors the watched run's current coordinate. Repair is the
// app's sentence, printed verbatim: it is the same one a wake carries, and a
// CLI that reworded it would be a second answer to "which verb settles this".
type watchRunState struct {
	ItemID     string `json:"itemId"`
	WorkflowID string `json:"workflowId"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
	PhaseID    string `json:"phaseId"`
	Resting    bool   `json:"resting"`
	Repair     string `json:"repair"`
}

var runWatchCommand = execCommand{
	name:  "agent-overflow run watch",
	usage: runWatchUsage,
	bind: func(flags *flag.FlagSet) func(*client, []string, io.Writer) (int, error) {
		tree := flags.Bool("tree", false, "report transitions of the runs this run called, too")
		timeout := flags.Duration("timeout", 0, "give up watching after this long (default: never)")
		jsonOutput := flags.Bool("json", false, "write one NDJSON object per transition")
		return func(c *client, args []string, stdout io.Writer) (int, error) {
			if err := requireArgs("agent-overflow run watch", args, 1, "exactly one run id"); err != nil {
				return exitError, err
			}
			if *timeout < 0 {
				return exitError, usageError("agent-overflow run watch", "--timeout cannot be negative")
			}
			return watchRun(c, args[0], *tree, *timeout, *jsonOutput, stdout)
		}
	},
}

// watchRun drives the long poll to a verdict. Every exit is a printed line: the
// one failure this verb exists to prevent is a monitor that stops without
// saying why.
func watchRun(c *client, itemID string, tree bool, timeout time.Duration, asJSON bool, stdout io.Writer) (int, error) {
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	cursor := int64(0)
	for {
		wait := int64(0)
		if !deadline.IsZero() {
			// Never zero: a budget that has already run out still buys one call, so
			// the timeout is reported with the run's actual coordinate rather than
			// as a process that exited saying nothing.
			if wait = time.Until(deadline).Milliseconds() + 1; wait < 1 {
				wait = 1
			}
		}
		call, err := watchPoll(c, itemID, cursor, tree, wait)
		if err != nil {
			if disconnect := disconnectedError(err); disconnect != nil {
				// The cause travels in the line rather than as a returned error: the
				// skeleton maps any error to the operational exit code, and this
				// outcome's whole point is an exit code of its own.
				if writeErr := writeOutput(stdout, watchDisconnectLine(asJSON, itemID, cursor, disconnect)); writeErr != nil {
					return exitError, writeErr
				}
				return exitWatchDisconnected, nil
			}
			return exitError, err
		}
		if call.Gap && cursor != 0 {
			if err := writeOutput(stdout, watchGapLine(asJSON, cursor, call.Cursor)); err != nil {
				return exitError, err
			}
		}
		for _, raw := range call.Transitions {
			if err := writeWatchTransition(stdout, asJSON, raw); err != nil {
				return exitError, err
			}
		}
		var run watchRunState
		if err := json.Unmarshal(call.Run, &run); err != nil {
			return exitError, fmt.Errorf("decode WorkflowAgentWatchRun result: %w", err)
		}
		if cursor == 0 && !asJSON {
			// The opening line names what is being watched, before anything has
			// happened: a caller that loses the terminal still knows which run this
			// process is holding.
			if err := writeOutput(stdout, "watching "+run.line()+"\n"); err != nil {
				return exitError, err
			}
		}
		cursor = call.Cursor
		if run.Resting {
			if err := writeWatchSummary(stdout, asJSON, call.Run, run); err != nil {
				return exitError, err
			}
			if run.State == stateDone {
				return exitOK, nil
			}
			return exitFindings, nil
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			if err := writeOutput(stdout, watchTimeoutLine(asJSON, run, timeout)); err != nil {
				return exitError, err
			}
			return exitWatchTimeout, nil
		}
	}
}

// watchPoll makes one call, retrying a TRANSPORT failure exactly once and
// immediately. A localhost hop is not always a straight line — the Windows↔WSL2
// relay tears connections down with a clean FIN — and a watch that reported the
// backend dead on the first such teardown would be the silent-death failure
// wearing a different hat. One retry separates a torn connection from an app
// that is gone; nothing sleeps, and a refusal the app expressed is never
// retried.
func watchPoll(c *client, itemID string, cursor int64, tree bool, waitMillis int64) (watchCall, error) {
	input := map[string]any{"itemId": itemID, "cursor": cursor, "tree": tree, "waitMillis": waitMillis}
	var call watchCall
	_, err := c.callInto(&call, "WorkflowAgentWatchRun", input)
	if err == nil {
		return call, nil
	}
	if disconnectedError(err) == nil {
		return watchCall{}, err
	}
	call = watchCall{}
	if _, retryErr := c.callInto(&call, "WorkflowAgentWatchRun", input); retryErr != nil {
		return watchCall{}, retryErr
	}
	return call, nil
}

// disconnectedError reports whether an error is the transport failing rather
// than the app answering. Anything the app expressed — an rpcError, or the
// route's own 401 for a credential whose session ended — arrived, which proves
// the app is alive and means the caller already has a message it can act on.
func disconnectedError(err error) error {
	var unreachable *transportError
	if errors.As(err, &unreachable) {
		return err
	}
	return nil
}

func watchDisconnectLine(asJSON bool, itemID string, cursor int64, cause error) string {
	if asJSON {
		return watchJSONLine(map[string]any{
			"watch": "disconnected", "itemId": itemID, "cursor": cursor, "error": cause.Error(),
		})
	}
	return fmt.Sprintf(
		"watch ended: run=%s the app stopped answering, twice in a row (%v), so this watch cannot say what the run did next; the run itself is unaffected — re-run `agent-overflow run watch %s` once the app is back, or `agent-overflow run status %s` for where it is now (cursor=%d)\n",
		itemID, cause, itemID, itemID, cursor)
}

func watchGapLine(asJSON bool, from, to int64) string {
	if asJSON {
		return watchJSONLine(map[string]any{"watch": "gap", "from": from, "to": to})
	}
	return fmt.Sprintf(
		"watch gap: transitions between %d and %d were not retained (the app restarted, or they aged out); the state below is current\n",
		from, to)
}

func watchTimeoutLine(asJSON bool, run watchRunState, timeout time.Duration) string {
	if asJSON {
		return watchJSONLine(map[string]any{
			"watch": "timeout", "itemId": run.ItemID, "state": run.State, "timeout": timeout.String(),
		})
	}
	return fmt.Sprintf("watch timed out after %s: %s — still running, watch again or check `agent-overflow run status %s`\n",
		timeout, run.line(), run.ItemID)
}

// watchJSONLine renders one of the watch's OWN objects — the events that are the
// CLI's to report rather than the app's to forward. It encodes rather than
// formats because one of the values is an arbitrary error string, and a
// hand-quoted line is a stream a consumer cannot parse.
func watchJSONLine(fields map[string]any) string {
	encoded, err := json.Marshal(fields)
	if err != nil {
		// json.Marshal of a map of strings and integers cannot fail; if it somehow
		// did, a parseable object saying so beats a line nobody can read.
		return fmt.Sprintf(`{"watch":"encode-failed","error":%q}`+"\n", err.Error())
	}
	return string(encoded) + "\n"
}

// writeWatchTransition prints one transition. --json forwards the app's own
// object verbatim, one per line: a stream cannot be one document, and inventing
// a CLI-side shape for it would be the second definition this package exists
// not to grow.
func writeWatchTransition(stdout io.Writer, asJSON bool, raw json.RawMessage) error {
	if asJSON {
		return writeOutput(stdout, string(raw)+"\n")
	}
	var transition watchTransition
	if err := json.Unmarshal(raw, &transition); err != nil {
		return fmt.Errorf("decode watch transition: %w", err)
	}
	return writeOutput(stdout, transition.line()+"\n")
}

// writeWatchSummary is the last line of a watch that ended because the run
// rested: the run's own state, and the sentence naming the verb that settles
// it. --json forwards the app's `run` object, which carries both.
func writeWatchSummary(stdout io.Writer, asJSON bool, raw json.RawMessage, run watchRunState) error {
	if asJSON {
		return writeOutput(stdout, string(raw)+"\n")
	}
	summary := run.line()
	if run.Repair != "" {
		summary += "\n" + run.Repair
	}
	return writeOutput(stdout, summary+"\n")
}

func (t watchTransition) line() string {
	phase := t.PhaseID
	if phase != "" && t.Attempt > 0 {
		phase = fmt.Sprintf("%s#%d", phase, t.Attempt)
	}
	from := t.From
	if from == "" {
		// The birth transition of a run that has just started. It has no previous
		// state, and printing an empty one would read as a state named "".
		from = "(new)"
	}
	return fields(
		watchTimestamp(t.At),
		"run="+t.ItemID,
		optionalField("phase", phase),
		from+"->"+t.To,
		optionalField("reason", t.Reason),
		optionalField("cause", causeField(t.Cause)),
	)
}

func (r watchRunState) line() string {
	return fields(
		"run="+r.ItemID,
		optionalField("workflow", r.WorkflowID),
		"state="+r.State,
		optionalField("reason", r.Reason),
		optionalField("phase", r.PhaseID),
	)
}

// watchTimestamp renders a transition's wall clock in UTC. A watch is a log,
// and a log whose lines are ordered by a timestamp nobody can compare across
// machines is a log with one fewer useful column.
func watchTimestamp(millis int64) string {
	if millis <= 0 {
		return ""
	}
	return time.UnixMilli(millis).UTC().Format(time.RFC3339)
}
