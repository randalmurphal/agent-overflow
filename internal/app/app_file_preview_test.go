package app

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/network"
	"agent-overflow/internal/transport"
)

func TestFilePreviewUsesAuthenticatedHostPresence(t *testing.T) {
	app := newPreviewTestApp(t, &fakeScanner{})
	app.SetTransportServer(startTestTransportServer(t))
	t.Cleanup(func() { _ = app.closePreviewGateway() })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("page"), 0o600); err != nil {
		t.Fatal(err)
	}
	remote, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{SessionID: "paired"})
	if _, err := app.MintFilePreviewURL(remote, "index.html", dir); err == nil {
		t.Fatal("remote caller received loopback fallback")
	}
	local := transport.WithCallerProof(remote, transport.CallerProof{HostPresent: true})
	link, err := app.MintFilePreviewURL(local, "index.html", dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		t.Fatalf("%q %v", link, err)
	}
	if !app.previewGatewayBuilt() {
		t.Fatal("file-only preview omitted from shutdown")
	}
	if err := app.closePreviewGateway(); err != nil {
		t.Fatal(err)
	}
	if app.previewGatewayBuilt() {
		t.Fatal("shutdown retained preview manager")
	}
}

func TestSharingPolicyChangeRetiresExistingPreviewGateway(t *testing.T) {
	app := newPreviewTestApp(t, &fakeScanner{})
	app.SetTransportServer(startTestTransportServer(t))
	t.Cleanup(func() { _ = app.closePreviewGateway() })
	before := app.previewGateway()
	if before == nil {
		t.Fatal("no gateway")
	}
	if _, err := app.SetNetworkSettings(context.Background(), network.Settings{BindAll: true}); err != nil {
		t.Fatal(err)
	}
	if app.previewGatewayBuilt() {
		t.Fatal("changed sharing policy retained the old preview listener set")
	}
}
