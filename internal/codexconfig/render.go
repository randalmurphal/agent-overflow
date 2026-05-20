package codexconfig

import (
	"fmt"

	"agent-overflow/internal/mcp"
)

// RenderForOverlay projects a Server into the JSON shape Codex's
// `configOverrides["mcp_servers"]` entry expects. Stdio: {command,
// args, env}; streamable_http: {url, http_headers,
// bearer_token_env_var}. The `enabled` key is intentionally omitted
// when true — Codex defaults to enabled, and writing the implicit
// default into an overlay would mask a user-side `enabled = false`
// they set by hand for an unrelated reason.
func (s Server) RenderForOverlay() (map[string]any, error) {
	out := map[string]any{}
	switch s.Transport {
	case TransportStdio:
		if s.Command == "" {
			return nil, fmt.Errorf("codexconfig: stdio server %q missing command", s.Name)
		}
		out["command"] = s.Command
		if len(s.Args) > 0 {
			out["args"] = append([]string{}, s.Args...)
		}
		if len(s.Env) > 0 {
			out["env"] = copyStringMap(s.Env)
		}
	case TransportStreamable:
		if s.URL == "" {
			return nil, fmt.Errorf("codexconfig: streamable_http server %q missing url", s.Name)
		}
		out["url"] = s.URL
		if len(s.HTTPHeaders) > 0 {
			out["http_headers"] = copyStringMap(s.HTTPHeaders)
		}
		if s.BearerTokenEnv != "" {
			out["bearer_token_env_var"] = s.BearerTokenEnv
		}
	default:
		return nil, fmt.Errorf("codexconfig: unsupported transport %q for %s", s.Transport, s.Name)
	}
	if !s.Enabled {
		out["enabled"] = false
	}
	return out, nil
}

// ToSpec returns the mcp.Spec the probe layer consumes. Codex's
// "streamable_http" maps to the probe's TransportHTTP since the
// handshake is identical (POST initialize, parse 2xx/401).
func (s Server) ToSpec() mcp.Spec {
	probeTransport := mcp.TransportStdio
	if s.Transport == TransportStreamable {
		probeTransport = mcp.TransportHTTP
	}
	return mcp.Spec{
		Provider:  "codex",
		Name:      s.Name,
		Transport: probeTransport,
		Enabled:   s.Enabled,
		Command:   s.Command,
		Args:      append([]string{}, s.Args...),
		Env:       copyStringMap(s.Env),
		URL:       s.URL,
		Headers:   copyStringMap(s.HTTPHeaders),
		BearerEnv: s.BearerTokenEnv,
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
