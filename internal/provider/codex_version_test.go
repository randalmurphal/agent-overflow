package provider

import (
	"errors"
	"strings"
	"testing"
	"unsafe"
)

// TestParseCodexVersion pins the one Codex version grammar: strict SemVer
// with three numbers, an optional prerelease and optional build metadata,
// and the result an owned copy.
func TestParseCodexVersion(t *testing.T) {
	for _, token := range []string{
		"0.154.0", "0.154.0-alpha.2", "0.154.0-rc.1", "0.154.0+company.1",
		"0.154.0-rc.1+build.7", "10.0.0", "0.154.0-0", "0.154.0-x-y.z",
	} {
		got, ok := ParseCodexVersion(token)
		if !ok || got != token {
			t.Errorf("ParseCodexVersion(%q) = (%q, %v), want it admitted whole", token, got, ok)
		}
		if ok && unsafe.StringData(got) == unsafe.StringData(token) {
			t.Errorf("ParseCodexVersion(%q) shares the input's bytes", token)
		}
	}
	for _, token := range []string{
		"", "0.154", "0", "0.154.0.1", "v0.154.0", "01.154.0", "0.01.0", "0.154.00",
		"0.154.0-.", "0.154.0-", "0.154.0+", "0.154.0+.", "0.154.0-rc..1", "0.154.0-01",
		"dev-0.154.0", "0.154.0 ", " 0.154.0", "0.154.0-rc_1", "garbage",
	} {
		if got, ok := ParseCodexVersion(token); ok {
			t.Errorf("ParseCodexVersion(%q) = %q, want it refused", token, got)
		}
	}
}

// TestCompareCodexVersions is SemVer precedence: a prerelease below its
// release, numeric identifiers by value and below alphanumeric ones, a
// shorter prerelease below a longer one it prefixes, and build metadata
// ignored.
func TestCompareCodexVersions(t *testing.T) {
	ordered := []string{
		"0.37.0", "0.120.0-alpha", "0.120.0-alpha.1", "0.120.0-alpha.beta",
		"0.120.0-beta.2", "0.120.0-beta.11", "0.120.0-rc.1", "0.120.0", "0.120.1", "1.0.0",
	}
	for i := range ordered {
		for j := range ordered {
			got := CompareCodexVersions(ordered[i], ordered[j])
			if (i < j && got >= 0) || (i > j && got <= 0) || (i == j && got != 0) {
				t.Errorf("CompareCodexVersions(%q, %q) = %d", ordered[i], ordered[j], got)
			}
		}
	}
	if got := CompareCodexVersions("0.154.0+company.1", "0.154.0"); got != 0 {
		t.Errorf("build metadata ordered %d against its release, want 0", got)
	}
}

// TestParseCodexCLIVersion reads `codex --version` output. It keeps the
// display forms a wrapper prints (a v prefix, a two-part version), reads
// the same build ParseCodexVersion reads, and refuses a malformed one.
func TestParseCodexCLIVersion(t *testing.T) {
	for input, want := range map[string]string{
		"codex-cli 0.120.0":           "0.120.0",
		"codex 0.37":                  "0.37.0",
		"codex v0.37.0-rc.1":          "0.37.0-rc.1",
		"codex v0.154.0":              "0.154.0",
		"codex-cli 0.154.0+company.1": "0.154.0+company.1",
		"codex 0.154-rc.1":            "0.154.0-rc.1",
		"(codex-cli 0.154.0)":         "0.154.0",
		"99.0.0 (ao-mockprovider; impersonates Claude Code and codex-cli 99.0.0)": "99.0.0",
		"codex version unknown": "",
		"codex-cli 0.154.0-.":   "",
		"codex-cli 0.154.0+":    "",
		"codex-cli 01.154.0":    "",
		"codex-cli dev-0.154.0": "",
		"":                      "",
	} {
		if got := ParseCodexCLIVersion(input); got != want {
			t.Errorf("ParseCodexCLIVersion(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestCheckCodexVersionBoundary pins the minimum: the last release before
// it is refused, the minimum and later releases are admitted, a
// pre-release of the minimum orders below it and is refused, and text that
// is not a version is a caller's error.
func TestCheckCodexVersionBoundary(t *testing.T) {
	cases := []struct {
		version  string
		admitted bool
	}{
		{"0.153.4", false},
		{"0.154.0-alpha.10", false},
		{"0.154.0-rc.1", false},
		{"0.154.0", true},
		{"0.154.0+company.1", true},
		{"0.154.1", true},
		{"0.168.0", true},
		{"", false},
		{"garbage", false},
		{"0.154.0-.", false},
		{"0.154.0+", false},
		{"01.154.0", false},
	}
	for _, tc := range cases {
		err := CheckCodexVersion("/opt/codex", tc.version)
		if tc.admitted {
			if err != nil {
				t.Errorf("CheckCodexVersion(%q) = %v, want admitted", tc.version, err)
			}
			continue
		}
		// Text that is not a version is a caller's error, never a refusal:
		// a reply that names no build is not read.
		var versionErr *CodexVersionError
		if _, readable := ParseCodexVersion(tc.version); !readable {
			if err == nil || errors.As(err, &versionErr) || errors.Is(err, ErrCodexVersionUnsupported) {
				t.Errorf("CheckCodexVersion(%q) = %v, want a plain error", tc.version, err)
			}
			continue
		}
		if !errors.As(err, &versionErr) || !errors.Is(err, ErrCodexVersionUnsupported) || versionErr.Version != tc.version {
			t.Errorf("CheckCodexVersion(%q) = %v, want a *CodexVersionError", tc.version, err)
		}
	}
}

func TestCodexVersionErrorMessages(t *testing.T) {
	cases := []struct {
		err  CodexVersionError
		want string
	}{
		{
			CodexVersionError{Path: "/home/me/.local/bin/codex", Version: "0.150.0"},
			"Codex v0.150.0 (/home/me/.local/bin/codex) is installed; Agent Overflow needs v0.154.0 or newer. Update Codex.",
		},
		{
			CodexVersionError{Path: "/Users/me/My Tools/codex", Version: "0.150.0"},
			"Codex v0.150.0 (/Users/me/My Tools/codex) is installed; Agent Overflow needs v0.154.0 or newer. Update Codex.",
		},
		{
			CodexVersionError{Version: "0.150.0"},
			"Codex v0.150.0 is installed; Agent Overflow needs v0.154.0 or newer. Update Codex.",
		},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}

// TestCheckCodexVersionBoundsTheTextItKeeps builds a refusal from a long
// version below the minimum. It keeps at most 120 bytes of it, copied, so a
// cached refusal neither pins nor repeats megabytes of provider output.
func TestCheckCodexVersionBoundsTheTextItKeeps(t *testing.T) {
	version := "0.153.0-" + strings.Repeat("x", 4096)
	var refusal *CodexVersionError
	if !errors.As(CheckCodexVersion("/opt/codex", version), &refusal) {
		t.Fatal("a version below the minimum was admitted")
	}
	if want := version[:120] + "..."; refusal.Version != want {
		t.Fatalf("Version = %q, want %q", refusal.Version, want)
	}
	if unsafe.StringData(refusal.Version) == unsafe.StringData(version) {
		t.Fatal("Version shares the input's bytes")
	}
	if len(refusal.Error()) > 400 {
		t.Fatalf("message is %d bytes: %q", len(refusal.Error()), refusal.Error())
	}
}

// TestMinimumCodexCLIVersionCoversBackgroundTerminalTerminate pins the
// dependency internal/provider/codex/session_background.go relies on:
// `thread/backgroundTerminals/terminate` shipped in codex 0.140.0, and
// TerminateBackgroundTerminal calls it with no runtime capability probe
// because the provider-wide floor already excludes anything older.
// Lowering MinimumCodexCLIVersion below 0.140.0 would silently reintroduce
// a "method not found" failure on the per-row stop path, so it fails here
// instead.
func TestMinimumCodexCLIVersionCoversBackgroundTerminalTerminate(t *testing.T) {
	const backgroundTerminalTerminateFloor = "0.140.0"
	if CompareCodexVersions(MinimumCodexCLIVersion, backgroundTerminalTerminateFloor) < 0 {
		t.Fatalf(
			"MinimumCodexCLIVersion = %q is below the %q floor required by "+
				"thread/backgroundTerminals/terminate; either raise the minimum or "+
				"add a runtime capability check in internal/provider/codex/session_background.go",
			MinimumCodexCLIVersion, backgroundTerminalTerminateFloor,
		)
	}
}
