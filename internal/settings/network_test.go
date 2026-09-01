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

// The listen port persists through the same one write path, and zero is
// a real stored value meaning "automatic" rather than an absent one.
func TestListenPortRoundTripsThroughTheOneWritePath(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.SetNetwork(NetworkSettings{ListenPort: 7777})
	if err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	if updated.Network.ListenPort != 7777 {
		t.Fatalf("ListenPort = %d, want 7777", updated.Network.ListenPort)
	}
	if got := NewService(dir).Get().Network.ListenPort; got != 7777 {
		t.Fatalf("reloaded ListenPort = %d, want 7777", got)
	}

	if _, err := svc.SetNetwork(NetworkSettings{ListenPort: 0}); err != nil {
		t.Fatalf("SetNetwork clearing the port: %v", err)
	}
	if got := NewService(dir).Get().Network.ListenPort; got != 0 {
		t.Fatalf("reloaded ListenPort = %d after clearing, want 0", got)
	}
}

// Refused, never clamped. A clamp would bind SOME port and report
// success, and the operator would then be looking for their backend at a
// number nothing ever chose.
func TestSetNetworkRefusesAnUnusableListenPort(t *testing.T) {
	for _, port := range []int{-1, 65536, 1 << 20} {
		if _, err := NewService(t.TempDir()).SetNetwork(NetworkSettings{ListenPort: port}); err == nil {
			t.Errorf("SetNetwork accepted listenPort %d", port)
		}
	}
	// Privileged ports are allowed: a backend with CAP_NET_BIND_SERVICE
	// can hold one, and this package cannot see that capability.
	for _, port := range []int{1, 443, 65535} {
		if _, err := NewService(t.TempDir()).SetNetwork(NetworkSettings{ListenPort: port}); err != nil {
			t.Errorf("SetNetwork refused listenPort %d: %v", port, err)
		}
	}
}

// The bind half is two values and only one of them can be wrong, so an
// out-of-range port drops to automatic without taking the toggle, the
// domain, or the tailnet with it.
func TestAHandEditedFileLosesOnlyTheUnusablePort(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{
		"network": {
			"bindAll": true,
			"listenPort": 99999,
			"canonicalDomain": "ao.example.com",
			"tailnetEnabled": true
		}
	}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	loaded := NewService(dir).Get().Network
	if loaded.ListenPort != 0 {
		t.Fatalf("the out-of-range port survived load as %d", loaded.ListenPort)
	}
	if !loaded.BindAll {
		t.Error("the bind toggle was dropped along with the port")
	}
	if loaded.CanonicalDomain != "ao.example.com" {
		t.Errorf("the domain was dropped along with the port: %q", loaded.CanonicalDomain)
	}
	if !loaded.TailnetEnabled {
		t.Error("the tailnet was dropped along with the port")
	}
}

// The tailnet half persists through the same one write path, and its
// control URL is validated where a person can be told rather than at
// bring-up, minutes later, with nothing connecting the two.
func TestTailnetPreferenceRoundTripsThroughTheOneWritePath(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.SetNetwork(NetworkSettings{
		TailnetEnabled:    true,
		TailnetControlURL: "  https://headscale.example/  ",
	})
	if err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	if !updated.Network.WantsTailnet() {
		t.Fatal("the enabled toggle did not survive the write")
	}
	if updated.Network.TailnetControlURL != "https://headscale.example/" {
		t.Fatalf("tailnetControlUrl = %q, want the trimmed spelling", updated.Network.TailnetControlURL)
	}

	reloaded := NewService(dir).Get().Network
	if !reloaded.TailnetEnabled || reloaded.TailnetControlURL != "https://headscale.example/" {
		t.Fatalf("reloaded tailnet preference = %+v", reloaded)
	}

	// Off by default, and an empty control URL means the public
	// coordination server rather than an unset field nothing reads.
	fresh := NewService(t.TempDir()).Get().Network
	if fresh.WantsTailnet() || fresh.TailnetControlURL != "" {
		t.Fatalf("a fresh install starts with %+v, want the feature off", fresh)
	}
}

// A control URL a node could not register with is refused at the write,
// because the alternative is a node parked waiting for a sign-in that
// never arrives.
func TestSetNetworkRefusesAnUnusableControlURL(t *testing.T) {
	for _, url := range []string{
		"headscale.example",
		"ftp://headscale.example",
		"https://",
		"://nope",
	} {
		t.Run(url, func(t *testing.T) {
			svc := NewService(t.TempDir())
			if _, err := svc.SetNetwork(NetworkSettings{TailnetEnabled: true, TailnetControlURL: url}); err == nil {
				t.Fatalf("SetNetwork accepted tailnetControlUrl %q", url)
			}
		})
	}
}

// The tailnet half is independent of the TLS half in both directions: a
// domain typo must not take the node off the tailnet, and an unusable
// control URL drops the whole tailnet half rather than quietly
// registering this node with a coordination server the user never named.
func TestAHandEditedFileKeepsTheTailnetHalfSeparate(t *testing.T) {
	t.Run("a bad domain keeps the tailnet preference", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{
			"network": {
				"canonicalDomain": "https://backend.example/",
				"tailnetEnabled": true,
				"tailnetControlUrl": "https://headscale.example/"
			}
		}`), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		loaded := NewService(dir).Get().Network
		if loaded.CanonicalDomain != "" {
			t.Fatalf("the unusable domain survived load: %q", loaded.CanonicalDomain)
		}
		if !loaded.WantsTailnet() || loaded.TailnetControlURL != "https://headscale.example/" {
			t.Fatalf("the tailnet half was dropped with the domain: %+v", loaded)
		}
	})

	t.Run("a bad control URL drops the whole tailnet half", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{
			"network": {
				"bindAll": true,
				"tailnetEnabled": true,
				"tailnetControlUrl": "headscale.example"
			}
		}`), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		loaded := NewService(dir).Get().Network
		if !loaded.BindAll {
			t.Fatal("the bind toggle was dropped along with the tailnet half")
		}
		if loaded.WantsTailnet() {
			t.Fatal("the node would join the public coordination server instead of the one that was configured")
		}
		if loaded.TailnetControlURL != "" {
			t.Fatalf("the unusable control URL survived load: %q", loaded.TailnetControlURL)
		}
	})

	t.Run("a bad control URL keeps the domain half", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{
			"network": {
				"canonicalDomain": "backend.example",
				"acmeDnsHook": ["update-dns"],
				"tailnetEnabled": true,
				"tailnetControlUrl": "headscale.example"
			}
		}`), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		loaded := NewService(dir).Get().Network
		if loaded.CanonicalDomain != "backend.example" || len(loaded.ACMEDNSHook) != 1 {
			t.Fatalf("the domain half was dropped with the tailnet half: %+v", loaded)
		}
		if loaded.WantsTailnet() || loaded.TailnetControlURL != "" {
			t.Fatalf("the unusable tailnet half survived load: %+v", loaded)
		}
	})
}
