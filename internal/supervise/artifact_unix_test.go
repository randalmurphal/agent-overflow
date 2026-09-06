//go:build !windows

package supervise

import (
	"context"
	"path/filepath"
	"testing"
)

func TestServiceArtifactLaunchesThroughTheOldSupervisorEntryPoint(t *testing.T) {
	file, digest := testArtifactZIP(t, testBundleEntries("#!/bin/sh\nprintf '{\"protocolVersion\":1,\"version\":\"2.0.0\"}\\n'\n"))
	a, err := PrepareArtifact(context.Background(), file, "agent-overflow-darwin-arm64.zip", digest)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	layout, _ := NewLayout(filepath.Join(t.TempDir(), "space and $ dollar"))
	if err := a.Stage(layout, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	entry, _ := layout.VersionBinary("2.0.0")
	answer, err := PreflightBinary(context.Background(), entry)
	if err != nil || answer.Version != "2.0.0" || answer.ProtocolVersion != ProtocolVersion {
		t.Fatal("old supervisor cannot preflight bundle", answer, err)
	}
}
