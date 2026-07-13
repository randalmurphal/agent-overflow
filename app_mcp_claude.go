package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/store"
)

// ---------------- Claude implementation ----------------

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
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: target.Name})
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
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: target.Name})
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
	if a.store != nil {
		if err := a.store.RemoveNewThreadDisabledMCPServer(mcpProviderClaude, name); err != nil {
			return err
		}
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: name})
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
	providerName, workspacePath, name, fallback, err := a.prepareNewThreadMCPDisabledUpdate(thread.Provider, workspacePath, name)
	if err != nil {
		return err
	}
	if err := st.SetDisabled(workspacePath, name, disabled); err != nil {
		return fmt.Errorf("set claude mcp disabled: %w", err)
	}
	if err := a.persistNewThreadMCPDisabledUpdate(providerName, workspacePath, name, !disabled, fallback); err != nil {
		return err
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: name})
	if thread.Mode == "design" {
		// Design Claude sessions launched with --strict-mcp-config — user
		// MCP isn't visible to them regardless of the disabled flag.
		// Skip the live reconcile so we don't spend an RPC on a no-op.
		return nil
	}
	hasSession := a.hasActiveSession(thread.ID)
	go func() {
		// SQLite dual-write and reconcile run inside the per-thread lock
		// so rapid toggles serialize correctly.
		unlock := a.threadLocks().Lock(thread.ID)
		defer unlock()
		current, err := a.ensureDisabledMcpSnapshot(thread.ID, thread.Provider, workspacePath)
		if err != nil {
			log.Printf("mcp: thread %s claude snapshot disabled: %v", thread.ID, err)
			return
		}
		updated := mutateDisabledSet(current, name, disabled)
		if err := a.store.SetDisabledMcpServers(thread.ID, updated); err != nil {
			log.Printf("mcp: thread %s claude set disabled: %v", thread.ID, err)
			return
		}
		if !hasSession {
			return
		}
		err = a.reconcileClaudeMCPLive(thread)
		if err == nil {
			return
		}
		log.Printf("mcp: thread %s claude live reconcile: %v", thread.ID, err)
		if a.lifeCtx().Err() != nil {
			return
		}
		a.emitErrorToThread(thread.ID, fmt.Sprintf("mcp: live reconcile failed: %s", sanitizeMCPError(err.Error())))
	}()
	return nil
}

// reconcileClaudeMCPOnInit pushes the per-thread MCP disabled set after a
// Claude session initializes. Skips design threads and pre-feature threads
// whose lazy snapshot resolved to "native config already enables everything".
func (a *App) reconcileClaudeMCPOnInit(threadID string) {
	t, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("mcp: thread %s post-init reconcile: load thread: %v", threadID, err)
		return
	}
	if t.Mode == "design" {
		return
	}
	disabled, snapshotted, err := a.store.GetDisabledMcpServers(t.ID)
	if err != nil {
		log.Printf("mcp: thread %s post-init reconcile: snapshot: %v", threadID, err)
		return
	}
	if !snapshotted {
		disabled, err = a.ensureDisabledMcpSnapshot(t.ID, t.Provider, t.WorkspacePath)
		if err != nil {
			log.Printf("mcp: thread %s post-init reconcile: snapshot: %v", threadID, err)
			return
		}
		if len(disabled) == 0 {
			return
		}
	}
	if snapshotted && len(disabled) == 0 {
		nativeDisabled := a.snapshotProviderDisabledMCPServers(t.Provider, t.WorkspacePath)
		if len(*nativeDisabled) == 0 {
			return
		}
	}
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	if err := a.reconcileClaudeMCPLive(t); err != nil {
		log.Printf("mcp: thread %s post-init reconcile: %v", threadID, err)
	}
}

// reconcileClaudeMCPLive pushes the thread-specific user MCP set to the
// live Claude session via mcp_set_servers. Reads the disabled set from
// SQLite (per-thread state) instead of the workspace config file.
func (a *App) reconcileClaudeMCPLive(thread store.Thread) error {
	sess, ok := a.sessionManager().get(thread.ID)
	if !ok || sess.claude == nil {
		return nil
	}
	disabled, err := a.ensureDisabledMcpSnapshot(thread.ID, thread.Provider, thread.WorkspacePath)
	if err != nil {
		return fmt.Errorf("ensure mcp snapshot for reconcile: %w", err)
	}
	disabledSet := make(map[string]bool, len(disabled))
	for _, name := range disabled {
		disabledSet[name] = true
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
			continue
		}
		if disabledSet[srv.Name] {
			continue
		}
		spec, err := srv.RenderForCLI()
		if err != nil {
			log.Printf("mcp: thread %s render %q for live reconcile: %v", thread.ID, srv.Name, err)
			continue
		}
		target[srv.Name] = spec
	}
	if sess.workflowChatMCP {
		spec, err := a.workflowChatMCPServerSpec(thread)
		if err != nil {
			return err
		}
		target[workflowChatMCPServerName] = spec
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), mcpLiveReconcileTimeout)
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
