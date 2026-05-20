package claudeconfig

import (
	"reflect"
	"testing"

	"agent-overflow/internal/mcp"
)

func TestRenderForCLIStdio(t *testing.T) {
	srv := Server{
		Name:      "everything",
		Source:    SourceUser,
		Transport: TransportStdio,
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-everything"},
		Env:       map[string]string{"FOO": "bar"},
	}
	got, err := srv.RenderForCLI()
	if err != nil {
		t.Fatalf("RenderForCLI: %v", err)
	}
	want := map[string]any{
		"type":    "stdio",
		"command": "npx",
		"args":    []string{"-y", "@modelcontextprotocol/server-everything"},
		"env":     map[string]string{"FOO": "bar"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RenderForCLI stdio:\n got = %#v\n want = %#v", got, want)
	}
}

func TestRenderForCLIHTTP(t *testing.T) {
	srv := Server{
		Name:      "linear",
		Transport: TransportHTTP,
		URL:       "https://mcp.linear.app",
		Headers:   map[string]string{"X-Other": "ok"},
	}
	got, err := srv.RenderForCLI()
	if err != nil {
		t.Fatalf("RenderForCLI: %v", err)
	}
	if got["type"] != "http" {
		t.Errorf("type = %v, want http", got["type"])
	}
	if got["url"] != "https://mcp.linear.app" {
		t.Errorf("url = %v", got["url"])
	}
	headers, _ := got["headers"].(map[string]string)
	if headers["X-Other"] != "ok" {
		t.Errorf("headers X-Other = %v", headers["X-Other"])
	}
}

func TestRenderForCLISSE(t *testing.T) {
	srv := Server{
		Name:      "events",
		Transport: TransportSSE,
		URL:       "https://example.com/sse",
	}
	got, err := srv.RenderForCLI()
	if err != nil {
		t.Fatalf("RenderForCLI: %v", err)
	}
	if got["type"] != "sse" {
		t.Errorf("type = %v, want sse", got["type"])
	}
}

func TestRenderForCLIErrors(t *testing.T) {
	cases := []struct {
		name   string
		server Server
	}{
		{"stdio missing command", Server{Name: "x", Transport: TransportStdio}},
		{"http missing url", Server{Name: "x", Transport: TransportHTTP}},
		{"sse missing url", Server{Name: "x", Transport: TransportSSE}},
		{"unsupported transport", Server{Name: "x", Transport: "wat"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.server.RenderForCLI(); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestToSpecMapsSSEToHTTPProbeTransport(t *testing.T) {
	srv := Server{Name: "events", Transport: TransportSSE, URL: "https://e"}
	spec := srv.ToSpec()
	if spec.Transport != mcp.TransportHTTP {
		t.Errorf("ToSpec transport for SSE = %q, want %q", spec.Transport, mcp.TransportHTTP)
	}
	if spec.Provider != "claude" {
		t.Errorf("ToSpec provider = %q", spec.Provider)
	}
	if spec.CacheKey() != "claude:events" {
		t.Errorf("ToSpec cache key = %q", spec.CacheKey())
	}
}

func TestToSpecEnabledMirrorsDisabledFlag(t *testing.T) {
	off := Server{Name: "off", Transport: TransportStdio, Command: "x", Disabled: true}.ToSpec()
	if off.Enabled {
		t.Errorf("Disabled server should produce Enabled=false spec")
	}
	on := Server{Name: "on", Transport: TransportStdio, Command: "x"}.ToSpec()
	if !on.Enabled {
		t.Errorf("Non-disabled server should produce Enabled=true spec")
	}
}
