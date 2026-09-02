package wsldistro

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoad_MissingFileReturnsNilNil(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if cfg != nil {
		t.Fatalf("cfg = %+v, want nil", cfg)
	}
}

func TestLoad_DecodesValidFile(t *testing.T) {
	dir := t.TempDir()
	body := `{"distro": "Ubuntu-24.04", "installed_version": "v0.1.2", "installed_distro": "Ubuntu-24.04"}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg = nil, want non-nil")
	}
	if cfg.Distro != "Ubuntu-24.04" {
		t.Errorf("Distro = %q, want Ubuntu-24.04", cfg.Distro)
	}
	if cfg.InstalledVer != "v0.1.2" {
		t.Errorf("InstalledVer = %q, want v0.1.2", cfg.InstalledVer)
	}
	if cfg.InstalledDistro != "Ubuntu-24.04" {
		t.Errorf("InstalledDistro = %q, want Ubuntu-24.04", cfg.InstalledDistro)
	}
}

func TestLoad_BadJSONErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("not json"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load on malformed json: expected error")
	}
}

func TestLoad_EmptyDirErrors(t *testing.T) {
	if _, err := Load(""); err == nil {
		t.Fatal("Load(\"\") should error")
	}
}

func TestSave_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Distro:          "Debian",
		InstalledVer:    "v1.0.0",
		InstalledDistro: "Debian",
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if got == nil || *got != *cfg {
		t.Fatalf("round-trip mismatch: got=%+v want=%+v", got, cfg)
	}
}

func TestSave_CreatesParentDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "agent-overflow") // doesn't exist yet
	cfg := &Config{Distro: "Ubuntu-22.04"}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save into missing dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestSave_NilConfigErrors(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, nil); err == nil {
		t.Fatal("Save with nil config should error")
	}
}

func TestSave_EmptyDirErrors(t *testing.T) {
	cfg := &Config{Distro: "Ubuntu"}
	if err := Save("", cfg); err == nil {
		t.Fatal("Save(\"\") should error")
	}
}

func TestLoad_PassesThroughOtherIOErrors(t *testing.T) {
	// Save a directory at the wsl.json path so ReadFile returns
	// EISDIR — anything that isn't ErrNotExist must propagate, not
	// quietly become "no config".
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, FileName), 0o755); err != nil {
		t.Fatalf("seed dir-as-file: %v", err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load with dir-at-file-path: expected error")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load should not have collapsed EISDIR into nil-nil: %v", err)
	}
}

// TestSave_FileMode pins the file permission contract. wsl.json holds
// per-user launcher state and must be 0o600 so a multi-user host
// doesn't expose another user's distro choice (and any future field on
// Config that ends up sensitive). The dir mode bound is 0o700 for the
// same reason.
func TestSave_FileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits don't translate cleanly on NTFS")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "agent-overflow")
	if err := Save(dir, &Config{Distro: "Ubuntu"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 0o600", got)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %o, want 0o700", got)
	}
}

// TestSave_AtomicNoTempFileSurvives pins the rename contract: after a
// successful Save the directory contains exactly wsl.json (no leftover
// temp files). The implementation writes to a tempfile + renames; if a
// future refactor regressed to a non-atomic write, the cleanup
// guarantee would slip and a half-written temp could be observed by a
// concurrent reader.
func TestSave_AtomicNoTempFileSurvives(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, &Config{Distro: "Ubuntu"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected exactly wsl.json after Save, got: %v", names)
	}
	if entries[0].Name() != FileName {
		t.Errorf("entry name = %q, want %q", entries[0].Name(), FileName)
	}
}

// TestSave_RewriteIsAtomic exercises the steady-state path. Saving over
// an existing file must not leave any intermediate state — the on-disk
// content must always be either the old or new payload, never empty
// or mid-transition.
func TestSave_RewriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, &Config{Distro: "Ubuntu", InstalledVer: "v0.1.0"}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := Save(dir, &Config{Distro: "Debian", InstalledVer: "v0.1.0"}); err != nil {
		t.Fatalf("rewrite Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load after rewrite: %v", err)
	}
	if got == nil || got.Distro != "Debian" {
		t.Errorf("rewrite did not stick: %+v", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("rewrite left stray temp files: %v", names)
	}
}

func TestConfigRoundTripsInstalledBinPath(t *testing.T) {
	dir := t.TempDir()
	want := &Config{
		Distro: "Ubuntu", InstalledVer: "v1.2.3", InstalledDistro: "Ubuntu",
		InstalledBinPath: "/home/alice/.local/bin/agent-overflow",
	}
	if err := Save(dir, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil || got.InstalledBinPath != want.InstalledBinPath {
		t.Fatalf("InstalledBinPath = %+v, want %q", got, want.InstalledBinPath)
	}
}
