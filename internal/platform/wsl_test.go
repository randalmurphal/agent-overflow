package platform

import (
	"errors"
	"testing"
)

func TestIsWSLFromOSReleaseDetectsMicrosoftKernel(t *testing.T) {
	cases := []struct {
		name    string
		release string
		want    bool
	}{
		{name: "wsl2", release: "5.15.146.1-microsoft-standard-WSL2", want: true},
		{name: "wsl1", release: "4.4.0-19041-Microsoft", want: true},
		{name: "native linux", release: "6.8.0-31-generic", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsWSLFromOSRelease(func(path string) ([]byte, error) {
				if path != WSLOSReleasePath {
					t.Fatalf("path = %q, want %q", path, WSLOSReleasePath)
				}
				return []byte(tc.release), nil
			})
			if got != tc.want {
				t.Fatalf("IsWSLFromOSRelease(%q) = %v, want %v", tc.release, got, tc.want)
			}
		})
	}
}

func TestIsWSLFromOSReleaseReturnsFalseOnReadError(t *testing.T) {
	got := IsWSLFromOSRelease(func(string) ([]byte, error) {
		return nil, errors.New("read failed")
	})
	if got {
		t.Fatal("IsWSLFromOSRelease returned true on read error")
	}
}
