package wake

import (
	"regexp"
	"strings"
	"testing"
)

// `RepairSentence` is not a second answer to "which verb settles this" — it is
// THE answer, exported so `agent-overflow run watch` prints the wake's own
// sentence verbatim instead of composing one of its own (D58).
//
// That only holds if every park a wake names a verb for also has a repair
// sentence. `checkpoint` is where the two came apart: the closing named `run
// resume` in its own branch while the sentence had none, so the park a
// supervising agent produces for ITSELF (a soft stop, D36) watched to a resting
// line that named no verb at all.

// parkedReasons is every reason `closing` branches on for a `needs-human` park,
// with the gate kinds that change the verb. Each entry is a park a reader is
// expected to act on, so each must name how.
var parkedReasons = []struct {
	reason       string
	gateDecision string
}{
	{reasonCheckpoint, ""},
	{reasonUnitFailed, ""},
	{reasonPaused, ""},
	{reasonInterrupted, ""},
	{reasonGate, ""},
	{reasonGate, gateDecisionHuman},
	{reasonGate, gateDecisionPark},
	{reasonQuestion, ""},
	{reasonStuck, ""},
	{reasonProviderRetriesExhausted, ""},
	{reasonProviderUsageLimited, ""},
	{reasonLoopLimitExhausted, ""},
	{reasonRetriesExhausted, ""},
}

// runVerbs extracts every `agent-overflow run <verb>` a piece of copy names, so
// the parity check compares the commands rather than the prose around them.
var runVerbs = regexp.MustCompile(`agent-overflow run ([a-z][a-z-]*)`)

func namedVerbs(text string) []string {
	matches := runVerbs.FindAllStringSubmatch(text, -1)
	verbs := make([]string, 0, len(matches))
	for _, match := range matches {
		verbs = append(verbs, match[1])
	}
	return verbs
}

// Every park a wake reports has a repair sentence, and that sentence names
// every verb the closing around it names. A closing that reaches for a verb the
// sentence does not carry is a park whose two surfaces disagree — the reader of
// `run watch` gets the run and no way to settle it.
func TestEveryParkedReasonCarriesTheVerbItsClosingNames(t *testing.T) {
	for _, test := range parkedReasons {
		run := Run{ItemID: "run-11", Goal: "g", WorkflowID: "w",
			State: stateNeedsHuman, Reason: test.reason, GateDecision: test.gateDecision}
		repair := RepairSentence(run.ItemID, run.State, run.Reason, run.GateDecision, run.GateLabel)
		if strings.TrimSpace(repair) == "" {
			t.Fatalf("reason %q (gate decision %q) has no repair sentence; `run watch` would rest with no verb",
				test.reason, test.gateDecision)
		}
		for _, verb := range namedVerbs(closing(Input{Run: run})) {
			if !strings.Contains(repair, "agent-overflow run "+verb) {
				t.Fatalf("reason %q (gate decision %q): the closing names `run %s` and the repair sentence does not:\nclosing: %s\nrepair:  %s",
					test.reason, test.gateDecision, verb, closing(Input{Run: run}), repair)
			}
		}
	}
}

// The same parity at the descendant level, where the verb matters most: the run
// to act on is one the reader has never seen, and `run watch --tree` is how they
// saw it park at all.
func TestEveryDescendantParkCarriesTheVerbItsClosingNames(t *testing.T) {
	for _, test := range parkedReasons {
		child := &Descendant{ItemID: "wave-7", WorkflowID: "wave", Depth: 2,
			State: stateNeedsHuman, Reason: test.reason, GateDecision: test.gateDecision}
		repair := RepairSentence(child.ItemID, child.State, child.Reason, child.GateDecision, child.GateLabel)
		if strings.TrimSpace(repair) == "" {
			t.Fatalf("descendant reason %q (gate decision %q) has no repair sentence",
				test.reason, test.gateDecision)
		}
		text := closing(Input{
			Run:        Run{ItemID: "root-1", Goal: "g", WorkflowID: "campaign", State: "running"},
			Descendant: child,
		})
		for _, verb := range namedVerbs(text) {
			if !strings.Contains(repair, "agent-overflow run "+verb) {
				t.Fatalf("descendant reason %q (gate decision %q): the closing names `run %s` and the repair sentence does not:\nclosing: %s\nrepair:  %s",
					test.reason, test.gateDecision, verb, text, repair)
			}
		}
		// And the verb always carries the DESCENDANT's id: acting on the root is
		// the mistake the descendant closing exists to prevent.
		if strings.Contains(repair, `"root-1"`) {
			t.Fatalf("descendant reason %q names the root: %s", test.reason, repair)
		}
	}
}

// A checkpoint's copy has ONE definition. `run watch` prints the repair sentence
// and nothing else, so what a soft-stopped run's watcher reads has to be the same
// words its bound thread reads.
func TestCheckpointRepairSentenceIsTheClosingItself(t *testing.T) {
	repair := RepairSentence("run-3", stateNeedsHuman, reasonCheckpoint, "", "")
	mustContain(t, repair,
		"This is the stop that was asked for, not a failure.",
		"`agent-overflow run resume \"run-3\"` takes the call it skipped, or leave it parked.")

	root := closing(Input{Run: Run{ItemID: "run-3", Goal: "g", WorkflowID: "w",
		State: stateNeedsHuman, Reason: reasonCheckpoint}})
	if root != repair {
		t.Fatalf("the checkpoint closing and its repair sentence differ:\nclosing: %s\nrepair:  %s", root, repair)
	}
	// A reason with no one verb still prints none: silence there is the deliberate
	// answer, and this parity must not be read as "every park gets a resume".
	if got := RepairSentence("run-3", stateNeedsHuman, "stalled", "", ""); got != "" {
		t.Fatalf("a reason that names its own cause invented a verb: %s", got)
	}
}
