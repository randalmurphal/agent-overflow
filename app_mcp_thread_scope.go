package main

import (
	"fmt"
	"log"
	"sort"

	"agent-overflow/internal/store"
)

// snapshotDisabledMcpServers reads the current global disabled set from
// the provider config file for use as a new thread's initial snapshot.
func (a *App) snapshotDisabledMcpServers(providerName, workspacePath string) *[]string {
	var disabled []string
	switch providerName {
	case mcpProviderClaude:
		st, err := a.claudeConfig()
		if err != nil {
			log.Printf("mcp: snapshot claude disabled: %v", err)
			return &disabled
		}
		servers, err := st.ListServers(workspacePath)
		if err != nil {
			log.Printf("mcp: snapshot claude disabled: %v", err)
			return &disabled
		}
		for _, srv := range servers {
			if srv.Disabled {
				disabled = append(disabled, srv.Name)
			}
		}
	case mcpProviderCodex:
		st, err := a.codexConfig()
		if err != nil {
			log.Printf("mcp: snapshot codex disabled: %v", err)
			return &disabled
		}
		servers, err := st.ListServers()
		if err != nil {
			log.Printf("mcp: snapshot codex disabled: %v", err)
			return &disabled
		}
		for _, srv := range servers {
			if !srv.Enabled {
				disabled = append(disabled, srv.Name)
			}
		}
	}
	return &disabled
}

// ensureDisabledMcpSnapshot returns the per-thread disabled set,
// lazy-snapshotting from the global config if the thread has no
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

// ListMcpServersForThread returns the MCP server library with per-thread
// disabled state from SQLite instead of global config. Used by the
// composer toolbar popup. Settings UI continues to use ListMcpServers.
func (a *App) ListMcpServersForThread(threadID string) ([]MCPServer, error) {
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers for thread: %w", err)
	}
	disabled, err := a.ensureDisabledMcpSnapshot(t.ID, t.Provider, t.WorkspacePath)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers for thread: %w", err)
	}
	disabledSet := make(map[string]bool, len(disabled))
	for _, name := range disabled {
		disabledSet[name] = true
	}

	switch t.Provider {
	case "claude":
		st, err := a.claudeConfig()
		if err != nil {
			return nil, err
		}
		servers, err := st.ListServers(t.WorkspacePath)
		if err != nil {
			return nil, fmt.Errorf("list claude mcp servers for thread: %w", err)
		}
		out := make([]MCPServer, 0, len(servers))
		for _, srv := range servers {
			ws := claudeServerToWire(srv)
			ws.Disabled = disabledSet[srv.Name]
			out = append(out, ws)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil

	case "codex":
		st, err := a.codexConfig()
		if err != nil {
			return nil, err
		}
		servers, err := st.ListServers()
		if err != nil {
			return nil, fmt.Errorf("list codex mcp servers for thread: %w", err)
		}
		out := make([]MCPServer, 0, len(servers))
		for _, srv := range servers {
			ws := codexServerToWire(srv)
			ws.Disabled = disabledSet[srv.Name]
			out = append(out, ws)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil

	default:
		return nil, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, t.Provider)
	}
}

// buildCodexMCPServersForThread builds the full enabled server set for
// a Codex thread, reading disabled state from SQLite. The returned map
// replaces native Codex config discovery via config.mcp_servers in
// thread start params. Returns nil when nothing is disabled (native
// discovery gives the correct result).
func (a *App) buildCodexMCPServersForThread(t store.Thread) (map[string]any, error) {
	disabled, err := a.ensureDisabledMcpSnapshot(t.ID, t.Provider, t.WorkspacePath)
	if err != nil {
		return nil, err
	}
	if len(disabled) == 0 {
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

// mutateDisabledSet returns a copy of the disabled set with name
// added (disabled=true) or removed (disabled=false).
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
