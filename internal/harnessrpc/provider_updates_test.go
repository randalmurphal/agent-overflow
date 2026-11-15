package harnessrpc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/providerupdate/updatefake"
)

// withMockInstall gives the harness a home and a stand-in mock binary,
// returning the install the mock would simulate.
func withMockInstall(t *testing.T, h *Harness) updatefake.MockInstall {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "ao-mockprovider")
	if err := os.WriteFile(exe, []byte("mock"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(exe, old, old); err != nil {
		t.Fatal(err)
	}
	h.config.HomeDir = filepath.Join(dir, "home")
	h.config.MockProvider = exe
	return updatefake.MockInstall{Home: h.config.HomeDir, Executable: exe}
}

func mtime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func TestHarnessSetProviderUpdateFeedSeedsAndResetRestores(t *testing.T) {
	h, host := newHarnessTestHost(t)
	install := withMockInstall(t, h)
	if err := h.HarnessSetProviderUpdateFeed(HarnessProviderUpdateFeedSpec{Latest: "99.1.0"}); err == nil {
		t.Fatal("a boot with no fake release hosts accepted a seed")
	}

	host.updateFeed = updatefake.New()
	if err := h.HarnessSetProviderUpdateFeed(HarnessProviderUpdateFeedSpec{Latest: "99.2.0"}); err == nil {
		t.Fatal("a version outside the catalogue was accepted")
	}
	if err := h.HarnessSetProviderUpdateFeed(HarnessProviderUpdateFeedSpec{Latest: "99.1.0"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := host.updateFeed.Latest(); got != "99.1.0" {
		t.Fatalf("latest = %s after the seed", got)
	}
	if got, err := install.Target(); got != "99.1.0" || err != nil {
		t.Fatalf("mock update target = %q, %v after the seed; want what the feed advertises", got, err)
	}
	if host.updateRechecks != 0 {
		t.Fatal("a seed checked; the caller checks")
	}
	if err := h.HarnessSetProviderUpdateFeed(HarnessProviderUpdateFeedSpec{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := host.updateFeed.Latest(); got != updatefake.Baseline {
		t.Fatalf("latest = %s after an empty seed", got)
	}
	if got, err := install.Target(); got != "" || err != nil {
		t.Fatalf("mock update target = %q, %v after an empty seed; want none", got, err)
	}

	// Reset restores the default and re-checks only when a spec had seeded.
	if err := h.HarnessReset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if host.updateRechecks != 0 {
		t.Errorf("a reset of an unseeded feed re-checked %d times", host.updateRechecks)
	}
	if err := h.HarnessSetProviderUpdateFeed(HarnessProviderUpdateFeedSpec{Latest: "99.0.1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := h.HarnessReset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := host.updateFeed.Latest(); got != updatefake.Baseline || host.updateRechecks != 1 {
		t.Fatalf("after reset latest = %s, rechecks = %d; want the default and one re-check", got, host.updateRechecks)
	}
	if got, err := install.Target(); got != "" || err != nil {
		t.Fatalf("mock update target = %q, %v after reset; want none", got, err)
	}
}

// TestHarnessResetRestoresTheMockInstallBeforeItRechecks covers an update a
// spec ran: reset must put the baseline back as a new binary, so the
// recheck reads it, and a later seed is offered again.
func TestHarnessResetRestoresTheMockInstallBeforeItRechecks(t *testing.T) {
	h, host := newHarnessTestHost(t)
	install := withMockInstall(t, h)
	host.updateFeed = updatefake.New()
	if err := h.HarnessSetProviderUpdateFeed(HarnessProviderUpdateFeedSpec{Latest: "99.0.1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// What the mock's update does with that target.
	if err := install.Install("99.0.1"); err != nil {
		t.Fatalf("install: %v", err)
	}
	installedAt := mtime(t, install.Executable)

	var atRecheck string
	var movedAtRecheck bool
	host.onUpdateRecheck = func() {
		atRecheck, _ = install.Installed()
		movedAtRecheck = mtime(t, install.Executable).After(installedAt)
	}
	if err := h.HarnessReset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if host.updateRechecks != 1 || atRecheck != updatefake.Baseline || !movedAtRecheck {
		t.Fatalf("recheck ran %d times seeing installed %q, binary moved %v; want one recheck of a new baseline binary",
			host.updateRechecks, atRecheck, movedAtRecheck)
	}

	// Seeding again advertises the update to the restored install.
	if err := h.HarnessSetProviderUpdateFeed(HarnessProviderUpdateFeedSpec{Latest: "99.0.1"}); err != nil {
		t.Fatalf("seed again: %v", err)
	}
	if got, _ := install.Installed(); got != updatefake.Baseline {
		t.Fatalf("installed = %s after the second seed, want the baseline", got)
	}
	if got, _ := install.Target(); got != "99.0.1" {
		t.Fatalf("target = %s after the second seed", got)
	}
}

func TestHarnessSetProviderUpdateFeedRefusesTheRealHome(t *testing.T) {
	h, host := newHarnessTestHost(t)
	host.updateFeed = updatefake.New()
	h.config.MockProvider = filepath.Join(t.TempDir(), "ao-mockprovider")
	// AO_HARNESS_KEEP_HOME leaves HomeDir empty.
	err := h.HarnessSetProviderUpdateFeed(HarnessProviderUpdateFeedSpec{Latest: "99.1.0"})
	if err == nil || !strings.Contains(err.Error(), "AO_HARNESS_KEEP_HOME") {
		t.Fatalf("a keep-home seed = %v, want it refused for AO_HARNESS_KEEP_HOME", err)
	}
	if got := host.updateFeed.Latest(); got != updatefake.Baseline {
		t.Fatalf("a refused seed left the feed at %s", got)
	}
	if err := h.HarnessReset(); err != nil {
		t.Fatalf("reset of a keep-home boot: %v", err)
	}
}
