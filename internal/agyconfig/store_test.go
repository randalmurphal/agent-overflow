package agyconfig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreListAndSetDisabled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := PathForHome(dir)
	st := New(path)

	// Missing file returns empty list cleanly.
	servers, err := st.ListServers()
	if err != nil || len(servers) != 0 {
		t.Fatalf("missing file: servers=%+v err=%v", servers, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	initial := `{
  "extraTop": "preserve-me",
  "mcpServers": {
    "beta": {
      "command": "npx",
      "args": ["-y", "demo"],
      "customField": 42
    },
    "alpha": {
      "disabled": false,
      "serverUrl": "https://example.com/mcp",
      "headers": {"Authorization": "Bearer secret"}
    }
  }
}`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	servers, err = st.ListServers()
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(servers) != 2 || servers[0].Name != "alpha" || servers[0].Transport != TransportHTTP || servers[0].Disabled ||
		servers[1].Name != "beta" || servers[1].Transport != TransportStdio || servers[1].Disabled {
		t.Fatalf("unexpected servers: %+v", servers)
	}

	if err := st.SetDisabled("alpha", true); err != nil {
		t.Fatalf("SetDisabled(alpha, true): %v", err)
	}
	if err := st.SetDisabled("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetDisabled(missing) err = %v, want ErrNotFound", err)
	}

	servers, err = st.ListServers()
	if err != nil {
		t.Fatalf("ListServers after toggle: %v", err)
	}
	if !servers[0].Disabled || servers[1].Disabled {
		t.Fatalf("unexpected disabled states: %+v", servers)
	}

	// Verify unknown top-level and server-level fields were preserved verbatim.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["extraTop"] != "preserve-me" {
		t.Fatalf("top-level field lost: %+v", parsed)
	}
	alpha := parsed["mcpServers"].(map[string]any)["alpha"].(map[string]any)
	headers := alpha["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer secret" {
		t.Fatalf("server headers lost: %+v", alpha)
	}
}

func TestSetDisabledHandlesNullEntryAndConcurrentWrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	raw := `{"mcpServers":{"nullServer":null,"a":{"command":"a"},"b":{"command":"b"}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	st := New(path)
	if err := st.SetDisabled("nullServer", true); err != nil {
		t.Fatalf("SetDisabled(nullServer): %v", err)
	}

	errCh := make(chan error, 2)
	go func() { errCh <- st.SetDisabled("a", true) }()
	go func() { errCh <- st.SetDisabled("b", true) }()
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent SetDisabled: %v", err)
		}
	}
	servers, err := st.ListServers()
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	disabled := map[string]bool{}
	for _, s := range servers {
		disabled[s.Name] = s.Disabled
	}
	if !disabled["nullServer"] || !disabled["a"] || !disabled["b"] {
		t.Fatalf("expected all three servers disabled after concurrent writes, got %+v", disabled)
	}
}
