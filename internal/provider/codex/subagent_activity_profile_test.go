package codex

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// v2StartedWithProfile is a codex 0.162+ started activity: the child's
// resolved model and reasoning effort ride on the item.
func v2StartedWithProfile(threadID, itemID, childThreadID, agentPath, model, effort string) []byte {
	item := map[string]any{
		"id":            itemID,
		"type":          "subAgentActivity",
		"kind":          "started",
		"agentThreadId": childThreadID,
		"agentPath":     agentPath,
		"model":         model,
	}
	if effort != "" {
		item["reasoningEffort"] = effort
	}
	encoded, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "item/completed",
		"params":  map[string]any{"threadId": threadID, "turnId": "root-turn", "item": item},
	})
	return encoded
}

type spawnProfileInput struct {
	Input struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoningEffort"`
	} `json:"input"`
}

func TestStartedActivityProfileRidesOnTheSpawnRow(t *testing.T) {
	t.Parallel()
	params, _ := json.Marshal(map[string]any{
		"threadId": "root-provider-thread",
		"turnId":   "root-turn",
		"item": map[string]any{
			"id": "spawn-a", "type": "subAgentActivity", "kind": "started",
			"agentThreadId": "child-a", "agentPath": "/root/reviewer",
			"model": "gpt-5.6-sol", "reasoningEffort": "xhigh",
		},
	})
	events := classifySubAgentActivityCompleted("ao-thread", params, time.Unix(0, 0).UTC())
	if len(events) != 2 {
		t.Fatalf("events = %+v, want spawn start + completion", events)
	}
	for _, event := range events {
		var meta spawnProfileInput
		if err := json.Unmarshal(event.Meta, &meta); err != nil {
			t.Fatalf("decode meta: %v", err)
		}
		if meta.Input.Model != "gpt-5.6-sol" || meta.Input.ReasoningEffort != "xhigh" {
			t.Fatalf("%s meta profile = %+v, want gpt-5.6-sol/xhigh", event.Kind, meta.Input)
		}
	}
}

func TestNonStartedActivityCarriesNoProfile(t *testing.T) {
	t.Parallel()
	item := map[string]json.RawMessage{}
	for key, value := range map[string]string{
		"id": "msg-1", "type": "subAgentActivity", "kind": "interacted",
		"agentThreadId": "child-a", "agentPath": "/root/reviewer", "model": "gpt-5.6-sol",
	} {
		item[key], _ = json.Marshal(value)
	}
	activity, ok := decodeSubAgentActivityItem(item)
	if !ok || activity.Model != "" {
		t.Fatalf("interacted activity = %+v, want no profile", activity)
	}
}

// A started activity that carries the profile records it, so the child gets
// no thread/resume round trip; an older one without it still gets the read.
func TestStartedActivityProfileReplacesTheChildResumeRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		line     []byte
		wantRead bool
	}{
		{"codex 0.162 activity", v2StartedWithProfile("root-provider-thread", "spawn-a", "child-a", "/root/reviewer", "gpt-5.6-sol", "high"), false},
		{"older activity", v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/reviewer"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newMultiAgentV2RoutingSession(t, func(provider.ProviderEvent) {})
			ctx, cancel := context.WithCancel(context.Background())
			proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "sh", Args: []string{"-c", "cat > /dev/null"}})
			if err != nil {
				t.Fatalf("spawn: %v", err)
			}
			s.proc = proc
			s.ctx = ctx
			s.cancel = cancel
			s.pending = make(map[int64]chan json.RawMessage)
			s.collabMetadataReads = make(chan struct{}, 1)
			t.Cleanup(func() {
				cancel()
				_ = proc.Close()
			})

			s.dispatchLine(tc.line)

			s.mu.Lock()
			_, reading := s.collab.profileReadsByThread["child-a"]
			stored := s.collab.agentMetaByThread["child-a"]
			s.mu.Unlock()
			if reading != tc.wantRead {
				t.Fatalf("child profile read scheduled = %v, want %v", reading, tc.wantRead)
			}
			if !tc.wantRead && (!stored.ProfileKnown || stored.Model != "gpt-5.6-sol" || stored.ReasoningEffort != "high") {
				t.Fatalf("stored profile = %+v, want the activity's gpt-5.6-sol/high", stored)
			}
		})
	}
}

func TestResumedV2SpawnRowWithModelNeedsNoProfileRead(t *testing.T) {
	t.Parallel()
	v2Meta, _ := json.Marshal(map[string]any{"input": map[string]any{
		"tool": "spawn_agent", "activityKind": "started", "model": "gpt-5.6-sol", "reasoningEffort": "low",
		"agentPath": "/root/reviewer", "receiverThreadIds": []string{"child-a"},
	}})
	v1Meta, _ := json.Marshal(map[string]any{"input": map[string]any{
		"tool": "spawn_agent", "model": "gpt-5.6-requested", "receiverThreadIds": []string{"child-b"},
	}})
	ownerships, err := collabResumeOwnerships([]ResumeCollabLaunch{
		{ItemID: "spawn-a", Meta: v2Meta},
		{ItemID: "spawn-b", Meta: v1Meta},
	})
	if err != nil || len(ownerships) != 2 {
		t.Fatalf("ownerships = %+v, err %v", ownerships, err)
	}
	if !ownerships[0].ProfileKnown {
		t.Fatal("V2 row with its recorded model must not ask the child again")
	}
	// A V1 row's model is the requested one, never the effective profile.
	if ownerships[1].ProfileKnown {
		t.Fatal("V1 row's requested model was taken as the effective profile")
	}
}
