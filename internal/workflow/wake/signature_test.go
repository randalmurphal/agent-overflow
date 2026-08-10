package wake

import (
	"strings"
	"testing"
)

// The coalescing identity (K2). Two rules, tested in both directions: the same
// ask signs the same, and a genuinely new state never signs the same as the one
// before it.

func restingInput() Input {
	return Input{Run: Run{
		ItemID: "run-1", Goal: "Ship it", WorkflowID: "build",
		State: stateNeedsHuman, Reason: reasonGate, PhaseID: "review", Attempt: 2,
		Detail: "approve the risky migration?", Cause: "", GateDecision: gateDecisionHuman,
	}}
}

func TestSignatureIsStableForTheSameAsk(t *testing.T) {
	first, second := restingInput(), restingInput()
	// Everything the reader does not act on differs between the two: a second
	// look at the same parked run re-reads the record, and the record moves.
	second.Outputs = []Output{{Name: "verdict", Value: "reject"}}
	second.References = []Reference{{Label: "narrative", Value: "/runs/run-1/review/2/narrative.md"}}
	second.AttemptOutputs = []Output{{Name: "severity", Value: "high"}}
	second.AttemptOutputOverflow = 3
	second.Run.WorktreePath = "/work/run-1"
	second.Run.Branch = "ao/run-1"
	second.Run.Goal = "Ship it (renamed)"
	if Signature(first) != Signature(second) {
		t.Fatalf("the same ask signed differently:\n%s\n%s", Signature(first), Signature(second))
	}
}

func TestSignatureChangesForEveryGenuinelyNewState(t *testing.T) {
	base := Signature(restingInput())
	for _, testCase := range []struct {
		name   string
		mutate func(*Input)
	}{
		{"a different run", func(in *Input) { in.Run.ItemID = "run-2" }},
		{"a different state", func(in *Input) { in.Run.State = stateFailed }},
		{"a different reason", func(in *Input) { in.Run.Reason = reasonQuestion }},
		{"a different phase", func(in *Input) { in.Run.PhaseID = "implement" }},
		{"a new attempt", func(in *Input) { in.Run.Attempt = 3 }},
		{"a different question", func(in *Input) { in.Run.Detail = "approve the other migration?" }},
		{"a different cause", func(in *Input) { in.Run.Cause = "worktree would not cut" }},
		{"a different gate kind", func(in *Input) { in.Run.GateDecision = gateDecisionPark }},
		{"a descendant park", func(in *Input) {
			in.Descendant = &Descendant{ItemID: "wave-3", State: stateNeedsHuman, Reason: reasonQuestion, Attempt: 1}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mutated := restingInput()
			testCase.mutate(&mutated)
			if Signature(mutated) == base {
				t.Fatalf("%s signed identically to the wake before it: %s", testCase.name, base)
			}
		})
	}
}

func TestSignatureDistinguishesTwoDescendantsParkedAlike(t *testing.T) {
	first := restingInput()
	first.Descendant = &Descendant{ItemID: "wave-3", State: stateNeedsHuman, Reason: reasonQuestion, PhaseID: "review", Attempt: 1}
	second := restingInput()
	second.Descendant = &Descendant{ItemID: "wave-4", State: stateNeedsHuman, Reason: reasonQuestion, PhaseID: "review", Attempt: 1}
	if Signature(first) == Signature(second) {
		t.Fatal("two different called runs parked the same way signed identically")
	}
}

// Free text is bounded here exactly as the message bounds it: two causes that
// differ only past the composer's budget produce byte-identical messages, and a
// reader told the same words twice has been told the same thing twice.
func TestSignatureBoundsFreeTextTheWayTheMessageDoes(t *testing.T) {
	long := strings.Repeat("y", MaxCauseRunes*2)
	first, second := restingInput(), restingInput()
	first.Run.Cause = long + "-first"
	second.Run.Cause = long + "-second"
	if Signature(first) != Signature(second) {
		t.Fatal("causes that render identically signed differently")
	}
	if Compose(first) != Compose(second) {
		t.Fatal("the premise is wrong: those causes do not render identically")
	}
}

func TestProgressSignatureSeparatesEveryLapOfALoop(t *testing.T) {
	lap := func(attempt int) ProgressInput {
		return ProgressInput{
			Run:  ProgressRun{ItemID: "campaign-1", WorkflowID: "campaign"},
			Gate: ProgressGate{ItemID: "campaign-1", PhaseID: "wave", Attempt: attempt, Decision: "loop", Target: "wave"},
		}
	}
	if ProgressSignature(lap(11)) == ProgressSignature(lap(12)) {
		t.Fatal("two waves of the same loop signed identically; every wave after the first would be swallowed")
	}
	repeat := lap(12)
	repeat.Outputs = []Output{{Name: "verdict", Value: "pass"}}
	if ProgressSignature(lap(12)) != ProgressSignature(repeat) {
		t.Fatal("the same gate traversal signed differently on a second look at its outputs")
	}
	// A progress wake and a resting wake are never each other's duplicate, even
	// when they name the same run, phase, and attempt.
	resting := restingInput()
	resting.Run.PhaseID, resting.Run.Attempt = "wave", 12
	if ProgressSignature(lap(12)) == Signature(resting) {
		t.Fatal("a progress wake signed as a resting wake")
	}
}
