package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/wsldistro"
	"agent-overflow/internal/wsllauncher"
)

// withWSLSeams swaps the package-level seams for the lifetime of the
// test and restores them on Cleanup. Keeps each test self-contained
// without bleeding fakes into other app_*_test.go files.
func withWSLSeams(
	t *testing.T,
	isWSL func() bool,
	listDistros func(context.Context) ([]wsllauncher.Distro, error),
	configDir func() (string, bool),
) {
	t.Helper()
	prevIs := wslIsWSL
	prevList := wslListDistros
	prevDir := wslConfigDir
	prevLoad := wslLoadConfig
	prevSave := wslSaveConfig
	wslIsWSL = isWSL
	wslListDistros = listDistros
	wslConfigDir = configDir
	wslLoadConfig = wsldistro.Load
	wslSaveConfig = wsldistro.Save
	t.Cleanup(func() {
		wslIsWSL = prevIs
		wslListDistros = prevList
		wslConfigDir = prevDir
		wslLoadConfig = prevLoad
		wslSaveConfig = prevSave
	})
}

func TestIsWSL_RoutesThroughSeam(t *testing.T) {
	app := &App{}
	withWSLSeams(t,
		func() bool { return true },
		nil,
		func() (string, bool) { return "", false },
	)
	got, err := app.IsWSL()
	if err != nil {
		t.Fatalf("IsWSL: %v", err)
	}
	if !got {
		t.Errorf("IsWSL = false, want true")
	}

	withWSLSeams(t,
		func() bool { return false },
		nil,
		func() (string, bool) { return "", false },
	)
	got, err = app.IsWSL()
	if err != nil {
		t.Fatalf("IsWSL: %v", err)
	}
	if got {
		t.Errorf("IsWSL = true, want false")
	}
}

func TestListWSLDistros_NotWSLReturnsNilNil(t *testing.T) {
	app := &App{}
	withWSLSeams(t,
		func() bool { return false },
		func(context.Context) ([]wsllauncher.Distro, error) {
			t.Fatal("ListDistros should not be called when IsWSL=false")
			return nil, nil
		},
		func() (string, bool) { return "", false },
	)
	got, err := app.ListWSLDistros()
	if err != nil {
		t.Fatalf("ListWSLDistros: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestListWSLDistros_WSLReturnsLiveList(t *testing.T) {
	app := &App{}
	want := []wsllauncher.Distro{
		{Name: "Ubuntu-24.04", Default: true, Version: 2, State: "Running"},
		{Name: "Debian", Version: 2, State: "Stopped"},
	}
	withWSLSeams(t,
		func() bool { return true },
		func(context.Context) ([]wsllauncher.Distro, error) { return want, nil },
		func() (string, bool) { return "", false },
	)
	got, err := app.ListWSLDistros()
	if err != nil {
		t.Fatalf("ListWSLDistros: %v", err)
	}
	if len(got) != 2 || got[0].Name != "Ubuntu-24.04" || got[1].Name != "Debian" {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestListWSLDistros_WrapsListError(t *testing.T) {
	app := &App{}
	withWSLSeams(t,
		func() bool { return true },
		func(context.Context) ([]wsllauncher.Distro, error) {
			return nil, errors.New("wsl.exe vanished")
		},
		func() (string, bool) { return "", false },
	)
	_, err := app.ListWSLDistros()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "list WSL distros") || !strings.Contains(err.Error(), "wsl.exe vanished") {
		t.Errorf("error doesn't wrap underlying message: %v", err)
	}
}

func TestGetWSLDistroPreference_NotWSLReturnsEmpty(t *testing.T) {
	app := &App{}
	withWSLSeams(t,
		func() bool { return false },
		nil,
		func() (string, bool) {
			t.Fatal("WSLConfigDir should not be called when IsWSL=false")
			return "", false
		},
	)
	got, err := app.GetWSLDistroPreference()
	if err != nil {
		t.Fatalf("GetWSLDistroPreference: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestGetWSLDistroPreference_NoConfigDirReturnsEmpty(t *testing.T) {
	app := &App{}
	withWSLSeams(t,
		func() bool { return true },
		nil,
		func() (string, bool) { return "", false },
	)
	got, err := app.GetWSLDistroPreference()
	if err != nil {
		t.Fatalf("GetWSLDistroPreference: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (no config dir)", got)
	}
}

func TestGetWSLDistroPreference_MissingFileReturnsEmpty(t *testing.T) {
	app := &App{}
	dir := t.TempDir()
	withWSLSeams(t,
		func() bool { return true },
		nil,
		func() (string, bool) { return dir, true },
	)
	got, err := app.GetWSLDistroPreference()
	if err != nil {
		t.Fatalf("GetWSLDistroPreference: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (file not yet created)", got)
	}
}

func TestGetWSLDistroPreference_RoundTripsSavedValue(t *testing.T) {
	app := &App{}
	dir := t.TempDir()
	if err := wsldistro.Save(dir, &wsldistro.Config{Distro: "Ubuntu-24.04"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	withWSLSeams(t,
		func() bool { return true },
		nil,
		func() (string, bool) { return dir, true },
	)
	got, err := app.GetWSLDistroPreference()
	if err != nil {
		t.Fatalf("GetWSLDistroPreference: %v", err)
	}
	if got != "Ubuntu-24.04" {
		t.Errorf("got %q, want Ubuntu-24.04", got)
	}
}

func TestSetWSLDistroPreference_RejectsWhenNotWSL(t *testing.T) {
	app := &App{}
	withWSLSeams(t,
		func() bool { return false },
		nil,
		func() (string, bool) { return "", false },
	)
	if _, err := app.SetWSLDistroPreference("Debian"); err == nil {
		t.Fatal("expected error from non-WSL host")
	}
}

func TestSetWSLDistroPreference_RejectsEmptyName(t *testing.T) {
	app := &App{}
	withWSLSeams(t,
		func() bool { return true },
		nil,
		func() (string, bool) { return "", false },
	)
	for _, in := range []string{"", "   ", "\t\n"} {
		if _, err := app.SetWSLDistroPreference(in); err == nil {
			t.Errorf("input %q: expected error, got nil", in)
		}
	}
}

func TestSetWSLDistroPreference_RejectsUnknownDistro(t *testing.T) {
	app := &App{}
	withWSLSeams(t,
		func() bool { return true },
		func(context.Context) ([]wsllauncher.Distro, error) {
			return []wsllauncher.Distro{{Name: "Ubuntu-24.04"}}, nil
		},
		func() (string, bool) { return t.TempDir(), true },
	)
	_, err := app.SetWSLDistroPreference("Fedora")
	if err == nil {
		t.Fatal("expected error for unknown distro")
	}
	if !strings.Contains(err.Error(), `"Fedora"`) {
		t.Errorf("error doesn't mention the bad name: %v", err)
	}
}

func TestSetWSLDistroPreference_RejectsWhenConfigDirUnavailable(t *testing.T) {
	app := &App{}
	withWSLSeams(t,
		func() bool { return true },
		func(context.Context) ([]wsllauncher.Distro, error) {
			return []wsllauncher.Distro{{Name: "Ubuntu-24.04"}}, nil
		},
		func() (string, bool) { return "", false },
	)
	_, err := app.SetWSLDistroPreference("Ubuntu-24.04")
	if err == nil {
		t.Fatal("expected error when config dir unavailable")
	}
	if !strings.Contains(err.Error(), "launcher config directory") {
		t.Errorf("error message %q doesn't mention config dir", err.Error())
	}
}

func TestSetWSLDistroPreference_PersistsAndPreservesInstallFields(t *testing.T) {
	app := &App{}
	dir := t.TempDir()
	// Seed a config with launcher-owned fields populated; the setter
	// must update only Distro and leave InstalledVer / InstalledDistro
	// alone — those track what the launcher last installed and where.
	if err := wsldistro.Save(dir, &wsldistro.Config{
		Distro:          "Ubuntu-24.04",
		InstalledVer:    "v0.1.0",
		InstalledDistro: "Ubuntu-24.04",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	withWSLSeams(t,
		func() bool { return true },
		func(context.Context) ([]wsllauncher.Distro, error) {
			return []wsllauncher.Distro{
				{Name: "Ubuntu-24.04"},
				{Name: "Debian"},
			}, nil
		},
		func() (string, bool) { return dir, true },
	)

	got, err := app.SetWSLDistroPreference("Debian")
	if err != nil {
		t.Fatalf("SetWSLDistroPreference: %v", err)
	}
	if got != "Debian" {
		t.Errorf("returned %q, want Debian", got)
	}

	// Re-load from disk: Distro flipped, install fields untouched.
	cfg, err := wsldistro.Load(dir)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg == nil {
		t.Fatal("config missing after Save")
	}
	if cfg.Distro != "Debian" {
		t.Errorf("Distro = %q, want Debian", cfg.Distro)
	}
	if cfg.InstalledVer != "v0.1.0" {
		t.Errorf("InstalledVer = %q, want v0.1.0 (must be preserved)", cfg.InstalledVer)
	}
	if cfg.InstalledDistro != "Ubuntu-24.04" {
		t.Errorf("InstalledDistro = %q, want Ubuntu-24.04 (must be preserved)", cfg.InstalledDistro)
	}
}

func TestSetWSLDistroPreference_TrimsName(t *testing.T) {
	app := &App{}
	dir := t.TempDir()
	withWSLSeams(t,
		func() bool { return true },
		func(context.Context) ([]wsllauncher.Distro, error) {
			return []wsllauncher.Distro{{Name: "Ubuntu-24.04"}}, nil
		},
		func() (string, bool) { return dir, true },
	)
	got, err := app.SetWSLDistroPreference("  Ubuntu-24.04  ")
	if err != nil {
		t.Fatalf("SetWSLDistroPreference with padded name: %v", err)
	}
	if got != "Ubuntu-24.04" {
		t.Errorf("got %q, want trimmed Ubuntu-24.04", got)
	}
	// Confirm what landed on disk was the trimmed value too.
	cfg, err := wsldistro.Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Distro != "Ubuntu-24.04" {
		t.Errorf("on-disk Distro = %q, want Ubuntu-24.04", cfg.Distro)
	}
}

func TestSetWSLDistroPreference_CreatesConfigOnFirstWrite(t *testing.T) {
	app := &App{}
	dir := filepath.Join(t.TempDir(), "agent-overflow") // not yet created
	withWSLSeams(t,
		func() bool { return true },
		func(context.Context) ([]wsllauncher.Distro, error) {
			return []wsllauncher.Distro{{Name: "Ubuntu-24.04"}}, nil
		},
		func() (string, bool) { return dir, true },
	)
	if _, err := app.SetWSLDistroPreference("Ubuntu-24.04"); err != nil {
		t.Fatalf("first-write SetWSLDistroPreference: %v", err)
	}
	cfg, err := wsldistro.Load(dir)
	if err != nil {
		t.Fatalf("reload after first write: %v", err)
	}
	if cfg == nil || cfg.Distro != "Ubuntu-24.04" {
		t.Errorf("first-write did not land on disk: %+v", cfg)
	}
}
