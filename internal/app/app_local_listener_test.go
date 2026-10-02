package app

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/localcontrol"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

func newLocalListenerTestApp(t *testing.T) (*App, string) {
	t.Helper()
	app := newTestAppWithStore(t)
	srv := startTestTransportServer(t)
	app.SetTransportServer(srv)
	app.configDir = t.TempDir()
	project := store.Project{ID: "local-listener", Path: t.TempDir(), Name: "Repo", CreatedAt: 1, UpdatedAt: 1}
	if _, err := app.store.CreateProject(project); err != nil {
		t.Fatal(err)
	}
	return app, srv.Addr()
}

func aoEndpointFor(t *testing.T, app *App) string {
	t.Helper()
	credential, err := app.mintAOCredential(store.Thread{ID: "chat", ProjectID: "local-listener", Mode: threadmode.ModeChat})
	if err != nil {
		t.Fatal(err)
	}
	return credential.env[aocli.EnvEndpoint]
}

// Clients beside the providers dial ::1, which stays in the Linux kernel
// under WSL virtioproxy, while the main bind keeps the IPv4 address Windows
// reaches. The ::1 door serves the same routes and credentials, and the page
// it mints still names the main bind, so a desktop window's origin is stable.
func TestLocalClientsUseTheIPv6LoopbackListener(t *testing.T) {
	app, mainAddr := newLocalListenerTestApp(t)
	app.startLocalListener()
	local := app.localListenerAddr()
	addr, err := netip.ParseAddrPort(local)
	if err != nil || addr.Addr() != netip.IPv6Loopback() {
		t.Fatalf("local listener = %q, want a [::1] address", local)
	}

	if got, want := aoEndpointFor(t, app), "http://"+local; got != want {
		t.Fatalf("AO_ENDPOINT = %q, want %q", got, want)
	}

	app.publishLocalControl()
	endpoint, err := localcontrol.Read(app.configDir)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Address != local {
		t.Fatalf("control.json address = %q, want %q", endpoint.Address, local)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	answer, err := localcontrol.Page(ctx, endpoint)
	if err != nil {
		t.Fatalf("authenticated request through %s: %v", local, err)
	}
	page, err := url.Parse(answer.URL)
	if err != nil {
		t.Fatal(err)
	}
	if page.Host != mainAddr {
		t.Fatalf("page URL host = %q, want the main bind %q", page.Host, mainAddr)
	}
}

func TestLocalClientsReturnToTheMainBindWhenTheListenerFails(t *testing.T) {
	app, mainAddr := newLocalListenerTestApp(t)
	app.startLocalListener()
	app.publishLocalControl()
	s := &app.localListener
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()

	app.localListenerFailed(ln, errors.New("accept loop ended"))

	if local := app.localListenerAddr(); local != "" {
		t.Fatalf("failed listener still advertised at %q", local)
	}
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		t.Fatal("the failed listener is still accepting")
	}
	endpoint, err := localcontrol.Read(app.configDir)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Address != mainAddr {
		t.Fatalf("control.json address = %q, want the main bind %q", endpoint.Address, mainAddr)
	}
	if got, want := aoEndpointFor(t, app), "http://"+mainAddr; got != want {
		t.Fatalf("AO_ENDPOINT = %q, want %q", got, want)
	}
}
