package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestTriggerWorkspaceMCPAuthSingleFlightsWithoutThread(t *testing.T) {
	app := &App{}
	app.appCtx, app.appCancel = context.WithCancel(context.Background())
	t.Cleanup(app.appCancel)

	wait := make(chan workspaceMCPAuthOutcome, 1)
	closed := make(chan struct{}, 1)
	started := make(chan struct{})
	var starts atomic.Int32
	app.workspaceMCPAuthStarter = func(
		_ context.Context, providerName, workspacePath, serverName string,
	) (*workspaceMCPAuthHandle, error) {
		if providerName != "codex" || workspacePath != "/repo" || serverName != "atlassian" {
			return nil, fmt.Errorf("starter target = %q %q %q", providerName, workspacePath, serverName)
		}
		if starts.Add(1) == 1 {
			close(started)
		}
		return &workspaceMCPAuthHandle{
			result: MCPAuthInitResult{
				AuthURL:            "https://example.test/oauth",
				Provider:           "codex",
				RequiresUserAction: true,
			},
			wait: func(context.Context) workspaceMCPAuthOutcome { return <-wait },
			close: func() error {
				closed <- struct{}{}
				return nil
			},
		}, nil
	}

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

	wait <- workspaceMCPAuthOutcome{success: true}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("temporary provider process was not closed after completion")
	}
}

func TestTriggerWorkspaceMCPAuthFailedStartCanRetry(t *testing.T) {
	app := &App{}
	var starts atomic.Int32
	app.workspaceMCPAuthStarter = func(
		_ context.Context, _, _, _ string,
	) (*workspaceMCPAuthHandle, error) {
		if starts.Add(1) == 1 {
			return nil, errors.New("spawn failed")
		}
		return &workspaceMCPAuthHandle{
			result: MCPAuthInitResult{AuthURL: "https://example.test/retry", Provider: "claude"},
			wait:   func(context.Context) workspaceMCPAuthOutcome { return workspaceMCPAuthOutcome{} },
			close:  func() error { return nil },
		}, nil
	}

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
	app.workspaceMCPAuthStarter = func(
		_ context.Context, _, _, _ string,
	) (*workspaceMCPAuthHandle, error) {
		return &workspaceMCPAuthHandle{
			result: MCPAuthInitResult{AuthURL: "https://example.test/oauth", Provider: "codex"},
			wait: func(ctx context.Context) workspaceMCPAuthOutcome {
				<-ctx.Done()
				return workspaceMCPAuthOutcome{error: ctx.Err().Error()}
			},
			close: func() error {
				close(closed)
				return nil
			},
		}, nil
	}
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
