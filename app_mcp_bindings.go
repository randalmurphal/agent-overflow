package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

// ErrMCPProviderUnsupported reports an inbound binding call carrying a
// provider value AO doesn't currently route MCP for. New providers
// register here when they ship.
var ErrMCPProviderUnsupported = errors.New("mcp: unsupported provider")

// ErrMCPSessionUnavailable fires when a thread's provider session is
// not live (and auto-start failed) so a binding cannot drive the
// provider-side operation it needs.
var ErrMCPSessionUnavailable = errors.New("mcp: thread session not available")

// ErrMCPReadOnlyEntry fires when a binding tries to mutate a Claude
// plugin/cloud entry. Those are surfaced to the UI so the user can
// toggle them on/off via `disabledMcpServers`, but AO doesn't own
// their definitions.
var ErrMCPReadOnlyEntry = errors.New("mcp: entry is not user-managed")

const (
	mcpProviderClaude = "claude"
	mcpProviderCodex  = "codex"

	// mcpEphemeralFetchTimeout bounds the one-shot `claude mcp list` /
	// `codex mcpServerStatus/list` subprocess invocations that back the
	// popup's initial fetch and explicit Refresh. Generous enough to
	// absorb a slow first-launch CLI warm-up without leaving the popup
	// spinning indefinitely if the binary is wedged.
	mcpEphemeralFetchTimeout = 20 * time.Second
	// mcpAuthRoundTripTimeout bounds the provider-side MCP OAuth
	// handshake. The user is interacting with a browser tab during the
	// flow, so a longer ceiling is intentional — too short and a
	// distracted approval re-issues a fresh login.
	mcpAuthRoundTripTimeout = 60 * time.Second
	// mcpLiveReconcileTimeout bounds the per-thread live reconcile
	// sent to an active provider session (Claude `mcp_set_servers`,
	// Codex `RefreshMCPServers`). 30s is the same ceiling as
	// reconcileCodexAfterStart and absorbs the provider's per-server
	// connect fan-out without holding a thread-action lock forever.
	mcpLiveReconcileTimeout = 30 * time.Second
)

// MCPServer is the wire shape every MCP binding speaks. It unifies
// claudeconfig.Server (which carries Source + the per-workspace
// Disabled flag) and codexconfig.Server (which carries a global
// Enabled flag and Codex-specific transport names) into a single shape
// the frontend renders without a provider branch. Transport values
// stay provider-native ("stdio" | "http" | "sse" for Claude;
// "stdio" | "streamable_http" for Codex) so the editor form can pick
// the right input set.
//
// Disabled is the unified UI flag — true means "this server is not
// active in the current scope". For Claude that translates to the
// thread's workspace `disabledMcpServers` list; for Codex it
// translates to the global `enabled = false` field in
// ~/.codex/config.toml.
type MCPServer struct {
	Provider       string            `json:"provider"`
	Name           string            `json:"name"`
	Source         string            `json:"source,omitempty"`
	Transport      string            `json:"transport"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	BearerTokenEnv string            `json:"bearerTokenEnv,omitempty"`
	Disabled       bool              `json:"disabled"`
}

// MCPAuthInitResult is the response shape for TriggerMcpAuth.
type MCPAuthInitResult struct {
	AuthURL            string `json:"authUrl"`
	Provider           string `json:"provider"`
	RequiresUserAction bool   `json:"requiresUserAction"`
}

// TriggerMcpAuth drives the provider-side OAuth handshake for an
// http/sse / streamable_http MCP server. The thread must be live
// because the OAuth listener is owned by the provider process; if no
// session is live, we auto-start one for the round-trip.
func (a *App) TriggerMcpAuth(threadID, name string) (MCPAuthInitResult, error) {
	return a.triggerMcpAuth(threadID, name)
}

// ListMcpServers returns every MCP server visible to the caller's
// scope. `provider` selects which config file we read. For Claude,
// `workspacePath` resolves the `projects.<path>.disabledMcpServers`
// list so the unified `Disabled` flag reflects this workspace; passing
// an empty workspacePath returns the library with every entry as
// enabled (used by the Settings UI's "library" view that doesn't
// belong to a specific thread). For Codex, workspacePath is ignored —
// the `enabled` flag is global.
func (a *App) ListMcpServers(provider, workspacePath string) ([]MCPServer, error) {
	switch provider {
	case mcpProviderClaude:
		return a.listClaudeMcpServers(workspacePath)
	case mcpProviderCodex:
		return a.listCodexMcpServers()
	default:
		return nil, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, provider)
	}
}

// CreateMcpServer adds a new entry to the provider's config file.
// Plugin/cloud Claude entries are refused — AO only manages
// SourceUser. The provider name is taken from input.Provider so the
// binding stays a single Wails entry rather than two parallel ones.
func (a *App) CreateMcpServer(input MCPServer) (MCPServer, error) {
	switch input.Provider {
	case mcpProviderClaude:
		return a.createClaudeMcpServer(input)
	case mcpProviderCodex:
		return a.createCodexMcpServer(input)
	default:
		return MCPServer{}, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, input.Provider)
	}
}

// UpdateMcpServer replaces the entry at input.Provider+input.Name with
// the new shape. Renaming is not supported.
func (a *App) UpdateMcpServer(input MCPServer) (MCPServer, error) {
	switch input.Provider {
	case mcpProviderClaude:
		return a.updateClaudeMcpServer(input)
	case mcpProviderCodex:
		return a.updateCodexMcpServer(input)
	default:
		return MCPServer{}, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, input.Provider)
	}
}

// DeleteMcpServer removes the entry. For Claude the call also strips
// the name from every workspace's `disabledMcpServers` so re-adding
// the server later doesn't silently surface as disabled.
func (a *App) DeleteMcpServer(provider, name string) error {
	switch provider {
	case mcpProviderClaude:
		return a.deleteClaudeMcpServer(name)
	case mcpProviderCodex:
		return a.deleteCodexMcpServer(name)
	default:
		return fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, provider)
	}
}

// SetMcpServerEnabled toggles the unified Disabled flag for a server
// in the calling thread's scope. Claude: writes to the workspace's
// `disabledMcpServers` array. Codex: writes the global
// `enabled = false` field. After the file write the binding tries to
// reconcile the calling thread's live session so the change applies
// without a manual restart (Claude: mcp_set_servers; Codex:
// config/mcpServer/reload). Other live sessions outside this thread
// pick up the change on their next session start — that's the
// documented divergence from "every thread sees live disk state".
func (a *App) SetMcpServerEnabled(threadID, name string, enabled bool) error {
	if a.store == nil {
		return errors.New("set mcp server enabled: store unavailable")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("set mcp server enabled: load thread: %w", err)
	}
	switch thread.Provider {
	case string(provider.Claude):
		return a.setClaudeMcpDisabled(thread, name, !enabled)
	case string(provider.Codex):
		return a.setCodexMcpEnabled(thread, name, enabled)
	default:
		return fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, thread.Provider)
	}
}

// GetMcpServerStatus returns the cached status for one server. If
// no fresh entry exists, it runs the appropriate ephemeral fetcher
// (Claude `mcp list` / Codex `mcpServerStatus/list`) to populate
// the cache, then returns the result. force=true bypasses cache
// hits but still single-flights concurrent callers.
func (a *App) GetMcpServerStatus(providerName, name string, force bool) (mcpstatus.ServerStatus, error) {
	prov, err := parseMCPStatusProvider(providerName)
	if err != nil {
		return mcpstatus.ServerStatus{}, err
	}
	key := mcpstatus.Key{Provider: prov, Name: name}
	fetcher, err := a.mcpStatusFetcher(prov)
	if err != nil {
		return mcpstatus.ServerStatus{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpEphemeralFetchTimeout)
	defer cancel()
	return a.mcpStatus().GetOrFetch(ctx, key, fetcher, force)
}

// ListMcpServerStatuses returns the cached snapshot for one
// provider. Does NOT trigger an ephemeral fetch — used to render
// the popup on first open before the explicit refresh kicks in.
// Live sessions push their own state into the cache continuously,
// so for any provider that has run a thread this lifetime the
// snapshot is already populated.
func (a *App) ListMcpServerStatuses(providerName string) ([]mcpstatus.ServerStatus, error) {
	prov, err := parseMCPStatusProvider(providerName)
	if err != nil {
		return nil, err
	}
	out := a.mcpStatus().SnapshotProvider(prov)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RefreshMcpServerStatus forces a fresh ephemeral fetch for the
// named provider and returns the resulting list. The popup's
// Refresh button uses this when the cached snapshot looks stale or
// the user wants to re-check after editing a server.
func (a *App) RefreshMcpServerStatus(providerName string) ([]mcpstatus.ServerStatus, error) {
	prov, err := parseMCPStatusProvider(providerName)
	if err != nil {
		return nil, err
	}
	fetcher, err := a.mcpStatusFetcher(prov)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpEphemeralFetchTimeout)
	defer cancel()
	servers, err := a.mcpStatus().RefreshProvider(ctx, prov, fetcher)
	if err != nil {
		return nil, err
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	return servers, nil
}

// mcpStatusFetcher returns the per-provider ephemeral fetcher,
// configured with the user's installed CLI binary. Errors surface
// when the user hasn't installed the provider's CLI; callers
// translate that into a user-facing "binary not found" toast.
func (a *App) mcpStatusFetcher(prov mcpstatus.Provider) (mcpstatus.Fetcher, error) {
	switch prov {
	case mcpstatus.ProviderClaude:
		bin := a.providerBinaryPath(string(provider.Claude))
		if bin == "" {
			return nil, fmt.Errorf("mcp status: claude binary not configured")
		}
		return &claude.MCPStatusFetcher{Binary: bin}, nil
	case mcpstatus.ProviderCodex:
		bin := a.providerBinaryPath(string(provider.Codex))
		if bin == "" {
			return nil, fmt.Errorf("mcp status: codex binary not configured")
		}
		return &codex.MCPStatusFetcher{Binary: bin}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, prov)
	}
}

func parseMCPStatusProvider(s string) (mcpstatus.Provider, error) {
	switch s {
	case mcpProviderClaude:
		return mcpstatus.ProviderClaude, nil
	case mcpProviderCodex:
		return mcpstatus.ProviderCodex, nil
	default:
		return "", fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, s)
	}
}
