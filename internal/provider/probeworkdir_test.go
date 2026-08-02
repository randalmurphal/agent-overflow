package provider

import (
	"os"
	"strings"
	"testing"
)

func TestValidateProbeWorkDir(t *testing.T) {
	cases := []struct {
		name    string
		workDir string
		wantErr string
	}{
		{
			// The bug this rule exists for: every production caller used to
			// leave WorkDir empty, so the probe inherited the app process's
			// launch directory and cached that answer for every workspace.
			name:    "empty is refused",
			workDir: "",
			wantErr: "required",
		},
		{
			// A relative path resolves against the same inherited cwd, so it
			// is the empty case wearing a disguise.
			name:    "relative is refused",
			workDir: "some/project",
			wantErr: "must be absolute",
		},
		{
			name:    "dot is refused",
			workDir: ".",
			wantErr: "must be absolute",
		},
		{
			name: "absolute is accepted",
			// os.TempDir rather than a literal: filepath.IsAbs is
			// OS-specific and "/" is not absolute on Windows.
			workDir: os.TempDir(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateProbeWorkDir("claude", tc.workDir)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateProbeWorkDir(%q) = %v, want nil", tc.workDir, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateProbeWorkDir(%q) = nil, want error containing %q", tc.workDir, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "claude") {
				t.Errorf("error %q does not name the provider", err)
			}
		})
	}
}
