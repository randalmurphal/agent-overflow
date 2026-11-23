package provider

import (
	"errors"
	"strings"
	"testing"
)

func TestParseCodexCLIVersion(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{
			name:   "parses current cli format",
			input:  "codex-cli 0.120.0",
			expect: "0.120.0",
		},
		{
			name:   "normalizes missing patch",
			input:  "codex 0.37",
			expect: "0.37.0",
		},
		{
			name:   "preserves prerelease suffix",
			input:  "codex v0.37.0-rc.1",
			expect: "0.37.0-rc.1",
		},
		{
			name:   "rejects missing semver",
			input:  "codex version unknown",
			expect: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseCodexCLIVersion(tt.input); got != tt.expect {
				t.Fatalf("parseCodexCLIVersion(%q) = %q, want %q", tt.input, got, tt.expect)
			}
		})
	}
}

func TestCompareCodexCLIVersions(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
		want  int
	}{
		{
			name:  "supports equal versions",
			left:  "0.37.0",
			right: "0.37.0",
			want:  0,
		},
		{
			name:  "compares later minor version",
			left:  "0.120.0",
			right: "0.37.0",
			want:  1,
		},
		{
			name:  "compares prerelease lower than final",
			left:  "0.37.0-rc.1",
			right: "0.37.0",
			want:  -1,
		},
		{
			name:  "normalizes missing patch before compare",
			left:  "0.37",
			right: "0.37.0",
			want:  0,
		},
		{
			name:  "shorter prerelease sorts lower",
			left:  "0.37.0-rc",
			right: "0.37.0-rc.1",
			want:  -1,
		},
		{
			name:  "longer prerelease sorts higher",
			left:  "0.37.0-rc.1",
			right: "0.37.0-rc",
			want:  1,
		},
		{
			name:  "falls back for invalid semver",
			left:  "garbage",
			right: "0.37.0",
			want:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compareCodexCLIVersions(tt.left, tt.right)
			switch {
			case tt.want == 0 && got != 0:
				t.Fatalf("compareCodexCLIVersions(%q, %q) = %d, want 0", tt.left, tt.right, got)
			case tt.want < 0 && got >= 0:
				t.Fatalf("compareCodexCLIVersions(%q, %q) = %d, want < 0", tt.left, tt.right, got)
			case tt.want > 0 && got <= 0:
				t.Fatalf("compareCodexCLIVersions(%q, %q) = %d, want > 0", tt.left, tt.right, got)
			}
		})
	}
}

// TestCheckCodexVersionBoundary pins the minimum: the last release before
// it is refused, the minimum and later releases are admitted, a
// pre-release of the minimum orders below it and is refused, and an
// unreadable version is refused as unreadable.
func TestCheckCodexVersionBoundary(t *testing.T) {
	cases := []struct {
		version  string
		admitted bool
	}{
		{"0.153.4", false},
		{"0.154.0-alpha.10", false},
		{"0.154.0", true},
		{"0.154.1", true},
		{"0.168.0", true},
		{"", false},
		{"garbage", false},
	}
	for _, tc := range cases {
		err := CheckCodexVersion("/opt/codex", tc.version, "codex-cli "+tc.version)
		if tc.admitted {
			if err != nil {
				t.Errorf("CheckCodexVersion(%q) = %v, want admitted", tc.version, err)
			}
			continue
		}
		var versionErr *CodexVersionError
		if !errors.As(err, &versionErr) || !errors.Is(err, ErrCodexVersionUnsupported) {
			t.Errorf("CheckCodexVersion(%q) = %v, want a *CodexVersionError", tc.version, err)
			continue
		}
		readable := tc.version != "" && tc.version != "garbage"
		if (versionErr.Version != "") != readable {
			t.Errorf("CheckCodexVersion(%q).Version = %q, readable %v", tc.version, versionErr.Version, readable)
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
		{
			CodexVersionError{Path: "/opt/codex", Raw: "codex_cli_rs/dev (Linux)"},
			`Agent Overflow could not read the version of Codex (/opt/codex) from "codex_cli_rs/dev (Linux)". Update Codex to v0.154.0 or newer.`,
		},
		{
			CodexVersionError{Path: "/opt/codex"},
			"Agent Overflow could not read the version of Codex (/opt/codex). Update Codex to v0.154.0 or newer.",
		},
		{
			CodexVersionError{Path: "/opt/codex", Raw: strings.Repeat("x", 200)},
			`Agent Overflow could not read the version of Codex (/opt/codex) from "` + strings.Repeat("x", 120) + `...". Update Codex to v0.154.0 or newer.`,
		},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}

func TestCompareCodexPrereleaseIdentifier(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
		want  int
	}{
		{
			name:  "numeric compares numerically",
			left:  "2",
			right: "10",
			want:  -1,
		},
		{
			name:  "numeric sorts before text",
			left:  "1",
			right: "beta",
			want:  -1,
		},
		{
			name:  "text sorts after numeric",
			left:  "beta",
			right: "1",
			want:  1,
		},
		{
			name:  "text compares lexicographically",
			left:  "beta",
			right: "alpha",
			want:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compareCodexPrereleaseIdentifier(tt.left, tt.right)
			switch {
			case tt.want == 0 && got != 0:
				t.Fatalf("compareCodexPrereleaseIdentifier(%q, %q) = %d, want 0", tt.left, tt.right, got)
			case tt.want < 0 && got >= 0:
				t.Fatalf("compareCodexPrereleaseIdentifier(%q, %q) = %d, want < 0", tt.left, tt.right, got)
			case tt.want > 0 && got <= 0:
				t.Fatalf("compareCodexPrereleaseIdentifier(%q, %q) = %d, want > 0", tt.left, tt.right, got)
			}
		})
	}
}

func TestIsNumericString(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{input: "", want: false},
		{input: "123", want: true},
		{input: "12a", want: false},
	}

	for _, tt := range tests {
		if got := isNumericString(tt.input); got != tt.want {
			t.Fatalf("isNumericString(%q) = %v, want %v", tt.input, got, tt.want)
		}
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
	if compareCodexCLIVersions(MinimumCodexCLIVersion, backgroundTerminalTerminateFloor) < 0 {
		t.Fatalf(
			"MinimumCodexCLIVersion = %q is below the %q floor required by "+
				"thread/backgroundTerminals/terminate; either raise the minimum or "+
				"add a runtime capability check in internal/provider/codex/session_background.go",
			MinimumCodexCLIVersion, backgroundTerminalTerminateFloor,
		)
	}
}
