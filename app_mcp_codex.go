package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"agent-overflow/internal/codexconfig"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/store"
)

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
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: target.Name})
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
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: target.Name})
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
	if a.store != nil {
		if err := a.store.RemoveNewThreadDisabledMCPServer(mcpProviderCodex, name); err != nil {
			return err
		}
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: name})
	return nil
}

func (a *App) setCodexMcpEnabled(thread store.Thread, name string, enabled bool) error {
	st, err := a.codexConfig()
	if err != nil {
		return err
	}
	providerName, workspacePath, name, fallback, err := a.prepareNewThreadMCPDisabledUpdate(thread.Provider, thread.WorkspacePath, name)
	if err != nil {
		return err
	}
	if err := st.SetEnabled(name, enabled); err != nil {
		return fmt.Errorf("set codex mcp enabled: %w", err)
	}
	if err := a.persistNewThreadMCPDisabledUpdate(providerName, workspacePath, name, enabled, fallback); err != nil {
		return err
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: name})
	go func() {
		unlock := a.threadLocks().Lock(thread.ID)
		defer unlock()
		current, err := a.ensureDisabledMcpSnapshot(thread.ID, thread.Provider, thread.WorkspacePath)
		if err != nil {
			log.Printf("mcp: thread %s codex snapshot disabled: %v", thread.ID, err)
			return
		}
		updated := mutateDisabledSet(current, name, !enabled)
		if err := a.store.SetDisabledMcpServers(thread.ID, updated); err != nil {
			log.Printf("mcp: thread %s codex set disabled: %v", thread.ID, err)
			return
		}
		sess, ok := a.sessionManager().get(thread.ID)
		if !ok || sess.codex == nil {
			return
		}
		ctx, cancel := context.WithTimeout(a.lifeCtx(), mcpLiveReconcileTimeout)
		defer cancel()
		err = sess.codex.RefreshMCPServers(ctx)
		if err == nil {
			return
		}
		log.Printf("mcp: thread %s codex live reload: %v", thread.ID, err)
		if ctx.Err() != nil {
			return
		}
		a.emitErrorToThread(thread.ID, fmt.Sprintf("mcp: live reload failed: %s", sanitizeMCPError(err.Error())))
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
