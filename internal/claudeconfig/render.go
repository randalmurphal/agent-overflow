package claudeconfig

import (
	"fmt"
)

// RenderForCLI projects a Server into the JSON shape Claude accepts
// both at process spawn (`--mcp-config <json>`) and over the live
// `mcp_set_servers` control_request. The two wire surfaces use the
// same per-entry shape: stdio → {type, command, args, env}; http/sse
// → {type, url, headers}. The transport discriminator is always
// written explicitly so a future Claude that flips its default can't
// silently re-route the entry.
func (s Server) RenderForCLI() (map[string]any, error) {
	out := map[string]any{"type": s.Transport}
	switch s.Transport {
	case TransportStdio:
		if s.Command == "" {
			return nil, fmt.Errorf("claudeconfig: stdio server %q missing command", s.Name)
		}
		out["command"] = s.Command
		if len(s.Args) > 0 {
			out["args"] = append([]string{}, s.Args...)
		}
		if len(s.Env) > 0 {
			out["env"] = copyStringMap(s.Env)
		}
	case TransportHTTP, TransportSSE:
		if s.URL == "" {
			return nil, fmt.Errorf("claudeconfig: %s server %q missing url", s.Transport, s.Name)
		}
		out["url"] = s.URL
		if len(s.Headers) > 0 {
			out["headers"] = copyStringMap(s.Headers)
		}
	default:
		return nil, fmt.Errorf("claudeconfig: unsupported transport %q for %s", s.Transport, s.Name)
	}
	return out, nil
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
