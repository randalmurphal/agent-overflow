package codex

import "testing"

func TestHTTPAndDisabledMCPServerShapes(t *testing.T) {
	spec := HTTPMCPServer("http://127.0.0.1/mcp", map[string]string{"Authorization": "Bearer token"})
	if spec["url"] != "http://127.0.0.1/mcp" {
		t.Fatalf("url = %v", spec["url"])
	}
	headers, ok := spec["http_headers"].(map[string]string)
	if !ok || headers["Authorization"] != "Bearer token" {
		t.Fatalf("http_headers = %#v", spec["http_headers"])
	}
	if enabled, ok := DisabledMCPServer()["enabled"].(bool); !ok || enabled {
		t.Fatalf("disabled overlay = %#v", DisabledMCPServer())
	}
}
