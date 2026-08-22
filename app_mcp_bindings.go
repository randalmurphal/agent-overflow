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
	"agent-overflow/internal/threadmode"
)

// ErrMCPProviderUnsupported reports an inbound binding call carrying a
// provider value AO doesn't currently route MCP for. New providers
// register here when they ship.
var ErrMCPProviderUnsupported = errors.New("mcp: unsupported provider")

// ErrMCPSessionUnavailable fires when a thread's provider session is
// not live (and auto-start failed) so a binding cannot drive the
// provider-side operation it needs.
var ErrMCPSessionUnavailable = errors.New("mcp: thread session not available")

// ErrMCPDesignThreadUnsupported reports an MCP operation that needs a
// session able to see workspace MCP servers, asked of a design thread.
// Design sessions launch with `--strict-mcp-config` and only ever load
// their own design servers, so the provider round-trip would resolve
// nothing. Config-level operations (the enable/disable toggle) are
// unaffected — those write files, not sessions.
var ErrMCPDesignThreadUnsupported = errors.New("mcp: design threads load only their own MCP config; sign in from a regular thread")

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
	// mcpLiveApplyTimeout bounds the RPCs that apply an MCP change to a
	// live provider session: Claude `mcp_toggle` / `mcp_reconnect` and
	// Codex `config/mcpServer/reload`. Enabling or reconnecting makes
	// the provider actually connect the server, so the ceiling absorbs
	// a cold stdio-server spawn (npx download and the like) without
	// holding the caller forever when a server is wedged.
	mcpLiveApplyTimeout = 30 * time.Second
)

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

// ReconnectMcpServer re-runs the connection for one MCP server on the
// thread's live session — the fix for "I authenticated / repaired the
// server after the session spawned" without waiting out the idle
// reaper. Claude reconnects the named server in place (`mcp_reconnect`);
// Codex has no per-server primitive, so it re-reads config and
// re-pushes MCP state into the loaded thread (`config/mcpServer/reload`).
// Requires a live session: with none, the next session start connects
// fresh anyway, so there is nothing to fix.
func (a *App) ReconnectMcpServer(threadID, name string) error {
	if a.store == nil {
		return errors.New("reconnect mcp server: store unavailable")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("reconnect mcp server: load thread: %w", err)
	}
	if thread.Mode == threadmode.ModeDesign {
		return ErrMCPDesignThreadUnsupported
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return ErrMCPSessionUnavailable
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), mcpLiveApplyTimeout)
	defer cancel()
	switch thread.Provider {
	case string(provider.Claude):
		if sess.claude == nil {
			return ErrMCPSessionUnavailable
		}
		if err := sess.claude.ReconnectMCPServer(ctx, name); err != nil {
			return err
		}
	case string(provider.Codex):
		if sess.codex == nil {
			return ErrMCPSessionUnavailable
		}
		// The user is explicitly re-running this server's startup, so the
		// retained failure describes a run they just invalidated.
		sess.codex.ForgetMCPStartupState(name)
		if err := sess.codex.RefreshMCPServers(ctx); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, thread.Provider)
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.Provider(thread.Provider), Name: name})
	return nil
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
	fetcher, err := a.mcpStatusFetcher(prov, "")
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
// named provider and returns the resulting list. The menu chains this
// after a config-sourced listing whose cache looks cold, then re-lists
// — it is how enabled plugin servers (invisible in ~/.claude.json)
// reach the no-session view. workspacePath becomes the fetch cwd so
// `claude mcp list` sees the workspace's project-scope servers; pass
// "" for a workspace-agnostic refresh.
func (a *App) RefreshMcpServerStatus(providerName, workspacePath string) ([]mcpstatus.ServerStatus, error) {
	prov, err := parseMCPStatusProvider(providerName)
	if err != nil {
		return nil, err
	}
	fetcher, err := a.mcpStatusFetcher(prov, workspacePath)
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
// configured with the user's installed CLI binary. cwd scopes the
// fetch to a workspace (project-layer servers are cwd-dependent for
// both CLIs); "" runs from the app's own cwd. Errors surface when the
// user hasn't installed the provider's CLI; callers translate that
// into a user-facing "binary not found" toast.
func (a *App) mcpStatusFetcher(prov mcpstatus.Provider, cwd string) (mcpstatus.Fetcher, error) {
	switch prov {
	case mcpstatus.ProviderClaude:
		bin := a.providerBinaryPath(string(provider.Claude))
		if bin == "" {
			return nil, fmt.Errorf("mcp status: claude binary not configured")
		}
		return &claude.MCPStatusFetcher{Binary: bin, Cwd: cwd}, nil
	case mcpstatus.ProviderCodex:
		bin := a.providerBinaryPath(string(provider.Codex))
		if bin == "" {
			return nil, fmt.Errorf("mcp status: codex binary not configured")
		}
		return &codex.MCPStatusFetcher{Binary: bin, Cwd: cwd}, nil
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
