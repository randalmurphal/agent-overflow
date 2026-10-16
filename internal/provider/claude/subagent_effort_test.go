package claude

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func agentLaunchLine(toolUseID, parentToolUseID, input string) []byte {
	parent := ""
	if parentToolUseID != "" {
		parent = `"parent_tool_use_id":"` + parentToolUseID + `",`
	}
	return []byte(`{"type":"assistant",` + parent + `"message":{"id":"msg-` + toolUseID + `","role":"assistant","content":[{"type":"tool_use","id":"` + toolUseID + `","name":"Agent","input":` + input + `}]}}`)
}

func launchInheritedEffort(t *testing.T, parser *Parser, line []byte) (string, bool) {
	t.Helper()
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, event := range events {
		if event.Kind != provider.EventToolStart || event.ItemType != "Agent" {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(event.Meta, &meta); err != nil {
			t.Fatalf("decode meta: %v", err)
		}
		effort, ok := meta[inheritedEffortMetaKey].(string)
		return effort, ok
	}
	t.Fatalf("no Agent tool start in %+v", events)
	return "", false
}

func TestAgentLaunchRecordsTheSessionEffortItInherits(t *testing.T) {
	parser := NewParser()
	parser.SetSessionEffort("xhigh")

	effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("toolu-a", "", `{"description":"look","prompt":"p"}`))
	if !ok || effort != "xhigh" {
		t.Fatalf("inherited_effort = %q (present %v), want xhigh", effort, ok)
	}
	// The stamp is the launcher's effort, not the launch's request: the
	// row reads input.effort for that.
	effort, _ = launchInheritedEffort(t, parser, agentLaunchLine("toolu-b", "", `{"prompt":"p","effort":"low"}`))
	if effort != "xhigh" {
		t.Fatalf("inherited_effort with an input effort = %q, want xhigh", effort)
	}
}

func TestAgentLaunchWithoutAKnownSessionEffortIsNotStamped(t *testing.T) {
	parser := NewParser()
	if effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p"}`)); ok {
		t.Fatalf("inherited_effort = %q on a session with no --effort, want absent", effort)
	}
}

func TestNestedAgentLaunchInheritsItsLaunchersEffort(t *testing.T) {
	parser := NewParser()
	parser.SetSessionEffort("medium")

	launchInheritedEffort(t, parser, agentLaunchLine("toolu-outer", "", `{"prompt":"p","effort":"max"}`))
	effort, _ := launchInheritedEffort(t, parser, agentLaunchLine("toolu-inner", "toolu-outer", `{"prompt":"p"}`))
	if effort != "max" {
		t.Fatalf("nested inherited_effort = %q, want the outer agent's max", effort)
	}
	// A fork ignores its input effort, so what it nests inherits the
	// fork's launcher's effort.
	launchInheritedEffort(t, parser, agentLaunchLine("toolu-fork", "", `{"prompt":"p","subagent_type":"fork","effort":"low"}`))
	effort, _ = launchInheritedEffort(t, parser, agentLaunchLine("toolu-under-fork", "toolu-fork", `{"prompt":"p"}`))
	if effort != "medium" {
		t.Fatalf("inherited_effort under a fork = %q, want the session's medium", effort)
	}
	// An outer agent that named none runs at what it inherited.
	launchInheritedEffort(t, parser, agentLaunchLine("toolu-plain", "", `{"prompt":"p"}`))
	effort, _ = launchInheritedEffort(t, parser, agentLaunchLine("toolu-under-plain", "toolu-plain", `{"prompt":"p"}`))
	if effort != "medium" {
		t.Fatalf("inherited_effort under a plain agent = %q, want medium", effort)
	}
}

func TestEffortReplyMovesTheEffortLaterLaunchesInherit(t *testing.T) {
	parser := NewParser()
	parser.SetSessionEffort("high")

	launchInheritedEffort(t, parser, agentLaunchLine("toolu-before", "", `{"prompt":"p"}`))
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"syn-1","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"Set effort level to low (this session only): Quick"}]}}`)); err != nil {
		t.Fatalf("parse effort reply: %v", err)
	}
	effort, _ := launchInheritedEffort(t, parser, agentLaunchLine("toolu-after", "", `{"prompt":"p"}`))
	if effort != "low" {
		t.Fatalf("inherited_effort after /effort low = %q, want low", effort)
	}
	// A launch delivered again keeps what was true when it launched.
	effort, _ = launchInheritedEffort(t, parser, agentLaunchLine("toolu-before", "", `{"prompt":"p"}`))
	if effort != "high" {
		t.Fatalf("re-delivered launch inherited_effort = %q, want its launch-time high", effort)
	}

	// A tier AO cannot represent leaves the effort unknown.
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"syn-2","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"Set effort level to auto (this session only): Default"}]}}`)); err != nil {
		t.Fatalf("parse effort reply: %v", err)
	}
	if effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("toolu-auto", "", `{"prompt":"p"}`)); ok {
		t.Fatalf("inherited_effort after /effort auto = %q, want absent", effort)
	}
	// Other command output leaves it alone.
	parser.SetSessionEffort("max")
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"syn-3","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"Current effort level: low"}]}}`)); err != nil {
		t.Fatalf("parse readback: %v", err)
	}
	effort, _ = launchInheritedEffort(t, parser, agentLaunchLine("toolu-readback", "", `{"prompt":"p"}`))
	if effort != "max" {
		t.Fatalf("inherited_effort after a readback = %q, want max", effort)
	}
}

func TestOnlyAgentLaunchesAreStamped(t *testing.T) {
	parser := NewParser()
	parser.SetSessionEffort("xhigh")
	events, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"m","role":"assistant","content":[{"type":"tool_use","id":"toolu-bash","name":"Bash","input":{"command":"ls"}}]}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if _, ok := meta[inheritedEffortMetaKey]; ok {
		t.Fatalf("Bash tool start carries inherited_effort: %s", events[0].Meta)
	}
}

func TestMirrorProjectedAgentLaunchIsStamped(t *testing.T) {
	parser := NewParser()
	parser.SetSessionEffort("medium")
	launchInheritedEffort(t, parser, agentLaunchLine("toolu-bg", "", `{"prompt":"p","effort":"xhigh","run_in_background":true}`))

	projected := parser.observeProjectedToolCalls(testThread, []provider.ProviderEvent{{
		Kind:            provider.EventToolStart,
		ThreadID:        testThread,
		ItemID:          "toolu-nested",
		ItemType:        "Agent",
		ParentToolUseID: "toolu-bg",
		Meta:            json.RawMessage(`{"toolName":"Agent","input":{"prompt":"p","big":12345678901234567890}}`),
		Timestamp:       time.Unix(0, 0),
	}})
	if len(projected) != 1 {
		t.Fatalf("projected = %+v", projected)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(projected[0].Meta, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if string(meta[inheritedEffortMetaKey]) != `"xhigh"` {
		t.Fatalf("projected nested launch inherited_effort = %s, want the background agent's xhigh", meta[inheritedEffortMetaKey])
	}
	if string(meta["input"]) != `{"prompt":"p","big":12345678901234567890}` {
		t.Fatalf("projected input re-encoded: %s", meta["input"])
	}
}

func effortReplyLine(id, text string) []byte {
	encoded, _ := json.Marshal(text)
	return []byte(`{"type":"assistant","message":{"id":"` + id + `","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":` + string(encoded) + `}]}}`)
}

func mustParse(t *testing.T, parser *Parser, line []byte) []provider.ProviderEvent {
	t.Helper()
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse %s: %v", line, err)
	}
	return events
}

// The CLI's /effort replies other than "Set effort level to" (2.1.300):
// auto, auto under CLAUDE_CODE_EFFORT_LEVEL, and the cap substitution.
func TestEffortAutoAndCapRepliesMoveTheSessionEffort(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
		want  string
	}{
		{"auto", "Effort level set to auto (this session only)", ""},
		{"auto saved", "Effort level set to auto", ""},
		{"auto under the env override", "Effort set to auto for this session, but CLAUDE_CODE_EFFORT_LEVEL=high still controls this session", ""},
		{"settings cleared under the env override", "Cleared effort from settings, but CLAUDE_CODE_EFFORT_LEVEL=high still controls this session", ""},
		{"capped", "Effort 'max' exceeds the cap for claude-opus-4-7 set by your settings or organization; set to 'high' instead (this session only): Deep", "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parser := NewParser()
			parser.SetSessionEffort("xhigh")
			mustParse(t, parser, effortReplyLine("syn", tc.reply))
			effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("toolu-after", "", `{"prompt":"p"}`))
			if effort != tc.want || ok != (tc.want != "") {
				t.Fatalf("inherited_effort after %q = %q (present %v), want %q", tc.reply, effort, ok, tc.want)
			}
		})
	}
}

// claude-tui stamps each main-loop assistant envelope with its API
// request's effort; a subagent's envelope says nothing about the session.
func TestRequestEffortOnTheMainLoopMovesTheSessionEffort(t *testing.T) {
	parser := NewParser()
	parser.SetSessionEffort("high")
	mustParse(t, parser, []byte(`{"type":"assistant","ao_request_effort":"low","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"toolu-a","name":"Agent","input":{"prompt":"p"}}]}}`))
	if effort, _ := launchInheritedEffort(t, parser, agentLaunchLine("toolu-b", "", `{"prompt":"p"}`)); effort != "low" {
		t.Fatalf("inherited_effort after a low request = %q, want low", effort)
	}
	mustParse(t, parser, []byte(`{"type":"assistant","parent_tool_use_id":"toolu-a","ao_request_effort":"max","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"sub"}]}}`))
	if effort, _ := launchInheritedEffort(t, parser, agentLaunchLine("toolu-c", "", `{"prompt":"p"}`)); effort != "low" {
		t.Fatalf("a subagent request moved the session effort to %q", effort)
	}
	// A request that names no effort (auto, or a model without tiers)
	// leaves it unknown rather than stale.
	mustParse(t, parser, []byte(`{"type":"assistant","ao_request_effort":"","message":{"id":"m3","role":"assistant","content":[{"type":"text","text":"x"}]}}`))
	if effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("toolu-d", "", `{"prompt":"p"}`)); ok {
		t.Fatalf("inherited_effort after a request without effort = %q, want absent", effort)
	}
}

func launchMeta(t *testing.T, parser *Parser, line []byte) map[string]any {
	t.Helper()
	for _, event := range mustParse(t, parser, line) {
		if event.Kind == provider.EventToolStart && event.ItemType == "Agent" {
			var meta map[string]any
			if err := json.Unmarshal(event.Meta, &meta); err != nil {
				t.Fatalf("decode meta: %v", err)
			}
			return meta
		}
	}
	t.Fatalf("no Agent start for %s", line)
	return nil
}

// An Agent `model` alias resolves the way the CLI resolves it: the
// launcher's own family keeps the launcher's model, any other alias takes
// the CLI's alias target, and a fork ignores it.
func TestAgentModelAliasResolvesToTheModelItRunsOn(t *testing.T) {
	parser := NewParser()
	parser.SetModel("claude-opus-4-7")
	parser.SetModelAliases(map[string]string{"haiku": "claude-haiku-4-5", "opus": "claude-opus-4-8", "sonnet": "claude-sonnet-4-6"})
	for i, tc := range []struct {
		input string
		want  any
	}{
		{`{"prompt":"p","model":"haiku","effort":"max"}`, "claude-haiku-4-5"},
		{`{"prompt":"p","model":"opus"}`, "claude-opus-4-7"},
		{`{"prompt":"p","model":"fable"}`, nil},
		{`{"prompt":"p","model":"haiku","subagent_type":"fork"}`, nil},
		{`{"prompt":"p"}`, nil},
	} {
		meta := launchMeta(t, parser, agentLaunchLine(fmt.Sprintf("toolu-alias-%d", i), "", tc.input))
		if got := meta[requestedModelMetaKey]; got != tc.want {
			t.Errorf("%s: requested_model = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// A resumed agent runs at the effort its launch named, else at its
// sender's effort when the resume happens: the carrier records the latter.
func TestResumeCarrierRecordsTheEffortAtResumeTime(t *testing.T) {
	parser := NewParser()
	parser.SetSessionEffort("high")
	mustParse(t, parser, agentLaunchLine("launch-1", "", `{"prompt":"p"}`))
	mustParse(t, parser, []byte(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"launch-1","task_type":"local_agent"}`))
	mustParse(t, parser, effortReplyLine("syn-1", "Set effort level to low (this session only): Quick"))
	mustParse(t, parser, []byte(sendMessageToolUse("carrier-2")))
	events := mustParse(t, parser, []byte(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"carrier-2","task_type":"local_agent","prompt":"keep going"}`))
	if meta := findRebindStart(t, events, "carrier-2"); meta[inheritedEffortMetaKey] != "low" {
		t.Fatalf("carrier inherited_effort = %v, want the effort at resume time (low)", meta[inheritedEffortMetaKey])
	}
	// The resumed round now runs at low, so a launch nested in it (still
	// parented to the original launch) inherits low.
	if effort, _ := launchInheritedEffort(t, parser, agentLaunchLine("nested", "launch-1", `{"prompt":"p"}`)); effort != "low" {
		t.Fatalf("nested launch in the resumed round inherited %q, want low", effort)
	}
}

// A launch that named its effort keeps running at it after a resume.
func TestResumeOfANamedEffortLaunchKeepsItsEffort(t *testing.T) {
	parser := NewParser()
	parser.SetSessionEffort("high")
	mustParse(t, parser, agentLaunchLine("launch-1", "", `{"prompt":"p","effort":"max"}`))
	mustParse(t, parser, []byte(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"launch-1","task_type":"local_agent"}`))
	mustParse(t, parser, effortReplyLine("syn-1", "Set effort level to low (this session only): Quick"))
	mustParse(t, parser, []byte(sendMessageToolUse("carrier-2")))
	mustParse(t, parser, []byte(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"carrier-2","task_type":"local_agent","prompt":"keep going"}`))
	if effort, _ := launchInheritedEffort(t, parser, agentLaunchLine("nested", "launch-1", `{"prompt":"p"}`)); effort != "max" {
		t.Fatalf("nested launch in a named-effort resumed round inherited %q, want max", effort)
	}
}

// A relative effort ("lower"/"higher", experiment-gated) names no effort AO
// can reproduce, so what nests inside it inherits none.
func TestRelativeEffortLeavesTheAgentsEffortUnknown(t *testing.T) {
	parser := NewParser()
	parser.SetSessionEffort("high")
	mustParse(t, parser, agentLaunchLine("outer", "", `{"prompt":"p","effort":"higher"}`))
	if effort, ok := launchInheritedEffort(t, parser, agentLaunchLine("inner", "outer", `{"prompt":"p"}`)); ok {
		t.Fatalf("launch nested under a relative effort inherited %q, want none", effort)
	}
}
