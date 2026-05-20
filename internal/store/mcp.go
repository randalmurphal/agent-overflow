package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// MCP server name pattern: lowercase letters / digits / underscores /
// hyphens, starting with a letter. The name is projected into Claude's
// tool naming convention `mcp__<server>__<tool>`, which is split on
// double-underscore — so a server name containing `__` would corrupt
// the parse on the wire. Anchored regex enforces it at the persistence
// boundary so the rule can't drift.
var mcpServerNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func normalizeMCPServer(server MCPServer) (MCPServer, error) {
	server.Name = strings.TrimSpace(server.Name)
	server.Transport = strings.TrimSpace(strings.ToLower(server.Transport))
	server.Command = strings.TrimSpace(server.Command)
	server.URL = strings.TrimSpace(server.URL)
	server.BearerEnv = strings.TrimSpace(server.BearerEnv)

	if server.Name == "" {
		return MCPServer{}, fmt.Errorf("store: mcp server name cannot be empty")
	}
	if !mcpServerNamePattern.MatchString(server.Name) {
		return MCPServer{}, fmt.Errorf("store: mcp server name %q must match %s", server.Name, mcpServerNamePattern)
	}
	if strings.Contains(server.Name, "__") {
		return MCPServer{}, fmt.Errorf("store: mcp server name %q cannot contain double underscores (collides with Claude tool naming)", server.Name)
	}

	switch server.Transport {
	case "stdio":
		if server.Command == "" {
			return MCPServer{}, fmt.Errorf("store: mcp stdio server %q requires command", server.Name)
		}
		server.URL = ""
		server.Headers = nil
		server.BearerEnv = ""
	case "http", "sse":
		if server.URL == "" {
			return MCPServer{}, fmt.Errorf("store: mcp %s server %q requires url", server.Transport, server.Name)
		}
		server.Command = ""
		server.Args = nil
		server.Env = nil
	default:
		return MCPServer{}, fmt.Errorf("store: unsupported mcp transport %q", server.Transport)
	}
	return server, nil
}

func encodeArgs(args []string) string {
	if len(args) == 0 {
		return "[]"
	}
	data, _ := json.Marshal(args)
	return string(data)
}

func encodeStringMap(values map[string]string) string {
	if len(values) == 0 {
		return "{}"
	}
	data, _ := json.Marshal(values)
	return string(data)
}

func decodeArgs(raw string) ([]string, error) {
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode mcp server args: %w", err)
	}
	return out, nil
}

func decodeStringMap(raw string) (map[string]string, error) {
	if raw == "" || raw == "{}" {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode mcp server map: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// CreateMCPServer inserts a library row. The id is the caller's
// responsibility (uuid in practice). Returns the persisted row so the
// caller doesn't need to re-fetch to surface created_at/updated_at to
// the frontend.
func (s *Store) CreateMCPServer(server MCPServer) (MCPServer, error) {
	normalized, err := normalizeMCPServer(server)
	if err != nil {
		return MCPServer{}, err
	}
	if normalized.ID == "" {
		return MCPServer{}, fmt.Errorf("store: mcp server id cannot be empty")
	}
	now := nowMillis()
	if normalized.CreatedAt == 0 {
		normalized.CreatedAt = now
	}
	normalized.UpdatedAt = now

	_, err = s.db.Exec(
		`INSERT INTO mcp_servers (
			id, name, transport, command, args_json, env_json,
			url, headers_json, bearer_env, enabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		normalized.ID, normalized.Name, normalized.Transport,
		normalized.Command, encodeArgs(normalized.Args), encodeStringMap(normalized.Env),
		normalized.URL, encodeStringMap(normalized.Headers), normalized.BearerEnv,
		boolToInt(normalized.Enabled), normalized.CreatedAt, normalized.UpdatedAt,
	)
	if err != nil {
		return MCPServer{}, fmt.Errorf("store: create mcp server %s: %w", normalized.Name, err)
	}
	return normalized, nil
}

// UpdateMCPServer replaces every column except id + created_at on the
// existing row. Caller passes the full desired state; partial-update
// callers should fetch-merge-write at the binding boundary so the
// store stays a flat CRUD layer.
func (s *Store) UpdateMCPServer(server MCPServer) (MCPServer, error) {
	normalized, err := normalizeMCPServer(server)
	if err != nil {
		return MCPServer{}, err
	}
	if normalized.ID == "" {
		return MCPServer{}, fmt.Errorf("store: mcp server id cannot be empty")
	}
	normalized.UpdatedAt = nowMillis()

	res, err := s.db.Exec(
		`UPDATE mcp_servers SET
			name = ?, transport = ?, command = ?, args_json = ?, env_json = ?,
			url = ?, headers_json = ?, bearer_env = ?, enabled = ?, updated_at = ?
		 WHERE id = ?`,
		normalized.Name, normalized.Transport, normalized.Command,
		encodeArgs(normalized.Args), encodeStringMap(normalized.Env),
		normalized.URL, encodeStringMap(normalized.Headers), normalized.BearerEnv,
		boolToInt(normalized.Enabled), normalized.UpdatedAt, normalized.ID,
	)
	if err != nil {
		return MCPServer{}, fmt.Errorf("store: update mcp server %s: %w", normalized.Name, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return MCPServer{}, fmt.Errorf("store: update mcp server rows: %w", err)
	}
	if rows == 0 {
		return MCPServer{}, sql.ErrNoRows
	}
	// Re-read created_at so the caller can echo the full row.
	row := s.db.QueryRow(`SELECT created_at FROM mcp_servers WHERE id = ?`, normalized.ID)
	if err := row.Scan(&normalized.CreatedAt); err != nil {
		return MCPServer{}, fmt.Errorf("store: re-read mcp server: %w", err)
	}
	return normalized, nil
}

// DeleteMCPServer removes the row plus any thread_mcp_servers entries
// (FK ON DELETE CASCADE). Missing rows are a no-op so stale UI state
// can reconcile without an error.
func (s *Store) DeleteMCPServer(id string) error {
	_, err := s.db.Exec(`DELETE FROM mcp_servers WHERE id = ?`, strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("store: delete mcp server %s: %w", id, err)
	}
	return nil
}

// GetMCPServer reads a single library row. Returns sql.ErrNoRows when
// the id is unknown.
func (s *Store) GetMCPServer(id string) (MCPServer, error) {
	row := s.db.QueryRow(
		`SELECT id, name, transport, command, args_json, env_json,
		        url, headers_json, bearer_env, enabled, created_at, updated_at
		   FROM mcp_servers WHERE id = ?`,
		strings.TrimSpace(id),
	)
	return scanMCPServer(row)
}

// GetMCPServerByName is the name-keyed lookup used when projecting
// merge precedence: design MCP names are well-known and the user
// library can declare the same name. The caller decides what to do
// about the collision (currently: design wins).
func (s *Store) GetMCPServerByName(name string) (MCPServer, error) {
	row := s.db.QueryRow(
		`SELECT id, name, transport, command, args_json, env_json,
		        url, headers_json, bearer_env, enabled, created_at, updated_at
		   FROM mcp_servers WHERE name = ?`,
		strings.TrimSpace(name),
	)
	return scanMCPServer(row)
}

// ListMCPServers returns the full library newest-first. The library is
// tiny in practice (a handful of servers per user); we don't paginate.
func (s *Store) ListMCPServers() ([]MCPServer, error) {
	rows, err := s.db.Query(
		`SELECT id, name, transport, command, args_json, env_json,
		        url, headers_json, bearer_env, enabled, created_at, updated_at
		   FROM mcp_servers
		  ORDER BY name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list mcp servers: %w", err)
	}
	defer rows.Close()
	var servers []MCPServer
	for rows.Next() {
		server, err := scanMCPServer(rows)
		if err != nil {
			return nil, err
		}
		servers = append(servers, server)
	}
	return servers, rows.Err()
}

func scanMCPServer(scanner interface{ Scan(...any) error }) (MCPServer, error) {
	var (
		server      MCPServer
		argsRaw     string
		envRaw      string
		headersRaw  string
		enabled     int
	)
	if err := scanner.Scan(
		&server.ID, &server.Name, &server.Transport, &server.Command,
		&argsRaw, &envRaw, &server.URL, &headersRaw, &server.BearerEnv,
		&enabled, &server.CreatedAt, &server.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return MCPServer{}, err
		}
		return MCPServer{}, fmt.Errorf("store: scan mcp server: %w", err)
	}
	args, err := decodeArgs(argsRaw)
	if err != nil {
		return MCPServer{}, err
	}
	env, err := decodeStringMap(envRaw)
	if err != nil {
		return MCPServer{}, err
	}
	headers, err := decodeStringMap(headersRaw)
	if err != nil {
		return MCPServer{}, err
	}
	server.Args = args
	server.Env = env
	server.Headers = headers
	server.Enabled = enabled != 0
	return server, nil
}

// ListThreadMCPServerIDs returns the enabled server ids for one thread,
// preserving alphabetical-by-name order so the rendered config stays
// deterministic across reloads.
func (s *Store) ListThreadMCPServerIDs(threadID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT t.server_id
		   FROM thread_mcp_servers t
		   JOIN mcp_servers s ON s.id = t.server_id
		  WHERE t.thread_id = ?
		  ORDER BY s.name ASC`,
		strings.TrimSpace(threadID),
	)
	if err != nil {
		return nil, fmt.Errorf("store: list thread mcp servers: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan thread mcp server: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetThreadMCPServers replaces the per-thread enable list atomically.
// Empty list is allowed (clears the row set). Server ids that don't
// exist in the library cause the transaction to fail by FK constraint.
func (s *Store) SetThreadMCPServers(threadID string, serverIDs []string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return fmt.Errorf("store: thread id required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin set thread mcp servers: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM thread_mcp_servers WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("store: clear thread mcp servers: %w", err)
	}
	now := nowMillis()
	for _, id := range serverIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO thread_mcp_servers (thread_id, server_id, created_at) VALUES (?, ?, ?)`,
			threadID, id, now,
		); err != nil {
			return fmt.Errorf("store: insert thread mcp server %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// GetMCPThreadProfile returns the single-row "last selected" seed used
// to populate freshly created threads. sql.ErrNoRows when never set.
func (s *Store) GetMCPThreadProfile() (MCPThreadProfile, error) {
	var raw string
	var updatedAt int64
	err := s.db.QueryRow(
		`SELECT server_ids_json, updated_at FROM mcp_thread_profile WHERE id = 1`,
	).Scan(&raw, &updatedAt)
	if err != nil {
		return MCPThreadProfile{}, err
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return MCPThreadProfile{}, fmt.Errorf("store: decode mcp thread profile: %w", err)
	}
	return MCPThreadProfile{ServerIDs: ids, UpdatedAt: updatedAt}, nil
}

// SetMCPThreadProfile upserts the seed row. Empty list is allowed (the
// user toggled everything off; new threads should mirror that).
func (s *Store) SetMCPThreadProfile(serverIDs []string) error {
	if serverIDs == nil {
		serverIDs = []string{}
	}
	data, err := json.Marshal(serverIDs)
	if err != nil {
		return fmt.Errorf("store: encode mcp thread profile: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO mcp_thread_profile (id, server_ids_json, updated_at)
		 VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		    server_ids_json = excluded.server_ids_json,
		    updated_at = excluded.updated_at`,
		string(data), nowMillis(),
	)
	if err != nil {
		return fmt.Errorf("store: upsert mcp thread profile: %w", err)
	}
	return nil
}
