package wake

import (
	"fmt"
	"strings"

	"agent-overflow/internal/untrustedtext"
)

// Compose renders the wake message injected into a bound thread when a run
// RESTS. The result is plain text with no envelope payloads: an agent that needs
// the full record reads it through the references.
func Compose(in Input) string {
	var out strings.Builder
	out.WriteString(dataNotice)
	out.WriteString("\n\n")
	out.WriteString(headline(in.Run, in.Descendant != nil))
	if in.Descendant != nil {
		out.WriteByte('\n')
		out.WriteString(descendantLine(*in.Descendant))
		writeCause(&out, in.Descendant.Cause)
		if line := chainLine(in.Descendant.Chain); line != "" {
			out.WriteByte('\n')
			out.WriteString(line)
		}
		writeWorkspace(&out, "The called run works in", in.Descendant.WorktreePath, in.Descendant.Branch)
	} else {
		if line := detailLine(in.Run); line != "" {
			out.WriteByte('\n')
			out.WriteString(line)
		}
		writeCause(&out, in.Run.Cause)
	}
	writeWorkspace(&out, "Workspace:", in.Run.WorktreePath, in.Run.Branch)
	writeOutputs(&out, in.Outputs)
	writeAttemptOutputs(&out, in)
	writeReferences(&out, in.References)
	out.WriteString("\n\n")
	out.WriteString(closing(in))
	return out.String()
}

// headline is the one line that always exists: what the run is and where it
// came to rest. A descendant park leaves the root running, which the headline
// reports as `waiting` rather than as a resting state it never reached.
func headline(run Run, waitingOnDescendant bool) string {
	state := stateText(run.State, run.Reason)
	if waitingOnDescendant {
		// The root did not rest — it is still `running`, blocked on the call it
		// is waiting for. Reporting the raw state would read as "nothing has
		// happened", which is the opposite of why this message was sent.
		state = "waiting"
	}
	return fmt.Sprintf("Run %s (workflow %s) is %s — goal %s.",
		untrustedtext.Field(run.ItemID), untrustedtext.Field(run.WorkflowID),
		state, untrustedtext.Quote(run.Goal, MaxGoalRunes))
}

func stateText(state, reason string) string {
	if reason == "" {
		return state
	}
	return state + " (" + reason + ")"
}

// detailLine renders the phase and the free text the resting envelope carried.
// A run with neither contributes no line rather than an empty one.
func detailLine(run Run) string {
	phase := phaseCoordinate(run.PhaseID, run.Attempt)
	detail := strings.TrimSpace(run.Detail)
	switch {
	case phase != "" && detail != "":
		return fmt.Sprintf("Phase %s: %s", phase, untrustedtext.Quote(detail, MaxDetailRunes))
	case phase != "":
		return "Phase " + phase + "."
	case detail != "":
		return untrustedtext.Quote(detail, MaxDetailRunes)
	default:
		return ""
	}
}

// phaseCoordinate names a phase and, when it is known, which attempt of it. The
// attempt is what tells a second wake about a retried phase from a repeat of
// the first one, and it is the coordinate the drill-down verbs take. An unknown
// attempt (0) renders the phase alone rather than an "attempt 0" no read verb
// would accept.
func phaseCoordinate(phaseID string, attempt int) string {
	phaseID = strings.TrimSpace(phaseID)
	if phaseID == "" {
		return ""
	}
	rendered := untrustedtext.Field(phaseID)
	if attempt > 0 {
		rendered += fmt.Sprintf(" (attempt %d)", attempt)
	}
	return rendered
}

// writeCause renders the engine's diagnosis as its own line, distinct from the
// detail above it: the detail is what the PHASE said, and conflating the two
// would let engine prose read as something a model reported. A park with no
// engine-diagnosed cause contributes nothing — the absence is the answer, and
// an empty label would read as a diagnosis that was lost.
func writeCause(out *strings.Builder, cause string) {
	cause = strings.TrimSpace(cause)
	if cause == "" {
		return
	}
	out.WriteString("\nThe engine stopped it here: ")
	out.WriteString(untrustedtext.Quote(cause, MaxCauseRunes))
}

// writeWorkspace names where the work lives. A read-only run has no worktree
// and contributes no line; a branch with no worktree still does, because a run
// whose workspace is the project root is still working on a branch somebody has
// to look at.
func writeWorkspace(out *strings.Builder, label, worktreePath, branch string) {
	worktreePath = strings.TrimSpace(worktreePath)
	branch = strings.TrimSpace(branch)
	switch {
	case worktreePath != "" && branch != "":
		fmt.Fprintf(out, "\n%s %s on branch %s.", label,
			untrustedtext.Quote(worktreePath, MaxPathRunes), untrustedtext.Quote(branch, MaxPathRunes))
	case worktreePath != "":
		fmt.Fprintf(out, "\n%s %s.", label, untrustedtext.Quote(worktreePath, MaxPathRunes))
	case branch != "":
		fmt.Fprintf(out, "\n%s branch %s.", label, untrustedtext.Quote(branch, MaxPathRunes))
	}
}

func descendantLine(child Descendant) string {
	line := fmt.Sprintf("A called run %s down parked: run %s (workflow %s) is %s",
		ordinalDepth(child.Depth), untrustedtext.Field(child.ItemID),
		untrustedtext.Field(child.WorkflowID), stateText(child.State, child.Reason))
	if phase := phaseCoordinate(child.PhaseID, child.Attempt); phase != "" {
		line += " in phase " + phase
	}
	if detail := strings.TrimSpace(child.Detail); detail != "" {
		return line + ": " + untrustedtext.Quote(detail, MaxDetailRunes)
	}
	return line + "."
}

// chainLine renders the run ids between the root and the park, elided in the
// middle when the tree is deep. A campaign's hundredth wave has ninety-eight
// ancestors nobody will act on; the ends are what a reader navigates from, and
// the elision states how many were left out rather than quietly dropping them.
//
// A chain of one is the root alone, which the headline already named, so it
// contributes no line.
func chainLine(chain []string) string {
	if len(chain) < 2 {
		return ""
	}
	rendered := make([]string, 0, MaxChainRuns+1)
	if len(chain) <= MaxChainRuns {
		for _, itemID := range chain {
			rendered = append(rendered, untrustedtext.Field(itemID))
		}
	} else {
		head := MaxChainRuns / 2
		tail := MaxChainRuns - head
		for _, itemID := range chain[:head] {
			rendered = append(rendered, untrustedtext.Field(itemID))
		}
		rendered = append(rendered, fmt.Sprintf("…%d more…", len(chain)-MaxChainRuns))
		for _, itemID := range chain[len(chain)-tail:] {
			rendered = append(rendered, untrustedtext.Field(itemID))
		}
	}
	return "Call chain: " + strings.Join(rendered, " → ") + "."
}

// ordinalDepth keeps the depth readable without a pluralization table: a direct
// child is "one level", anything deeper states the number.
func ordinalDepth(depth int) string {
	if depth <= 1 {
		return "one level"
	}
	return fmt.Sprintf("%d levels", depth)
}

func writeOutputs(out *strings.Builder, outputs []Output) {
	if len(outputs) == 0 {
		return
	}
	out.WriteString("\n\nOutputs:")
	for index, output := range outputs {
		if index == MaxOutputs {
			fmt.Fprintf(out, "\n- …and %d more (read the run record).", len(outputs)-MaxOutputs)
			return
		}
		writeOutputLine(out, output)
	}
}

// writeAttemptOutputs renders what the PARKED attempt produced — the verdict,
// the severity, the count a gate is asking a human about. The app bounds the
// list before it gets here (the same digest `run inspect` prints), so the
// overflow tail restates the app's count rather than recomputing one, and names
// the read that returns the attempt whole.
func writeAttemptOutputs(out *strings.Builder, in Input) {
	if len(in.AttemptOutputs) == 0 {
		return
	}
	itemID, phaseID, attempt := in.Run.ItemID, in.Run.PhaseID, in.Run.Attempt
	whose := "the parked attempt"
	if child := in.Descendant; child != nil {
		itemID, phaseID, attempt = child.ItemID, child.PhaseID, child.Attempt
		whose = "the called run's parked attempt"
	}
	fmt.Fprintf(out, "\n\nWhat %s produced%s:", whose, attemptCoordinate(phaseID, attempt))
	for _, output := range in.AttemptOutputs {
		writeOutputLine(out, output)
	}
	if in.AttemptOutputOverflow > 0 {
		fmt.Fprintf(out, "\n- …and %d more (%s).",
			in.AttemptOutputOverflow, inspectCommand(itemID, phaseID, attempt))
	}
}

// attemptCoordinate is the parenthetical form of a phase/attempt pair, for the
// headings that already sit inside a sentence. It stays empty for an unnamed
// phase rather than rendering an empty pair.
func attemptCoordinate(phaseID string, attempt int) string {
	phaseID = strings.TrimSpace(phaseID)
	if phaseID == "" {
		return ""
	}
	rendered := " (phase " + untrustedtext.Field(phaseID)
	if attempt > 0 {
		rendered += fmt.Sprintf(", attempt %d", attempt)
	}
	return rendered + ")"
}

func writeOutputLine(out *strings.Builder, output Output) {
	fmt.Fprintf(out, "\n- %s: %s",
		untrustedtext.Field(output.Name), untrustedtext.Quote(output.Value, MaxOutputRunes))
}

func writeReferences(out *strings.Builder, references []Reference) {
	if len(references) == 0 {
		return
	}
	out.WriteString("\n\nReferences:")
	for index, reference := range references {
		if index == MaxReferences {
			fmt.Fprintf(out, "\n- …and %d more (read the run record).", len(references)-MaxReferences)
			return
		}
		fmt.Fprintf(out, "\n- %s: %s",
			untrustedtext.Field(reference.Label), untrustedtext.Field(reference.Value))
	}
}
