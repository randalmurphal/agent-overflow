package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/buildvariant"
	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
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

// The GitLab release feed reads through the App's forge API transport,
// resolved per call: before Start there is none, and once the Core has one
// the request goes to the feed's host with the transport's token, bounded
// by the caller's deadline rather than the transport's read timeout.
func TestGitLabReleaseClientUsesTheAppsForgeAPI(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []*http.Request
	delay := make(chan time.Duration, 1)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(context.Background()))
		mu.Unlock()
		select {
		case d := <-delay:
			time.Sleep(d)
		default:
		}
		_, _ = w.Write([]byte(`[{"tag_name":"v1"}]`))
	}))
	t.Cleanup(fake.Close)
	a := &App{}
	client := a.gitlabReleaseClient("gitlab.example.com")
	var out bytes.Buffer
	if err := client.Stream(t.Context(), "projects/grp%2Fapp/releases", &out, 1<<20); !errors.Is(err, gitops.ErrNoForgeAPI) {
		t.Fatalf("Stream before Start = %v, want ErrNoForgeAPI", err)
	}

	svc, err := forgeapi.New(forgeapi.Options{Version: "test", ReadTimeout: 250 * time.Millisecond,
		Isolated: &forgeapi.Isolated{BaseURL: fake.URL, Token: "fake-token"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	a.git = gitops.NewCore(gitops.WithForgeAPI(svc))
	if err := client.Stream(t.Context(), "projects/grp%2Fapp/releases", &out, 1<<20); err != nil || out.String() != `[{"tag_name":"v1"}]` {
		t.Fatalf("Stream = %q, %v", out.String(), err)
	}
	mu.Lock()
	got := seen[0]
	mu.Unlock()
	if got.URL.EscapedPath() != "/gitlab/api/v4/projects/grp%2Fapp/releases" || got.Host != "gitlab.example.com" ||
		got.Header.Get("Private-Token") != "fake-token" || got.Header.Get("Accept") != "*/*" {
		t.Fatalf("request = %s %s host %q accept %q", got.Method, got.URL.EscapedPath(), got.Host, got.Header.Get("Accept"))
	}

	// A call that outlasts the read timeout completes inside the caller's
	// deadline, and fails without one.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	delay <- 750 * time.Millisecond
	out.Reset()
	if err := client.Stream(ctx, "projects/grp%2Fapp/releases", &out, 1<<20); err != nil {
		t.Fatalf("Stream under a 10s deadline = %v", err)
	}
	delay <- 750 * time.Millisecond
	if err := client.Stream(t.Context(), "projects/grp%2Fapp/releases", io.Discard, 1<<20); err == nil {
		t.Fatal("Stream without a deadline outlasted the read timeout")
	}
}
