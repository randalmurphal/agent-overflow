package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/buildvariant"
	"agent-overflow/internal/wsldistro"
)

func newBootUpdaterTestApp(currentVersion string) *App {
	return &App{updater: appupdate.New(currentVersion, appupdate.Deps{})}
}

func TestInitWSLUpdaterSkipsDevBuild(t *testing.T) {
	t.Setenv(wsldistro.AppDataEnv, t.TempDir())
	a := newBootUpdaterTestApp("dev")
	initWSLUpdaterIn(a, "dev", t.TempDir(), appupdate.LauncherFailure{})

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
	initWSLUpdaterIn(a, "0.0.10", t.TempDir(), appupdate.LauncherFailure{})

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
	initWSLUpdaterIn(a, "0.0.10", "", appupdate.LauncherFailure{})

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
	initWSLUpdaterIn(a, "0.0.10", t.TempDir(), appupdate.LauncherFailure{})

	// A build without remote access updates only from the GitLab project its
	// release links in, and stays unconfigured without one. Run with
	// -ldflags=-X=agent-overflow/internal/appupdate.gitlabProject=HOST/GROUP/PROJECT
	// to check the shipped configuration.
	want := ErrUpdateNotReady
	if !buildvariant.RemoteAccess && !appupdate.ReleaseProjectLinked() {
		want = appupdate.ErrUpdatesUnsupported
	}
	if err := a.RestartToUpdate(); !errors.Is(err, want) {
		t.Fatalf("RestartToUpdate error = %v, want %v", err, want)
	}
}

// The GitLab release feed's glab runs through the App's git Core, so an
// isolated boot runs its fake glab and never the one on PATH.
func TestGlabAPIRunnerUsesTheIsolatedForgeCLI(t *testing.T) {
	markers := installForgeTraps(t)
	fake, record := writeFakeForge(t)
	a := &App{}
	ConfigureIsolation(a, IsolationConfig{ForgeCLI: fake})

	var out bytes.Buffer
	exitCode, _, err := a.glabAPIRunner(context.Background(), []string{"--hostname", "gitlab.example.com", "--", "projects/grp%2Fapp/releases"}, &out, 1<<20)
	if err != nil || exitCode != 0 {
		t.Fatalf("glabAPIRunner = %d, %v", exitCode, err)
	}
	if !strings.Contains(out.String(), "from fake") {
		t.Fatalf("runner output = %q, want the fake's answer", out.String())
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "glab ") {
		t.Fatalf("fake saw %q, want it run as glab", got)
	}
	if ran := trapsThatRan(t, markers); len(ran) != 0 {
		t.Fatalf("the real %v on PATH ran under an isolated App", ran)
	}
}
