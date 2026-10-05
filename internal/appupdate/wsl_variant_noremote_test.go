//go:build noremote

package appupdate

import (
	"context"
	"errors"
	"testing"
)

func TestWSLUpdaterInstallsTheNoRemoteLauncher(t *testing.T) {
	platform, filename := configureWSLFromGitLab(t)
	if platform != "wsl-noremote" || filename != "agent-overflow-wsl-noremote-amd64.exe" {
		t.Fatalf("target %s resolved %s, want wsl-noremote and agent-overflow-wsl-noremote-amd64.exe", platform, filename)
	}
}

// A noremote build linked with no release project has no feed: it never
// falls back to the GitHub releases of the standard build.
func TestANoRemoteBuildWithoutAReleaseProjectHasNoFeed(t *testing.T) {
	previous := gitlabProject
	gitlabProject = ""
	t.Cleanup(func() { gitlabProject = previous })
	a := New("0.1.0", Deps{Context: context.Background})
	err := a.ConfigureWSL(WSLConfig{CurrentVersion: "0.1.0", Arch: "amd64", StagingRoot: t.TempDir(), MarkerDir: t.TempDir()})
	if !errors.Is(err, errNoReleaseProject) {
		t.Fatalf("ConfigureWSL = %v, want the missing-project refusal", err)
	}
}
