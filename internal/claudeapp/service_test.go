package claudeapp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

func TestContextUsageDistinguishesMissingAndOtherProvider(t *testing.T) {
	service := New(Deps{Session: func(threadID string) (*claude.Session, bool) {
		return nil, threadID == "other-provider"
	}})
	missing, err := service.GetContextUsage("missing")
	if err != nil || missing.Usage != nil || !strings.Contains(missing.Reason, "running Claude session") {
		t.Fatalf("missing result = %+v, err=%v", missing, err)
	}
	mismatch, err := service.GetContextUsage("other-provider")
	if err != nil || mismatch.Usage != nil || !strings.Contains(mismatch.Reason, "only available on Claude") {
		t.Fatalf("mismatch result = %+v, err=%v", mismatch, err)
	}
}

func TestLiveSessionControlsAndContextRoundTrip(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "fake-claude")
	script := `#!/bin/sh
set -u
while IFS= read -r line; do
    reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
    case "$line" in
        *'"get_context_usage"'*)
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"totalTokens":24028,"maxTokens":1000000,"percentage":2,"model":"claude-fable-5","categories":[{"name":"System prompt","tokens":4027},{"name":"Deferred","tokens":100,"isDeferred":true}]}}}\n' "$reqid"
            ;;
        *'"stop_task"'*)
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
        *'"background_tasks"'*)
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"backgrounded":true}}}\n' "$reqid"
            ;;
    esac
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake CLI: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	session, err := claude.NewSession(ctx, "thread", claude.Config{Binary: scriptPath}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	service := New(Deps{Session: func(string) (*claude.Session, bool) { return session, true }})

	usage, err := service.GetContextUsage("thread")
	if err != nil {
		t.Fatalf("GetContextUsage: %v", err)
	}
	if usage.Usage == nil || usage.Usage.TotalTokens != 24028 || len(usage.Usage.Categories) != 2 || !usage.Usage.Categories[1].Deferred {
		t.Fatalf("usage = %+v", usage)
	}
	if err := service.StopTask("thread", "task-1"); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	if err := service.BackgroundTask("thread", "tool-1"); err != nil {
		t.Fatalf("BackgroundTask: %v", err)
	}
}

func TestTaskControlsReportMissingAndProviderMismatch(t *testing.T) {
	service := New(Deps{Session: func(threadID string) (*claude.Session, bool) {
		return nil, threadID == "other-provider"
	}})
	if err := service.StopTask("missing", "task"); err == nil || !strings.Contains(err.Error(), "no active session") {
		t.Fatalf("missing StopTask error = %v", err)
	}
	if err := service.BackgroundTask("other-provider", "tool"); err == nil || !strings.Contains(err.Error(), "not a Claude thread") {
		t.Fatalf("mismatch BackgroundTask error = %v", err)
	}
}

func TestSkillsRejectBlankAndRelativeWorkspacePaths(t *testing.T) {
	service := New(Deps{})
	for _, path := range []string{"", "relative/path"} {
		if _, err := service.Skills(path); err == nil {
			t.Fatalf("Skills(%q) succeeded", path)
		}
	}
}
