package claude

import (
	"encoding/json"
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
