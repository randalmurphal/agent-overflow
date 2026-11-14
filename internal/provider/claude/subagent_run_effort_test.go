package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// The session mirror's assistant rows carry `effort`, the
// `output_config.effort` their API request sent (2.1.321 bundle), so a
// subagent's first mirrored row says what it runs at. The parser records it
// once on the row the agent's task is bound to.

func mirroredAgentRow(agentID, uuid, effortField string) []byte {
	return []byte(`{"type":"transcript_mirror","filePath":"/home/user/.claude/projects/p/s/subagents/agent-` + agentID + `.jsonl","entries":[{"type":"assistant","uuid":"` + uuid + `","agentId":"` + agentID + `","isSidechain":true,"timestamp":"2026-11-13T12:00:00Z","message":{"id":"msg-` + uuid + `","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"working"}]}` + effortField + `}]}`)
}

func agentTaskStarted(agentID, toolUseID string) []byte {
	return []byte(`{"type":"system","subtype":"task_started","task_id":"` + agentID + `","tool_use_id":"` + toolUseID + `","task_type":"local_agent","description":"look"}`)
}

// runEffortReport is the run_effort the events record on itemID, with
// whether one was recorded.
func runEffortReport(t *testing.T, events []provider.ProviderEvent, itemID string) (any, bool) {
	t.Helper()
	for _, event := range events {
		if event.Kind != provider.EventToolStart || event.ItemID != itemID {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(event.Meta, &meta); err != nil {
			t.Fatalf("decode meta: %v", err)
		}
		if effort, ok := meta[runEffortMetaKey]; ok {
			return effort, true
		}
	}
	return nil, false
}

func TestMirroredSubagentRowRecordsTheEffortItRanAt(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p","model":"haiku"}`))
	mustParse(t, parser, agentTaskStarted("aid1", "toolu-a"))

	if effort, ok := runEffortReport(t, mustParse(t, parser, mirroredAgentRow("aid1", "r1", `,"effort":"medium"`)), "toolu-a"); !ok || effort != "medium" {
		t.Fatalf("run_effort = %v (present %v), want medium", effort, ok)
	}
	// Delivered again, or a later row: recorded once.
	if effort, ok := runEffortReport(t, mustParse(t, parser, mirroredAgentRow("aid1", "r1", `,"effort":"medium"`)), "toolu-a"); ok {
		t.Fatalf("redelivered row recorded run_effort %v again", effort)
	}
	if effort, ok := runEffortReport(t, mustParse(t, parser, mirroredAgentRow("aid1", "r2", `,"effort":"high"`)), "toolu-a"); ok {
		t.Fatalf("later row recorded run_effort %v", effort)
	}
}

func TestMirroredRowWithoutAnEffortRecordsNothing(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p"}`))
	mustParse(t, parser, agentTaskStarted("aid1", "toolu-a"))
	if effort, ok := runEffortReport(t, mustParse(t, parser, mirroredAgentRow("aid1", "r1", "")), "toolu-a"); ok {
		t.Fatalf("row without effort recorded %v", effort)
	}
	// A tier AO cannot name is not recorded either.
	if effort, ok := runEffortReport(t, mustParse(t, parser, mirroredAgentRow("aid1", "r2", `,"effort":"ultracode"`)), "toolu-a"); ok {
		t.Fatalf("unknown tier recorded %v", effort)
	}
	// The first row that names one is.
	if effort, ok := runEffortReport(t, mustParse(t, parser, mirroredAgentRow("aid1", "r3", `,"effort":"low"`)), "toolu-a"); !ok || effort != "low" {
		t.Fatalf("run_effort = %v (present %v), want low", effort, ok)
	}
}

// The agent's rows can reach the parser before its task_started binds the
// agent to its launch: the effort waits for the binding.
func TestMirroredEffortBeforeTaskStartedWaitsForTheBinding(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p"}`))
	if effort, ok := runEffortReport(t, mustParse(t, parser, mirroredAgentRow("aid1", "r1", `,"effort":"xhigh"`)), "toolu-a"); ok {
		t.Fatalf("unbound agent recorded %v", effort)
	}
	if effort, ok := runEffortReport(t, mustParse(t, parser, agentTaskStarted("aid1", "toolu-a")), "toolu-a"); !ok || effort != "xhigh" {
		t.Fatalf("task_started run_effort = %v (present %v), want xhigh", effort, ok)
	}
}

// agent_metadata names the launch before task_started does.
func TestMirroredEffortFollowsAgentMetadata(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p"}`))
	batch := []byte(`{"type":"transcript_mirror","filePath":"/x/subagents/agent-aid1.jsonl","entries":[{"type":"agent_metadata","agentType":"general-purpose","toolUseId":"toolu-a"},{"type":"assistant","uuid":"r1","agentId":"aid1","isSidechain":true,"message":{"id":"m","role":"assistant","model":"claude-haiku-4-5","content":[]},"effort":"low"}]}`)
	if effort, ok := runEffortReport(t, mustParse(t, parser, batch), "toolu-a"); !ok || effort != "low" {
		t.Fatalf("run_effort = %v (present %v), want low", effort, ok)
	}
	if effort, ok := runEffortReport(t, mustParse(t, parser, agentTaskStarted("aid1", "toolu-a")), "toolu-a"); ok {
		t.Fatalf("task_started recorded run_effort %v again", effort)
	}
}

// Ownerless metadata is a forked skill's sidechain, which is no agent
// launch: nothing is recorded.
func TestOwnerlessMetadataRecordsNoEffort(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	batch := []byte(`{"type":"transcript_mirror","filePath":"/x/subagents/agent-fork1.jsonl","entries":[{"type":"agent_metadata","agentType":"general-purpose"},{"type":"assistant","uuid":"r1","agentId":"fork1","isSidechain":true,"message":{"id":"m","role":"assistant","model":"claude-haiku-4-5","content":[]},"effort":"low"}]}`)
	for _, event := range mustParse(t, parser, batch) {
		if effort, ok := runEffortReport(t, []provider.ProviderEvent{event}, event.ItemID); ok {
			t.Fatalf("ownerless sidechain recorded run_effort %v on %s", effort, event.ItemID)
		}
	}
}

// A §E6 resumed round's rows record the effort it runs at on its carrier.
func TestResumedRoundRecordsItsEffortOnTheCarrier(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, agentLaunchLine("launch-1", "", `{"prompt":"p"}`))
	mustParse(t, parser, agentTaskStarted("agent-1", "launch-1"))
	mustParse(t, parser, mirroredAgentRow("agent-1", "r1", `,"effort":"xhigh"`))
	mustParse(t, parser, []byte(sendMessageToolUse("carrier-2")))
	mustParse(t, parser, []byte(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"carrier-2","task_type":"local_agent","prompt":"keep going"}`))
	events := mustParse(t, parser, mirroredAgentRow("agent-1", "r2", `,"effort":"low"`))
	if effort, ok := runEffortReport(t, events, "carrier-2"); !ok || effort != "low" {
		t.Fatalf("carrier run_effort = %v (present %v), want low", effort, ok)
	}
	if effort, ok := runEffortReport(t, events, "launch-1"); ok {
		t.Fatalf("resumed round restamped the launch with %v", effort)
	}
}

// An API error row is the CLI's, not a request's.
func TestMirroredAPIErrorRowRecordsNoEffort(t *testing.T) {
	parser := newModelScopedParser("2.1.321")
	mustParse(t, parser, agentLaunchLine("toolu-a", "", `{"prompt":"p"}`))
	mustParse(t, parser, agentTaskStarted("aid1", "toolu-a"))
	batch := []byte(`{"type":"transcript_mirror","filePath":"/x/subagents/agent-aid1.jsonl","entries":[{"type":"assistant","uuid":"r1","agentId":"aid1","isSidechain":true,"isApiErrorMessage":true,"message":{"id":"m","role":"assistant","model":"<synthetic>","content":[]},"effort":"max"}]}`)
	if effort, ok := runEffortReport(t, mustParse(t, parser, batch), "toolu-a"); ok {
		t.Fatalf("API error row recorded %v", effort)
	}
}
