package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestReadChildThreadProfileUsesMetadataOnlyResume(t *testing.T) {
	capturePath := t.TempDir() + "/request.json"
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			IFS= read -r line || exit 1
			printf '%s\n' "$line" > "$CAPTURE_PATH"
			id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
			printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"child-1","agentNickname":"Euler","agentRole":"reviewer"},"model":"gpt-5.6-luna","reasoningEffort":"low"}}\n' "$id"
		`},
		Env: map[string]string{"CAPTURE_PATH": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	s := &Session{
		proc:    proc,
		ctx:     ctx,
		pending: make(map[int64]chan json.RawMessage),
		cancel:  cancel,
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		_ = proc.Close()
	})

	meta, err := s.readChildThreadProfileOnce(ctx, "child-1")
	if err != nil {
		t.Fatalf("read child profile: %v", err)
	}
	if meta.ThreadID != "child-1" || meta.AgentNickname != "Euler" || meta.AgentRole != "reviewer" ||
		meta.Model != "gpt-5.6-luna" || meta.ReasoningEffort != "low" || !meta.ProfileKnown {
		t.Fatalf("profile = %+v", meta)
	}

	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured request: %v", err)
	}
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
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "cat > /dev/null; sleep 60"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	events := make(chan provider.ProviderEvent, 1)
	s := &Session{
		proc:                proc,
		ctx:                 ctx,
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		collabMetadataReads: make(chan struct{}, 1),
		onEvent: func(event provider.ProviderEvent) {
			events <- event
		},
		cancel: cancel,
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-1": "spawn-1"},
			agentMetaByThread:   make(map[string]collabReceiverMeta),
		},
	}
	t.Cleanup(func() {
		cancel()
		_ = proc.Close()
	})

	s.scheduleCollabProfileRead("child-1", "spawn-1", collabLaunchMeta{
		AgentPath:         "/root/reviewer",
		ReceiverThreadIDs: []string{"child-1"},
	})
	pending, rpcID := waitForPending(t, s, 3*time.Second)
	pending <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"thread":{"id":"child-1"},"model":"gpt-5.6-luna","reasoningEffort":"xhigh"}}`, rpcID))

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
