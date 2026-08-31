package app

import (
	"errors"
	"testing"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/wsldistro"
)

func newBootUpdaterTestApp(currentVersion string) *App {
	return &App{updater: appupdate.New(currentVersion, appupdate.Deps{})}
}

func TestInitWSLUpdaterSkipsDevBuild(t *testing.T) {
	t.Setenv(wsldistro.AppDataEnv, t.TempDir())
	a := newBootUpdaterTestApp("dev")
	initWSLUpdaterIn(a, "dev", t.TempDir())

	availability, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if availability.Supported {
		t.Fatal("dev build configured WSL self-update")
	}
}

func TestInitWSLUpdaterRequiresLauncherEnv(t *testing.T) {
	t.Setenv(wsldistro.AppDataEnv, "")
	a := newBootUpdaterTestApp("0.0.10")
	initWSLUpdaterIn(a, "0.0.10", t.TempDir())

	availability, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if availability.Supported {
		t.Fatal("WSL self-update configured without the launcher-injected AppData path")
	}
}

func TestInitWSLUpdaterRequiresMarkerDir(t *testing.T) {
	t.Setenv(wsldistro.AppDataEnv, t.TempDir())
	a := newBootUpdaterTestApp("0.0.10")
	initWSLUpdaterIn(a, "0.0.10", "")

	availability, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if availability.Supported {
		t.Fatal("WSL self-update configured without a marker directory")
	}
}

func TestInitWSLUpdaterConfiguresService(t *testing.T) {
	t.Setenv(wsldistro.AppDataEnv, t.TempDir())
	a := newBootUpdaterTestApp("0.0.10")
	initWSLUpdaterIn(a, "0.0.10", t.TempDir())

	if err := a.RestartToUpdate(); !errors.Is(err, ErrUpdateNotReady) {
		t.Fatalf("RestartToUpdate error = %v, want ErrUpdateNotReady from configured service", err)
	}
}
