//go:build !noremote

package appupdate

import "testing"

func TestWSLUpdaterInstallsTheStandardLauncher(t *testing.T) {
	platform, filename := configureWSLFromGitLab(t)
	if platform != "wsl" || filename != "agent-overflow-wsl-amd64.exe" {
		t.Fatalf("target %s resolved %s, want wsl and agent-overflow-wsl-amd64.exe", platform, filename)
	}
}
