package app

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/mcpapp"
)

func TestTriggerWorkspaceMCPAuthSingleFlightsWithoutThread(t *testing.T) {
	app := &App{}
	app.appCtx, app.appCancel = context.WithCancel(context.Background())
	t.Cleanup(app.appCancel)

	wait := make(chan mcpapp.WorkspaceAuthOutcome, 1)
	closed := make(chan struct{}, 1)
	started := make(chan struct{})
	var starts atomic.Int32
	deps := app.mcpDeps()
	deps.WorkspaceAuthStarter = func(
		_ context.Context, providerName, workspacePath, serverName string,
	) (*mcpapp.WorkspaceAuthHandle, error) {
		if providerName != "codex" || workspacePath != "/repo" || serverName != "atlassian" {
			return nil, fmt.Errorf("starter target = %q %q %q", providerName, workspacePath, serverName)
		}
		if starts.Add(1) == 1 {
			close(started)
		}
		return &mcpapp.WorkspaceAuthHandle{
			Result: mcpapp.MCPAuthInitResult{
				AuthURL:            "https://example.test/oauth",
				Provider:           "codex",
				RequiresUserAction: true,
			},
			Wait: func(context.Context) mcpapp.WorkspaceAuthOutcome { return <-wait },
			Close: func() error {
				closed <- struct{}{}
				return nil
			},
		}, nil
	}
	app.mcpApp = mcpapp.New(deps)
	app.mcpAppOnce.Do(func() {})

	type result struct {
		value MCPAuthInitResult
		err   error
	}
	results := make(chan result, 2)
	go func() {
		value, err := app.TriggerWorkspaceMcpAuth("codex", "/repo", "atlassian")
		results <- result{value: value, err: err}
	}()
	<-started
	go func() {
		value, err := app.TriggerWorkspaceMcpAuth("codex", "/repo", "atlassian")
		results <- result{value: value, err: err}
	}()
	for range 2 {
		got := <-results
		if got.err != nil || got.value.AuthURL != "https://example.test/oauth" {
			t.Fatalf("TriggerWorkspaceMcpAuth = %+v, %v", got.value, got.err)
		}
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("provider starts = %d, want 1", got)
	}

	wait <- mcpapp.WorkspaceAuthOutcome{Success: true}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("temporary provider process was not closed after completion")
	}
}

func TestTriggerWorkspaceMCPAuthFailedStartCanRetry(t *testing.T) {
	app := &App{}
	var starts atomic.Int32
	deps := app.mcpDeps()
	deps.WorkspaceAuthStarter = func(
		_ context.Context, _, _, _ string,
	) (*mcpapp.WorkspaceAuthHandle, error) {
		if starts.Add(1) == 1 {
			return nil, errors.New("spawn failed")
		}
		return &mcpapp.WorkspaceAuthHandle{
			Result: mcpapp.MCPAuthInitResult{AuthURL: "https://example.test/retry", Provider: "claude"},
			Wait:   func(context.Context) mcpapp.WorkspaceAuthOutcome { return mcpapp.WorkspaceAuthOutcome{} },
			Close:  func() error { return nil },
		}, nil
	}
	app.mcpApp = mcpapp.New(deps)
	app.mcpAppOnce.Do(func() {})

	if _, err := app.TriggerWorkspaceMcpAuth("claude", "/repo", "srv"); err == nil {
		t.Fatal("first start unexpectedly succeeded")
	}
	result, err := app.TriggerWorkspaceMcpAuth("claude", "/repo", "srv")
	if err != nil || result.AuthURL != "https://example.test/retry" {
		t.Fatalf("retry = %+v, %v", result, err)
	}
	if starts.Load() != 2 {
		t.Fatalf("provider starts = %d, want 2", starts.Load())
	}
}

func TestTriggerWorkspaceMCPAuthShutdownClosesFlowWithoutEvent(t *testing.T) {
	app := &App{}
	app.appCtx, app.appCancel = context.WithCancel(context.Background())
	var emits atomic.Int32
	app.testEmitHook = func(string, any) { emits.Add(1) }
	closed := make(chan struct{})
	deps := app.mcpDeps()
	deps.WorkspaceAuthStarter = func(
		_ context.Context, _, _, _ string,
	) (*mcpapp.WorkspaceAuthHandle, error) {
		return &mcpapp.WorkspaceAuthHandle{
			Result: mcpapp.MCPAuthInitResult{AuthURL: "https://example.test/oauth", Provider: "codex"},
			Wait: func(ctx context.Context) mcpapp.WorkspaceAuthOutcome {
				<-ctx.Done()
				return mcpapp.WorkspaceAuthOutcome{Error: ctx.Err().Error()}
			},
			Close: func() error {
				close(closed)
				return nil
			},
		}, nil
	}
	app.mcpApp = mcpapp.New(deps)
	app.mcpAppOnce.Do(func() {})
	if _, err := app.TriggerWorkspaceMcpAuth("codex", "/repo", "srv"); err != nil {
		t.Fatalf("TriggerWorkspaceMcpAuth: %v", err)
	}
	app.appCancel()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("temporary provider process remained open after app cancellation")
	}
	if emits.Load() != 0 {
		t.Fatalf("post-shutdown events = %d, want 0", emits.Load())
	}
}
