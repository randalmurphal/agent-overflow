package mcp

import (
	"reflect"
	"sort"
	"testing"

	"agent-overflow/internal/store"
)

func TestRenderClaudeSpec_StdioFull(t *testing.T) {
	server := store.MCPServer{
		Name:      "everything",
		Transport: TransportStdio,
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-everything"},
		Env:       map[string]string{"FOO": "bar"},
	}
	got, err := RenderClaudeSpec(server)
	if err != nil {
		t.Fatalf("RenderClaudeSpec: %v", err)
	}
	want := map[string]any{
		"type":    "stdio",
		"command": "npx",
		"args":    []string{"-y", "@modelcontextprotocol/server-everything"},
		"env":     map[string]string{"FOO": "bar"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RenderClaudeSpec stdio:\n got = %#v\n want = %#v", got, want)
	}
}

func TestRenderClaudeSpec_HTTPWithBearer(t *testing.T) {
	server := store.MCPServer{
		Name:      "linear",
		Transport: TransportHTTP,
		URL:       "https://mcp.linear.app",
		Headers:   map[string]string{"X-Other": "ok"},
		BearerEnv: "LINEAR_TOKEN",
	}
	got, err := RenderClaudeSpec(server)
	if err != nil {
		t.Fatalf("RenderClaudeSpec: %v", err)
	}
	headers, _ := got["headers"].(map[string]string)
	if headers["Authorization"] != "Bearer ${LINEAR_TOKEN}" {
		t.Errorf("Authorization header missing or wrong: %q", headers["Authorization"])
	}
	if headers["X-Other"] != "ok" {
		t.Errorf("X-Other header missing: %q", headers["X-Other"])
	}
	if got["type"] != "http" {
		t.Errorf("type = %v, want http", got["type"])
	}
	if got["url"] != "https://mcp.linear.app" {
		t.Errorf("url = %v", got["url"])
	}
}

func TestRenderClaudeSpec_SSE(t *testing.T) {
	server := store.MCPServer{
		Name:      "events",
		Transport: TransportSSE,
		URL:       "https://example.com/sse",
	}
	got, err := RenderClaudeSpec(server)
	if err != nil {
		t.Fatalf("RenderClaudeSpec: %v", err)
	}
	if got["type"] != "sse" {
		t.Errorf("type = %v, want sse", got["type"])
	}
}

func TestRenderClaudeSpec_ErrorCases(t *testing.T) {
	cases := []struct {
		name   string
		server store.MCPServer
	}{
		{"stdio missing command", store.MCPServer{Name: "x", Transport: TransportStdio}},
		{"http missing url", store.MCPServer{Name: "x", Transport: TransportHTTP}},
		{"sse missing url", store.MCPServer{Name: "x", Transport: TransportSSE}},
		{"unsupported transport", store.MCPServer{Name: "x", Transport: "wat"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RenderClaudeSpec(tc.server); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestRenderCodexSpec_StdioFull(t *testing.T) {
	server := store.MCPServer{
		Name:      "fs",
		Transport: TransportStdio,
		Command:   "mcp-fs",
		Args:      []string{"--root", "/tmp"},
		Env:       map[string]string{"DEBUG": "1"},
	}
	got, err := RenderCodexSpec(server)
	if err != nil {
		t.Fatalf("RenderCodexSpec: %v", err)
	}
	want := map[string]any{
		"command": "mcp-fs",
		"args":    []string{"--root", "/tmp"},
		"env":     map[string]string{"DEBUG": "1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RenderCodexSpec stdio:\n got = %#v\n want = %#v", got, want)
	}
}

func TestRenderCodexSpec_HTTPWithBearer(t *testing.T) {
	server := store.MCPServer{
		Name:      "github",
		Transport: TransportHTTP,
		URL:       "https://api.github.com/mcp",
		Headers:   map[string]string{"X-Foo": "bar"},
		BearerEnv: "GH_TOKEN",
	}
	got, err := RenderCodexSpec(server)
	if err != nil {
		t.Fatalf("RenderCodexSpec: %v", err)
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

func TestRenderCodexSpec_ErrorCases(t *testing.T) {
	if _, err := RenderCodexSpec(store.MCPServer{Name: "x", Transport: TransportStdio}); err == nil {
		t.Error("stdio missing command should error")
	}
	if _, err := RenderCodexSpec(store.MCPServer{Name: "x", Transport: TransportHTTP}); err == nil {
		t.Error("http missing url should error")
	}
	if _, err := RenderCodexSpec(store.MCPServer{Name: "x", Transport: "wat"}); err == nil {
		t.Error("unsupported transport should error")
	}
}

func TestMergeForProvider_ClaudeUserOnly(t *testing.T) {
	library := []store.MCPServer{
		{ID: "id-a", Name: "alpha", Transport: TransportStdio, Command: "alpha-cmd", Enabled: true},
		{ID: "id-b", Name: "beta", Transport: TransportHTTP, URL: "https://b", Enabled: true},
	}
	res, err := MergeForProvider(ProviderClaude, nil, library, []string{"id-a", "id-b"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(res.Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(res.Servers))
	}
	if res.Servers["alpha"] == nil || res.Servers["beta"] == nil {
		t.Errorf("missing servers: %#v", res.Servers)
	}
	if len(res.Collisions) != 0 {
		t.Errorf("unexpected collisions: %v", res.Collisions)
	}
}

func TestMergeForProvider_ClaudeDesignWinsOnCollision(t *testing.T) {
	design := map[string]any{
		"alpha": map[string]any{"type": "stdio", "command": "design-cmd"},
	}
	library := []store.MCPServer{
		{ID: "id-a", Name: "alpha", Transport: TransportStdio, Command: "user-cmd", Enabled: true},
	}
	res, err := MergeForProvider(ProviderClaude, design, library, []string{"id-a"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	spec, _ := res.Servers["alpha"].(map[string]any)
	if spec["command"] != "design-cmd" {
		t.Errorf("design entry should win: got command=%v", spec["command"])
	}
	if len(res.Collisions) != 1 || res.Collisions[0] != "alpha" {
		t.Errorf("collisions = %v, want [alpha]", res.Collisions)
	}
}

func TestMergeForProvider_ClaudeSkipsDisabledLibraryRow(t *testing.T) {
	library := []store.MCPServer{
		{ID: "id-a", Name: "alpha", Transport: TransportStdio, Command: "alpha-cmd", Enabled: false},
	}
	res, err := MergeForProvider(ProviderClaude, nil, library, []string{"id-a"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if _, ok := res.Servers["alpha"]; ok {
		t.Errorf("disabled library row should not render")
	}
}

func TestMergeForProvider_CodexMaskUnselectedAsEnabledFalse(t *testing.T) {
	library := []store.MCPServer{
		{ID: "id-a", Name: "alpha", Transport: TransportStdio, Command: "alpha-cmd", Enabled: true},
		{ID: "id-b", Name: "beta", Transport: TransportHTTP, URL: "https://b", Enabled: true},
		{ID: "id-c", Name: "gamma", Transport: TransportStdio, Command: "gamma-cmd", Enabled: true},
	}
	// Thread selected only alpha. beta + gamma must come through as
	// `enabled: false` overlays with FULL transport spec so Codex's
	// serde doesn't reject the bare disable.
	res, err := MergeForProvider(ProviderCodex, nil, library, []string{"id-a"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	alpha, _ := res.Servers["alpha"].(map[string]any)
	if alpha["command"] != "alpha-cmd" {
		t.Errorf("alpha should be rendered: %#v", alpha)
	}
	if alpha["enabled"] != true {
		t.Errorf("alpha overlay should force enabled:true (got %v)", alpha["enabled"])
	}

	beta, _ := res.Servers["beta"].(map[string]any)
	if beta == nil {
		t.Fatalf("beta should be masked, not absent")
	}
	if beta["enabled"] != false {
		t.Errorf("beta enabled = %v, want false", beta["enabled"])
	}
	if beta["url"] != "https://b" {
		t.Errorf("beta needs URL for the deserializer: %#v", beta)
	}

	gamma, _ := res.Servers["gamma"].(map[string]any)
	if gamma == nil {
		t.Fatalf("gamma should be masked, not absent")
	}
	if gamma["enabled"] != false || gamma["command"] != "gamma-cmd" {
		t.Errorf("gamma overlay must carry full transport spec + enabled:false: %#v", gamma)
	}
}

func TestMergeForProvider_CodexDesignEntryHidesMaskCollision(t *testing.T) {
	design := map[string]any{
		"alpha": map[string]any{"command": "design", "enabled": true},
	}
	library := []store.MCPServer{
		{ID: "id-a", Name: "alpha", Transport: TransportStdio, Command: "user", Enabled: true},
	}
	// User did not select alpha; design owns the name. We must NOT
	// overwrite design's entry with an enabled:false mask.
	res, err := MergeForProvider(ProviderCodex, design, library, nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	alpha, _ := res.Servers["alpha"].(map[string]any)
	if alpha["command"] != "design" || alpha["enabled"] != true {
		t.Errorf("design entry must be preserved against library masking: %#v", alpha)
	}
}

func TestMergeForProvider_UnsupportedProvider(t *testing.T) {
	library := []store.MCPServer{
		{ID: "id-a", Name: "alpha", Transport: TransportStdio, Command: "cmd", Enabled: true},
	}
	if _, err := MergeForProvider("haiku", nil, library, []string{"id-a"}); err == nil {
		t.Error("unknown provider should error")
	}
}

func TestMergeForProvider_CollisionOrderIsStable(t *testing.T) {
	design := map[string]any{
		"zeta":  map[string]any{},
		"alpha": map[string]any{},
		"mu":    map[string]any{},
	}
	library := []store.MCPServer{
		{ID: "1", Name: "mu", Transport: TransportStdio, Command: "x", Enabled: true},
		{ID: "2", Name: "alpha", Transport: TransportStdio, Command: "x", Enabled: true},
		{ID: "3", Name: "zeta", Transport: TransportStdio, Command: "x", Enabled: true},
	}
	res, err := MergeForProvider(ProviderClaude, design, library, []string{"3", "1", "2"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	expected := []string{"alpha", "mu", "zeta"}
	if !sort.StringsAreSorted(res.Collisions) {
		t.Errorf("collisions not sorted: %v", res.Collisions)
	}
	if !reflect.DeepEqual(res.Collisions, expected) {
		t.Errorf("collisions = %v, want %v", res.Collisions, expected)
	}
}

func TestFilterEnabled(t *testing.T) {
	in := []store.MCPServer{
		{ID: "a", Enabled: true},
		{ID: "b", Enabled: false},
		{ID: "c", Enabled: true},
	}
	out := FilterEnabled(in)
	if len(out) != 2 {
		t.Fatalf("FilterEnabled = %d, want 2", len(out))
	}
	if out[0].ID != "a" || out[1].ID != "c" {
		t.Errorf("FilterEnabled result = %v", out)
	}
}

func TestSelectServersByID(t *testing.T) {
	library := []store.MCPServer{
		{ID: "a", Name: "alpha", Enabled: true},
		{ID: "b", Name: "beta", Enabled: true},
		{ID: "c", Name: "gamma", Enabled: false},
	}
	out := SelectServersByID(library, []string{"b", "c", "missing", "a"})
	if len(out) != 2 {
		t.Fatalf("SelectServersByID = %d entries, want 2", len(out))
	}
	if out[0].ID != "b" || out[1].ID != "a" {
		t.Errorf("SelectServersByID order = [%s, %s], want [b, a]", out[0].ID, out[1].ID)
	}
}

func TestSelectServersByID_EmptyInputs(t *testing.T) {
	if got := SelectServersByID(nil, []string{"a"}); got != nil {
		t.Errorf("nil library should return nil, got %v", got)
	}
	if got := SelectServersByID([]store.MCPServer{{ID: "a", Enabled: true}}, nil); got != nil {
		t.Errorf("nil ids should return nil, got %v", got)
	}
}
