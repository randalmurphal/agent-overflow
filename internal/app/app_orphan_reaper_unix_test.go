//go:build !windows

package app

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/settings"
)

// A provider session's group is recorded for the orphan reaper on macOS
// only. On Linux a provider keeps its parent-death signal, and a dying
// backend does not also SIGKILL its group two seconds later.
func TestProviderGroupsAreWatchedOnlyOnMacOS(t *testing.T) {
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
	registry := orphanreaper.NewRegistry(filepath.Join(t.TempDir(), "orphan-registry.json"))
	a.orphanRegistry = registry
	a.watchProviderGroup(leader.Process.Pid, "session-token")
	records, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := runtime.GOOS == "darwin"; (len(records) == 1) != want {
		t.Fatalf("records %+v, want a record %v", records, want)
	}
	a.releaseProviderGroup(leader.Process.Pid)
	if records, err := registry.Load(); err != nil || len(records) != 0 {
		t.Fatalf("after release the registry holds %+v %v", records, err)
	}
}

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
