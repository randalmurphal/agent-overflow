package appupdate

import (
	"context"
	"testing"
)

// configureWSLFromGitLab configures the WSL updater against a GitLab
// release that ships both launcher variants and returns the platform token
// the backend targets and the launcher its passive check resolves.
func configureWSLFromGitLab(t *testing.T) (platform, filename string) {
	t.Helper()
	f := newFakeGitLab(t)
	nr, std := noremoteAsset("v0.2.0"), standardAsset("v0.2.0")
	f.publish("v0.2.0", std, nr, sumsOf(std, nr))
	setGitLabProject(t, testGitLabProject)

	a := New("0.1.0", Deps{Context: context.Background})
	if err := a.ConfigureWSL(WSLConfig{
		CurrentVersion: "0.1.0",
		Arch:           "amd64",
		StagingRoot:    t.TempDir(),
		MarkerDir:      t.TempDir(),
		Provider:       Config{GitLab: f.gitlab},
	}); err != nil {
		t.Fatalf("ConfigureWSL: %v", err)
	}
	availability, err := a.CheckForUpdate()
	if err != nil || !availability.Available {
		t.Fatalf("CheckForUpdate = %+v, %v; want v0.2.0 available", availability, err)
	}
	return a.updater.provider.req.Platform, a.updater.pending.Artifact.Filename
}
