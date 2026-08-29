package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
)

func TestWaitForInstanceFileRemovalWaitsForBackendOwnedEvidence(t *testing.T) {
	dataDir := t.TempDir()
	path := harnessclient.InstanceFilePath(dataDir)
	if err := os.WriteFile(path, []byte("present"), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = os.Remove(path)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForInstanceFileRemoval(ctx, dataDir); err != nil {
		t.Fatalf("waitForInstanceFileRemoval: %v", err)
	}
}

func TestBootstrapForTargetRejectsCopiedInstanceFile(t *testing.T) {
	ownedRoot := t.TempDir()
	selectedRoot := t.TempDir()
	writeInstanceFile(t, ownedRoot, os.Getpid())
	target := target{
		ID:       "copied",
		DataRoot: selectedRoot,
		DataDir:  filepath.Join(ownedRoot, appDataDirName),
	}
	if _, err := bootstrapForTarget(target); err == nil {
		t.Fatal("accepted bootstrap copied from another data root")
	}
}

func TestLauncherRegistrationRejectsProfileMismatch(t *testing.T) {
	root := t.TempDir()
	bs := harnessclient.Bootstrap{
		DataRoot: root,
		Identity: instanceinfo.Identity{
			Mode:                     instanceinfo.ModePerf,
			BootNonce:                "boot",
			LauncherPid:              42,
			LauncherProcessStartTime: "1",
			LauncherExecutablePath:   `C:\Agent\agent-overflow.exe`,
			LauncherProfile:          "soak",
			LauncherDataRoot:         root,
			LauncherWebviewProfile:   `C:\Agent\webview-soak`,
			LauncherPIDNamespace:     "windows",
		},
	}
	if reg := launcherRegistrationFor(bs, root); reg.valid() {
		t.Fatal("accepted launcher profile that did not match instance mode")
	}
}
