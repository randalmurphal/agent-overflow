package app

import (
	"context"
	"errors"
	"testing"
	"time"
	"unsafe"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

func TestBrowserSettingsDefaultToEnabledPersistent(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	server := appbrowser.NewMCPServer(manager, true)
	app := &App{}
	app.browser.manager, app.browser.mcp = manager, server
	t.Cleanup(func() { _ = server.Close(); _ = manager.Close() })

	thread := store.Thread{ID: "t", Provider: string(provider.Claude), WorkspacePath: t.TempDir(), ProjectPath: t.TempDir()}
	servers, err := app.browserMCPConfigForThread(thread, "session")
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[appbrowser.ServerName] == nil {
		t.Fatalf("servers = %#v", servers)
	}
	thread.Provider = string(provider.ClaudeTUI)
	servers, err = app.browserMCPConfigForThread(thread, "session")
	if err != nil || len(servers) != 0 {
		t.Fatalf("TUI servers = %#v, %v", servers, err)
	}
}

func TestPatchTouchesBrowserSettings(t *testing.T) {
	t.Parallel()
	if !patchTouchesBrowserSettings(map[string]any{"browserEnabled": false}) {
		t.Fatal("browser setting not detected")
	}
	if patchTouchesBrowserSettings(map[string]any{"streamingEnabled": false}) {
		t.Fatal("unrelated setting detected")
	}
}

func TestRefreshLiveBrowserMCPUsesClaudeToggle(t *testing.T) {
	t.Parallel()
	app, _, _ := newMCPTestApp(t)
	captureDir := t.TempDir()
	sess, err := claude.NewSession(context.Background(), "browser-claude", claude.Config{
		Reaper:    procutiltest.Reaper(),
		Retention: claude.UserHomeRetention(0),
		Binary:    writeClaudeMcpToggleCaptureBinary(t, captureDir),
		WorkDir:   t.TempDir(),
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
	t.Parallel()
	app, _, _ := newMCPTestApp(t)
	app.browser.mcp = appbrowser.NewMCPServer(nil, true)
	app.browser.mcp.SetThreadEnabled("browser-claude-disabled", false)
	captureDir := t.TempDir()
	sess, err := claude.NewSession(context.Background(), "browser-claude-disabled", claude.Config{
		Reaper:    procutiltest.Reaper(),
		Retention: claude.UserHomeRetention(0),
		Binary:    writeClaudeMcpToggleCaptureBinary(t, captureDir),
		WorkDir:   t.TempDir(),
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
	t.Parallel()
	app, _, _ := newMCPTestApp(t)
	captureDir := t.TempDir()
	sess, err := claude.NewSession(context.Background(), "browser-coalesced", claude.Config{
		Reaper:    procutiltest.Reaper(),
		Retention: claude.UserHomeRetention(0),
		Binary:    writeClaudeMcpToggleCaptureBinary(t, captureDir),
		WorkDir:   t.TempDir(),
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
	t.Parallel()
	app, _, _ := newMCPTestApp(t)
	captureDir := t.TempDir()
	sess, err := codex.NewSession(context.Background(), "browser-codex", codex.Config{
		Reaper:  procutiltest.Reaper(),
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

func TestBrowserMCPRowSeparatesPreferenceFromSettings(t *testing.T) {
	t.Parallel()
	app, _, _ := newMCPTestApp(t)
	app.browser.mcp = appbrowser.NewMCPServer(nil, true)
	t.Cleanup(func() {
		if err := app.browser.mcp.Close(); err != nil {
			t.Error(err)
		}
	})
	thread := store.Thread{ID: "browser-state", Provider: string(provider.Codex)}
	if _, err := app.UpdateSettings(context.Background(), map[string]any{"browserEnabled": false}); err != nil {
		t.Fatal(err)
	}
	for _, rows := range [][]ThreadMCPServer{nil, {{Name: appbrowser.ServerName, Status: "connected", Tools: []string{"read"}}}} {
		row := findServer(app.withBrowserMCPRow(thread, rows, true), appbrowser.ServerName)
		if row.Disabled || row.Status != "disabled" || row.ToggleDisabledReason == "" || len(row.Tools) != 0 {
			t.Fatalf("blocked row = %#v", row)
		}
	}
	if err := app.setBrowserThreadMCPEnabled(thread, true); err == nil {
		t.Fatal("enabled browser while settings blocked it")
	}
	app.browser.mcp.SetThreadEnabled(thread.ID, false)
	if _, err := app.UpdateSettings(context.Background(), map[string]any{"browserEnabled": true}); err != nil {
		t.Fatal(err)
	}
	row := findServer(app.withBrowserMCPRow(thread, nil, false), appbrowser.ServerName)
	if !row.Disabled || row.ToggleDisabledReason != "" {
		t.Fatalf("saved off preference was not preserved: %#v", row)
	}
	app.browser.mcp.SetThreadEnabled(thread.ID, true)
	row = findServer(app.withBrowserMCPRow(thread, []ThreadMCPServer{{Name: appbrowser.ServerName, Status: "connected", Disabled: true, ToggleDisabledReason: "external"}}, true), appbrowser.ServerName)
	if row.Disabled || row.ToggleDisabledReason != "" {
		t.Fatalf("managed owner did not restore control: %#v", row)
	}
}

// TestBrowserCompanionPaneDetachUnbindsItsConnectionTie: see checkByIDTie.
func TestBrowserCompanionPaneDetachUnbindsItsConnectionTie(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-tie")
	access, err := app.bindBrowser(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.NewPage(t.Context(), access); err != nil {
		t.Fatalf("open page: %v", err)
	}
	checkByIDTie(t, byIDTie{
		subscribe: func(ctx context.Context) (string, error) {
			pane, err := app.BrowserCompanionPaneAttach(ctx, thread.ID)
			return pane.ID, err
		},
		unsubscribe: app.BrowserCompanionPaneDetach,
		key:         companionPaneCleanupKey,
		live:        func(id string) bool { return manager.SetPaneRect(appbrowser.MountID(id), appbrowser.PaneRect{}) == nil },
		principal:   transport.ConnPrincipal{HostWindow: true},
	})
}

// Only the host's own window mounts a browser pane: the native view lives
// there. A connection that is not it (a browser tab on this machine with the
// same launch credential) is refused and mounts nothing, and so is a call
// that came over no connection, which nothing would detach.
func TestBrowserCompanionPaneAttachIsTheHostWindowsAlone(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-host-window")
	access, err := app.bindBrowser(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.NewPage(t.Context(), access); err != nil {
		t.Fatalf("open page: %v", err)
	}
	for _, tc := range []struct {
		name    string
		ctx     func() context.Context
		refusal error
	}{
		{"the host window", func() context.Context {
			ctx, _ := transport.WithConnState(t.Context(), transport.ConnPrincipal{HostWindow: true})
			return ctx
		}, nil},
		{"another client with the launch credential", func() context.Context {
			ctx, _ := transport.WithConnState(t.Context(), transport.ConnPrincipal{})
			return ctx
		}, errBrowserPaneNotHostWindow},
		{"a call over no connection", func() context.Context { return t.Context() }, errNoCallerConnection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane, err := app.BrowserCompanionPaneAttach(tc.ctx(), thread.ID)
			if tc.refusal != nil {
				if !errors.Is(err, tc.refusal) || pane.ID != "" {
					t.Fatalf("attach = %+v, %v; want %v", pane, err, tc.refusal)
				}
				return
			}
			if err != nil || pane.ID == "" {
				t.Fatalf("attach = %+v, %v", pane, err)
			}
			manager.DetachPane(appbrowser.MountID(pane.ID))
		})
	}
}
