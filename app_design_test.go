package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/design"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

func TestDesignBindingsListAndGetArtifactHTML(t *testing.T) {
	app := newTestAppWithDesign(t)

	artifact, err := app.reactor.Render("thread-design", design.RenderInput{
		HTML:        "<html><body>artifact</body></html>",
		Title:       "Artifact",
		Description: "Preview",
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	artifacts, err := app.ListDesignArtifacts("thread-design")
	if err != nil {
		t.Fatalf("ListDesignArtifacts() error = %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].ID != artifact.ID {
		t.Fatalf("artifacts = %+v, want stored artifact %q", artifacts, artifact.ID)
	}

	html, err := app.GetDesignArtifactHTML("thread-design", artifact.ID)
	if err != nil {
		t.Fatalf("GetDesignArtifactHTML() error = %v", err)
	}
	if html != "<html><body>artifact</body></html>" {
		t.Fatalf("html = %q", html)
	}
}

func TestChooseDesignOptionResolvesPendingRequest(t *testing.T) {
	app, presented := newTestAppWithDesignNotify(t)

	resultCh := make(chan design.ChoiceResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := app.reactor.PresentOptions(context.Background(), "thread-design", design.PresentOptionsInput{
			Prompt: "Choose",
			Options: []design.PresentOptionInput{
				{ID: "a", Title: "A", Description: "Alpha", HTML: "<html>A</html>"},
				{ID: "b", Title: "B", Description: "Beta", HTML: "<html>B</html>"},
			},
		})
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- result
	}()

	request := waitForAppDesignRequest(t, app, presented)
	if err := app.ChooseDesignOption("thread-design", request.RequestID, "b"); err != nil {
		t.Fatalf("ChooseDesignOption() error = %v", err)
	}

	select {
	case err := <-errCh:
		t.Fatalf("PresentOptions() error = %v", err)
	case result := <-resultCh:
		if result.Chosen != "b" {
			t.Fatalf("Chosen = %q, want b", result.Chosen)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for design choice")
	}
}

func TestClaudeDesignToolRenderStoresArtifact(t *testing.T) {
	app := newTestAppWithDesign(t)
	setThreadProvider(t, app, "thread-design", string(provider.Claude))

	// Wire the reactor's emit callback to signal when the artifact is rendered.
	rendered := make(chan struct{}, 1)
	app.reactor = design.NewReactor(app.artifacts, func(eventName string, _ any) {
		if eventName == "design:artifact" {
			select {
			case rendered <- struct{}{}:
			default:
			}
		}
	})

	meta, err := json.Marshal(map[string]any{
		"toolName": "render_design",
		"input": map[string]any{
			"html":        "<html><body>Claude render</body></html>",
			"title":       "Claude Render",
			"description": "Generated from a Claude tool block",
		},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	app.sessionEventHandler("thread-design", "session-1")(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "thread-design",
		ItemType:  "render_design",
		Meta:      meta,
		Timestamp: time.Now(),
	})

	select {
	case <-rendered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Claude render artifact")
	}

	artifacts, err := app.ListDesignArtifacts("thread-design")
	if err != nil {
		t.Fatalf("ListDesignArtifacts() error = %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("len(artifacts) = %d, want 1", len(artifacts))
	}
	if artifacts[0].Title != "Claude Render" {
		t.Fatalf("artifact title = %q, want Claude Render", artifacts[0].Title)
	}
}

func TestClaudeDesignOptionChoiceSendsFollowUpMessage(t *testing.T) {
	app, presented := newTestAppWithDesignNotify(t)
	setThreadProvider(t, app, "thread-design", string(provider.Claude))

	sent := make(chan string, 1)
	app.sendMessageFn = func(threadID, content string) error {
		if threadID != "thread-design" {
			t.Fatalf("threadID = %q, want thread-design", threadID)
		}
		sent <- content
		return nil
	}

	meta, err := json.Marshal(map[string]any{
		"toolName": "present_options",
		"input": map[string]any{
			"prompt": "Choose a direction",
			"options": []map[string]any{
				{"id": "a", "title": "Alpha", "description": "First", "html": "<html>A</html>"},
				{"id": "b", "title": "Beta", "description": "Second", "html": "<html>B</html>"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	app.sessionEventHandler("thread-design", "session-1")(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "thread-design",
		ItemType:  "present_options",
		Meta:      meta,
		Timestamp: time.Now(),
	})

	request := waitForAppDesignRequest(t, app, presented)
	if err := app.ChooseDesignOption("thread-design", request.RequestID, "b"); err != nil {
		t.Fatalf("ChooseDesignOption() error = %v", err)
	}

	select {
	case content := <-sent:
		if !strings.Contains(content, `"Beta"`) {
			t.Fatalf("content = %q, want selected title", content)
		}
		if !strings.Contains(content, "ID: b") {
			t.Fatalf("content = %q, want selected option ID", content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Claude design follow-up message")
	}
}

func TestDesignSessionConfigRegistersCodexMCP(t *testing.T) {
	app := newTestAppWithDesign(t)

	cfg, err := app.designSessionConfig(testDesignThread("thread-design"))
	if err != nil {
		t.Fatalf("designSessionConfig() error = %v", err)
	}
	if cfg.Prompt == "" {
		t.Fatal("expected design prompt")
	}
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("MCPServers len = %d, want 1", len(cfg.MCPServers))
	}
}

func TestStartSessionCleansUpDesignMCPRegistrationOnFailure(t *testing.T) {
	app := newTestAppWithDesign(t)
	app.settings = settings.NewService(t.TempDir())

	thread, err := app.store.GetThread("thread-design")
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	thread.WorkspacePath = t.TempDir()
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread() error = %v", err)
	}

	if _, err := app.settings.Update(map[string]any{
		"codexBinaryPath": filepath.Join(t.TempDir(), "missing-codex"),
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if got := designMCPRegistrationCount(app.designMCP); got != 0 {
		t.Fatalf("registration count before StartSession = %d, want 0", got)
	}

	if err := app.StartSession(thread.ID); err == nil {
		t.Fatal("StartSession() error = nil, want failure")
	}

	if got := designMCPRegistrationCount(app.designMCP); got != 0 {
		t.Fatalf("registration count after failed StartSession = %d, want 0", got)
	}
}

func newTestAppWithDesign(t *testing.T) *App {
	t.Helper()

	app := newTestAppWithStore(t)
	app.configDir = t.TempDir()
	app.artifacts = design.NewArtifactStore(filepath.Join(t.TempDir(), "design-artifacts"), app.store)
	app.reactor = design.NewReactor(app.artifacts, func(string, any) {})
	app.designMCP = codex.NewDesignMCPServer(app.reactor)
	t.Cleanup(func() {
		_ = app.designMCP.Close()
	})

	thread := testDesignThread("thread-design")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	return app
}

// newTestAppWithDesignNotify is like newTestAppWithDesign but returns a channel
// that signals when the reactor emits a "design:options" event. This allows
// tests to block on the channel instead of polling for pending requests.
func newTestAppWithDesignNotify(t *testing.T) (*App, <-chan struct{}) {
	t.Helper()

	presented := make(chan struct{}, 1)
	app := newTestAppWithStore(t)
	app.configDir = t.TempDir()
	app.artifacts = design.NewArtifactStore(filepath.Join(t.TempDir(), "design-artifacts"), app.store)
	app.reactor = design.NewReactor(app.artifacts, func(eventName string, _ any) {
		if eventName == "design:options" {
			select {
			case presented <- struct{}{}:
			default:
			}
		}
	})
	app.designMCP = codex.NewDesignMCPServer(app.reactor)
	t.Cleanup(func() {
		_ = app.designMCP.Close()
	})

	thread := testDesignThread("thread-design")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	return app, presented
}

func testDesignThread(id string) store.Thread {
	thread := testThread(id)
	thread.Mode = "design"
	return thread
}

func setThreadProvider(t *testing.T, app *App, threadID, providerName string) {
	t.Helper()

	thread, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	thread.Provider = providerName
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread() error = %v", err)
	}
}

// waitForAppDesignRequest blocks until a design options request appears for the
// thread. The provided channel must be signaled by the reactor's emit callback
// when a "design:options" event fires. See newTestAppWithDesignNotify.
func waitForAppDesignRequest(t *testing.T, app *App, presented <-chan struct{}) design.DesignOptionsRequest {
	t.Helper()

	select {
	case <-presented:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for design request")
	}

	request, ok := app.reactor.PendingRequest("thread-design")
	if !ok {
		t.Fatal("no pending design request after options event")
	}
	return request
}

func designMCPRegistrationCount(server *codex.DesignMCPServer) int {
	if server == nil {
		return 0
	}
	return server.RegisteredThreadCount()
}
