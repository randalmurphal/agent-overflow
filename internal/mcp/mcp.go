package mcp

import (
	"fmt"
	"sort"

	"agent-overflow/internal/store"
)

// Status mirrors the wire-level state both providers report for MCP
// connections, plus the AO-side "haven't probed yet" pseudo-state.
// String values match the JSON the bindings expose to the frontend.
type Status string

const (
	StatusUnknown   Status = "unknown"
	StatusReady     Status = "ready"
	StatusNeedsAuth Status = "needs-auth"
	StatusFailed    Status = "failed"
)

// Transport names mirror the SQLite CHECK constraint values.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
	TransportSSE   = "sse"
)

// RenderClaudeSpec projects a stored library row into the JSON map
// the Claude CLI's --mcp-config flag expects. The wrapping
// `withClaudeTransportType` helper in internal/provider/claude
// re-stamps the "type" discriminator so the same value composes with
// any caller-supplied design MCP entries without duplicating that
// logic here. Stdio servers get command/args/env; http and sse get
// url/headers, with bearer_env folded into the headers under
// "Authorization" so the provider doesn't have to know about the
// indirection convention.
func RenderClaudeSpec(server store.MCPServer) (map[string]any, error) {
	out := map[string]any{}
	switch server.Transport {
	case TransportStdio:
		if server.Command == "" {
			return nil, fmt.Errorf("mcp: stdio server %q missing command", server.Name)
		}
		out["type"] = "stdio"
		out["command"] = server.Command
		if len(server.Args) > 0 {
			out["args"] = append([]string{}, server.Args...)
		}
		if len(server.Env) > 0 {
			out["env"] = copyStringMap(server.Env)
		}
	case TransportHTTP, TransportSSE:
		if server.URL == "" {
			return nil, fmt.Errorf("mcp: %s server %q missing url", server.Transport, server.Name)
		}
		out["type"] = server.Transport
		out["url"] = server.URL
		headers := copyStringMap(server.Headers)
		if server.BearerEnv != "" {
			if headers == nil {
				headers = map[string]string{}
			}
			// Claude expands ${VAR} natively in header values; the user
			// supplies the env-var name and AO never sees the secret.
			headers["Authorization"] = "Bearer ${" + server.BearerEnv + "}"
		}
		if len(headers) > 0 {
			out["headers"] = headers
		}
	default:
		return nil, fmt.Errorf("mcp: unsupported transport %q for %s", server.Transport, server.Name)
	}
	return out, nil
}

// RenderCodexSpec projects a stored row into the shape Codex's
// thread/start configOverrides["mcp_servers"] entry expects. Codex's
// serde rejects unknown fields on the StreamableHttp variant and uses
// `transport.type = "streamable_http" | "stdio"` as the discriminator
// rather than a flat `type` key. http and sse on the AO side both map
// to streamable_http here; AO's UI keeps them distinct because Claude
// honors the difference, but Codex collapses them upstream.
func RenderCodexSpec(server store.MCPServer) (map[string]any, error) {
	switch server.Transport {
	case TransportStdio:
		if server.Command == "" {
			return nil, fmt.Errorf("mcp: stdio server %q missing command", server.Name)
		}
		spec := map[string]any{
			"command": server.Command,
		}
		if len(server.Args) > 0 {
			spec["args"] = append([]string{}, server.Args...)
		}
		if len(server.Env) > 0 {
			spec["env"] = copyStringMap(server.Env)
		}
		return spec, nil
	case TransportHTTP, TransportSSE:
		if server.URL == "" {
			return nil, fmt.Errorf("mcp: %s server %q missing url", server.Transport, server.Name)
		}
		spec := map[string]any{
			"url": server.URL,
		}
		if len(server.Headers) > 0 {
			spec["http_headers"] = copyStringMap(server.Headers)
		}
		if server.BearerEnv != "" {
			spec["bearer_token_env_var"] = server.BearerEnv
		}
		return spec, nil
	default:
		return nil, fmt.Errorf("mcp: unsupported transport %q for %s", server.Transport, server.Name)
	}
}

// MergeResult is the output of MergeForProvider — a ready-to-inject
// map keyed by server name plus the list of names that collided
// between design-injected and user-selected entries. The caller
// surfaces collisions as a thread-scoped warning; design wins so the
// system-controlled MCP can't be shadowed by a user library entry.
type MergeResult struct {
	Servers    map[string]any
	Collisions []string
}

// Provider names mirror the values in store.Thread.Provider.
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
)

// MergeForProvider takes the design-mode map (already provider-shape,
// because design owns its own rendering) plus the user library and
// the per-thread selection of server ids, and produces the merged map
// the caller assigns onto Config.MCPServers. User entries render through
// RenderClaudeSpec / RenderCodexSpec; the caller chooses which by
// passing the provider name. A user entry with the same name as a
// design entry is silently dropped and the name is reported in
// Collisions.
//
// Codex per-thread `configOverrides["mcp_servers"]` is MERGED with the
// disk config (`~/.codex/config.toml`), not replaced. If AO has ever
// written a server to disk (for the OAuth flow), any thread that does
// NOT select that server would still see it leak in from disk. To
// enforce per-thread isolation we emit `{enabled: false}` for every
// AO-library entry whose name is neither selected for the thread nor
// owned by a design entry. Claude's `--mcp-config` is replace
// semantics, so this masking is a no-op for it.
func MergeForProvider(provider string, designServers map[string]any, library []store.MCPServer, selectedIDs []string) (MergeResult, error) {
	result := MergeResult{
		Servers: map[string]any{},
	}
	for name, spec := range designServers {
		result.Servers[name] = spec
	}

	selectedSet := make(map[string]struct{}, len(selectedIDs))
	for _, id := range selectedIDs {
		selectedSet[id] = struct{}{}
	}
	libraryByID := make(map[string]store.MCPServer, len(library))
	for _, srv := range library {
		libraryByID[srv.ID] = srv
	}

	// Render selected user servers in stable id order so the wire
	// output is deterministic across runs.
	for _, id := range selectedIDs {
		server, ok := libraryByID[id]
		if !ok || !server.Enabled {
			continue
		}
		if _, claimed := result.Servers[server.Name]; claimed {
			result.Collisions = append(result.Collisions, server.Name)
			continue
		}
		var (
			spec map[string]any
			err  error
		)
		switch provider {
		case ProviderClaude:
			spec, err = RenderClaudeSpec(server)
		case ProviderCodex:
			spec, err = RenderCodexSpec(server)
			if err == nil {
				// Force enabled: true on the overlay so a thread-level
				// AO selection wins over a user-side `enabled = false`
				// on the matching disk row. Codex's recursive overlay
				// merge keeps disk leaves that we don't mention, so
				// omitting `enabled` here would silently leave a
				// disk-disabled server disabled even after the user
				// selected it through AO.
				spec["enabled"] = true
			}
		default:
			return MergeResult{}, fmt.Errorf("mcp: unsupported provider %q", provider)
		}
		if err != nil {
			return MergeResult{}, err
		}
		result.Servers[server.Name] = spec
	}

	// Codex-only masking: hide any library entry the thread did not
	// select so a sibling thread's disk-write doesn't leak in. Library
	// entries already rendered (selected) or claimed by a design entry
	// are skipped. The masked overlay must include the full transport
	// spec — Codex's RawMcpServerConfig deserializer requires either
	// `command` (stdio) or `url` (streamable_http) to determine which
	// transport variant to materialize; a bare `{enabled: false}` would
	// fail with "invalid transport". The recursive merge in
	// codex_config::merge_toml_values folds our spec into whatever the
	// on-disk row contains, with overlay values winning, then
	// `enabled = false` flows into McpServerConfig.enabled and the
	// runtime filter `mcp_servers.values().filter(|s| s.enabled())`
	// drops the server before tool registration. Walking the library
	// in alphabetical name order so the masked overlay stays
	// deterministic.
	if provider == ProviderCodex {
		sortedLibrary := append([]store.MCPServer{}, library...)
		sort.Slice(sortedLibrary, func(i, j int) bool { return sortedLibrary[i].Name < sortedLibrary[j].Name })
		for _, server := range sortedLibrary {
			if _, selected := selectedSet[server.ID]; selected {
				continue
			}
			if _, claimed := result.Servers[server.Name]; claimed {
				continue
			}
			spec, err := RenderCodexSpec(server)
			if err != nil {
				// A library row that fails to render (bad transport, missing
				// fields) can't be masked because the deserializer would
				// reject the overlay. Skip silently — the entry stays
				// visible if it happens to be on disk, but a malformed row
				// is already broken everywhere else too.
				continue
			}
			spec["enabled"] = false
			result.Servers[server.Name] = spec
		}
	}

	// Deterministic collision ordering so any caller logging the list
	// gets a stable output across runs.
	sort.Strings(result.Collisions)
	return result, nil
}

// FilterEnabled returns only servers marked enabled at the library
// level. Per-thread enable lists feed through this filter so a
// library kill-switch (`enabled = 0`) hides the server from every
// thread, including ones that previously had it selected.
func FilterEnabled(servers []store.MCPServer) []store.MCPServer {
	out := make([]store.MCPServer, 0, len(servers))
	for _, server := range servers {
		if server.Enabled {
			out = append(out, server)
		}
	}
	return out
}

// SelectServersByID returns servers in id order for the given ids.
// Unknown ids and library-disabled servers are silently skipped — the
// thread enable list can outlive a deletion or a library-level toggle
// without breaking session launch.
func SelectServersByID(library []store.MCPServer, ids []string) []store.MCPServer {
	if len(ids) == 0 || len(library) == 0 {
		return nil
	}
	byID := make(map[string]store.MCPServer, len(library))
	for _, server := range library {
		byID[server.ID] = server
	}
	out := make([]store.MCPServer, 0, len(ids))
	for _, id := range ids {
		server, ok := byID[id]
		if !ok || !server.Enabled {
			continue
		}
		out = append(out, server)
	}
	return out
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
