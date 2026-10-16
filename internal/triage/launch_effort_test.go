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

// A launch row stored without a stamp (before stamps existed, or with
// the effort unknown at launch) must not gain one when its tool start is
// delivered again: the replay would stamp the effort of the later moment.
func TestReplayedStartDoesNotStampAnUnstampedLaunch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	start := func(fields map[string]any) {
		t.Helper()
		meta := map[string]any{
			"toolName": "Agent",
			"input":    map[string]any{"description": "look", "prompt": "p", "model": "haiku"},
		}
		for key, value := range fields {
			meta[key] = value
		}
		encoded, _ := json.Marshal(meta)
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agent-1",
			ItemType: "Agent", Meta: encoded, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("tool start: %v", err)
		}
	}
	start(nil)
	start(map[string]any{"inherited_effort": "xhigh", "requested_model": "claude-haiku-4-5"})

	row, found, err := st.GetThreadItem("t1", "agent-1")
	if err != nil || !found {
		t.Fatalf("launch row: found=%v err=%v", found, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(row.Meta), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	for _, key := range []string{"inherited_effort", "requested_model"} {
		if v, ok := meta[key]; ok {
			t.Fatalf("replay stamped %s = %v on a row stored without it", key, v)
		}
	}
}

// A resume carrier gets its stamp on a meta-only update, after its row
// exists: the first one lands, a later one does not replace it.
func TestCarrierKeepsItsFirstResumeStamp(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta, _ := json.Marshal(map[string]any{"toolName": "SendMessage", "input": map[string]any{"to": "a1"}})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "carrier-1",
		ItemType: "SendMessage", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier start: %v", err)
	}
	for _, effort := range []string{"low", "max"} {
		rebind, _ := json.Marshal(map[string]any{"task_id": "a1", "resumes_tool_use_id": "agent-1", "inherited_effort": effort})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "carrier-1",
			Meta: rebind, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("rebind %s: %v", effort, err)
		}
	}
	row, found, err := st.GetThreadItem("t1", "carrier-1")
	if err != nil || !found {
		t.Fatalf("carrier row: found=%v err=%v", found, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(row.Meta), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta["inherited_effort"] != "low" {
		t.Fatalf("carrier inherited_effort = %v, want its first stamp low", meta["inherited_effort"])
	}
}

func TestSettleLaunchStampsLeavesUnchangedMergesAlone(t *testing.T) {
	merged := `{"toolName":"Agent","inherited_effort":"max"}`
	if got := keepLaunchStamps(`{"inherited_effort":"max"}`, merged); got != merged {
		t.Fatalf("unchanged stamp re-encoded the merge: %s", got)
	}
	plain := `{"toolName":"Agent"}`
	if got := keepLaunchStamps(plain, plain); got != plain {
		t.Fatalf("unstamped merge re-encoded: %s", got)
	}
	if got := keepFirstLaunchStamps(plain, merged); got != merged {
		t.Fatalf("a meta-only first stamp was refused: %s", got)
	}
}

// An effort and the higher tiers it may stand for are one launch-time fact:
// a later delivery neither drops the alternatives from a stamped row nor
// pairs the row's effort with alternatives of another moment.
func TestEffortAlternativesSettleWithTheirEffort(t *testing.T) {
	stamped := `{"toolName":"Agent","inherited_effort":"high","inherited_effort_alternatives":["xhigh","max"]}`
	for name, tc := range map[string]struct {
		settle           func(existing, merged string) string
		existing, merged string
		want             string
	}{
		"redelivery keeps them": {
			keepLaunchStamps, stamped, `{"toolName":"Agent","inherited_effort":"high"}`,
			`["xhigh","max"]`,
		},
		"redelivery does not add them": {
			keepLaunchStamps, `{"toolName":"Agent","inherited_effort":"high"}`, stamped,
			"",
		},
		"a meta-only restamp does not add them to a first stamp": {
			keepFirstLaunchStamps, `{"toolName":"SendMessage","inherited_effort":"high"}`,
			`{"toolName":"SendMessage","inherited_effort":"high","inherited_effort_alternatives":["max"]}`,
			"",
		},
		"a first meta-only stamp carries them": {
			keepFirstLaunchStamps, `{"toolName":"SendMessage"}`,
			`{"toolName":"SendMessage","inherited_effort":"high","inherited_effort_alternatives":["max"]}`,
			`["max"]`,
		},
	} {
		var meta map[string]json.RawMessage
		if err := json.Unmarshal([]byte(tc.settle(tc.existing, tc.merged)), &meta); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if got := string(meta["inherited_effort_alternatives"]); got != tc.want {
			t.Errorf("%s: alternatives = %s, want %s", name, got, tc.want)
		}
	}
}

func TestLaunchNamedEffort(t *testing.T) {
	cases := []struct {
		name  string
		input launchIdentity
		want  string
	}{
		{"named", launchIdentity{Effort: "max"}, "max"},
		{"none named", launchIdentity{}, ""},
		{"a fork ignores its input effort", launchIdentity{Effort: "max", SubagentType: "fork"}, ""},
		{"relative passes through", launchIdentity{Effort: "higher"}, "higher"},
	}
	for _, tc := range cases {
		if got := launchNamedEffort(tc.input); got != tc.want {
			t.Errorf("%s: launchNamedEffort = %q, want %q", tc.name, got, tc.want)
		}
	}
}
