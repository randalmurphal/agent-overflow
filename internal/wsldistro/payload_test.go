package wsldistro

import (
	"strings"
	"testing"
)

// Distinct locally rebuilt launchers normally share a release version. Exercise
// the persisted production predicate: an upgrade must refresh the whole backend
// (and its embedded SPA), while an unchanged warm boot keeps its fast path.
func TestInstalledPayloadIdentitySurvivesSaveAndRejectsSameVersionRebuild(t *testing.T) {
	dir := t.TempDir()
	first := strings.Repeat("a", 64)
	next := strings.Repeat("b", 64)
	cfg := &Config{Distro: "Ubuntu", InstalledVer: "0.0.15", InstalledSHA256: first, InstalledDistro: "Ubuntu", InstalledBinPath: "/home/user/.local/bin/agent-overflow"}
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.HasPayload("Ubuntu", first) {
		t.Fatal("same payload was not reusable after loading")
	}
	if loaded.HasPayload("Ubuntu", next) {
		t.Fatal("same semantic version reused an older embedded backend")
	}
	if loaded.HasPayload("Debian", first) {
		t.Fatal("payload was reused in a distro where it was not installed")
	}
	loaded.InstalledSHA256 = ""
	if loaded.HasPayload("Ubuntu", first) || loaded.HasPayload("Ubuntu", "") {
		t.Fatal("legacy record must reinstall once")
	}
	var absent *Config
	if absent.HasPayload("Ubuntu", first) {
		t.Fatal("absent record was reused")
	}
}

// A replacement may fail before successful boot bookkeeping runs. A subsequent
// rollback must not reuse the old record over the newly written executable.
func TestPayloadReplacementInvalidatesRecordBeforeFailedBoot(t *testing.T) {
	dir := t.TempDir()
	fingerprint := strings.Repeat("c", 64)
	original := &Config{Distro: "Ubuntu", InstalledDistro: "Debian", InstalledSHA256: fingerprint, InstalledBinPath: "/bin/agent-overflow"}
	if err := Save(dir, original); err != nil {
		t.Fatal(err)
	}
	if err := InvalidatePayload(dir); err != nil {
		t.Fatal(err)
	}
	// No success write: installation or launch failed, or the process exited.
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.HasPayload("Debian", fingerprint) {
		t.Fatal("failed replacement left the previous build reusable")
	}
	if loaded.Distro != original.Distro {
		t.Fatal("payload invalidation changed the user's default distro")
	}
	if err := InvalidatePayload(dir); err != nil {
		t.Fatal(err)
	}
	if err := InvalidatePayload(""); err == nil {
		t.Fatal("invalid record location must prevent replacing the executable")
	}
}
