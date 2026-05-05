package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/design"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

func TestDesignBindingsCaptureAndListSnapshots(t *testing.T) {
	app := newTestAppWithDesign(t)

	if err := app.designWorkdir.EnsureThread("thread-design"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}

	snap, err := app.CaptureSnapshot("thread-design", "first cut")
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}
	if snap.Label != "first cut" {
		t.Fatalf("Label = %q, want first cut", snap.Label)
	}

	listed, err := app.ListDesignSnapshots("thread-design")
	if err != nil {
		t.Fatalf("ListDesignSnapshots: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != snap.ID {
		t.Fatalf("listed = %+v, want %s", listed, snap.ID)
	}
}

func TestGetDesignWorkdirInfoReturnsAbsolutePathAndManifest(t *testing.T) {
	app := newTestAppWithDesign(t)

	if err := app.designWorkdir.EnsureThread("thread-design"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	mainPath, err := app.designWorkdir.MainPath("thread-design")
	if err != nil {
		t.Fatalf("MainPath: %v", err)
	}
	// Drop two more files alongside the seeded index.html so the
	// manifest is non-trivial and we can pin the sort order.
	if err := os.WriteFile(filepath.Join(mainPath, "app.js"), []byte("//"), 0o644); err != nil {
		t.Fatalf("WriteFile app.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mainPath, "style.css"), []byte("/*"), 0o644); err != nil {
		t.Fatalf("WriteFile style.css: %v", err)
	}

	info, err := app.GetDesignWorkdirInfo("thread-design")
	if err != nil {
		t.Fatalf("GetDesignWorkdirInfo: %v", err)
	}
	if !filepath.IsAbs(info.MainPath) {
		t.Fatalf("MainPath = %q, want absolute", info.MainPath)
	}
	if info.MainPath != mainPath {
		t.Fatalf("MainPath = %q, want %q", info.MainPath, mainPath)
	}
	want := []string{"app.js", "index.html", "style.css"}
	if len(info.Files) != len(want) {
		t.Fatalf("Files = %v, want %v", info.Files, want)
	}
	for i, name := range want {
		if info.Files[i] != name {
			t.Fatalf("Files[%d] = %q, want %q (full %v)", i, info.Files[i], name, info.Files)
		}
	}
}

func TestGetDesignWorkdirInfoReturnsEmptySliceForFreshThread(t *testing.T) {
	app := newTestAppWithDesign(t)
	// Don't EnsureThread — main/ does not exist yet. The binding
	// must tolerate that and return Files as a non-nil empty slice
	// (so the JSON marshal emits [], not null).
	info, err := app.GetDesignWorkdirInfo("thread-fresh")
	if err != nil {
		t.Fatalf("GetDesignWorkdirInfo on fresh thread: %v", err)
	}
	if info.Files == nil {
		t.Fatal("Files = nil, want empty slice (json contract)")
	}
	if len(info.Files) != 0 {
		t.Fatalf("Files = %v, want empty", info.Files)
	}
}

func TestDesignBranchFromSnapshotRestoresMain(t *testing.T) {
	app := newTestAppWithDesign(t)

	if err := app.designWorkdir.EnsureThread("thread-design"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	mainPath, err := app.designWorkdir.MainPath("thread-design")
	if err != nil {
		t.Fatalf("MainPath: %v", err)
	}

	// Seed main with v1 contents and snapshot.
	if err := os.WriteFile(filepath.Join(mainPath, "index.html"), []byte("v1"), 0o644); err != nil {
		t.Fatalf("WriteFile v1: %v", err)
	}
	snapV1, err := app.CaptureSnapshot("thread-design", "v1")
	if err != nil {
		t.Fatalf("CaptureSnapshot v1: %v", err)
	}

	// Replace main with v2.
	if err := os.WriteFile(filepath.Join(mainPath, "index.html"), []byte("v2"), 0o644); err != nil {
		t.Fatalf("WriteFile v2: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(mainPath, "index.html")); string(got) != "v2" {
		t.Fatalf("pre-branch main = %q, want v2", string(got))
	}

	// Branch back to v1; main should restore to v1 contents.
	if err := app.BranchFromSnapshot("thread-design", snapV1.ID); err != nil {
		t.Fatalf("BranchFromSnapshot: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(mainPath, "index.html"))
	if err != nil {
		t.Fatalf("ReadFile post-branch: %v", err)
	}
	if string(got) != "v1" {
		t.Fatalf("post-branch main = %q, want v1", string(got))
	}
}

func TestDesignIngestDiagnosticBatchAppendsToBuffer(t *testing.T) {
	app := newTestAppWithDesign(t)

	batch := design.DiagnosticBatch{
		ThreadID: "thread-design",
		Diagnostics: []design.Diagnostic{
			{Severity: design.SeverityError, Message: "TypeError"},
			{Severity: design.SeverityWarn, Message: "deprecated"},
		},
	}
	if err := app.IngestDiagnosticBatch(batch); err != nil {
		t.Fatalf("IngestDiagnosticBatch: %v", err)
	}

	if app.designDiagnostics.LatestToken("thread-design") < 2 {
		t.Fatalf("token = %d, want >= 2", app.designDiagnostics.LatestToken("thread-design"))
	}
}

func TestDesignIngestScreenshotResolvesPendingCapture(t *testing.T) {
	app := newTestAppWithDesign(t)

	captureCh := make(chan design.ScreenshotRequest, 1)
	app.testEmitHook = func(eventName string, data any) {
		if eventName == design.CaptureEventName {
			if req, ok := data.(design.ScreenshotRequest); ok {
				captureCh <- req
			}
		}
	}

	pngCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		png, err := app.designScreenshots.Capture(t.Context(), "thread-design")
		if err != nil {
			errCh <- err
			return
		}
		pngCh <- png
	}()

	var captureRequest design.ScreenshotRequest
	select {
	case captureRequest = <-captureCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for capture event")
	}

	want := []byte{0x89, 0x50, 0x4e, 0x47}
	encoded := base64.StdEncoding.EncodeToString(want)
	if err := app.IngestScreenshot(design.ScreenshotResult{
		RequestID: captureRequest.RequestID,
		PNGBase64: encoded,
	}); err != nil {
		t.Fatalf("IngestScreenshot: %v", err)
	}

	select {
	case got := <-pngCh:
		if string(got) != string(want) {
			t.Fatalf("png mismatch: got %x, want %x", got, want)
		}
	case err := <-errCh:
		t.Fatalf("Capture err: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Capture did not return after IngestScreenshot")
	}
}

func TestDesignSessionConfigRegistersMCPForBothProviders(t *testing.T) {
	app := newTestAppWithDesign(t)

	for _, prov := range []string{"codex", "claude"} {
		setThreadProvider(t, app, "thread-design", prov)
		thread, err := app.store.GetThread("thread-design")
		if err != nil {
			t.Fatalf("GetThread: %v", err)
		}
		cfg, err := app.designSessionConfig(thread)
		if err != nil {
			t.Fatalf("designSessionConfig(%s): %v", prov, err)
		}
		if cfg.Prompt == "" {
			t.Fatalf("%s: empty prompt", prov)
		}
		// Phase 2: activateDesignSession registers MCP. Split from
		// designSessionConfig so it runs AFTER stopExistingSessionLocked
		// has torn down any predecessor session for the same thread.
		mcp, err := app.activateDesignSession(thread)
		if err != nil {
			t.Fatalf("activateDesignSession(%s): %v", prov, err)
		}
		if len(mcp) != 1 {
			t.Fatalf("%s: MCPServers len = %d, want 1", prov, len(mcp))
		}
		// Re-teardown so the next provider gets a fresh registration.
		app.teardownDesignThread(thread.ID)
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

// TestDesignWorkDirOverridePointsAtThreadDir is the load-bearing test
// for the agent's CWD: a design thread spawns its provider subprocess
// inside the per-thread workdir, NOT the thread's WorkspacePath. The
// bundled system prompt instructs the agent to operate on `main/`,
// `options/`, and `snapshots/` as direct children of its CWD —
// pointing it at the project repo instead would land the agent's
// Read/Edit/Write in the user's source tree.
func TestDesignWorkDirOverridePointsAtThreadDir(t *testing.T) {
	app := newTestAppWithDesign(t)

	thread, err := app.store.GetThread("thread-design")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	thread.WorkspacePath = "/some/project/path"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}

	override, err := app.designWorkDirOverride(thread)
	if err != nil {
		t.Fatalf("designWorkDirOverride: %v", err)
	}
	expected, err := app.designWorkdir.ThreadDir(thread.ID)
	if err != nil {
		t.Fatalf("ThreadDir: %v", err)
	}
	if override != expected {
		t.Fatalf("override = %q, want %q (per-thread design workdir, not WorkspacePath)", override, expected)
	}
	if override == thread.WorkspacePath {
		t.Fatalf("override leaked WorkspacePath %q — agent would write into the project repo", thread.WorkspacePath)
	}
}

// TestDesignWorkDirOverrideSkippedForChatThreads pins the inverse:
// non-design threads must be left alone so the agent runs against the
// thread's actual workspace. A regression that broadened the override
// would silently re-CWD every Claude/Codex chat to a global directory.
func TestDesignWorkDirOverrideSkippedForChatThreads(t *testing.T) {
	app := newTestAppWithDesign(t)

	thread, err := app.store.GetThread("thread-design")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	thread.Mode = "chat"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}

	override, err := app.designWorkDirOverride(thread)
	if err != nil {
		t.Fatalf("designWorkDirOverride: %v", err)
	}
	if override != "" {
		t.Fatalf("override = %q, want empty for chat-mode thread", override)
	}
}

func newTestAppWithDesign(t *testing.T) *App {
	t.Helper()

	app := newTestAppWithStore(t)
	app.configDir = t.TempDir()

	designBase := filepath.Join(t.TempDir(), "design-workdirs")
	app.designWorkdir = design.NewWorkDirManager(designBase, app.store)
	app.designDiagnostics = design.NewDiagnosticBuffer(nil)
	app.designScreenshots = design.NewScreenshotBroker(app.emit)
	app.designServer = design.FileHandler(designBase)
	app.designWatchers = make(map[string]*design.Watcher)
	app.reactor = design.NewReactor(app.designDiagnostics, app.designScreenshots)
	app.designMCP = design.NewMCPServer(app.reactor)
	t.Cleanup(func() {
		_ = app.designMCP.Close()
		// Stop any watchers spawned during the test.
		app.designWatchersMu.Lock()
		watchers := make([]*design.Watcher, 0, len(app.designWatchers))
		for _, w := range app.designWatchers {
			watchers = append(watchers, w)
		}
		app.designWatchers = nil
		app.designWatchersMu.Unlock()
		for _, w := range watchers {
			w.Stop()
		}
	})

	thread := testDesignThread("thread-design")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	return app
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

func designMCPRegistrationCount(server *design.MCPServer) int {
	if server == nil {
		return 0
	}
	return server.RegisteredThreadCount()
}

// Reference unused symbols so a future cleanup doesn't accidentally
// break them under -trimpath imports.
var _ = strings.TrimSpace
