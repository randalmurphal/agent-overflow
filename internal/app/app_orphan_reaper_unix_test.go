//go:build !windows

package app

import (
	"os/exec"
	"path/filepath"
	"testing"

	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/settings"
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
