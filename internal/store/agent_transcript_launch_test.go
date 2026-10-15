package store

import "testing"

// A forked Skill is an agent: its local_agent task (2.1.284+) or the
// skillFork stamp its completion writes says so. An inline skill, which
// has neither, stays a plain tool.
func TestIsAgentTranscriptLaunchForkedSkill(t *testing.T) {
	for _, tc := range []struct {
		name string
		item Item
		want bool
	}{
		{"agent", Item{Kind: "tool_call", ToolName: "Agent", Meta: `{}`}, true},
		{"fork with task", Item{Kind: "tool_call", ToolName: "Skill", Meta: `{"task_id":"a1","task_type":"local_agent"}`}, true},
		{"fork with stamp", Item{Kind: "tool_call", ToolName: "Skill", Meta: `{"skillFork":{"agentId":"a1","commandName":"review"}}`}, true},
		{"null stamp", Item{Kind: "tool_call", ToolName: "Skill", Meta: `{"skillFork":null}`}, false},
		{"inline skill", Item{Kind: "tool_call", ToolName: "Skill", Meta: `{"input":{"skill":"review"}}`}, false},
		{"skill with a description", Item{Kind: "tool_call", ToolName: "Skill", Meta: `{"description":"/review"}`}, false},
		{"fork completion row", Item{Kind: "tool_completion", ToolName: "Skill", Meta: `{"task_type":"local_agent"}`}, false},
	} {
		if got := IsAgentTranscriptLaunch(tc.item); got != tc.want {
			t.Errorf("%s: IsAgentTranscriptLaunch = %v, want %v", tc.name, got, tc.want)
		}
	}
}
