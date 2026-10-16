package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestRedeliveredAgentLaunchKeepsItsLaunchTimeInheritedEffort(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	start := func(inherited string) {
		t.Helper()
		meta, _ := json.Marshal(map[string]any{
			"toolName":         "Agent",
			"input":            map[string]any{"description": "look", "prompt": "p"},
			"inherited_effort": inherited,
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agent-1",
			ItemType: "Agent", Meta: meta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("tool start: %v", err)
		}
	}
	start("high")
	// A later process (or a mirror re-projection) delivers the same launch
	// after the session's effort moved.
	start("low")

	row, found, err := st.GetThreadItem("t1", "agent-1")
	if err != nil || !found {
		t.Fatalf("launch row: found=%v err=%v", found, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(row.Meta), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta["inherited_effort"] != "high" {
		t.Fatalf("inherited_effort = %v, want the launch-time high", meta["inherited_effort"])
	}
}

func TestAgentLaunchMetaKeepsTheRunInputs(t *testing.T) {
	want := map[string]string{"model": `"haiku"`, "effort": `"medium"`, "run_in_background": `false`}
	for _, tool := range []string{"Agent", "Task"} {
		raw, _ := json.Marshal(map[string]any{
			"toolName": tool,
			"input": map[string]any{
				"description": "look", "prompt": "p",
				"model": "haiku", "effort": "medium", "run_in_background": false,
			},
		})
		trimmed, _, err := applyToolMetaRule(tool, raw, 0)
		if err != nil {
			t.Fatalf("%s: applyToolMetaRule: %v", tool, err)
		}
		input := readNestedInput(t, trimmed)
		for key, value := range want {
			if got := string(input[key]); got != value {
				t.Errorf("%s: persisted input %s = %s, want %s", tool, key, got, value)
			}
		}
	}
}

func TestKeepLaunchInheritedEffortLeavesUnstampedRowsAlone(t *testing.T) {
	merged := `{"toolName":"Agent","inherited_effort":"max"}`
	if got := keepLaunchInheritedEffort(`{"toolName":"Agent"}`, merged); got != merged {
		t.Fatalf("unstamped existing row rewrote the merge: %s", got)
	}
	if got := keepLaunchInheritedEffort(`{"inherited_effort":"max"}`, merged); got != merged {
		t.Fatalf("unchanged stamp re-encoded the merge: %s", got)
	}
}

func TestLaunchRunEffort(t *testing.T) {
	stamped := `{"inherited_effort":"medium"}`
	cases := []struct {
		name  string
		meta  string
		input launchIdentity
		want  string
	}{
		{"input effort", stamped, launchIdentity{Effort: "max"}, "max"},
		{"inherited when the input names none", stamped, launchIdentity{}, "medium"},
		{"a fork ignores its input effort", stamped, launchIdentity{Effort: "max", SubagentType: "fork"}, "medium"},
		{"unknown", `{}`, launchIdentity{}, ""},
	}
	for _, tc := range cases {
		if got := launchRunEffort(tc.meta, tc.input); got != tc.want {
			t.Errorf("%s: launchRunEffort = %q, want %q", tc.name, got, tc.want)
		}
	}
}
