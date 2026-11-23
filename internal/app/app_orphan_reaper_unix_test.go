//go:build !windows

package app

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/mockexec"
)

// A serve boot's headless browser ends with the backend: its engine gets the
// app's orphan reaper, which records the browser's group.
func TestTheHeadlessBrowserIsGivenTheOrphanReaper(t *testing.T) {
	t.Parallel()
	leader := exec.Command("sleep", "60")
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = leader.Process.Kill()
		_ = leader.Wait()
	})
	a := NewApp()
	a.browser.headlessChromium = true
	registry := orphanreaper.NewRegistry(filepath.Join(t.TempDir(), "orphan-registry.json"))
	a.orphanRegistry = registry
	opts := a.headlessChromiumOptions(settings.Settings{})
	if opts == nil || opts.Reaper == nil {
		t.Fatalf("headless options %+v carry no reaper", opts)
	}
	opts.Reaper.Watch(leader.Process.Pid)
	records, err := registry.Load()
	if err != nil || len(records) != 1 || records[0].PGID != leader.Process.Pid || records[0].Owner != "headless browser" {
		t.Fatalf("records %+v %v, want the browser's group", records, err)
	}
}

// The services that start provider processes on the app's behalf are given
// the app's orphan reaper, which records each group under its owner.
func TestProviderServicesAreGivenTheOrphanReaper(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		owner  string
		reaper func(*App) procutil.Reaper
	}{
		{"mcp sign-in", func(a *App) procutil.Reaper { return a.mcpDeps().Reaper }},
		{"codex read", func(a *App) procutil.Reaper { return a.codexAppDeps().Reaper }},
	} {
		t.Run(c.owner, func(t *testing.T) {
			leader := exec.Command("sleep", "60")
			if err := leader.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = leader.Process.Kill()
				_ = leader.Wait()
			})
			a := NewApp()
			registry := orphanreaper.NewRegistry(filepath.Join(t.TempDir(), "orphan-registry.json"))
			a.orphanRegistry = registry
			reaper := c.reaper(a)
			if reaper == nil {
				t.Fatal("the service is given no reaper")
			}
			reaper.Watch(leader.Process.Pid)
			records, err := registry.Load()
			if err != nil || len(records) != 1 || records[0].PGID != leader.Process.Pid || records[0].Owner != c.owner {
				t.Fatalf("records %+v %v, want the group under %q", records, err, c.owner)
			}
		})
	}
}

// A conversation transfer's Codex snapshot starts its own app-server, which
// is recorded for the orphan reaper while it runs and released once it ends.
// The mock never answers, so the snapshot runs until its context ends.
func TestACodexTransferSnapshotsAppServerIsRecordedWhileItRuns(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	binary := mockexec.Write(t, filepath.Join(t.TempDir(), "codex"), "#!/bin/sh\nexec sleep 60\n")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	registry := orphanreaper.NewRegistry(filepath.Join(t.TempDir(), "orphan-registry.json"))
	app.orphanRegistry = registry
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		thread := store.Thread{Provider: string(provider.Codex), SessionRef: "thr-transfer"}
		_, _, err := app.collectTransferNative(ctx, thread, t.TempDir(), map[string]string{})
		done <- err
	}()
	record := waitOrphanRecord(t, registry)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a snapshot the app-server never answered succeeded")
	}
	if record.Owner != "conversation transfer" {
		t.Fatalf("record %+v, want the transfer's app-server", record)
	}
	if records, err := registry.Load(); err != nil || len(records) != 0 {
		t.Fatalf("after the snapshot ended the registry holds %+v %v", records, err)
	}
}
