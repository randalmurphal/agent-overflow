package codex

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestReadChildThreadProfileUsesMetadataOnlyResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newConnFixture(t)
	s := fx.session("parent-thread", "parent-provider-thread", nil)
	fx.answer("thread/resume", `{"thread":{"id":"child-1","agentNickname":"Euler","agentRole":"reviewer"},"model":"gpt-5.6-luna","reasoningEffort":"low"}`)

	meta, err := s.readChildThreadProfileOnce(ctx, "child-1")
	if err != nil {
		t.Fatalf("read child profile: %v", err)
	}
	if meta.ThreadID != "child-1" || meta.AgentNickname != "Euler" || meta.AgentRole != "reviewer" ||
		meta.Model != "gpt-5.6-luna" || meta.ReasoningEffort != "low" || !meta.ProfileKnown {
		t.Fatalf("profile = %+v", meta)
	}

	data := fx.written(1)[0]
	var frame struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("decode captured request: %v", err)
	}
	if frame.Method != "thread/resume" {
		t.Fatalf("method = %q, want thread/resume", frame.Method)
	}
	wantParams := map[string]any{"threadId": "child-1", "excludeTurns": true}
	if !reflect.DeepEqual(frame.Params, wantParams) {
		t.Fatalf("params = %#v, want %#v", frame.Params, wantParams)
	}
}

func TestScheduledChildProfileReplacesUnknownSpawnProfile(t *testing.T) {
	t.Parallel()
	events := make(chan provider.ProviderEvent, 1)
	fx := newConnFixture(t)
	s := fx.session("parent-thread", "parent-provider-thread", func(event provider.ProviderEvent) {
		events <- event
	})
	s.collab.childParentByThread["child-1"] = "spawn-1"

	s.scheduleCollabProfileRead("child-1", "spawn-1", collabLaunchMeta{
		AgentPath:         "/root/reviewer",
		ReceiverThreadIDs: []string{"child-1"},
	})
	fx.answerNext(`{"thread":{"id":"child-1"},"model":"gpt-5.6-luna","reasoningEffort":"xhigh"}`)

	var event provider.ProviderEvent
	select {
	case event = <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for profile update")
	}
	if event.Kind != provider.EventToolStart || event.ItemID != "spawn-1" {
		t.Fatalf("event = %+v", event)
	}
	var update struct {
		MetaUpdateOnly bool `json:"meta_update_only"`
		Input          struct {
			Model             string   `json:"model"`
			ReasoningEffort   string   `json:"reasoningEffort"`
			AgentPath         string   `json:"agentPath"`
			ReceiverThreadIDs []string `json:"receiverThreadIds"`
		} `json:"input"`
	}
	if err := json.Unmarshal(event.Meta, &update); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if !update.MetaUpdateOnly || update.Input.Model != "gpt-5.6-luna" || update.Input.ReasoningEffort != "xhigh" ||
		update.Input.AgentPath != "/root/reviewer" || !reflect.DeepEqual(update.Input.ReceiverThreadIDs, []string{"child-1"}) {
		t.Fatalf("update = %+v", update)
	}
	stored := s.collab.agentMetaByThread["child-1"]
	if stored.Model != "gpt-5.6-luna" || stored.ReasoningEffort != "xhigh" || !stored.ProfileKnown {
		t.Fatalf("stored profile = %+v", stored)
	}
}

func TestRememberCollabReceiverMetaClearsOptionalEffortFromAuthoritativeProfile(t *testing.T) {
	t.Parallel()
	s := &Session{collab: sessionCollabState{agentMetaByThread: map[string]collabReceiverMeta{
		"child-1": {
			ThreadID:        "child-1",
			Model:           "old-model",
			ReasoningEffort: "high",
			ProfileKnown:    true,
		},
	}}}

	s.rememberCollabReceiverMeta(collabReceiverMeta{
		ThreadID:     "child-1",
		Model:        "gpt-5.6-luna",
		ProfileKnown: true,
	})

	got := s.collab.agentMetaByThread["child-1"]
	if got.Model != "gpt-5.6-luna" || got.ReasoningEffort != "" || !got.ProfileKnown {
		t.Fatalf("profile transition = %+v", got)
	}
}

func TestCollabProfileReadCoalescesAndStopsAfterProfileIsKnown(t *testing.T) {
	t.Parallel()
	s := &Session{collab: sessionCollabState{
		agentMetaByThread: make(map[string]collabReceiverMeta),
	}}
	if !s.beginCollabProfileRead("child-1") {
		t.Fatal("first profile read was not admitted")
	}
	if s.beginCollabProfileRead("child-1") {
		t.Fatal("duplicate in-flight profile read was admitted")
	}
	s.finishCollabProfileRead("child-1")
	if !s.beginCollabProfileRead("child-1") {
		t.Fatal("profile read did not become retryable after completion")
	}
	s.finishCollabProfileRead("child-1")
	s.rememberCollabReceiverMeta(collabReceiverMeta{
		ThreadID:     "child-1",
		Model:        "gpt-5.6-luna",
		ProfileKnown: true,
	})
	if s.beginCollabProfileRead("child-1") {
		t.Fatal("known profile admitted another read")
	}
}
