package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// Claude Code 2.1.317+ runs a subagent on a model other than the one the
// session's effort was picked for at that model's saved effort or its
// default, which AO cannot see (claude-wire.md §Agent effort). The parser
// learns the subagent's model from its first own envelope and stamps the
// session's effort only when the models match.

func mainLoopLine(id, model string) []byte {
	return []byte(`{"type":"assistant","message":{"id":"` + id + `","model":"` + model + `","role":"assistant","content":[{"type":"text","text":"x"}]}}`)
}

func subagentLine(id, parentToolUseID, model string) []byte {
	modelField := ""
	if model != "" {
		modelField = `"model":"` + model + `",`
	}
	return []byte(`{"type":"assistant","parent_tool_use_id":"` + parentToolUseID + `","message":{"id":"` + id + `",` + modelField + `"role":"assistant","content":[{"type":"text","text":"sub"}]}}`)
}

// modelReport is the meta-only update the parser emits for the launch at
// launchID when its subagent reports its model; nil when none was emitted.
func modelReport(t *testing.T, events []provider.ProviderEvent, launchID string) map[string]any {
	t.Helper()
	for _, event := range events {
		if event.Kind != provider.EventToolStart || event.ItemID != launchID || event.ItemType != "" {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(event.Meta, &meta); err != nil {
			t.Fatalf("decode meta: %v", err)
		}
		if _, ok := meta["subagent_model"]; ok {
			return meta
		}
	}
	return nil
}

func newModelScopedParser(cliVersion string) *Parser {
	parser := NewParser()
	parser.SetCLIVersion(cliVersion)
	parser.SetModel("claude-opus-4-7[1m]")
	parser.SetSessionEffort("xhigh")
	return parser
}

func TestSubagentOnAnotherModelIsNotStampedWithTheSessionEffort(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, mainLoopLine("m0", "claude-opus-4-7"))
	if effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p","model":"haiku"}`)); ok {
		t.Fatalf("launch stamped %q before its model is known", effort)
	}
	report := modelReport(t, mustParse(t, parser, subagentLine("s1", "toolu-a", "claude-haiku-4-5-20251001")), "toolu-a")
	if report == nil {
		t.Fatal("no model report for the subagent")
	}
	if effort, ok := report[inheritedEffortMetaKey]; ok {
		t.Fatalf("subagent on Haiku stamped the session's effort %v", effort)
	}
}

func TestSubagentOnTheSessionModelIsStampedWhenItReportsIt(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, mainLoopLine("m0", "claude-opus-4-7"))
	launchInheritedEffort(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p"}`))
	// The session's effort moves after the launch; the agent took the
	// effort it launched at.
	mustParse(t, parser, effortReplyLine("syn-1", "Set effort level to low (this session only): Quick"))
	report := modelReport(t, mustParse(t, parser, subagentLine("s1", "toolu-a", "claude-opus-4-7")), "toolu-a")
	if report == nil || report[inheritedEffortMetaKey] != "xhigh" {
		t.Fatalf("same-model report = %v, want inherited_effort xhigh", report)
	}
}

func TestSubagentCompareUsesTheMainLoopModelOverInit(t *testing.T) {
	// The main loop runs on Sonnet although the session was seeded with
	// Opus (a plan-mode model, a reroute): the effort was picked for Sonnet.
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, mainLoopLine("m0", "claude-sonnet-4-6"))
	launchInheritedEffort(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p"}`))
	if report := modelReport(t, mustParse(t, parser, subagentLine("s1", "toolu-a", "claude-sonnet-4-6")), "toolu-a"); report[inheritedEffortMetaKey] != "xhigh" {
		t.Fatalf("report = %v, want inherited_effort xhigh", report)
	}
}

func TestNamedEffortIsInheritedByNestedLaunchesWhateverTheirModel(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, mainLoopLine("m0", "claude-opus-4-7"))
	launchInheritedEffort(t, parser, agentLaunchLine("outer", "", `{"prompt":"p","model":"haiku","effort":"low"}`))
	if report := modelReport(t, mustParse(t, parser, subagentLine("s1", "outer", "claude-haiku-4-5")), "outer"); report[inheritedEffortMetaKey] != nil {
		t.Fatalf("named launch on another model stamped %v", report[inheritedEffortMetaKey])
	}
	effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("inner", "outer", `{"prompt":"p","model":"sonnet"}`))
	if !ok || effort != "low" {
		t.Fatalf("nested launch inherited %q (present %v), want the outer agent's named low", effort, ok)
	}
}

func TestForkRunsAtItsLaunchersEffort(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, mainLoopLine("m0", "claude-opus-4-7"))
	if effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("fork-top", "", `{"prompt":"p","subagent_type":"fork","effort":"low","model":"haiku"}`)); !ok || effort != "xhigh" {
		t.Fatalf("top-level fork inherited %q (present %v), want the session's xhigh", effort, ok)
	}
	// A fork inside an agent on another model runs at that agent's
	// effort, which AO does not know.
	launchInheritedEffort(t, parser, agentLaunchLine("haiku-agent", "", `{"prompt":"p","model":"haiku"}`))
	mustParse(t, parser, subagentLine("s1", "haiku-agent", "claude-haiku-4-5"))
	if effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("fork-in-haiku", "haiku-agent", `{"prompt":"p","subagent_type":"fork"}`)); ok {
		t.Fatalf("fork inside an agent on another model inherited %q, want none", effort)
	}
	// Inside an agent on the session's model it runs at the session's.
	launchInheritedEffort(t, parser, agentLaunchLine("opus-agent", "", `{"prompt":"p"}`))
	mustParse(t, parser, subagentLine("s2", "opus-agent", "claude-opus-4-7"))
	if effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("fork-in-opus", "opus-agent", `{"prompt":"p","subagent_type":"fork"}`)); !ok || effort != "xhigh" {
		t.Fatalf("fork inside an agent on the session's model inherited %q (present %v), want xhigh", effort, ok)
	}
}

// A nested launch with no effort layer above it compares its model with
// the model the session's effort was picked for, not with its launcher's.
func TestNestedLaunchComparesWithTheSessionsPickedModel(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, mainLoopLine("m0", "claude-opus-4-7"))
	launchInheritedEffort(t, parser, agentLaunchLine("haiku-agent", "", `{"prompt":"p","model":"haiku"}`))
	mustParse(t, parser, subagentLine("s1", "haiku-agent", "claude-haiku-4-5"))
	// The main loop moves to Sonnet after the outer launch; the outer
	// agent carries the model its launch picked the effort for.
	mustParse(t, parser, mainLoopLine("m1", "claude-sonnet-4-6"))

	if effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("on-opus", "haiku-agent", `{"prompt":"p","model":"opus"}`)); ok {
		t.Fatalf("nested launch stamped %q before its model is known", effort)
	}
	if report := modelReport(t, mustParse(t, parser, subagentLine("s2", "on-opus", "claude-opus-4-7")), "on-opus"); report[inheritedEffortMetaKey] != "xhigh" {
		t.Fatalf("nested agent on the picked model = %v, want xhigh", report)
	}
	launchInheritedEffort(t, parser, agentLaunchLine("on-haiku", "haiku-agent", `{"prompt":"p"}`))
	if report := modelReport(t, mustParse(t, parser, subagentLine("s3", "on-haiku", "claude-haiku-4-5")), "on-haiku"); report[inheritedEffortMetaKey] != nil {
		t.Fatalf("nested agent on its launcher's other model stamped %v", report[inheritedEffortMetaKey])
	}
}

func TestModelReportIsDecidedOnce(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, mainLoopLine("m0", "claude-opus-4-7"))
	launchInheritedEffort(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p"}`))
	// An envelope without a model decides nothing.
	if report := modelReport(t, mustParse(t, parser, subagentLine("s0", "toolu-a", "")), "toolu-a"); report != nil {
		t.Fatalf("model-less envelope reported %v", report)
	}
	if report := modelReport(t, mustParse(t, parser, subagentLine("s1", "toolu-a", "claude-opus-4-7")), "toolu-a"); report[inheritedEffortMetaKey] != "xhigh" {
		t.Fatalf("first report = %v", report)
	}
	if report := modelReport(t, mustParse(t, parser, subagentLine("s2", "toolu-a", "claude-opus-4-7")), "toolu-a"); report != nil {
		t.Fatalf("second envelope reported again: %v", report)
	}
	// The launch delivered again (a mirror replay) carries the decided stamp.
	if effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p"}`)); !ok || effort != "xhigh" {
		t.Fatalf("re-delivered launch inherited %q (present %v), want xhigh", effort, ok)
	}
}

func TestResumeCarrierFollowsTheResumedAgentsModel(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		want        any
	}{
		{"same model", "claude-opus-4-7", "low"},
		{"other model", "claude-haiku-4-5", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parser := newModelScopedParser("2.1.321")
			mustParse(t, parser, mainLoopLine("m0", "claude-opus-4-7"))
			mustParse(t, parser, agentLaunchLine("launch-1", "", `{"prompt":"p"}`))
			mustParse(t, parser, []byte(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"launch-1","task_type":"local_agent"}`))
			mustParse(t, parser, subagentLine("s1", "launch-1", tc.model))
			mustParse(t, parser, effortReplyLine("syn-1", "Set effort level to low (this session only): Quick"))
			mustParse(t, parser, []byte(sendMessageToolUse("carrier-2")))
			events := mustParse(t, parser, []byte(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"carrier-2","task_type":"local_agent","prompt":"keep going"}`))
			if got := findRebindStart(t, events, "carrier-2")[inheritedEffortMetaKey]; got != tc.want {
				t.Fatalf("carrier inherited_effort = %v, want %v", got, tc.want)
			}
		})
	}
}

// A resume of a launch an earlier process ran, which named no effort, has
// no record of the model the effort was picked for.
func TestResumeOfAnUnnamedHistoricalLaunchIsNotStamped(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	parser.agentLaunchLookup = func(string) (AgentLaunchRecord, bool, error) {
		return AgentLaunchRecord{ToolUseID: "launch-1", Model: "claude-opus-4-7"}, true, nil
	}
	mustParse(t, parser, []byte(sendMessageToolUse("carrier-2")))
	events := mustParse(t, parser, []byte(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"carrier-2","task_type":"local_agent","prompt":"keep going"}`))
	if got, ok := findRebindStart(t, events, "carrier-2")[inheritedEffortMetaKey]; ok {
		t.Fatalf("carrier inherited_effort = %v, want none", got)
	}
}

// Before 2.1.317 every launch that names no effort runs at its launcher's.
func TestOlderCLIStampsTheLaunchersEffortAtLaunch(t *testing.T) {
	parser := newModelScopedParser("2.1.316")
	mustParse(t, parser, mainLoopLine("m0", "claude-opus-4-7"))
	if effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p","model":"haiku"}`)); !ok || effort != "xhigh" {
		t.Fatalf("launch on 2.1.316 inherited %q (present %v), want xhigh", effort, ok)
	}
	if report := modelReport(t, mustParse(t, parser, subagentLine("s1", "toolu-a", "claude-haiku-4-5")), "toolu-a"); report[inheritedEffortMetaKey] != nil {
		t.Fatalf("model report restamped %v", report[inheritedEffortMetaKey])
	}
}

// The init envelope's version decides the rule, over the seeded one.
func TestInitVersionSelectsTheRule(t *testing.T) {
	parser := newModelScopedParser("")
	mustParse(t, parser, []byte(`{"type":"system","subtype":"init","session_id":"s","model":"claude-opus-4-7","claude_code_version":"2.1.300"}`))
	mustParse(t, parser, mainLoopLine("m0", "claude-opus-4-7"))
	if effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p","model":"haiku"}`)); !ok || effort != "xhigh" {
		t.Fatalf("launch under a 2.1.300 init inherited %q (present %v), want xhigh", effort, ok)
	}
	// An unknown version takes the 2.1.317 rule.
	unknown := newModelScopedParser("")
	mustParse(t, unknown, mainLoopLine("m0", "claude-opus-4-7"))
	if effort, ok := launchInheritedEffort(t, unknown, agentLaunchLine("toolu-a", "", `{"prompt":"p","model":"haiku"}`)); ok {
		t.Fatalf("launch under an unknown version stamped %q at launch", effort)
	}
}

// claude-tui reconstructs each subagent request's `output_config.effort`
// onto its envelope: the effort the subagent runs at, reported once.
func TestSubagentRequestEffortIsRecordedAsItsRunEffort(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, mainLoopLine("m0", "claude-opus-4-7"))
	launchInheritedEffort(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p","model":"haiku"}`))
	line := []byte(`{"type":"assistant","parent_tool_use_id":"toolu-a","ao_request_effort":"medium","message":{"id":"s1","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"text","text":"sub"}]}}`)
	report := modelReport(t, mustParse(t, parser, line), "toolu-a")
	if report == nil || report[runEffortMetaKey] != "medium" {
		t.Fatalf("report = %v, want run_effort medium", report)
	}
	// A request that names none ran without one, which is recorded too.
	launchInheritedEffort(t, parser, agentLaunchLine("toolu-b", "", `{"prompt":"p","model":"haiku"}`))
	line = []byte(`{"type":"assistant","parent_tool_use_id":"toolu-b","ao_request_effort":"","message":{"id":"s2","model":"claude-haiku-4-5","role":"assistant","content":[{"type":"text","text":"sub"}]}}`)
	if report := modelReport(t, mustParse(t, parser, line), "toolu-b"); report == nil || report[runEffortMetaKey] != "" {
		t.Fatalf("report = %v, want an empty run_effort", report)
	}
	// Headless envelopes carry no request effort: no run_effort.
	launchInheritedEffort(t, parser, agentLaunchLine("toolu-c", "", `{"prompt":"p"}`))
	if report := modelReport(t, mustParse(t, parser, subagentLine("s3", "toolu-c", "claude-opus-4-7")), "toolu-c"); report == nil {
		t.Fatal("no report")
	} else if _, ok := report[runEffortMetaKey]; ok {
		t.Fatalf("headless report carries run_effort: %v", report)
	}
}
