package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

func browserConfigFromSettings(current settings.Settings) appbrowser.Config {
	return appbrowser.Config{
		Enabled:               current.BrowserEnabled,
		ShowWindow:            current.BrowserShowWindow,
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

func mergeMCPServers(groups ...map[string]any) map[string]any {
	count := 0
	for _, group := range groups {
		count += len(group)
	}
	if count == 0 {
		return nil
	}
	merged := make(map[string]any, count)
	for _, group := range groups {
		for name, spec := range group {
			merged[name] = spec
		}
	}
	return merged
}

func isAppManagedMCPServer(name string) bool {
	return strings.TrimSpace(name) == appbrowser.ServerName
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
	for _, key := range []string{"browserEnabled", "browserShowWindow", "browserPersistSiteData", "browserAllowOutsideWorkspace"} {
		if _, ok := patch[key]; ok {
			return true
		}
	}
	return false
}

func (a *App) scheduleBrowserSettings(next settings.Settings) {
	if a.browser.mcp != nil {
		a.browser.mcp.SetEnabled(next.BrowserEnabled)
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
	// Compare against the last provider refresh, not UpdateSettings' immediate
	// previous snapshot. A later display/persistence update may supersede a
	// queued enable update before its worker runs; the final worker must still
	// deliver that skipped enable transition to live provider sessions.
	if a.browser.liveEnabled.Swap(next.BrowserEnabled) != next.BrowserEnabled {
		a.refreshLiveBrowserMCP(next.BrowserEnabled)
	}
}

func (a *App) refreshLiveBrowserMCP(enabled bool) {
	for _, live := range a.sessionManager().browserMCPSessions() {
		switch {
		case live.claude != nil:
			go func(threadID string) {
				ctx, cancel := context.WithTimeout(a.lifeCtx(), mcpLiveApplyTimeout)
				defer cancel()
				if err := live.claude.ToggleMCPServer(ctx, appbrowser.ServerName, enabled); err != nil {
					a.emitWireErrorToThread(threadID, "browser tools: live Claude refresh failed: "+sanitizeMCPError(err.Error()))
				}
			}(live.threadID)
		case live.codex != nil:
			live.codex.ForgetMCPStartupState(appbrowser.ServerName)
			a.requestCodexMCPReload(live.threadID)
		}
	}
}
