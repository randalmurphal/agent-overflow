package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
	"unsafe"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/mcpapp"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

// SetBrowserNativeWindow hands the browser manager the desktop window an
// in-process engine hosts its views inside. Call it before Start: the manager
// is built during startup, and an engine chosen without a window would be the
// managed-Chrome one for the life of the process. A getter (rather than a
// pointer) because the window is created later, on the app loop.
func SetBrowserNativeWindow(a *App, window func() unsafe.Pointer) {
	a.browser.nativeWindow = window
}

func browserConfigFromSettings(current settings.Settings) appbrowser.Config {
	return appbrowser.Config{
		Enabled:               current.BrowserEnabled,
		PersistSiteData:       current.BrowserPersistSiteData,
		AllowOutsideWorkspace: current.BrowserAllowOutsideWorkspace,
	}
}

func (a *App) browserMCPConfigForThread(thread store.Thread) (map[string]any, error) {
	if thread.Provider != string(provider.Claude) && thread.Provider != string(provider.Codex) {
		return nil, nil
	}
	if a.browser.mcp == nil {
		// Focused fixtures that construct a bare App do not boot optional
		// subsystems. Production initSubsystems always wires this server.
		return nil, nil
	}
	return a.browser.mcp.RegisterThread(appbrowser.Access{
		ThreadID:    thread.ID,
		Workspace:   thread.WorkspacePath,
		ProjectRoot: thread.ProjectPath,
	})
}

func isAppManagedMCPServer(name string) bool {
	return strings.TrimSpace(name) == appbrowser.ServerName
}

func (a *App) withBrowserMCPRow(thread store.Thread, rows []ThreadMCPServer, live bool) []ThreadMCPServer {
	if a.browser.mcp == nil {
		return rows
	}
	globalEnabled := a.currentSettings().BrowserEnabled
	threadEnabled := globalEnabled && a.browser.mcp.ThreadEnabled(thread.ID)
	source := mcpRowSourceConfig
	if live {
		source = mcpRowSourceSession
	}
	for i := range rows {
		if !isAppManagedMCPServer(rows[i].Name) {
			continue
		}
		if !threadEnabled {
			rows[i].Disabled = true
			rows[i].Status = string(mcpstatus.StatusDisabled)
			rows[i].Tools = nil
		}
		rows[i].Source = source
		return rows
	}
	status := mcpstatus.StatusNotStarted
	if !threadEnabled {
		status = mcpstatus.StatusDisabled
	}
	return append(rows, ThreadMCPServer{
		Provider: thread.Provider,
		Name:     appbrowser.ServerName,
		Status:   string(status),
		Disabled: !threadEnabled,
		Source:   source,
	})
}

func (a *App) setBrowserThreadMCPEnabled(thread store.Thread, enabled bool) error {
	if !a.currentSettings().BrowserEnabled {
		return fmt.Errorf("browser tools are disabled in Settings")
	}
	if a.browser.mcp == nil {
		return fmt.Errorf("browser MCP unavailable")
	}
	a.browser.mcp.SetThreadEnabled(thread.ID, enabled)
	if _, ok := a.sessionManager().get(thread.ID); !ok {
		a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.Provider(thread.Provider), Name: appbrowser.ServerName})
		return nil
	}
	if err := a.mcpService().ApplyManagedServerEnabled(thread.ID, appbrowser.ServerName, enabled); err != nil {
		a.browser.mcp.SetThreadEnabled(thread.ID, !enabled)
		return err
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.Provider(thread.Provider), Name: appbrowser.ServerName})
	return nil
}

func (a *App) teardownBrowserThread(threadID string) {
	if a.browser.mcp != nil {
		a.browser.mcp.UnregisterThread(threadID)
	}
}

// ClearBrowserSiteData closes active browser contexts before deleting their
// encrypted checkpoints, preventing a later teardown from writing cleared
// cookies back to disk.
func (a *App) ClearBrowserSiteData() error {
	if a.browser.manager == nil {
		return fmt.Errorf("browser manager unavailable")
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), 30*time.Second)
	defer cancel()
	return a.browser.manager.ClearSiteData(ctx)
}

func patchTouchesBrowserSettings(patch map[string]any) bool {
	for _, key := range []string{"browserEnabled", "browserPersistSiteData", "browserAllowOutsideWorkspace"} {
		if _, ok := patch[key]; ok {
			return true
		}
	}
	return false
}

func (a *App) scheduleBrowserSettings(next settings.Settings) {
	if !next.BrowserEnabled && a.browser.mcp != nil {
		// Revoke authority before the async process teardown begins. Enabling
		// takes the opposite order in applyBrowserSettings: the manager must be
		// ready to accept calls before the tool list becomes visible.
		a.browser.mcp.SetEnabled(false)
	}
	generation := a.browser.settingsGeneration.Add(1)
	a.browser.applyWG.Add(1)
	go func() {
		defer a.browser.applyWG.Done()
		a.browser.applyMu.Lock()
		defer a.browser.applyMu.Unlock()
		if generation != a.browser.settingsGeneration.Load() {
			return
		}
		a.applyBrowserSettings(next)
	}()
}

func (a *App) applyBrowserSettings(next settings.Settings) {
	if a.browser.manager != nil {
		if err := a.browser.manager.Reconfigure(browserConfigFromSettings(next)); err != nil {
			log.Printf("browser: apply settings: %v", err)
		}
	}
	if a.browser.mcp != nil {
		a.browser.mcp.SetEnabled(next.BrowserEnabled)
	}
	// Compare against the last provider refresh, not UpdateSettings' immediate
	// previous snapshot. A later site-data/authority update may supersede a
	// queued enable update before its worker runs; the final worker must still
	// deliver that skipped enable transition to live provider sessions.
	if a.browser.liveEnabled.Swap(next.BrowserEnabled) != next.BrowserEnabled {
		a.refreshLiveBrowserMCP(next.BrowserEnabled)
		for _, providerName := range []mcpstatus.Provider{mcpstatus.ProviderClaude, mcpstatus.ProviderCodex} {
			a.mcpStatus().Invalidate(mcpstatus.Key{Provider: providerName, Name: appbrowser.ServerName})
		}
	}
}

func (a *App) refreshLiveBrowserMCP(enabled bool) {
	for _, live := range a.sessionManager().browserMCPSessions() {
		threadEnabled := enabled && (a.browser.mcp == nil || a.browser.mcp.ThreadEnabled(live.ThreadID))
		go func(threadID string, threadEnabled bool) {
			if err := a.mcpService().ApplyManagedServerEnabled(threadID, appbrowser.ServerName, threadEnabled); err != nil {
				a.emitWireErrorToThread(threadID, "browser tools: live provider refresh failed: "+mcpapp.SanitizeError(err.Error()))
			}
		}(live.ThreadID, threadEnabled)
	}
}
