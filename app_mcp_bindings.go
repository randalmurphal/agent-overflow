package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/codexconfig"
	"agent-overflow/internal/mcp"
	"agent-overflow/internal/mcpprobe"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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

// ProbeMcpServer runs the handshake for the named server. `force`
// bypasses the cache (the user-facing "Refresh" button uses it); the
// default path returns the cached result when fresh.
func (a *App) ProbeMcpServer(provider, name string, force bool) (mcpprobe.Result, error) {
	spec, err := a.resolveProbeSpec(provider, name)
	if err != nil {
		return mcpprobe.Result{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return a.mcpProbe().Get(ctx, spec, force), nil
}

// GetMcpProbeSnapshot returns the currently-cached probe results
// keyed by `provider:name`. The popup uses it to render instant status
// on first open without waiting for one probe per server.
func (a *App) GetMcpProbeSnapshot() (map[string]mcpprobe.Result, error) {
	return a.mcpProbe().Snapshot(), nil
}

// TriggerMcpAuth drives the provider-side OAuth handshake for an
// http/sse / streamable_http MCP server. The thread must be live
// because the OAuth listener is owned by the provider process; if no
// session is live, we auto-start one for the round-trip.
func (a *App) TriggerMcpAuth(threadID, name string) (MCPAuthInitResult, error) {
	if a.store == nil {
		return MCPAuthInitResult{}, errors.New("trigger mcp auth: store unavailable")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return MCPAuthInitResult{}, err
	}
	if !a.hasActiveSession(threadID) {
		if err := a.startSession(threadID); err != nil {
			return MCPAuthInitResult{}, fmt.Errorf("auto-start session: %w", err)
		}
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return MCPAuthInitResult{}, ErrMCPSessionUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	switch thread.Provider {
	case string(provider.Claude):
		if sess.claude == nil {
			return MCPAuthInitResult{}, ErrMCPSessionUnavailable
		}
		res, err := sess.claude.AuthenticateMCP(ctx, name)
		if err != nil {
			return MCPAuthInitResult{}, err
		}
		return MCPAuthInitResult{
			AuthURL:            res.AuthURL,
			Provider:           string(provider.Claude),
			RequiresUserAction: res.RequiresUserAction,
		}, nil
	case string(provider.Codex):
		if sess.codex == nil {
			return MCPAuthInitResult{}, ErrMCPSessionUnavailable
		}
		res, err := sess.codex.AuthenticateMCP(ctx, name, nil, 0)
		if err != nil {
			return MCPAuthInitResult{}, err
		}
		return MCPAuthInitResult{
			AuthURL:            res.AuthorizationURL,
			Provider:           string(provider.Codex),
			RequiresUserAction: true,
		}, nil
	default:
		return MCPAuthInitResult{}, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, thread.Provider)
	}
}

// handleCodexMCPOAuthCompleted is the side-channel callback Codex
// fires after the user's browser hop completes the OAuth handshake.
// AO's job is small but load-bearing: invalidate the probe cache so
// the next status read reflects the freshly-credentialed session, and
// surface a `mcp:oauth-completed` event for any popup listening.
func (a *App) handleCodexMCPOAuthCompleted(threadID, serverName string, success bool, errMsg string) {
	cacheKey := mcp.Spec{Provider: mcpProviderCodex, Name: serverName}.CacheKey()
	a.mcpProbe().Invalidate(cacheKey)
	a.emit("mcp:oauth-completed", map[string]any{
		"threadId":   threadID,
		"provider":   mcpProviderCodex,
		"serverName": serverName,
		"success":    success,
		"error":      errMsg,
	})
	if !success {
		msg := strings.TrimSpace(errMsg)
		if msg == "" {
			msg = "sign-in did not complete"
		}
		a.emitErrorToThread(threadID, fmt.Sprintf("mcp: %s: %s", serverName, msg))
	}
}

// ---------------- Claude implementation ----------------

const (
	mcpProviderClaude = "claude"
	mcpProviderCodex  = "codex"
)

func (a *App) listClaudeMcpServers(workspacePath string) ([]MCPServer, error) {
	st, err := a.claudeConfig()
	if err != nil {
		return nil, err
	}
	servers, err := st.ListServers(workspacePath)
	if err != nil {
		return nil, fmt.Errorf("list claude mcp servers: %w", err)
	}
	out := make([]MCPServer, 0, len(servers))
	for _, srv := range servers {
		out = append(out, claudeServerToWire(srv))
	}
	return out, nil
}

func (a *App) createClaudeMcpServer(input MCPServer) (MCPServer, error) {
	if input.Source != "" && input.Source != string(claudeconfig.SourceUser) {
		return MCPServer{}, fmt.Errorf("%w: source=%s", ErrMCPReadOnlyEntry, input.Source)
	}
	st, err := a.claudeConfig()
	if err != nil {
		return MCPServer{}, err
	}
	target := wireToClaudeServer(input)
	target.Source = claudeconfig.SourceUser
	if err := st.CreateServer(target); err != nil {
		return MCPServer{}, fmt.Errorf("create claude mcp server: %w", err)
	}
	a.mcpProbe().Invalidate(mcp.Spec{Provider: mcpProviderClaude, Name: target.Name}.CacheKey())
	return claudeServerToWire(target), nil
}

func (a *App) updateClaudeMcpServer(input MCPServer) (MCPServer, error) {
	if input.Source != "" && input.Source != string(claudeconfig.SourceUser) {
		return MCPServer{}, fmt.Errorf("%w: source=%s", ErrMCPReadOnlyEntry, input.Source)
	}
	st, err := a.claudeConfig()
	if err != nil {
		return MCPServer{}, err
	}
	target := wireToClaudeServer(input)
	target.Source = claudeconfig.SourceUser
	if err := st.UpdateServer(target); err != nil {
		return MCPServer{}, fmt.Errorf("update claude mcp server: %w", err)
	}
	a.mcpProbe().Invalidate(mcp.Spec{Provider: mcpProviderClaude, Name: target.Name}.CacheKey())
	return claudeServerToWire(target), nil
}

func (a *App) deleteClaudeMcpServer(name string) error {
	st, err := a.claudeConfig()
	if err != nil {
		return err
	}
	if err := st.DeleteServer(name); err != nil {
		return fmt.Errorf("delete claude mcp server: %w", err)
	}
	a.mcpProbe().Invalidate(mcp.Spec{Provider: mcpProviderClaude, Name: name}.CacheKey())
	return nil
}

func (a *App) setClaudeMcpDisabled(thread store.Thread, name string, disabled bool) error {
	workspacePath := strings.TrimSpace(thread.WorkspacePath)
	if workspacePath == "" {
		return fmt.Errorf("set claude mcp disabled: thread %s has no workspace path", thread.ID)
	}
	st, err := a.claudeConfig()
	if err != nil {
		return err
	}
	if err := st.SetDisabled(workspacePath, name, disabled); err != nil {
		return fmt.Errorf("set claude mcp disabled: %w", err)
	}
	a.mcpProbe().Invalidate(mcp.Spec{Provider: mcpProviderClaude, Name: name}.CacheKey())
	if thread.Mode == "design" {
		// Design Claude sessions launched with --strict-mcp-config — user
		// MCP isn't visible to them regardless of the disabled flag.
		// Skip the live reconcile so we don't spend an RPC on a no-op.
		return nil
	}
	if !a.hasActiveSession(thread.ID) {
		return nil
	}
	go func() {
		// Serialize through the per-thread action lock so rapid toggles
		// can't deliver mcp_set_servers RPCs out of order — the second
		// goroutine waits for the first's round-trip to land before
		// reading the latest disk state and pushing again.
		unlock := a.threadLocks().Lock(thread.ID)
		defer unlock()
		if err := a.reconcileClaudeMCPLive(thread); err != nil {
			a.emitErrorToThread(thread.ID, fmt.Sprintf("mcp: live reconcile failed: %v", err))
			log.Printf("mcp: thread %s claude live reconcile: %v", thread.ID, err)
		}
	}()
	return nil
}

// reconcileClaudeMCPLive pushes the workspace-effective user MCP set
// to the live Claude session via mcp_set_servers. The desired-set
// shape is the same one --mcp-config produces at launch, so a session
// that started without --mcp-config sees the user MCP appear here for
// the first time. (Claude treats absence-of-name as remove; the full
// set is replace-with-diff.) Only called for chat/plan threads —
// design threads are --strict-mcp-config locked.
func (a *App) reconcileClaudeMCPLive(thread store.Thread) error {
	sess, ok := a.sessionManager().get(thread.ID)
	if !ok || sess.claude == nil {
		return nil
	}
	st, err := a.claudeConfig()
	if err != nil {
		return err
	}
	servers, err := st.ListServers(thread.WorkspacePath)
	if err != nil {
		return fmt.Errorf("list claude mcp servers for reconcile: %w", err)
	}
	target := map[string]any{}
	for _, srv := range servers {
		if srv.Source != claudeconfig.SourceUser {
			// Plugin / cloud entries are managed by Claude itself; AO can't
			// re-create their connection state via mcp_set_servers.
			continue
		}
		if srv.Disabled {
			continue
		}
		spec, err := srv.RenderForCLI()
		if err != nil {
			log.Printf("mcp: thread %s render %q for live reconcile: %v", thread.ID, srv.Name, err)
			continue
		}
		target[srv.Name] = spec
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	diff, err := sess.claude.SetMCPServers(ctx, target)
	if err != nil {
		return err
	}
	if diff != nil && (len(diff.Added) > 0 || len(diff.Removed) > 0 || len(diff.Errors) > 0) {
		log.Printf("mcp: thread %s claude diff added=%v removed=%v errors=%v", thread.ID, diff.Added, diff.Removed, diff.Errors)
	}
	return nil
}

func claudeServerToWire(s claudeconfig.Server) MCPServer {
	return MCPServer{
		Provider:  mcpProviderClaude,
		Name:      s.Name,
		Source:    string(s.Source),
		Transport: s.Transport,
		Command:   s.Command,
		Args:      append([]string{}, s.Args...),
		Env:       copyStringMap(s.Env),
		URL:       s.URL,
		Headers:   copyStringMap(s.Headers),
		Disabled:  s.Disabled,
	}
}

func wireToClaudeServer(w MCPServer) claudeconfig.Server {
	return claudeconfig.Server{
		Name:      strings.TrimSpace(w.Name),
		Transport: w.Transport,
		Command:   w.Command,
		Args:      append([]string{}, w.Args...),
		Env:       copyStringMap(w.Env),
		URL:       w.URL,
		Headers:   copyStringMap(w.Headers),
	}
}

// ---------------- Codex implementation ----------------

func (a *App) listCodexMcpServers() ([]MCPServer, error) {
	st, err := a.codexConfig()
	if err != nil {
		return nil, err
	}
	servers, err := st.ListServers()
	if err != nil {
		return nil, fmt.Errorf("list codex mcp servers: %w", err)
	}
	out := make([]MCPServer, 0, len(servers))
	for _, srv := range servers {
		out = append(out, codexServerToWire(srv))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (a *App) createCodexMcpServer(input MCPServer) (MCPServer, error) {
	st, err := a.codexConfig()
	if err != nil {
		return MCPServer{}, err
	}
	target := wireToCodexServer(input)
	if err := st.CreateServer(target); err != nil {
		return MCPServer{}, fmt.Errorf("create codex mcp server: %w", err)
	}
	a.mcpProbe().Invalidate(mcp.Spec{Provider: mcpProviderCodex, Name: target.Name}.CacheKey())
	return codexServerToWire(target), nil
}

func (a *App) updateCodexMcpServer(input MCPServer) (MCPServer, error) {
	st, err := a.codexConfig()
	if err != nil {
		return MCPServer{}, err
	}
	target := wireToCodexServer(input)
	if err := st.UpdateServer(target); err != nil {
		return MCPServer{}, fmt.Errorf("update codex mcp server: %w", err)
	}
	a.mcpProbe().Invalidate(mcp.Spec{Provider: mcpProviderCodex, Name: target.Name}.CacheKey())
	return codexServerToWire(target), nil
}

func (a *App) deleteCodexMcpServer(name string) error {
	st, err := a.codexConfig()
	if err != nil {
		return err
	}
	if err := st.DeleteServer(name); err != nil {
		return fmt.Errorf("delete codex mcp server: %w", err)
	}
	a.mcpProbe().Invalidate(mcp.Spec{Provider: mcpProviderCodex, Name: name}.CacheKey())
	return nil
}

func (a *App) setCodexMcpEnabled(thread store.Thread, name string, enabled bool) error {
	st, err := a.codexConfig()
	if err != nil {
		return err
	}
	if err := st.SetEnabled(name, enabled); err != nil {
		return fmt.Errorf("set codex mcp enabled: %w", err)
	}
	a.mcpProbe().Invalidate(mcp.Spec{Provider: mcpProviderCodex, Name: name}.CacheKey())
	if !a.hasActiveSession(thread.ID) {
		return nil
	}
	sess, ok := a.sessionManager().get(thread.ID)
	if !ok || sess.codex == nil {
		return nil
	}
	go func() {
		// Serialize through the per-thread action lock so rapid toggles
		// can't deliver RefreshMCPServers RPCs out of order against the
		// same Codex session.
		unlock := a.threadLocks().Lock(thread.ID)
		defer unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sess.codex.RefreshMCPServers(ctx); err != nil {
			a.emitErrorToThread(thread.ID, fmt.Sprintf("mcp: live reload failed: %v", err))
			log.Printf("mcp: thread %s codex live reload: %v", thread.ID, err)
		}
	}()
	return nil
}

func codexServerToWire(s codexconfig.Server) MCPServer {
	return MCPServer{
		Provider:       mcpProviderCodex,
		Name:           s.Name,
		Transport:      s.Transport,
		Command:        s.Command,
		Args:           append([]string{}, s.Args...),
		Env:            copyStringMap(s.Env),
		URL:            s.URL,
		Headers:        copyStringMap(s.HTTPHeaders),
		BearerTokenEnv: s.BearerTokenEnv,
		Disabled:       !s.Enabled,
	}
}

func wireToCodexServer(w MCPServer) codexconfig.Server {
	return codexconfig.Server{
		Name:           strings.TrimSpace(w.Name),
		Transport:      w.Transport,
		Command:        w.Command,
		Args:           append([]string{}, w.Args...),
		Env:            copyStringMap(w.Env),
		URL:            w.URL,
		HTTPHeaders:    copyStringMap(w.Headers),
		BearerTokenEnv: w.BearerTokenEnv,
		Enabled:        !w.Disabled,
	}
}

// resolveProbeSpec hydrates a probe Spec for the given (provider,
// name) tuple by reading the canonical entry from the matching
// adapter. Missing entries return an error rather than a stub Spec so
// the cache doesn't memoize an "unknown server" failure.
func (a *App) resolveProbeSpec(providerName, name string) (mcp.Spec, error) {
	switch providerName {
	case mcpProviderClaude:
		st, err := a.claudeConfig()
		if err != nil {
			return mcp.Spec{}, err
		}
		servers, err := st.ListServers("")
		if err != nil {
			return mcp.Spec{}, err
		}
		for _, srv := range servers {
			if srv.Name == name {
				return srv.ToSpec(), nil
			}
		}
		return mcp.Spec{}, fmt.Errorf("mcp: claude server %q not found", name)
	case mcpProviderCodex:
		st, err := a.codexConfig()
		if err != nil {
			return mcp.Spec{}, err
		}
		servers, err := st.ListServers()
		if err != nil {
			return mcp.Spec{}, err
		}
		for _, srv := range servers {
			if srv.Name == name {
				return srv.ToSpec(), nil
			}
		}
		return mcp.Spec{}, fmt.Errorf("mcp: codex server %q not found", name)
	default:
		return mcp.Spec{}, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, providerName)
	}
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
