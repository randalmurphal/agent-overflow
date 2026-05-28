package codexconfig

import (
	"reflect"
	"testing"
)

func TestRenderForOverlayStdio(t *testing.T) {
	srv := Server{
		Name:      "fs",
		Transport: TransportStdio,
		Command:   "mcp-fs",
		Args:      []string{"--root", "/tmp"},
		Env:       map[string]string{"DEBUG": "1"},
		Enabled:   true,
	}
	got, err := srv.RenderForOverlay()
	if err != nil {
		t.Fatalf("RenderForOverlay: %v", err)
	}
	want := map[string]any{
		"command": "mcp-fs",
		"args":    []string{"--root", "/tmp"},
		"env":     map[string]string{"DEBUG": "1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RenderForOverlay stdio:\n got = %#v\n want = %#v", got, want)
	}
}

func TestRenderForOverlayStreamable(t *testing.T) {
	srv := Server{
		Name:           "github",
		Transport:      TransportStreamable,
		URL:            "https://api.github.com/mcp",
		HTTPHeaders:    map[string]string{"X-Foo": "bar"},
		BearerTokenEnv: "GH_TOKEN",
		Enabled:        true,
	}
	got, err := srv.RenderForOverlay()
	if err != nil {
		t.Fatalf("RenderForOverlay: %v", err)
	}
	if got["url"] != "https://api.github.com/mcp" {
		t.Errorf("url = %v", got["url"])
	}
	headers, _ := got["http_headers"].(map[string]string)
	if headers["X-Foo"] != "bar" {
		t.Errorf("http_headers X-Foo = %v", headers["X-Foo"])
	}
	if got["bearer_token_env_var"] != "GH_TOKEN" {
		t.Errorf("bearer_token_env_var = %v", got["bearer_token_env_var"])
	}
}

func TestRenderForOverlayOmitsEnabledWhenTrue(t *testing.T) {
	srv := Server{Name: "x", Transport: TransportStdio, Command: "y", Enabled: true}
	got, _ := srv.RenderForOverlay()
	if _, ok := got["enabled"]; ok {
		t.Errorf("enabled=true should be omitted; got %#v", got)
	}
}

func TestRenderForOverlayEmitsEnabledFalseExplicitly(t *testing.T) {
	srv := Server{Name: "x", Transport: TransportStdio, Command: "y", Enabled: false}
	got, _ := srv.RenderForOverlay()
	if v, ok := got["enabled"]; !ok || v != false {
		t.Errorf("enabled=false should be emitted explicitly; got %#v", got)
	}
}

func TestRenderForOverlayErrors(t *testing.T) {
	if _, err := (Server{Name: "x", Transport: TransportStdio, Enabled: true}).RenderForOverlay(); err == nil {
		t.Error("stdio missing command should error")
	}
	if _, err := (Server{Name: "x", Transport: TransportStreamable, Enabled: true}).RenderForOverlay(); err == nil {
		t.Error("streamable missing url should error")
	}
	if _, err := (Server{Name: "x", Transport: "wat", Enabled: true}).RenderForOverlay(); err == nil {
		t.Error("unsupported transport should error")
	}
}
