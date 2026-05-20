package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/mcp"
	"agent-overflow/internal/mcpprobe"
)

// MCP probe cache TTLs. Stdio entries take longer to refresh (subprocess
// fan-out) so we keep them around for 10 minutes; HTTP/SSE entries are
// cheap to re-check (sub-ms in the spike) but their status can change
// behind a kicked OAuth token, so 60s. Explicit Invalidate on CRUD + OAuth
// completion is what keeps the popup honest; TTL is the safety net.
const (
	mcpProbeStdioTTL = 10 * time.Minute
	mcpProbeHTTPTTL  = 60 * time.Second
)

// mcpProbe returns the lazy-init probe cache. Tests building a bare
// *App via &App{...} get a working cache on first call without
// pre-wiring; production wiring doesn't need an explicit init.
func (a *App) mcpProbe() *mcpprobe.Cache {
	a.mcpProbeCacheOnce.Do(func() {
		a.mcpProbeCache = mcpprobe.NewCache(mcpProbeStdioTTL, mcpProbeHTTPTTL)
	})
	return a.mcpProbeCache
}

// mergeMCPServersForThread combines the design-mode MCP server map
// (already in provider shape) with the user MCP library selection for
// the thread. Design wins on name collisions; collision names are
// returned so the caller can surface a thread-scoped warning. Returns
// (nil, nil, nil) when neither side has anything to wire.
//
// The full library is passed (not just the selected subset) so the
// merger can emit `{enabled: false}` overlays for unselected entries —
// required for Codex per-thread isolation, since its overrides MERGE
// with disk config rather than replacing it.
func (a *App) mergeMCPServersForThread(threadID, providerName string, designServers map[string]any) (map[string]any, []string, error) {
	if a.store == nil {
		if len(designServers) == 0 {
			return nil, nil, nil
		}
		out := make(map[string]any, len(designServers))
		for k, v := range designServers {
			out[k] = v
		}
		return out, nil, nil
	}
	ids, err := a.store.ListThreadMCPServerIDs(threadID)
	if err != nil {
		return nil, nil, fmt.Errorf("merge mcp servers: list thread ids: %w", err)
	}
	library, err := a.store.ListMCPServers()
	if err != nil {
		return nil, nil, fmt.Errorf("merge mcp servers: list library: %w", err)
	}
	if len(designServers) == 0 && len(ids) == 0 && len(library) == 0 {
		return nil, nil, nil
	}
	result, err := mcp.MergeForProvider(providerName, designServers, library, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("merge mcp servers: %w", err)
	}
	if len(result.Servers) == 0 {
		return nil, result.Collisions, nil
	}
	return result.Servers, result.Collisions, nil
}

// seedThreadMCPServersFromProfile copies the last-selected MCP server
// ids from mcp_thread_profile onto a freshly created thread. Called at
// CreateThread completion so new threads inherit "what the user last
// had enabled" — matches the chat-model-profile seeding behaviour.
// Library entries that no longer exist (deleted in the meantime) are
// silently dropped via the FK constraint; the caller doesn't need to
// reconcile.
func (a *App) seedThreadMCPServersFromProfile(threadID string) {
	if a.store == nil {
		return
	}
	profile, err := a.store.GetMCPThreadProfile()
	if err != nil {
		// sql.ErrNoRows is the common case (no profile seeded yet) —
		// nothing to copy. Any other error is logged and treated as
		// "no seed available" so thread creation still succeeds.
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("mcp: seed thread %s: read profile: %v", threadID, err)
		}
		return
	}
	if len(profile.ServerIDs) == 0 {
		return
	}
	if err := a.store.SetThreadMCPServers(threadID, profile.ServerIDs); err != nil {
		log.Printf("mcp: seed thread %s from profile: %v", threadID, err)
	}
}
