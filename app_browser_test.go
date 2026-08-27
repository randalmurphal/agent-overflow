package main

import (
	"context"
	"testing"
	"time"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

func TestBrowserSettingsDefaultToEnabledVisiblePersistent(t *testing.T) {
	config := browserConfigFromSettings(settings.DefaultSettings)
	if !config.Enabled || !config.ShowWindow || !config.PersistSiteData || config.AllowOutsideWorkspace {
		t.Fatalf("browser config = %+v", config)
	}
}

func TestBrowserMCPConfigRegistersOnlyHeadlessProviders(t *testing.T) {
	manager := appbrowser.NewManager(nil, t.TempDir(), appbrowser.Config{Enabled: true})
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

func TestMergeMCPServersKeepsDesignAndBrowser(t *testing.T) {
	merged := mergeMCPServers(map[string]any{"design": 1}, map[string]any{appbrowser.ServerName: 2})
	if len(merged) != 2 || merged["design"] != 1 || merged[appbrowser.ServerName] != 2 {
		t.Fatalf("merged = %#v", merged)
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
	app.sessions["browser-claude"] = session{provider: string(provider.Claude), token: "browser-toggle", claude: sess}
	app.refreshLiveBrowserMCP(false)
	envelope := readClaudeMcpToggleCapture(t, captureDir, 3*time.Second)
	if envelope.Request["serverName"] != appbrowser.ServerName || envelope.Request["enabled"] != false {
		t.Fatalf("browser toggle = %#v", envelope.Request)
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
	app.sessions["browser-coalesced"] = session{provider: string(provider.Claude), token: "browser-coalesced", claude: sess}
	app.browser.liveEnabled.Store(true)

	// Hold the worker lock so both updates are queued before either can apply.
	// The second update supersedes the first but does not itself change enabled;
	// it must still deliver the true -> false transition to the provider.
	app.browser.applyMu.Lock()
	app.scheduleBrowserSettings(settings.Settings{BrowserEnabled: false})
	app.scheduleBrowserSettings(settings.Settings{BrowserEnabled: false, BrowserShowWindow: true})
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
	app.sessions["browser-codex"] = session{provider: string(provider.Codex), token: "browser-reload", codex: sess}
	app.refreshLiveBrowserMCP(false)
	if method := readCodexReloadCapture(t, captureDir, 3*time.Second); method != "config/mcpServer/reload" {
		t.Fatalf("reload method = %q", method)
	}
}
