package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func normalizeMCPDefaultsProvider(providerName string) (string, error) {
	providerName = strings.TrimSpace(providerName)
	if _, ok := legalProviders[providerName]; !ok {
		return "", fmt.Errorf("%w: %q", ErrInvalidProvider, providerName)
	}
	return providerName, nil
}

func normalizeMCPDefaultsScope(providerName, workspacePath string) (provider string, workspaceScope string, err error) {
	providerName, err = normalizeMCPDefaultsProvider(providerName)
	if err != nil {
		return "", "", err
	}
	if providerName == "codex" {
		return providerName, "", nil
	}
	return providerName, strings.TrimSpace(workspacePath), nil
}

func normalizeDisabledMCPServerNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// GetNewThreadDisabledMCPServers returns the disabled set used when a new
// thread snapshots its initial MCP state for this provider/workspace scope.
// found=false means the app should fall back to provider-native config for
// backwards compatibility.
func (s *Store) GetNewThreadDisabledMCPServers(providerName, workspacePath string) (names []string, found bool, err error) {
	providerName, workspaceScope, err := normalizeMCPDefaultsScope(providerName, workspacePath)
	if err != nil {
		return nil, false, err
	}
	var raw string
	err = s.reader().QueryRow(
		`SELECT disabled_servers
		   FROM new_thread_mcp_defaults
		  WHERE provider = ?
		    AND workspace_path = ?`,
		providerName, workspaceScope,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: get new-thread MCP defaults for %s/%q: %w", providerName, workspaceScope, err)
	}
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, false, fmt.Errorf("store: decode new-thread MCP defaults for %s/%q: %w", providerName, workspaceScope, err)
	}
	return normalizeDisabledMCPServerNames(names), true, nil
}

// SetNewThreadDisabledMCPServers persists the disabled set used by future
// thread creation for this provider/workspace scope. It intentionally does not
// mutate existing threads.
func (s *Store) SetNewThreadDisabledMCPServers(providerName, workspacePath string, names []string) error {
	providerName, workspaceScope, err := normalizeMCPDefaultsScope(providerName, workspacePath)
	if err != nil {
		return err
	}
	names = normalizeDisabledMCPServerNames(names)
	data, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("store: encode new-thread MCP defaults for %s/%q: %w", providerName, workspaceScope, err)
	}
	_, err = s.db.Exec(
		`INSERT INTO new_thread_mcp_defaults (provider, workspace_path, disabled_servers, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(provider, workspace_path) DO UPDATE SET
		 	disabled_servers = excluded.disabled_servers,
		 	updated_at = excluded.updated_at`,
		providerName, workspaceScope, string(data), nowMillis(),
	)
	if err != nil {
		return fmt.Errorf("store: set new-thread MCP defaults for %s/%q: %w", providerName, workspaceScope, err)
	}
	return nil
}

// MutateNewThreadDisabledMCPServers applies an atomic read/modify/write for
// one new-thread MCP defaults scope. fallback is used only when no AO defaults
// row exists yet.
func (s *Store) MutateNewThreadDisabledMCPServers(providerName, workspacePath string, fallback []string, mutate func([]string) []string) ([]string, error) {
	providerName, workspaceScope, err := normalizeMCPDefaultsScope(providerName, workspacePath)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: mutate new-thread MCP defaults for %s/%q: begin: %w", providerName, workspaceScope, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	names := normalizeDisabledMCPServerNames(fallback)
	var raw string
	err = tx.QueryRow(
		`SELECT disabled_servers
		   FROM new_thread_mcp_defaults
		  WHERE provider = ?
		    AND workspace_path = ?`,
		providerName, workspaceScope,
	).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("store: mutate new-thread MCP defaults for %s/%q: load: %w", providerName, workspaceScope, err)
	}
	if err == nil {
		if err := json.Unmarshal([]byte(raw), &names); err != nil {
			return nil, fmt.Errorf("store: mutate new-thread MCP defaults for %s/%q: decode: %w", providerName, workspaceScope, err)
		}
		names = normalizeDisabledMCPServerNames(names)
	}

	updated := normalizeDisabledMCPServerNames(mutate(names))
	data, err := json.Marshal(updated)
	if err != nil {
		return nil, fmt.Errorf("store: mutate new-thread MCP defaults for %s/%q: encode: %w", providerName, workspaceScope, err)
	}
	_, err = tx.Exec(
		`INSERT INTO new_thread_mcp_defaults (provider, workspace_path, disabled_servers, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(provider, workspace_path) DO UPDATE SET
		 	disabled_servers = excluded.disabled_servers,
		 	updated_at = excluded.updated_at`,
		providerName, workspaceScope, string(data), nowMillis(),
	)
	if err != nil {
		return nil, fmt.Errorf("store: mutate new-thread MCP defaults for %s/%q: save: %w", providerName, workspaceScope, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: mutate new-thread MCP defaults for %s/%q: commit: %w", providerName, workspaceScope, err)
	}
	committed = true
	return updated, nil
}

// RemoveNewThreadDisabledMCPServer removes a deleted server from every default
// set so re-adding a server later does not inherit stale disabled state.
func (s *Store) RemoveNewThreadDisabledMCPServer(providerName, name string) error {
	providerName, err := normalizeMCPDefaultsProvider(providerName)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: remove new-thread MCP default %s/%q: begin: %w", providerName, name, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	rows, err := tx.Query(
		`SELECT workspace_path, disabled_servers
		   FROM new_thread_mcp_defaults
		  WHERE provider = ?`,
		providerName,
	)
	if err != nil {
		return fmt.Errorf("store: remove new-thread MCP default %s/%q: list scopes: %w", providerName, name, err)
	}
	var updates []struct {
		workspacePath string
		names         []string
	}
	for rows.Next() {
		var workspacePath, raw string
		if err := rows.Scan(&workspacePath, &raw); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: remove new-thread MCP default %s/%q: scan scope: %w", providerName, name, err)
		}
		var names []string
		if err := json.Unmarshal([]byte(raw), &names); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: remove new-thread MCP default %s/%q: decode scope %q: %w", providerName, name, workspacePath, err)
		}
		next := make([]string, 0, len(names))
		for _, existing := range normalizeDisabledMCPServerNames(names) {
			if existing != name {
				next = append(next, existing)
			}
		}
		updates = append(updates, struct {
			workspacePath string
			names         []string
		}{workspacePath: workspacePath, names: next})
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: remove new-thread MCP default %s/%q: close scopes: %w", providerName, name, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: remove new-thread MCP default %s/%q: iterate scopes: %w", providerName, name, err)
	}
	for _, update := range updates {
		data, err := json.Marshal(update.names)
		if err != nil {
			return fmt.Errorf("store: remove new-thread MCP default %s/%q: encode scope %q: %w", providerName, name, update.workspacePath, err)
		}
		if _, err := tx.Exec(
			`UPDATE new_thread_mcp_defaults
			    SET disabled_servers = ?,
			        updated_at = ?
			  WHERE provider = ?
			    AND workspace_path = ?`,
			string(data), nowMillis(), providerName, update.workspacePath,
		); err != nil {
			return fmt.Errorf("store: remove new-thread MCP default %s/%q: update scope %q: %w", providerName, name, update.workspacePath, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: remove new-thread MCP default %s/%q: commit: %w", providerName, name, err)
	}
	committed = true
	return nil
}
