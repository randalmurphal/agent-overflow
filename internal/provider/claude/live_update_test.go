package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func liveUpdateBaseOptions() provider.SessionOptions {
	return provider.SessionOptions{
		Provider:        string(provider.Claude),
		Model:           "claude-sonnet-5",
		WorkDir:         "/tmp/work",
		ReasoningEffort: provider.EffortHigh,
		ContextWindow:   200000,
		Mode:            provider.ModeChat,
		RuntimeMode:     provider.RuntimeApprovalRequired,
		SystemPrompt:    "base prompt",
		Resume:          "session-ref-1",
	}
}

func TestPlanLiveUpdate(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*provider.SessionOptions)
		wantOK     bool
		wantUpdate LiveUpdate
	}{
		{
			name:   "identical options need nothing",
			mutate: func(*provider.SessionOptions) {},
			wantOK: true,
		},
		{
			name:       "model change is live",
			mutate:     func(o *provider.SessionOptions) { o.Model = "claude-fable-5" },
			wantOK:     true,
			wantUpdate: LiveUpdate{Model: "claude-fable-5"},
		},
		{
			name:       "runtime mode change is live",
			mutate:     func(o *provider.SessionOptions) { o.RuntimeMode = provider.RuntimeAutoAcceptEdits },
			wantOK:     true,
			wantUpdate: LiveUpdate{BasePermissionMode: "acceptEdits"},
		},
		{
			name: "model and runtime mode together are live",
			mutate: func(o *provider.SessionOptions) {
				o.Model = "claude-fable-5"
				o.RuntimeMode = provider.RuntimeFullAccess
			},
			wantOK:     true,
			wantUpdate: LiveUpdate{Model: "claude-fable-5", BasePermissionMode: "bypassPermissions"},
		},
		{
			name:   "resume cursor drift is lifecycle, not config",
			mutate: func(o *provider.SessionOptions) { o.Resume = "session-ref-2" },
			wantOK: true,
		},
		{
			name:   "effort change needs restart",
			mutate: func(o *provider.SessionOptions) { o.ReasoningEffort = provider.EffortLow },
			wantOK: false,
		},
		{
			name: "fast mode change needs restart",
			mutate: func(o *provider.SessionOptions) {
				// claude-opus-4-8 supports fast mode, so the toggle changes
				// the effective launch config (a non-capable model would
				// coerce to off on both sides and read as no change).
				o.FastMode = true
			},
			wantOK: false,
		},
		{
			name:   "context window change needs restart",
			mutate: func(o *provider.SessionOptions) { o.ContextWindow = provider.ClaudeExtendedContextWindow },
			wantOK: false,
		},
		{
			name:   "system prompt change needs restart",
			mutate: func(o *provider.SessionOptions) { o.SystemPrompt = "different prompt" },
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := liveUpdateBaseOptions()
			next := liveUpdateBaseOptions()
			if tt.name == "fast mode change needs restart" {
				prev.Model = "claude-opus-4-8"
				next.Model = "claude-opus-4-8"
			}
			tt.mutate(&next)

			update, ok := PlanLiveUpdate(prev, next)
			if ok != tt.wantOK {
				t.Fatalf("PlanLiveUpdate ok = %v, want %v", ok, tt.wantOK)
			}
			if update != tt.wantUpdate {
				t.Fatalf("PlanLiveUpdate update = %+v, want %+v", update, tt.wantUpdate)
			}
		})
	}
}

// liveUpdateTestSession spawns a fake-CLI session that acks every
// control_request with success and captures stdin lines to a file.
func liveUpdateTestSession(t *testing.T, cfg Config) (*Session, string) {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "fake-claude")
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	script := `#!/bin/sh
set -eu
capture="${CAPTURE_FILE:?}"
while IFS= read -r line; do
    printf '%s\n' "$line" >> "$capture"
    case "$line" in
        *'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}

	cfg.Binary = scriptPath
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	cfg.Env["CAPTURE_FILE"] = capturePath

	s, err := NewSession(context.Background(), testThread, cfg, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.controlRequestTimeout = 2 * time.Second
	return s, capturePath
}

func TestApplyLiveUpdateSendsSetModel(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.ApplyLiveUpdate(ctx, LiveUpdate{Model: "claude-fable-5"}); err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var captured struct {
		Type    string `json:"type"`
		Request struct {
			Subtype string `json:"subtype"`
			Model   string `json:"model"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &captured); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if captured.Type != "control_request" || captured.Request.Subtype != "set_model" || captured.Request.Model != "claude-fable-5" {
		t.Fatalf("captured line = %+v, want set_model claude-fable-5", captured)
	}
}

func TestApplyLiveUpdateAppliesBasePermissionMode(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.ApplyLiveUpdate(ctx, LiveUpdate{BasePermissionMode: "acceptEdits"}); err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var captured struct {
		Type    string `json:"type"`
		Request struct {
			Subtype string `json:"subtype"`
			Mode    string `json:"mode"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &captured); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if captured.Type != "control_request" || captured.Request.Subtype != "set_permission_mode" || captured.Request.Mode != "acceptEdits" {
		t.Fatalf("captured line = %+v, want set_permission_mode acceptEdits", captured)
	}
	if got := s.getCurrentPermissionMode(); got != "acceptEdits" {
		t.Fatalf("currentPermissionMode = %q, want acceptEdits", got)
	}
	// The new base survives a later plan-turn restore cycle.
	if got := s.desiredPermissionModeForTurn(provider.ModeChat); got != "acceptEdits" {
		t.Fatalf("desiredPermissionModeForTurn(chat) = %q, want acceptEdits", got)
	}
}

// TestApplyLiveUpdateBypassEscalationRequiresRestart pins the CLI
// constraint verified on 2.1.205: a session spawned without
// --allow-dangerously-skip-permissions cannot escalate to
// bypassPermissions via set_permission_mode. ApplyLiveUpdate must signal
// the restart fallback before any side effect (no wire write, model
// untouched).
func TestApplyLiveUpdateBypassEscalationRequiresRestart(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.ApplyLiveUpdate(ctx, LiveUpdate{Model: "claude-fable-5", BasePermissionMode: "bypassPermissions"})
	if !errors.Is(err, ErrLiveUpdateRequiresRestart) {
		t.Fatalf("ApplyLiveUpdate error = %v, want ErrLiveUpdateRequiresRestart", err)
	}

	// No control_request may have hit stdin — the model half of the update
	// must not half-apply when the permission half needs a restart.
	time.Sleep(100 * time.Millisecond)
	if data, err := os.ReadFile(capturePath); err == nil && len(data) > 0 {
		t.Fatalf("expected no stdin writes, captured: %s", data)
	}
}

func TestApplyLiveUpdateBypassAllowedWhenSpawnedWithAllowFlag(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{
		BasePermissionMode: "default",
		PermissionFlags:    []string{"--permission-mode", "default", "--allow-dangerously-skip-permissions"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.ApplyLiveUpdate(ctx, LiveUpdate{BasePermissionMode: "bypassPermissions"}); err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var captured struct {
		Request struct {
			Subtype string `json:"subtype"`
			Mode    string `json:"mode"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &captured); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if captured.Request.Subtype != "set_permission_mode" || captured.Request.Mode != "bypassPermissions" {
		t.Fatalf("captured line = %+v, want set_permission_mode bypassPermissions", captured)
	}
}
