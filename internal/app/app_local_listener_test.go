package app

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"agent-overflow/internal/localcontrol"
	"agent-overflow/internal/store"
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

// Clients beside the providers dial ::1, which stays in the Linux kernel
// under WSL virtioproxy, while the main bind keeps the IPv4 address Windows
// reaches. The ::1 door serves the same routes and credentials, and the page
// it mints still names the main bind, so a desktop window's origin is stable.
func TestLocalClientsUseTheIPv6LoopbackListener(t *testing.T) {
	t.Parallel()
	app, mainAddr := newLocalListenerTestApp(t)
	app.startLocalListener()
	local := app.localListenerAddr()
	addr, err := netip.ParseAddrPort(local)
	if err != nil || addr.Addr() != netip.IPv6Loopback() {
		t.Fatalf("local listener = %q, want a [::1] address", local)
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
	t.Parallel()
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
	// Assert on the listener, not by dialing its freed port: a parallel
	// test can bind that port, and a dial to a closed local port can
	// connect to itself. The deadline keeps an open listener from blocking.
	if tcp, ok := ln.(*net.TCPListener); ok {
		_ = tcp.SetDeadline(time.Now().Add(time.Second))
	}
	if conn, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("the failed listener is still open: Accept = %v", err)
	}
	endpoint, err := localcontrol.Read(app.configDir)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Address != mainAddr {
		t.Fatalf("control.json address = %q, want the main bind %q", endpoint.Address, mainAddr)
	}
}
