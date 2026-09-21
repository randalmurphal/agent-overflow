package mcpapp

import (
	"testing"

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
