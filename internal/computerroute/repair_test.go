package computerroute

import (
	"strings"
	"testing"
)

func TestRepairReusesTrustWithoutTrustingAnArbitraryDomain(t *testing.T) {
	pin := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name     string
		primary  Route
		known    []Route
		endpoint string
		wantPin  string
		refuse   bool
	}{
		{"private LAN moves", Route{"https://192.168.1.4:8443", pin}, nil, "https://192.168.1.8:9443", pin, false},
		{"WebPKI port changes", Route{"https://gpu.tailnet.test", ""}, nil, "https://gpu.tailnet.test:8443", "", false},
		{"new domain cannot claim ID", Route{"https://gpu.tailnet.test", ""}, nil, "https://another.test", "", true},
		{"known LAN pin from tailnet", Route{"https://gpu.tailnet.test", ""}, []Route{{"https://192.168.1.4", pin}}, "https://192.168.1.8", pin, false},
		{"cleartext refused", Route{"https://192.168.1.4", pin}, nil, "http://192.168.1.8", "", true},
		{"credentials refused", Route{"https://192.168.1.4", pin}, nil, "https://user:secret@192.168.1.8", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RepairCandidates(tc.primary, tc.known, tc.endpoint)
			if tc.refuse {
				if err == nil {
					t.Fatal("untrusted address was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Endpoint != tc.endpoint || got[0].CertFingerprint != tc.wantPin {
				t.Fatalf("unexpected candidates: %+v", got)
			}
		})
	}
}

func TestRepairDoesNotRestoreASupersededPin(t *testing.T) {
	oldPin := "sha256:" + strings.Repeat("a", 64)
	newPin := "sha256:" + strings.Repeat("b", 64)
	primary := Route{"https://192.168.1.4", oldPin}
	got, err := RepairCandidates(primary, []Route{{primary.Endpoint, newPin}}, "https://192.168.1.8")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].CertFingerprint != newPin {
		t.Fatalf("restored old certificate trust: %+v", got)
	}
}
