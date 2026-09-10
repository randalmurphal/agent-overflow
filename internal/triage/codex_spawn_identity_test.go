package triage

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestCodexSpawnIdentityLandsOnSettledRowWithoutRuntimeState(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn", "child")
	before, _, err := st.GetThreadItem("t1", "spawn")
	if err != nil {
		t.Fatal(err)
	}
	update := func(meta string) {
		t.Helper()
		if err := r.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn", ItemType: "collab_agent",
			Meta: json.RawMessage(meta), Timestamp: time.Now().Add(time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A rollout replay update carries only runtime flags: nothing to persist.
	update(`{"meta_update_only":true,"toolName":"collab_agent","is_background":true,"live_background_active":true,"input":{"tool":"spawn_agent","receiverThreadIds":["child"]}}`)
	after, _, err := st.GetThreadItem("t1", "spawn")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("runtime-only update mutated spawn\nbefore: %+v\nafter: %+v", before, after)
	}

	// The profile read lands identity and nothing else.
	update(`{"meta_update_only":true,"toolName":"collab_agent","live_background_active":true,"input":{"tool":"spawn_agent","receiverThreadIds":["other-child"],"newAgentNickname":"Hypatia","newAgentRole":"default","model":"gpt-5.6-sol","reasoningEffort":"high","agentPath":"/root/reviewer","taskName":"/root/reviewer"}}`)
	after, _, err = st.GetThreadItem("t1", "spawn")
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Active *bool `json:"live_background_active"`
		Input  struct {
			ReceiverThreadIDs []string `json:"receiverThreadIds"`
			Nickname          string   `json:"newAgentNickname"`
			Role              string   `json:"newAgentRole"`
			Model             string   `json:"model"`
			Effort            string   `json:"reasoningEffort"`
			AgentPath         string   `json:"agentPath"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(after.Meta), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Input.Nickname != "Hypatia" || meta.Input.Role != "default" || meta.Input.Model != "gpt-5.6-sol" || meta.Input.Effort != "high" || meta.Input.AgentPath != "/root/reviewer" {
		t.Fatalf("identity missing from spawn: %s", after.Meta)
	}
	if !reflect.DeepEqual(meta.Input.ReceiverThreadIDs, []string{"child"}) {
		t.Fatalf("identity update replaced the receiver list: %s", after.Meta)
	}
	if meta.Active != nil && *meta.Active {
		t.Fatalf("runtime state landed on spawn: %s", after.Meta)
	}
	expected := before
	expected.Meta = after.Meta
	if !reflect.DeepEqual(expected, after) {
		t.Fatalf("identity changed more than meta\nbefore: %+v\nafter: %+v", before, after)
	}

	// Repeating the same identity is a no-op write.
	settled := after
	update(`{"meta_update_only":true,"toolName":"collab_agent","input":{"tool":"spawn_agent","newAgentNickname":"Hypatia","model":"gpt-5.6-sol","reasoningEffort":"high"}}`)
	again, _, err := st.GetThreadItem("t1", "spawn")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(settled, again) {
		t.Fatalf("repeated identity mutated spawn\nbefore: %+v\nafter: %+v", settled, again)
	}
}
