package app

import (
	"context"
	"testing"
	"time"
	"unsafe"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

func TestBrowserSettingsDefaultToEnabledPersistent(t *testing.T) {
	config := browserConfigFromSettings(settings.DefaultSettings)
	if !config.Enabled || !config.PersistSiteData || config.AllowOutsideWorkspace {
		t.Fatalf("browser config = %+v", config)
	}
}

// The fake-engine pin crosses the bootstrap boundary as one bool, and
// startup hands it straight to ManagerOptions.FakeEngine — which wins
// engine selection ahead of every other fact. A boot that asks for the
// pin must get it, and one that lifted it (the manual real-engine gate,
// docs/specs/embedded-browser.md §10) must not have it reinstated here.
func TestConfigureIsolationCarriesTheBrowserEnginePin(t *testing.T) {
	pinned := &App{}
	ConfigureIsolation(pinned, IsolationConfig{MockBrowserEngine: true})
	if !pinned.browser.mockEngine {
		t.Fatal("MockBrowserEngine: true did not pin the fake engine")
	}
	lifted := &App{}
	ConfigureIsolation(lifted, IsolationConfig{MockBrowserEngine: false})
	if lifted.browser.mockEngine {
		t.Fatal("MockBrowserEngine: false still pinned the fake engine")
	}
}

// No window getter is the whole windowless story: selection reads only
// whether one EXISTS, so an App that was never handed one has no
// in-process engine at all. The isolated boots rely on the other half of
// that rule — a getter installed before Start whose pointer arrives
// later — so the presence, not the answer, is what must be recorded here.
func TestSetBrowserNativeWindowRecordsThePresenceOfAGetter(t *testing.T) {
	app := &App{}
	if app.browser.nativeWindow != nil {
		t.Fatal("a bare App already carries a window getter")
	}
	app2 := &App{}
	SetBrowserNativeWindow(app2, func() unsafe.Pointer { return nil })
	if app2.browser.nativeWindow == nil {
		t.Fatal("SetBrowserNativeWindow did not record the getter")
	}
	if app2.browser.nativeWindow() != nil {
		t.Fatal("a getter answering nil should still answer nil")
	}
}

func TestBrowserMCPConfigRegistersOnlyHeadlessProviders(t *testing.T) {
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	server := appbrowser.NewMCPServer(manager, true)
	app := &App{}
	app.browser.manager, app.browser.mcp = manager, server
	t.Cleanup(func() { _ = server.Close(); _ = manager.Close() })

	thread := store.Thread{ID: "t", Provider: string(provider.Claude), WorkspacePath: t.TempDir(), ProjectPath: t.TempDir()}
	servers, err := app.browserMCPConfigForThread(thread)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[appbrowser.ServerName] == nil {
		t.Fatalf("servers = %#v", servers)
	}
	thread.Provider = string(provider.ClaudeTUI)
	servers, err = app.browserMCPConfigForThread(thread)
	if err != nil || len(servers) != 0 {
		t.Fatalf("TUI servers = %#v, %v", servers, err)
	}
}

func TestPatchTouchesBrowserSettings(t *testing.T) {
	if !patchTouchesBrowserSettings(map[string]any{"browserEnabled": false}) {
		t.Fatal("browser setting not detected")
	}
	if patchTouchesBrowserSettings(map[string]any{"streamingEnabled": false}) {
		t.Fatal("unrelated setting detected")
	}
}

func TestRefreshLiveBrowserMCPUsesClaudeToggle(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	captureDir := t.TempDir()
	sess, err := claude.NewSession(context.Background(), "browser-claude", claude.Config{
		Binary:  writeClaudeMcpToggleCaptureBinary(t, captureDir),
		WorkDir: t.TempDir(),
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put("browser-claude", session{Provider: string(provider.Claude), Token: "browser-toggle", Claude: sess})
	app.refreshLiveBrowserMCP(false)
	envelope := readClaudeMcpToggleCapture(t, captureDir, 3*time.Second)
	if envelope.Request["serverName"] != appbrowser.ServerName || envelope.Request["enabled"] != false {
		t.Fatalf("browser toggle = %#v", envelope.Request)
	}
}

func TestRefreshLiveBrowserMCPPreservesThreadDisable(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	app.browser.mcp = appbrowser.NewMCPServer(nil, true)
	app.browser.mcp.SetThreadEnabled("browser-claude-disabled", false)
	captureDir := t.TempDir()
	sess, err := claude.NewSession(context.Background(), "browser-claude-disabled", claude.Config{
		Binary:  writeClaudeMcpToggleCaptureBinary(t, captureDir),
		WorkDir: t.TempDir(),
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put("browser-claude-disabled", session{Provider: string(provider.Claude), Token: "browser-toggle-disabled", Claude: sess})

	app.refreshLiveBrowserMCP(true)
	envelope := readClaudeMcpToggleCapture(t, captureDir, 3*time.Second)
	if envelope.Request["enabled"] != false {
		t.Fatalf("thread-disabled browser toggle = %#v", envelope.Request)
	}
}

func TestBrowserSettingsCoalescingKeepsSkippedEnableTransition(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	captureDir := t.TempDir()
	sess, err := claude.NewSession(context.Background(), "browser-coalesced", claude.Config{
		Binary:  writeClaudeMcpToggleCaptureBinary(t, captureDir),
		WorkDir: t.TempDir(),
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put("browser-coalesced", session{Provider: string(provider.Claude), Token: "browser-coalesced", Claude: sess})
	app.browser.liveEnabled.Store(true)

	// Hold the worker lock so both updates are queued before either can apply.
	// The second update supersedes the first but does not itself change enabled;
	// it must still deliver the true -> false transition to the provider.
	app.browser.applyMu.Lock()
	app.scheduleBrowserSettings(settings.Settings{BrowserEnabled: false})
	app.scheduleBrowserSettings(settings.Settings{BrowserEnabled: false, BrowserPersistSiteData: true})
	app.browser.applyMu.Unlock()
	app.browser.applyWG.Wait()

	envelope := readClaudeMcpToggleCapture(t, captureDir, 3*time.Second)
	if envelope.Request["serverName"] != appbrowser.ServerName || envelope.Request["enabled"] != false {
		t.Fatalf("coalesced browser toggle = %#v", envelope.Request)
	}
}

func TestRefreshLiveBrowserMCPUsesCodexReload(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	captureDir := t.TempDir()
	sess, err := codex.NewSession(context.Background(), "browser-codex", codex.Config{
		Binary:  writeCodexRefreshCaptureBinary(t, captureDir, "browser-codex-provider", ""),
		Model:   "gpt-5",
		WorkDir: t.TempDir(),
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put("browser-codex", session{Provider: string(provider.Codex), Token: "browser-reload", Codex: sess})
	app.refreshLiveBrowserMCP(false)
	if method := readCodexReloadCapture(t, captureDir, 3*time.Second); method != "config/mcpServer/reload" {
		t.Fatalf("reload method = %q", method)
	}
}
