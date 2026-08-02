package provider

import "testing"

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

func TestFormatCodexCLIUpgradeMessage(t *testing.T) {
	got := formatCodexCLIUpgradeMessage("0.36.0")
	want := "Codex CLI v0.36.0 is too old for Agent Overflow. Upgrade to v0.143.0 or newer and restart the app."
	if got != want {
		t.Fatalf("formatCodexCLIUpgradeMessage() = %q, want %q", got, want)
	}

	got = formatCodexCLIUpgradeMessage("")
	want = "Codex CLI is too old for Agent Overflow. Upgrade to v0.143.0 or newer and restart the app."
	if got != want {
		t.Fatalf("formatCodexCLIUpgradeMessage(empty) = %q, want %q", got, want)
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
// Lowering minimumCodexCLIVersion below 0.140.0 would silently reintroduce
// a "method not found" failure on the per-row stop path, so it fails here
// instead.
func TestMinimumCodexCLIVersionCoversBackgroundTerminalTerminate(t *testing.T) {
	const backgroundTerminalTerminateFloor = "0.140.0"
	if compareCodexCLIVersions(minimumCodexCLIVersion, backgroundTerminalTerminateFloor) < 0 {
		t.Fatalf(
			"minimumCodexCLIVersion = %q is below the %q floor required by "+
				"thread/backgroundTerminals/terminate; either raise the minimum or "+
				"add a runtime capability check in internal/provider/codex/session_background.go",
			minimumCodexCLIVersion, backgroundTerminalTerminateFloor,
		)
	}
}
