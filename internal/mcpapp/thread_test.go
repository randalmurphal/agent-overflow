package mcpapp

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/agyconfig"
	"agent-overflow/internal/codexconfig"
)

func TestCodexMCPPreferenceIsIndependentOfRuntime(t *testing.T) {
	for _, tc := range []struct {
		name, status                          string
		configured, enabled, disabled, locked bool
	}{
		{"off", "disabled", true, false, true, false},
		{"on", "connected", true, true, false, false},
		{"failed", "failed", true, true, false, false},
		{"needs auth", "needs-auth", true, true, false, false},
		{"blocked", "disabled", true, true, false, true},
		{"saved off while live", "connected", true, false, true, false},
		{"external off", "disabled", false, false, true, true},
		{"external on", "connected", false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configured := map[string]codexconfig.Server{}
			if tc.configured {
				configured["srv"] = codexconfig.Server{Name: "srv", Enabled: tc.enabled}
			}
			row := ThreadMCPServer{Name: "srv", Status: tc.status, Tools: []string{"read"}}
			applyCodexMCPPreference(&row, configured)
			if row.Disabled != tc.disabled || (row.ToggleDisabledReason != "") != tc.locked || row.Status != tc.status {
				t.Fatalf("projection = %#v", row)
			}
			if tc.status == "disabled" && len(row.Tools) != 0 {
				t.Fatal("disabled row retained tools")
			}
		})
	}
}

func TestAgyWorkspaceMCPListAndToggle(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfgPath := agyconfig.PathForHome(home)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"mcpServers":{"demo":{"disabled":false,"serverUrl":"https://example.com/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := New(Deps{
		AgyConfigPath: func() (string, error) { return cfgPath, nil },
	})
	rows, err := svc.ListWorkspaceMcpServers("agy", "")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers(agy): %v", err)
	}
	if len(rows) != 1 || rows[0].Provider != "agy" || rows[0].Name != "demo" || rows[0].Disabled {
		t.Fatalf("unexpected rows: %+v", rows)
	}

	if err := svc.SetWorkspaceMcpServerEnabled("agy", "", "demo", false); err != nil {
		t.Fatalf("SetWorkspaceMcpServerEnabled(false): %v", err)
	}
	rows, err = svc.ListWorkspaceMcpServers("agy", "")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers after disable: %v", err)
	}
	if len(rows) != 1 || !rows[0].Disabled || rows[0].Status != "disabled" {
		t.Fatalf("unexpected rows after disable: %+v", rows)
	}
}
