//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/harnessrun"
)

func TestLaunchManagedHarnessBindsFreshRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "fake-backend")
	script := "#!/bin/sh\nroot=\"\"\nwhile [ $# -gt 0 ]; do if [ \"$1\" = \"--data-dir\" ]; then root=$2; shift 2; else shift; fi; done\nprintf '__AO_HARNESS__: {\\\"url\\\":\\\"http://127.0.0.1:1/\\\",\\\"port\\\":1,\\\"token\\\":\\\"test\\\",\\\"dataRoot\\\":\\\"%s\\\",\\\"dataDir\\\":\\\"%s/agent-overflow\\\",\\\"pid\\\":%d,\\\"version\\\":\\\"test\\\"}\\n' \"$root\" \"$root\" $$\nsleep 30\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := harnessrun.RunPlan{Version: harnessrun.PlanVersion, RunID: "launch", Workload: "bench", DataRoot: root, Ownership: harnessrun.OwnershipFresh, Adapter: harnessrun.AdapterBench, Binary: binary}
	launched, err := launchManagedHarness(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = launched.Terminate(context.Background()) }()
	if err := sameManagedRoot(launched.Bootstrap.DataRoot, root); err != nil {
		t.Fatalf("bootstrap root = %q, want %q: %v", launched.Bootstrap.DataRoot, root, err)
	}
	if launched.PID <= 0 || !strings.Contains(launched.Bootstrap.URL, "127.0.0.1") {
		t.Fatalf("launch identity = %+v", launched.Bootstrap)
	}
}
