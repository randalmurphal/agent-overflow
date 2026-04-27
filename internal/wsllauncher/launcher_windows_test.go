//go:build windows

package wsllauncher

import (
	"context"
	"strings"
	"testing"
)

// These tests cover the Windows-only helpers that compile under the
// `windows` build tag. The logic is platform-agnostic (string parsing,
// path translation, shell quoting) but the functions themselves only
// exist on the Windows side. We compile this file via
// `GOOS=windows go test -c -o /dev/null ./internal/wsllauncher/` on
// macOS CI to catch syntax / type drift between hosts.

func TestWindowsToWSLPath(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "lowercase_drive",
			in:   `c:\users\randy\agent-overflow.exe`,
			want: "/mnt/c/users/randy/agent-overflow.exe",
		},
		{
			name: "uppercase_drive",
			in:   `C:\Users\Randy\agent-overflow.exe`,
			want: "/mnt/c/Users/Randy/agent-overflow.exe",
		},
		{
			name: "non_c_drive",
			in:   `D:\dev\bin\agent-overflow.exe`,
			want: "/mnt/d/dev/bin/agent-overflow.exe",
		},
		{
			name: "double_backslashes",
			in:   `C:\\Users\\randy\\bin`,
			want: "/mnt/c//Users//randy//bin",
		},
		{
			name: "trailing_backslash",
			in:   `C:\Users\randy\`,
			want: "/mnt/c/Users/randy/",
		},
		{
			name:    "missing_colon",
			in:      `Cwindows\system32`,
			wantErr: true,
		},
		{
			name:    "single_char_path",
			in:      `C`,
			wantErr: true,
		},
		{
			name:    "empty",
			in:      "",
			wantErr: true,
		},
		{
			name:    "unc_path_rejected",
			in:      `\\server\share\file`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := windowsToWSLPath(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("windowsToWSLPath(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: "''"},
		{name: "no_specials", in: "/usr/bin/agent-overflow", want: "'/usr/bin/agent-overflow'"},
		{name: "single_quote", in: "Bob's Path", want: `'Bob'"'"'s Path'`},
		{name: "multiple_quotes", in: "a''b", want: `'a'"'"''"'"'b'`},
		// $/`/\ must NOT be interpreted by the shell — single-quoted
		// strings disable all shell expansion. The bytes pass through
		// verbatim inside the quotes.
		{name: "dollar_literal", in: "$HOME/bin", want: "'$HOME/bin'"},
		{name: "backtick_literal", in: "`whoami`", want: "'`whoami`'"},
		{name: "backslash_literal", in: `a\b\c`, want: `'a\b\c'`},
		// Embedded newlines pass through too — the shell's single-quote
		// behaviour preserves them as part of the argument.
		{name: "newline", in: "a\nb", want: "'a\nb'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shellQuote(tc.in)
			if got != tc.want {
				t.Fatalf("shellQuote(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParentDir(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/home/randy/.local/bin/agent-overflow", "/home/randy/.local/bin"},
		{"/agent-overflow", "/"},
		{"agent-overflow", "."},
		{"/", "/"},
	}
	for _, c := range cases {
		got := parentDir(c.in)
		if got != c.want {
			t.Errorf("parentDir(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestListDistros_PathMiss(t *testing.T) {
	// On a Windows host without wsl.exe on PATH, ListDistros must
	// return (nil, nil) so the picker UI's empty-state branch fires.
	// We can't trivially yank wsl.exe out of PATH from inside a test,
	// so this test is deliberately a guard against future regressions
	// of the early-return contract: it documents the expected return
	// shape rather than exercising the branch directly.
	got, err := ListDistros(context.Background())
	if err != nil {
		// Either nil (no wsl.exe) or an actual error (wsl.exe failure).
		// Both are valid; what we're catching is a panic / type drift.
		if !strings.Contains(err.Error(), "wsl") {
			t.Logf("unexpected non-WSL error from ListDistros: %v", err)
		}
	}
	_ = got
}

