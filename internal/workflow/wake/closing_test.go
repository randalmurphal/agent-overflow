package wake

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/workflowtools"
)

// `RepairSentence` is not a second answer to "which call settles this"; it is
// THE answer, exported so run_watch reports the wake's own sentence verbatim
// instead of composing one of its own (D58).
//
// That only holds if every park a wake names a call for also has a repair
// sentence. `checkpoint` is where the two came apart: the closing named resume
// in its own branch while the sentence had none, so the park a supervising
// agent produces for ITSELF (a soft stop, D36) watched to rest with no call
// named at all.

// parkedReasons is every reason `closing` branches on for a `needs-human` park,
// with the gate kinds that change the call. Each entry is a park a reader is
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

// toolCalls extracts every ao-workflow-tools call a piece of copy names, as
// the tool plus its action when it has one, so the parity check compares the
// calls rather than the prose around them.
var toolCalls = regexp.MustCompile("`" + `((?:run|memory|workflow)_[a-z_]+)\(([^)]*)\)` + "`")

var callAction = regexp.MustCompile(`action: "([a-z_]+)"`)

func namedCalls(text string) []string {
	matches := toolCalls.FindAllStringSubmatch(text, -1)
	calls := make([]string, 0, len(matches))
	for _, match := range matches {
		call := match[1]
		if action := callAction.FindStringSubmatch(match[2]); action != nil {
			call += " " + action[1]
		}
		calls = append(calls, call)
	}
	return calls
}

// Every park a wake reports has a repair sentence, and that sentence names
// every call the closing around it names. A closing that reaches for a call the
// sentence does not carry is a park whose two surfaces disagree: the reader of
// run_watch gets the run and no way to settle it.
func TestEveryParkedReasonCarriesTheVerbItsClosingNames(t *testing.T) {
	for _, test := range parkedReasons {
		run := Run{ItemID: "run-11", Goal: "g", WorkflowID: "w",
			State: stateNeedsHuman, Reason: test.reason, GateDecision: test.gateDecision}
		repair := RepairSentence(run.ItemID, run.State, run.Reason, run.GateDecision, run.GateLabel)
		if strings.TrimSpace(repair) == "" {
			t.Fatalf("reason %q (gate decision %q) has no repair sentence; run_watch would rest with no call named",
				test.reason, test.gateDecision)
		}
		for _, call := range namedCalls(closing(Input{Run: run})) {
			if !slices.Contains(namedCalls(repair), call) {
				t.Fatalf("reason %q (gate decision %q): the closing names %s and the repair sentence does not:\nclosing: %s\nrepair:  %s",
					test.reason, test.gateDecision, call, closing(Input{Run: run}), repair)
			}
		}
	}
}

// The same parity at the descendant level, where the call matters most: the
// run to act on is one the reader has never seen, and run_watch with tree is
// how they saw it park at all.
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
		for _, call := range namedCalls(text) {
			if !slices.Contains(namedCalls(repair), call) {
				t.Fatalf("descendant reason %q (gate decision %q): the closing names %s and the repair sentence does not:\nclosing: %s\nrepair:  %s",
					test.reason, test.gateDecision, call, text, repair)
			}
		}
		// And the call always carries the DESCENDANT's id: acting on the root is
		// the mistake the descendant closing exists to prevent.
		if strings.Contains(repair, `"root-1"`) {
			t.Fatalf("descendant reason %q names the root: %s", test.reason, repair)
		}
	}
}

// A checkpoint's copy has ONE definition. run_watch reports the repair sentence
// and nothing else, so what a soft-stopped run's watcher reads has to be the same
// words its bound thread reads.
func TestCheckpointRepairSentenceIsTheClosingItself(t *testing.T) {
	repair := RepairSentence("run-3", stateNeedsHuman, reasonCheckpoint, "", "")
	mustContain(t, repair,
		"This is the stop that was asked for, not a failure.",
		"`run_control(run_id: \"run-3\", action: \"resume\")` takes the call it skipped, or leave it parked.")

	root := closing(Input{Run: Run{ItemID: "run-3", Goal: "g", WorkflowID: "w",
		State: stateNeedsHuman, Reason: reasonCheckpoint}})
	if root != repair {
		t.Fatalf("the checkpoint closing and its repair sentence differ:\nclosing: %s\nrepair:  %s", root, repair)
	}
	// A reason with no one call still prints none: silence there is the deliberate
	// answer, and this parity must not be read as "every park gets a resume".
	if got := RepairSentence("run-3", stateNeedsHuman, "stalled", "", ""); got != "" {
		t.Fatalf("a reason that names its own cause invented a call: %s", got)
	}
}

// Every call a wake names is one the tool server takes as written: the tool
// exists, each argument is in its schema, and each action or decision is one
// it accepts. Copy that names a call the server refuses sends its reader at a
// wall, which is worse than naming none.
func TestEveryNamedCallIsOneTheToolServerTakes(t *testing.T) {
	schemas := map[string]map[string]any{}
	for _, tool := range workflowtools.New(nil).Tools(workflowtools.Caller{}) {
		name, _ := tool["name"].(string)
		input, _ := tool["inputSchema"].(map[string]any)
		properties, _ := input["properties"].(map[string]any)
		schemas[name] = properties
	}
	argument := regexp.MustCompile(`([a-z_]+): ("[^"]*"(?: or "[^"]*")?|[a-z0-9]+)`)
	var texts []string
	for _, test := range parkedReasons {
		texts = append(texts, RepairSentence("run-11", stateNeedsHuman, test.reason, test.gateDecision, "label"))
	}
	texts = append(texts,
		RepairSentence("run-11", stateFailed, "", "", ""),
		inspectCommand("run-11", "review", 2))
	seen := 0
	for _, text := range texts {
		for _, match := range toolCalls.FindAllStringSubmatch(text, -1) {
			seen++
			properties, ok := schemas[match[1]]
			if !ok {
				t.Fatalf("%q names %s, which the tool server does not have", text, match[1])
			}
			for _, arg := range argument.FindAllStringSubmatch(match[2], -1) {
				property, ok := properties[arg[1]].(map[string]any)
				if !ok {
					t.Fatalf("%q passes %s to %s, which its schema does not take", text, arg[1], match[1])
				}
				enum, _ := property["enum"].([]string)
				if enum == nil {
					continue
				}
				for _, value := range strings.Split(arg[2], " or ") {
					if !slices.Contains(enum, strings.Trim(value, `"`)) {
						t.Fatalf("%q passes %s %s to %s, which takes only %v", text, arg[1], value, match[1], enum)
					}
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no repair sentence named a tool call")
	}
}
