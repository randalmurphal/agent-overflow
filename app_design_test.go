package main

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"agent-overflow/internal/design"
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
	app := newTestAppWithDesign(t)

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

	request := waitForAppDesignRequest(t, app)
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
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for design choice")
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
	thread.ProjectPath = thread.WorkspacePath
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

func testDesignThread(id string) store.Thread {
	thread := testThread(id)
	thread.InteractionMode = "design"
	return thread
}

func waitForAppDesignRequest(t *testing.T, app *App) design.DesignOptionsRequest {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if request, ok := app.reactor.PendingRequest("thread-design"); ok {
			return request
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for design request")
	return design.DesignOptionsRequest{}
}

func designMCPRegistrationCount(server *codex.DesignMCPServer) int {
	if server == nil {
		return 0
	}
	return reflect.ValueOf(server).Elem().FieldByName("threadToToken").Len()
}
