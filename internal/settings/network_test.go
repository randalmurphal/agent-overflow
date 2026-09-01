package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The domain half of the network key persists through the same one write
// path the bind toggle uses, and comes back spelled the way it will be
// compared against an SNI name.
func TestCanonicalDomainRoundTripsThroughTheOneWritePath(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.SetNetwork(NetworkSettings{
		CanonicalDomain: "  Backend.Example  ",
		ACMEDNSHook:     []string{" dns-hook ", "--zone", "example"},
	})
	if err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	if updated.Network.CanonicalDomain != "backend.example" {
		t.Fatalf("canonicalDomain = %q, want the trimmed lower-cased spelling", updated.Network.CanonicalDomain)
	}
	if !slices.Equal(updated.Network.ACMEDNSHook, []string{"dns-hook", "--zone", "example"}) {
		t.Fatalf("acmeDnsHook = %v, want each argument trimmed", updated.Network.ACMEDNSHook)
	}
	if !updated.Network.WantsACME() {
		t.Fatal("a domain plus a hook is not reported as wanting issuance")
	}

	reloaded := NewService(dir).Get().Network
	if reloaded.CanonicalDomain != "backend.example" {
		t.Fatalf("reloaded canonicalDomain = %q", reloaded.CanonicalDomain)
	}
	if !slices.Equal(reloaded.ACMEDNSHook, []string{"dns-hook", "--zone", "example"}) {
		t.Fatalf("reloaded acmeDnsHook = %v", reloaded.ACMEDNSHook)
	}

	// The generic patch path stays closed over the whole group, not just
	// over bindAll: a domain change moves which name the transport
	// answers to, which is the same class of change as the bind.
	if _, err := svc.Update(map[string]any{"network": map[string]any{"canonicalDomain": "elsewhere.example"}}); err == nil {
		t.Fatal("Update accepted the network key")
	}
	if svc.Get().Network.CanonicalDomain != "backend.example" {
		t.Fatal("a refused Update still changed the domain")
	}
}

// Every rule that would otherwise have to be answered at handshake time,
// where there is nobody to tell.
func TestSetNetworkRefusesAConfigurationThatCannotServe(t *testing.T) {
	tests := []struct {
		name  string
		in    NetworkSettings
		names string
	}{
		{
			name:  "a URL where a hostname belongs",
			in:    NetworkSettings{CanonicalDomain: "https://backend.example/"},
			names: "must not include a scheme",
		},
		{
			name:  "a host and port",
			in:    NetworkSettings{CanonicalDomain: "backend.example:8443"},
			names: "bare hostname",
		},
		{
			name:  "a hook with nothing to publish for",
			in:    NetworkSettings{ACMEDNSHook: []string{"dns-hook"}},
			names: "needs network.canonicalDomain",
		},
		{
			name:  "a blank argument in the hook",
			in:    NetworkSettings{CanonicalDomain: "backend.example", ACMEDNSHook: []string{"dns-hook", "  "}},
			names: "blank argument",
		},
		{
			name:  "half an external pair",
			in:    NetworkSettings{CanonicalDomain: "backend.example", ExternalCertFile: "/etc/ao/fullchain.pem"},
			names: "externalKeyFile is required",
		},
		{
			name:  "the other half",
			in:    NetworkSettings{CanonicalDomain: "backend.example", ExternalKeyFile: "/etc/ao/privkey.pem"},
			names: "externalCertFile is required",
		},
		{
			name: "a certificate nothing can select",
			in: NetworkSettings{
				ExternalCertFile: "/etc/ao/fullchain.pem",
				ExternalKeyFile:  "/etc/ao/privkey.pem",
			},
			names: "needs network.canonicalDomain",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			svc := NewService(dir)
			_, err := svc.SetNetwork(test.in)
			if err == nil {
				t.Fatalf("SetNetwork(%+v) was accepted", test.in)
			}
			if !strings.Contains(err.Error(), test.names) {
				t.Fatalf("error %q does not name %q", err, test.names)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "settings.json")); statErr == nil {
				var fileMap map[string]any
				data, readErr := os.ReadFile(filepath.Join(dir, "settings.json"))
				if readErr != nil {
					t.Fatalf("read: %v", readErr)
				}
				if err := json.Unmarshal(data, &fileMap); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if _, wrote := fileMap["network"]; wrote {
					t.Fatal("a refused write still persisted the network key")
				}
			}
		})
	}
}

// A relative path resolves against whatever directory launched the
// backend, which is not the same directory on the next launch.
func TestExternalCertificatePathsMustBeAbsolute(t *testing.T) {
	_, err := validateNetwork(NetworkSettings{
		CanonicalDomain:  "backend.example",
		ExternalCertFile: "certs/fullchain.pem",
		ExternalKeyFile:  filepath.Join(t.TempDir(), "privkey.pem"),
	})
	if err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("error = %v, want a refusal naming the absolute-path rule", err)
	}
}

// The external pair is the escape hatch and it wins: a user who already
// holds a certificate is not made to obtain another one.
func TestAnExternalPairStopsIssuance(t *testing.T) {
	n, err := validateNetwork(NetworkSettings{
		CanonicalDomain:  "backend.example",
		ACMEDNSHook:      []string{"dns-hook"},
		ExternalCertFile: "/etc/ao/fullchain.pem",
		ExternalKeyFile:  "/etc/ao/privkey.pem",
	})
	if err != nil {
		t.Fatalf("validateNetwork: %v", err)
	}
	if !n.HasExternalPair() {
		t.Fatal("both halves are set but HasExternalPair is false")
	}
	if n.WantsACME() {
		t.Fatal("issuance is still wanted with an external certificate configured")
	}
}

// A hand-edited file with one unusable value loses that value, not the
// whole group, and never the bind toggle: dropping to the self-signed
// certificate is what the backend did before any of this was configured.
func TestAHandEditedFileLosesOnlyTheUnusableHalf(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{
		"network": {
			"bindAll": true,
			"canonicalDomain": "https://backend.example/",
			"acmeDnsHook": ["dns-hook"]
		}
	}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	loaded := NewService(dir).Get().Network
	if !loaded.BindAll {
		t.Fatal("the bind toggle was dropped along with the domain")
	}
	if loaded.CanonicalDomain != "" || len(loaded.ACMEDNSHook) != 0 {
		t.Fatalf("the unusable TLS configuration survived load: %+v", loaded)
	}
}
