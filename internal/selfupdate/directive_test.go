package selfupdate

import (
	"encoding/hex"
	"strings"
	"testing"
)

// validDigestHex is a syntactically valid 64-hex-char (32-byte) SHA-256.
const validDigestHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestInstallDirectiveValidate(t *testing.T) {
	base := InstallDirective{Filename: "agent-overflow-wsl-amd64.exe", SHA256: validDigestHex, Version: "0.0.11"}

	tests := []struct {
		name string
		d    InstallDirective
		ok   bool
	}{
		{"canonical release asset", base, true},
		{"uppercase hex digest", InstallDirective{base.Filename, strings.ToUpper(validDigestHex), base.Version}, true},
		{"prerelease version", InstallDirective{base.Filename, base.SHA256, "0.0.11-rc.1+build_5"}, true},

		{"empty filename", InstallDirective{"", base.SHA256, base.Version}, false},
		{"windows traversal", InstallDirective{`..\evil.exe`, base.SHA256, base.Version}, false},
		{"unix traversal", InstallDirective{"../evil.exe", base.SHA256, base.Version}, false},
		{"nested path", InstallDirective{"a/b.exe", base.SHA256, base.Version}, false},
		{"parent dir", InstallDirective{"..", base.SHA256, base.Version}, false},
		{"absolute windows path", InstallDirective{`C:\Windows\system32\cmd.exe`, base.SHA256, base.Version}, false},
		{"not an exe", InstallDirective{"agent-overflow-wsl-amd64", base.SHA256, base.Version}, false},
		{"wrong extension", InstallDirective{"payload.dll", base.SHA256, base.Version}, false},
		{"leading dot", InstallDirective{".hidden.exe", base.SHA256, base.Version}, false},
		{"embedded space", InstallDirective{"agent overflow.exe", base.SHA256, base.Version}, false},
		{"over-length filename", InstallDirective{strings.Repeat("a", maxFilenameLen) + ".exe", base.SHA256, base.Version}, false},

		{"empty digest", InstallDirective{base.Filename, "", base.Version}, false},
		{"short digest", InstallDirective{base.Filename, validDigestHex[:32], base.Version}, false},
		{"long digest", InstallDirective{base.Filename, validDigestHex + "ab", base.Version}, false},
		{"non-hex digest", InstallDirective{base.Filename, strings.Repeat("z", 64), base.Version}, false},

		{"empty version", InstallDirective{base.Filename, base.SHA256, ""}, false},
		{"version with separator", InstallDirective{base.Filename, base.SHA256, "0.0.11/../x"}, false},
		{"version with space", InstallDirective{base.Filename, base.SHA256, "0.0.11 beta"}, false},
		{"version with leading dot", InstallDirective{base.Filename, base.SHA256, ".0.0.11"}, false},
		{"over-length version", InstallDirective{base.Filename, base.SHA256, strings.Repeat("1", maxVersionLen+1)}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.d.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate(%+v) = %v, want nil", tc.d, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("Validate(%+v) = nil, want an error", tc.d)
			}
		})
	}
}

func TestInstallDirectiveDigest(t *testing.T) {
	d := InstallDirective{Filename: "agent-overflow-wsl-amd64.exe", SHA256: validDigestHex, Version: "0.0.11"}
	got, err := d.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if len(got) != 32 {
		t.Fatalf("Digest len = %d, want 32", len(got))
	}
	if hex.EncodeToString(got) != validDigestHex {
		t.Fatalf("Digest = %x, want %s", got, validDigestHex)
	}

	if _, err := (InstallDirective{SHA256: validDigestHex[:32]}).Digest(); err == nil {
		t.Fatal("Digest of a short hex string = nil error, want an error")
	}
}
