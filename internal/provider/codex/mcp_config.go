package codex

// HTTPMCPServer returns Codex's provider-specific streamable-HTTP server
// config shape for a thread/start mcp_servers override.
func HTTPMCPServer(url string, headers map[string]string) map[string]any {
	spec := map[string]any{"url": url}
	if len(headers) > 0 {
		spec["http_headers"] = headers
	}
	return spec
}

// DisabledMCPServer suppresses a server inherited from config.toml. Omitting
// the name does not disable it because thread/start config is deep-merged.
func DisabledMCPServer() map[string]any { return map[string]any{"enabled": false} }
