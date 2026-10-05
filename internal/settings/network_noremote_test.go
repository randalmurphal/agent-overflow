//go:build noremote

package settings

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/buildvariant"
)

// Every network setting that would reach this backend from another
// machine is refused on the one write path; the loopback port is not.
func TestNoremoteSetNetworkRefusesEveryRemoteSetting(t *testing.T) {
	svc := NewService(t.TempDir())
	cases := []struct {
		field string
		in    NetworkSettings
	}{
		{"network.bindAll", NetworkSettings{BindAll: true}},
		{"network.canonicalDomain", NetworkSettings{CanonicalDomain: "backend.example"}},
		{"network.acmeDnsHook", NetworkSettings{ACMEDNSHook: []string{"hook"}}},
		{"network.externalCertFile", NetworkSettings{ExternalCertFile: "/c.pem", ExternalKeyFile: "/k.pem"}},
		{"network.tailnetEnabled", NetworkSettings{TailnetEnabled: true}},
		{"network.tailnetControlUrl", NetworkSettings{TailnetControlURL: "https://control.example"}},
		{"network.previewPorts", NetworkSettings{PreviewPorts: []int{5173}}},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			_, err := svc.SetNetwork(tc.in)
			if !errors.Is(err, buildvariant.ErrRemoteAccessUnavailable) {
				t.Fatalf("SetNetwork(%+v) = %v, want the remote-access refusal", tc.in, err)
			}
			if got := svc.Get().Network; got.remoteField() != "" {
				t.Fatalf("a refused write still changed the network settings: %+v", got)
			}
		})
	}
	updated, err := svc.SetNetwork(NetworkSettings{ListenPort: 41234})
	if err != nil {
		t.Fatalf("SetNetwork(listenPort) = %v; the loopback port stays configurable", err)
	}
	if updated.Network.ListenPort != 41234 {
		t.Fatalf("listenPort = %d, want 41234", updated.Network.ListenPort)
	}
}

// A settings.json written by a build with remote access loads with every
// remote setting off and the loopback port kept, and loading does not
// rewrite the file.
func TestNoremoteLoadIgnoresSavedRemoteSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	saved := []byte(`{"network":{"bindAll":true,"listenPort":41235,"tailnetEnabled":true,"canonicalDomain":"backend.example","previewPorts":[5173]}}`)
	if err := os.WriteFile(path, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	got := NewService(dir).Get().Network
	if field := got.remoteField(); field != "" {
		t.Fatalf("loaded %s from disk in a build without remote access: %+v", field, got)
	}
	if got.ListenPort != 41235 {
		t.Fatalf("listenPort = %d, want the saved 41235", got.ListenPort)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(saved) {
		t.Fatalf("loading rewrote settings.json:\n%s", after)
	}
}
