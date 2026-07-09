package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func codexLiveUpdateBaseOptions() provider.SessionOptions {
	return provider.SessionOptions{
		Provider:        string(provider.Codex),
		Model:           "gpt-5.5-codex",
		WorkDir:         "/tmp/work",
		ReasoningEffort: provider.EffortHigh,
		Mode:            provider.ModeChat,
		RuntimeMode:     provider.RuntimeApprovalRequired,
		SystemPrompt:    "base prompt",
		Resume:          "thread-ref-1",
	}
}

func TestCodexPlanLiveUpdate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*provider.SessionOptions)
		wantOK bool
		check  func(t *testing.T, u LiveUpdate)
	}{
		{
			name:   "identical options are live",
			mutate: func(*provider.SessionOptions) {},
			wantOK: true,
		},
		{
			name:   "model change is live",
			mutate: func(o *provider.SessionOptions) { o.Model = "gpt-5.6-codex" },
			wantOK: true,
			check: func(t *testing.T, u LiveUpdate) {
				if u.Model != "gpt-5.6-codex" {
					t.Fatalf("update.Model = %q, want gpt-5.6-codex", u.Model)
				}
			},
		},
		{
			name:   "effort change is live",
			mutate: func(o *provider.SessionOptions) { o.ReasoningEffort = provider.EffortLow },
			wantOK: true,
			check: func(t *testing.T, u LiveUpdate) {
				if u.ReasoningEffort != "low" {
					t.Fatalf("update.ReasoningEffort = %q, want low", u.ReasoningEffort)
				}
			},
		},
		{
			name:   "fast mode change is live",
			mutate: func(o *provider.SessionOptions) { o.FastMode = true },
			wantOK: true,
			check: func(t *testing.T, u LiveUpdate) {
				if u.ServiceTier != fastServiceTier {
					t.Fatalf("update.ServiceTier = %q, want %q", u.ServiceTier, fastServiceTier)
				}
			},
		},
		{
			name:   "runtime mode change is live",
			mutate: func(o *provider.SessionOptions) { o.RuntimeMode = provider.RuntimeFullAccess },
			wantOK: true,
			check: func(t *testing.T, u LiveUpdate) {
				if u.ApprovalPolicy != "never" || u.Sandbox != "danger-full-access" {
					t.Fatalf("update approval/sandbox = %q/%q, want never/danger-full-access", u.ApprovalPolicy, u.Sandbox)
				}
			},
		},
		{
			name:   "resume cursor drift is lifecycle, not config",
			mutate: func(o *provider.SessionOptions) { o.Resume = "thread-ref-2" },
			wantOK: true,
		},
		{
			name:   "context window change needs restart",
			mutate: func(o *provider.SessionOptions) { o.ContextWindow = 200000 },
			wantOK: false,
		},
		{
			name: "autocompact change needs restart",
			mutate: func(o *provider.SessionOptions) {
				o.AutoCompactPercent = 80
			},
			wantOK: false,
		},
		{
			name:   "system prompt change needs restart",
			mutate: func(o *provider.SessionOptions) { o.SystemPrompt = "different prompt" },
			wantOK: false,
		},
		{
			name:   "workdir change needs restart",
			mutate: func(o *provider.SessionOptions) { o.WorkDir = "/tmp/elsewhere" },
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := codexLiveUpdateBaseOptions()
			next := codexLiveUpdateBaseOptions()
			tt.mutate(&next)

			update, ok := PlanLiveUpdate(prev, next)
			if ok != tt.wantOK {
				t.Fatalf("PlanLiveUpdate ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				if (update != LiveUpdate{}) {
					t.Fatalf("PlanLiveUpdate update = %+v, want zero when restart is required", update)
				}
				return
			}
			if tt.check != nil {
				tt.check(t, update)
			}
		})
	}
}

// TestApplyLiveUpdateAppliesOnNextTurnStart pins the end-to-end behavior a
// mid-session config change depends on: ApplyLiveUpdate swaps the session's
// turn config and the very next Send carries the new model / effort /
// serviceTier / approvalPolicy / sandboxPolicy as turn/start overrides —
// no restart, same thread.
func TestApplyLiveUpdateAppliesOnNextTurnStart(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"turn/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-1\"}}}"
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread\"}}}"
    fi
done
`, capturePath)
	scriptPath := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	s, err := NewSession(context.Background(), testThread, Config{
		Binary:          scriptPath,
		Model:           "gpt-5.5-codex",
		WorkDir:         "/tmp",
		ReasoningEffort: "high",
		ApprovalPolicy:  "untrusted",
		Sandbox:         "read-only",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	s.ApplyLiveUpdate(LiveUpdate{
		Model:           "gpt-5.6-codex",
		ReasoningEffort: "low",
		ServiceTier:     fastServiceTier,
		ApprovalPolicy:  "never",
		Sandbox:         "danger-full-access",
	})

	if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_ = s.Close()

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	var turnStart map[string]any
	for _, line := range strings.Split(string(captured), "\n") {
		if !strings.Contains(line, `"method":"turn/start"`) {
			continue
		}
		if err := json.Unmarshal([]byte(line), &turnStart); err != nil {
			t.Fatalf("unmarshal turn/start: %v", err)
		}
		break
	}
	if turnStart == nil {
		t.Fatalf("captured no turn/start request: %s", string(captured))
	}

	params := turnStart["params"].(map[string]any)
	if params["model"] != "gpt-5.6-codex" {
		t.Fatalf("model = %v, want gpt-5.6-codex", params["model"])
	}
	if params["effort"] != "low" {
		t.Fatalf("effort = %v, want low", params["effort"])
	}
	if params["serviceTier"] != fastServiceTier {
		t.Fatalf("serviceTier = %v, want %q", params["serviceTier"], fastServiceTier)
	}
	if params["approvalPolicy"] != "never" {
		t.Fatalf("approvalPolicy = %v, want never", params["approvalPolicy"])
	}
	sandboxPolicy, ok := params["sandboxPolicy"].(map[string]any)
	if !ok || sandboxPolicy["type"] != "dangerFullAccess" {
		t.Fatalf("sandboxPolicy = %v, want dangerFullAccess", params["sandboxPolicy"])
	}
	// The collaboration-mode settings bag must reflect the new config too —
	// it carries model + reasoning_effort on every turn.
	collab := params["collaborationMode"].(map[string]any)
	settings := collab["settings"].(map[string]any)
	if settings["model"] != "gpt-5.6-codex" || settings["reasoning_effort"] != "low" {
		t.Fatalf("collaborationMode settings = %v, want new model/effort", settings)
	}

	if got := s.currentModel(); got != "gpt-5.6-codex" {
		t.Fatalf("currentModel = %q, want gpt-5.6-codex", got)
	}
}
