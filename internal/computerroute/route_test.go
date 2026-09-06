package computerroute

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{" https://GPU.Example:0443/ ", "https://gpu.example"},
		{"https://192.168.1.3:034115", "https://192.168.1.3:34115"},
		{"https://[fd00::1]:443/", "https://[fd00::1]"},
	} {
		got, err := Normalize(Route{Endpoint: tc.input})
		if err != nil || got.Endpoint != tc.want {
			t.Fatalf("%q: %+v, %v", tc.input, got, err)
		}
	}
	for _, input := range []string{"http://gpu", "https://user:secret@gpu", "https://gpu/path", "https://gpu/?ticket=secret", "https://gpu/?", "https://gpu/#secret", "https://gpu/#", "https://gpu/a/..", "https://gp\tu", "https://gpu:0", "https://gpu:65536", "https://gpu:", "https://gpu%00", "https://[fe80::1%25en0]", "https://gpu..example", "https://-gpu", "https://gpu_/", "https://gpu/%2f"} {
		if _, err := Normalize(Route{Endpoint: input}); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
	for _, pin := range []string{"sha256:short", "sha256:" + strings.Repeat("g", 64), "sha256:" + strings.Repeat("A", 64)} {
		if _, err := Normalize(Route{Endpoint: "https://gpu", CertFingerprint: pin}); err == nil {
			t.Errorf("accepted bad fingerprint %q", pin)
		}
	}
}

func TestMergeRetainsMissingRoutesAndBoundsTrust(t *testing.T) {
	old := Route{Endpoint: "https://gpu.example", CertFingerprint: "sha256:" + strings.Repeat("a", 64)}
	new := Route{Endpoint: "https://gpu.example", CertFingerprint: "sha256:" + strings.Repeat("b", 64)}
	lan := Route{Endpoint: "https://192.168.1.3:34115"}
	held := []Route{old, lan}
	if got := Merge(held, nil); !reflect.DeepEqual(got, held) {
		t.Fatalf("missing advertisement erased routes: %v", got)
	}
	if got := Merge(held, []Route{new, {Endpoint: "http://bad"}, new}); !reflect.DeepEqual(got, []Route{new, lan}) {
		t.Fatalf("trust update/duplicate: %v", got)
	}
	var many []Route
	for i := range 20 {
		many = append(many, Route{Endpoint: "https://" + string(rune('a'+i)) + ".example"})
	}
	if got := Merge(held, many); len(got) != MaxRoutes {
		t.Fatalf("unbounded: %v", got)
	}
}
