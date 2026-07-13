package main

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
)

// snapshotProviderDisabledMCPServers reads the provider-native disabled set.
// New installs with no AO defaults row still use this so thread creation
// preserves the user's existing Claude/Codex configuration.
func (a *App) snapshotProviderDisabledMCPServers(providerName, workspacePath string) *[]string {
	var disabled []string
	servers, err := a.ListMcpServers(providerName, workspacePath)
	if err != nil {
		log.Printf("mcp: snapshot %s disabled for %q: %v", providerName, workspacePath, err)
		return &disabled
	}
	disabled = disabledMCPServerNames(servers)
	return &disabled
}

// snapshotDisabledMcpServers returns the disabled set for a new thread's
// initial per-thread snapshot. AO defaults win when present; provider-native
// config is the compatibility fallback.
func (a *App) snapshotDisabledMcpServers(providerName, workspacePath string) *[]string {
	if a.store != nil {
		names, found, err := a.store.GetNewThreadDisabledMCPServers(providerName, workspacePath)
		if err == nil && found {
			return &names
		}
		if err != nil {
			log.Printf("mcp: load new-thread defaults for %s/%q: %v", providerName, workspacePath, err)
		}
	}
	return a.snapshotProviderDisabledMCPServers(providerName, workspacePath)
}

// ensureDisabledMcpSnapshot returns the per-thread disabled set,
// lazy-snapshotting from the new-thread defaults if the thread has no
// snapshot yet (NULL column, pre-feature thread).
func (a *App) ensureDisabledMcpSnapshot(threadID, providerName, workspacePath string) ([]string, error) {
	names, snapshotted, err := a.store.GetDisabledMcpServers(threadID)
	if err != nil {
		return nil, err
	}
	if snapshotted {
		return names, nil
	}
	snapshot := a.snapshotDisabledMcpServers(providerName, workspacePath)
	if err := a.store.SetDisabledMcpServers(threadID, *snapshot); err != nil {
		return nil, err
	}
	return *snapshot, nil
}

// ListMcpServersForNewThread returns the MCP library with defaults that future
// threads will snapshot. It does not require or create a thread row.
func (a *App) ListMcpServersForNewThread(providerName, workspacePath string) ([]MCPServer, error) {
	servers, err := a.ListMcpServers(providerName, workspacePath)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers for new thread: %w", err)
	}
	disabled := disabledMCPServerNames(servers)
	if a.store != nil {
		names, found, err := a.store.GetNewThreadDisabledMCPServers(providerName, workspacePath)
		if err == nil && found {
			disabled = names
		}
		if err != nil {
			log.Printf("mcp: load new-thread defaults for %s/%q: %v", providerName, workspacePath, err)
		}
	}
	return applyDisabledMCPServers(servers, disabled), nil
}

// SetNewThreadMcpServerEnabled updates only the defaults future threads will
// snapshot. Existing threads keep their own per-thread MCP state.
func (a *App) SetNewThreadMcpServerEnabled(providerName, workspacePath, name string, enabled bool) error {
	providerName, workspacePath, name, fallback, err := a.prepareNewThreadMCPDisabledUpdate(providerName, workspacePath, name)
	if err != nil {
		return err
	}
	return a.persistNewThreadMCPDisabledUpdate(providerName, workspacePath, name, enabled, fallback)
}

func (a *App) prepareNewThreadMCPDisabledUpdate(providerName, workspacePath, name string) (string, string, string, []string, error) {
	providerName = strings.TrimSpace(providerName)
	workspacePath = strings.TrimSpace(workspacePath)
	name = strings.TrimSpace(name)
	if providerName == "" {
		return "", "", "", nil, fmt.Errorf("set new-thread mcp server enabled: provider is required")
	}
	if name == "" {
		return "", "", "", nil, fmt.Errorf("set new-thread mcp server enabled: server name is required")
	}
	servers, err := a.ListMcpServers(providerName, workspacePath)
	if err != nil {
		return "", "", "", nil, err
	}
	if !mcpServerExists(servers, name) {
		return "", "", "", nil, fmt.Errorf("mcp server %q not found for %s", name, providerName)
	}
	return providerName, workspacePath, name, disabledMCPServerNames(servers), nil
}

func (a *App) persistNewThreadMCPDisabledUpdate(providerName, workspacePath, name string, enabled bool, fallback []string) error {
	if a.store == nil {
		return fmt.Errorf("set new-thread mcp server enabled: store unavailable")
	}
	_, err := a.store.MutateNewThreadDisabledMCPServers(providerName, workspacePath, fallback, func(current []string) []string {
		return mutateDisabledSet(current, name, !enabled)
	})
	return err
}

// ListMcpServersForThread returns the MCP server library with per-thread
// disabled state from SQLite instead of global config. Used by the composer
// toolbar popup. Settings UI continues to use ListMcpServers.
func (a *App) ListMcpServersForThread(threadID string) ([]MCPServer, error) {
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers for thread: %w", err)
	}
	disabled, err := a.ensureDisabledMcpSnapshot(t.ID, t.Provider, t.WorkspacePath)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers for thread: %w", err)
	}
	return a.listMcpServersWithDisabled(t.Provider, t.WorkspacePath, disabled, "thread")
}

func (a *App) listMcpServersWithDisabled(providerName, workspacePath string, disabled []string, scope string) ([]MCPServer, error) {
	servers, err := a.ListMcpServers(providerName, workspacePath)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers for %s: %w", scope, err)
	}
	return applyDisabledMCPServers(servers, disabled), nil
}

func disabledMCPServerNames(servers []MCPServer) []string {
	names := make([]string, 0)
	for _, server := range servers {
		if server.Disabled {
			names = append(names, server.Name)
		}
	}
	return names
}

func applyDisabledMCPServers(servers []MCPServer, disabled []string) []MCPServer {
	disabledSet := make(map[string]bool, len(disabled))
	for _, name := range disabled {
		disabledSet[name] = true
	}
	out := make([]MCPServer, 0, len(servers))
	for _, server := range servers {
		next := server
		next.Disabled = disabledSet[server.Name]
		out = append(out, next)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func mcpServerExists(servers []MCPServer, name string) bool {
	for _, server := range servers {
		if server.Name == name {
			return true
		}
	}
	return false
}

// buildCodexMCPServersForThread builds the thread/start overlay for Codex,
// reading disabled state from SQLite. A nil return means no per-thread snapshot
// exists and Codex should use native discovery. Disabled names are emitted as
// enabled:false because Codex deep-merges config; omission would inherit and
// enable the global server.
func (a *App) buildCodexMCPServersForThread(t store.Thread) (map[string]any, error) {
	disabled, snapshotted, err := a.store.GetDisabledMcpServers(t.ID)
	if err != nil {
		return nil, err
	}
	if !snapshotted {
		return nil, nil
	}
	disabledSet := make(map[string]bool, len(disabled))
	for _, name := range disabled {
		disabledSet[name] = true
	}
	st, err := a.codexConfig()
	if err != nil {
		return nil, err
	}
	servers, err := st.ListServers()
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, srv := range servers {
		if disabledSet[srv.Name] {
			out[srv.Name] = codex.DisabledMCPServer()
			continue
		}
		spec := map[string]any{}
		switch srv.Transport {
		case "stdio":
			spec["command"] = srv.Command
			if len(srv.Args) > 0 {
				spec["args"] = srv.Args
			}
			if len(srv.Env) > 0 {
				spec["env"] = srv.Env
			}
		case "streamable_http":
			spec["url"] = srv.URL
			if len(srv.HTTPHeaders) > 0 {
				spec["http_headers"] = srv.HTTPHeaders
			}
			if srv.BearerTokenEnv != "" {
				spec["bearer_token_env_var"] = srv.BearerTokenEnv
			}
		default:
			continue
		}
		out[srv.Name] = spec
	}
	return out, nil
}

// mutateDisabledSet returns a copy of the disabled set with name added
// (disabled=true) or removed (disabled=false).
func mutateDisabledSet(current []string, name string, disabled bool) []string {
	out := make([]string, 0, len(current)+1)
	for _, n := range current {
		if n != name {
			out = append(out, n)
		}
	}
	if disabled {
		out = append(out, name)
	}
	return out
}
