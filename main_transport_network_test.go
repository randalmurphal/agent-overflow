package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	appservice "agent-overflow/internal/app"
	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/netisolate"
	"agent-overflow/internal/servercert"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/transport"
)

// Use the production settings reader, bind resolver, port pin and real TLS
// listener. No App/provider runtime is needed to exercise the boot contract.
func TestTransportBootRestoresLANAndPreservesExplicitAndIsolatedBinds(t *testing.T) {
	remotetest.Require(t)
	for _, tc := range []struct {
		name     string
		lan      bool
		explicit bool
		isolated bool
		// confined boots an isolated App outside the test network namespace,
		// whose Reach is loopback, while still reading saved settings as a
		// soak boot does.
		confined bool
	}{
		{name: "ordinary LAN restart", lan: true},
		{name: "ordinary private restart"},
		{name: "explicit loopback beats saved LAN", lan: true, explicit: true},
		{name: "isolated harness ignores saved host settings", lan: true, isolated: true},
		{name: "isolated soak keeps saved LAN on loopback", lan: true, confined: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.confined && netisolate.Contained() {
				t.Skip("inside the test network namespace an isolated boot uses its LAN")
			}
			previous := dataDirRoot
			dataDirRoot = t.TempDir()
			t.Cleanup(func() { dataDirRoot = previous })
			dir := bootSettingsDir()
			savedPort := freePort(t)
			if _, err := settings.NewService(dir).SetNetwork(settings.NetworkSettings{BindAll: tc.lan, ListenPort: savedPort, CanonicalDomain: "ao.example.test"}); err != nil {
				t.Fatal(err)
			}
			material, err := servercert.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			certificates := transport.NewCertificateSource()
			certificates.SetSelfSigned(&material.Certificate)
			var firstPort int
			for range 2 {
				cfg := transport.Config{Dispatcher: transport.NewDispatcher(), EventBus: transport.NewEventBus(0), Certificates: certificates}
				listen := ""
				if tc.explicit {
					listen = "127.0.0.1:" + strconv.Itoa(freePort(t))
				}
				// An ordinary headless/desktop boot uses the zero option value.
				opts := bootTransportOptions{IgnorePersistedNetwork: tc.isolated}
				app := NewApp()
				if tc.confined {
					appservice.ConfigureIsolation(app.App, appservice.IsolationConfig{})
				}
				settingsPort, domain, err := configureTransportNetwork(&cfg, app, listen, opts.IgnorePersistedNetwork)
				if err != nil {
					t.Fatal(err)
				}
				if tc.isolated {
					if settingsPort != 0 || domain != "" || cfg.CanonicalHost != "" {
						t.Fatal("isolated boot imported host settings")
					}
				} else if settingsPort != savedPort || domain != "ao.example.test" || cfg.CanonicalHost != domain {
					t.Fatal("boot omitted stored port/canonical domain")
				}
				requested := cfg.Port
				pin := pinTransportPort(&cfg, dir, settingsPort, false)
				srv, err := transport.New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := srv.Start(); err != nil {
					t.Fatal(err)
				}
				stop := func() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if err := srv.Shutdown(ctx); err != nil {
						t.Error(err)
					}
				}
				t.Cleanup(stop)
				pin.adopt(srv.Addr())
				host, _, err := net.SplitHostPort(srv.Addr())
				if err != nil {
					t.Fatal(err)
				}
				ip := net.ParseIP(host)
				wantLAN := tc.lan && !tc.explicit && !tc.isolated && !tc.confined
				if ip == nil || ip.To4() == nil || (wantLAN && !ip.IsUnspecified()) || (!wantLAN && !ip.IsLoopback()) {
					t.Fatalf("listener %s does not match saved/explicit bind policy (LAN=%v)", srv.Addr(), wantLAN)
				}
				port := portFromAddr(srv.Addr())
				if tc.explicit && port != requested {
					t.Fatal("saved port overrode explicit CLI port")
				}
				if !tc.explicit && !tc.isolated && port != savedPort {
					t.Fatal("saved port was not restored")
				}
				if !tc.explicit && firstPort != 0 && port != firstPort {
					t.Fatal("restart changed the persisted origin")
				}
				firstPort = port
				// Both the launcher loopback hop and LAN clients terminate the same TLS
				// listener. The installed certificate must remain usable after restoring it.
				tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
				client := &http.Client{Transport: tr, Timeout: time.Second}
				req, err := http.NewRequest(http.MethodGet, "https://127.0.0.1:"+strconv.Itoa(port)+"/healthz", nil)
				if err != nil {
					t.Fatal(err)
				}
				if !tc.isolated {
					req.Host = domain
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				tr.CloseIdleConnections()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("TLS health response after boot: %s", resp.Status)
				}
				stop()
			}
		})
	}
}

// TestIsolatedBootRefusesANonLoopbackListen: outside the test network
// namespace an explicit --listen cannot put an isolated instance on the LAN.
func TestIsolatedBootRefusesANonLoopbackListen(t *testing.T) {
	t.Parallel()
	if netisolate.Contained() {
		t.Skip("inside the test network namespace an isolated boot uses its LAN")
	}
	isolated, ordinary := NewApp(), NewApp()
	appservice.ConfigureIsolation(isolated.App, appservice.IsolationConfig{})
	for _, tc := range []struct {
		listen string
		ok     bool
	}{
		{"127.0.0.1:0", true},
		{":0", true},
		{"0.0.0.0:0", false},
		{"192.168.1.20:0", false},
		{"localhost:0", false},
	} {
		if _, _, err := configureTransportNetwork(&transport.Config{}, isolated, tc.listen, true); (err == nil) != tc.ok {
			t.Errorf("isolated --listen %s: err = %v, want accepted %v", tc.listen, err, tc.ok)
		}
		if _, _, err := configureTransportNetwork(&transport.Config{}, ordinary, tc.listen, true); err != nil {
			t.Errorf("ordinary boot refused --listen %s: %v", tc.listen, err)
		}
	}
}
