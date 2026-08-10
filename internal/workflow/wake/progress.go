package wake

import (
	"fmt"
	"strings"

	"agent-overflow/internal/untrustedtext"
)

// The progress wake (K1): a gate took a route its author decorated with
// `notify:`, and the run CONTINUED. It is the message a campaign needed to say
// "wave 12 finished, starting 13" without parking to say it — the alternative,
// measured on a forty-wave run, was a human polling the database in a loop.
//
// It shares this package with the resting wake rather than living beside it,
// because the two answer the same reader with the same rules: the same data
// notice, the same quoting, the same budgets, the same bounded lists. What it
// deliberately does NOT share is the closing: a resting wake's last line names
// the verb that unblocks the run, and a progress wake's has to say the opposite
// — nothing is blocked, nothing is owed.

// ProgressRun is the ROOT run a progress wake is delivered for. Only a root
// binds a thread (§5), so this is always the run the reader started, even when
// the gate that fired belongs to a descendant.
type ProgressRun struct {
	ItemID       string
	Goal         string
	WorkflowID   string
	WorktreePath string
	Branch       string
}

// ProgressGate is the gate that fired, and the run it belongs to.
type ProgressGate struct {
	// ItemID and WorkflowID name the run whose gate this was. They equal the
	// root's when the root's own gate fired; a descendant's gate carries the
	// descendant's, which is what makes the message actionable against the run
	// that actually moved.
	ItemID     string
	WorkflowID string
	// Depth is how far below the root the gate sits, 0 for the root's own, and
	// Chain is the run ids root→gate, rendered like a descendant park's.
	Depth int
	Chain []string
	// PhaseID and Attempt are the attempt the gate consumed — the coordinate
	// the read verbs take, and half of what tells one wave's notify from the
	// next one's.
	PhaseID string
	Attempt int
	// Decision and Target are where the gate sent the run: `advance` to the
	// next phase, `loop` back to an earlier one. No other decision reaches a
	// progress wake, because no other decision leaves the run running.
	Decision string
	Target   string
}

// ProgressInput is everything the progress composer reads. Like Input it is
// flat and pre-resolved.
type ProgressInput struct {
	Run  ProgressRun
	Gate ProgressGate
	// Outputs is the bounded digest of the envelope the gate decided on, and
	// OutputOverflow how many the app left out. This is the whole point of the
	// message being worth sending: "wave 12 finished" with its verdict attached
	// is progress a reader can act on, without it it is a heartbeat.
	Outputs        []Output
	OutputOverflow int
}

// ComposeProgress renders the message a `notify:` route injects into the bound
// thread. It is pure, like Compose.
func ComposeProgress(in ProgressInput) string {
	var out strings.Builder
	out.WriteString(dataNotice)
	out.WriteString("\n\n")
	fmt.Fprintf(&out, "Run %s (workflow %s) is running — goal %s.",
		untrustedtext.Field(in.Run.ItemID), untrustedtext.Field(in.Run.WorkflowID),
		untrustedtext.Quote(in.Run.Goal, MaxGoalRunes))
	out.WriteByte('\n')
	out.WriteString(progressLine(in.Gate))
	if line := chainLine(in.Gate.Chain); line != "" {
		out.WriteByte('\n')
		out.WriteString(line)
	}
	writeWorkspace(&out, "Workspace:", in.Run.WorktreePath, in.Run.Branch)
	writeProgressOutputs(&out, in)
	out.WriteString("\n\n")
	out.WriteString(progressClosing(in.Gate))
	return out.String()
}

// progressLine says what finished and where the run went next. The route is
// named because "wave finished" and "wave finished and we are doing it again"
// are different news, and the loop-back form is the one a reader most needs to
// see repeated.
func progressLine(gate ProgressGate) string {
	subject := "This run"
	if gate.Depth > 0 {
		subject = fmt.Sprintf("A called run %s down (run %s, workflow %s)",
			ordinalDepth(gate.Depth), untrustedtext.Field(gate.ItemID),
			untrustedtext.Field(gate.WorkflowID))
	}
	line := fmt.Sprintf("%s finished phase %s and continued", subject,
		phaseCoordinate(gate.PhaseID, gate.Attempt))
	switch {
	case gate.Target != "":
		return fmt.Sprintf("%s: the gate chose %s to phase %s.",
			line, untrustedtext.Field(gate.Decision), untrustedtext.Field(gate.Target))
	case gate.Decision != "":
		return fmt.Sprintf("%s: the gate chose %s.", line, untrustedtext.Field(gate.Decision))
	default:
		return line + "."
	}
}

func writeProgressOutputs(out *strings.Builder, in ProgressInput) {
	if len(in.Outputs) == 0 {
		return
	}
	fmt.Fprintf(out, "\n\nWhat it produced%s:", attemptCoordinate(in.Gate.PhaseID, in.Gate.Attempt))
	for _, output := range in.Outputs {
		writeOutputLine(out, output)
	}
	if in.OutputOverflow > 0 {
		fmt.Fprintf(out, "\n- …and %d more (%s).", in.OutputOverflow,
			inspectCommand(in.Gate.ItemID, in.Gate.PhaseID, in.Gate.Attempt))
	}
}

// progressClosing is the line that keeps this message from being read as a
// park. A woken agent's default assumption is that it was woken because
// something needs it; saying otherwise outright is cheaper than letting it
// discover that by running `run status`.
func progressClosing(gate ProgressGate) string {
	closing := "Nothing is waiting on a reply — the run is still going. This message exists because the workflow asked to report at this gate."
	if gate.Depth > 0 {
		return closing + " Act only if the outputs above say something should change."
	}
	return closing
}
