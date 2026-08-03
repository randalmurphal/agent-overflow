package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// A thread with no live session gets a typed "not available" answer, NOT an
// error and NOT a zeroed breakdown. The distinction is the whole point: the
// UI renders Reason instead of an empty chart.
func TestGetThreadContextUsage_NoSessionIsAnAnswer(t *testing.T) {
	a := NewApp()

	usage, err := a.GetThreadContextUsage("no-such-thread")
	if err != nil {
		t.Fatalf("missing session must not be an error: %v", err)
	}
	if usage.Available {
		t.Fatal("Available should be false with no session")
	}
	if usage.Reason == "" {
		t.Error("an unavailable answer must explain itself")
	}
	if usage.TotalTokens != 0 || usage.MaxTokens != 0 || len(usage.Categories) != 0 {
		t.Errorf("unavailable answer must carry no numbers, got %+v", usage)
	}
}

// A Codex thread is a distinct unavailable reason: the frontend branches on
// provider before offering the affordance, but the binding must still answer
// honestly if it is reached.
func TestGetThreadContextUsage_CodexThreadIsAnAnswer(t *testing.T) {
	a := NewApp()
	a.mu.Lock()
	a.sessions["codex-thread"] = session{provider: "codex"}
	a.mu.Unlock()

	usage, err := a.GetThreadContextUsage("codex-thread")
	if err != nil {
		t.Fatalf("Codex thread must not be an error: %v", err)
	}
	if usage.Available {
		t.Fatal("Available should be false on a Codex thread")
	}
	if !strings.Contains(usage.Reason, "Claude") {
		t.Errorf("reason should name the provider constraint, got %q", usage.Reason)
	}
}

func TestGetThreadContextUsage_ShuttingDown(t *testing.T) {
	a := NewApp()
	a.shuttingDown.Store(true)

	if _, err := a.GetThreadContextUsage("any-thread"); err != ErrShuttingDown {
		t.Fatalf("err = %v, want ErrShuttingDown", err)
	}
}

// Happy path over a real claude.Session wired to a fake CLI: proves the
// session lookup, context, and projection glue hold end to end. The wire
// shape itself is covered by the provider-level tests.
func TestGetThreadContextUsage_RoundTrip(t *testing.T) {
	a := NewApp()

	scriptPath := t.TempDir() + "/fake-claude"
	script := `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"get_context_usage"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"totalTokens":24028,"maxTokens":1000000,"rawMaxTokens":1000000,"percentage":2,"model":"claude-fable-5","categories":[{"name":"System prompt","tokens":4027,"color":"promptBorder"},{"name":"System tools (deferred)","tokens":13467,"color":"inactive","isDeferred":true},{"name":"Free space","tokens":975972,"color":"promptBorder"}]}}}\n' "$reqid"
            ;;
    esac
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sess, err := claude.NewSession(ctx, "claude-thread", claude.Config{Binary: scriptPath}, func(_ provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	a.mu.Lock()
	a.sessions["claude-thread"] = session{provider: "claude", claude: sess}
	a.mu.Unlock()

	usage, err := a.GetThreadContextUsage("claude-thread")
	if err != nil {
		t.Fatalf("GetThreadContextUsage: %v", err)
	}
	if !usage.Available {
		t.Fatalf("Available should be true, got %+v", usage)
	}
	if usage.TotalTokens != 24028 || usage.MaxTokens != 1_000_000 || usage.Percentage != 2 {
		t.Errorf("scalars = %+v", usage)
	}
	if usage.Model != "claude-fable-5" {
		t.Errorf("Model = %q", usage.Model)
	}
	if len(usage.Categories) != 3 {
		t.Fatalf("Categories = %+v, want 3 rows in wire order", usage.Categories)
	}
	if usage.Categories[0].Name != "System prompt" || usage.Categories[2].Name != "Free space" {
		t.Errorf("wire order not preserved: %+v", usage.Categories)
	}
	if !usage.Categories[1].Deferred {
		t.Errorf("deferred flag lost in projection: %+v", usage.Categories[1])
	}
	if usage.Reason != "" {
		t.Errorf("an available answer must not carry a reason, got %q", usage.Reason)
	}
}

// A provider-side failure is an error, not an "unavailable" answer — the two
// states drive different UI and must never collapse.
func TestGetThreadContextUsage_ProviderErrorSurfaces(t *testing.T) {
	a := NewApp()

	scriptPath := t.TempDir() + "/fake-claude"
	script := `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"get_context_usage"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"context analysis failed"}}\n' "$reqid"
            ;;
    esac
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sess, err := claude.NewSession(ctx, "claude-thread", claude.Config{Binary: scriptPath}, func(_ provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	a.mu.Lock()
	a.sessions["claude-thread"] = session{provider: "claude", claude: sess}
	a.mu.Unlock()

	usage, err := a.GetThreadContextUsage("claude-thread")
	if err == nil {
		t.Fatalf("expected an error, got %+v", usage)
	}
	if !strings.Contains(err.Error(), "context analysis failed") {
		t.Errorf("error should carry the CLI message, got: %v", err)
	}
}
