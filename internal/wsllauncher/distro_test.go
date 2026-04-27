package wsllauncher

import (
	"embed"
	"errors"
	"testing"
)

//go:embed testdata/*.txt
var fixtures embed.FS

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fixtures.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("load fixture %s: %v", name, err)
	}
	return b
}

func TestParseDistroList(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    []Distro
		wantErr error
	}{
		{
			name:    "single_default",
			fixture: "single_default.txt",
			want: []Distro{
				{Name: "Ubuntu-22.04", Default: true, Version: 2, State: "Running"},
			},
		},
		{
			name:    "multi_with_default",
			fixture: "multi.txt",
			want: []Distro{
				{Name: "Ubuntu-22.04", Default: true, Version: 2, State: "Running"},
				{Name: "Debian", Default: false, Version: 2, State: "Stopped"},
				{Name: "Ubuntu", Default: false, Version: 1, State: "Running"},
			},
		},
		{
			name:    "wsl_not_installed",
			fixture: "empty.txt",
			want:    nil,
		},
		{
			name:    "no_bom_still_parses",
			fixture: "no_bom.txt",
			want: []Distro{
				{Name: "Alpine", Default: true, Version: 2, State: "Running"},
			},
		},
		{
			name:    "localized_header_skipped",
			fixture: "localized_header.txt",
			want: []Distro{
				{Name: "Fedora", Default: true, Version: 2, State: "Running"},
			},
		},
		{
			name:    "big_endian_bom_rejected",
			fixture: "bad_be_bom.txt",
			wantErr: ErrMalformedUTF16,
		},
		{
			// State="Installing" must pass through verbatim — wsl.exe
			// reports it during a fresh `wsl --install Ubuntu`. The
			// picker UI uses the raw value to render a "still
			// installing" badge.
			name:    "installing_state_passthrough",
			fixture: "installing.txt",
			want: []Distro{
				{Name: "AlmostUbuntu", Default: true, Version: 2, State: "Installing"},
			},
		},
		{
			// Header-only output ("no distros installed" on a Windows
			// host with the WSL feature enabled but no distros). Must
			// return empty + nil so the picker's empty-state branch
			// fires.
			name:    "no_distros_header_only",
			fixture: "no_distros.txt",
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDistroList(loadFixture(t, tc.fixture))
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("want error %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !equalDistros(got, tc.want) {
				t.Fatalf("distro mismatch:\ngot=  %#v\nwant= %#v", got, tc.want)
			}
		})
	}
}

func TestDecodeUTF16LE_OddLength(t *testing.T) {
	// Three bytes is invalid for UTF-16 (always 2-byte code units).
	_, err := decodeUTF16LE([]byte{0x41, 0x00, 0x42})
	if err == nil || !errors.Is(err, ErrMalformedUTF16) {
		t.Fatalf("want ErrMalformedUTF16, got %v", err)
	}
}

func TestSplitColumns(t *testing.T) {
	// Two-or-more spaces are the column delimiter; single spaces inside
	// a column (e.g. "Linux 22.04") must not split.
	got := splitColumns("Ubuntu 22.04    Running         2")
	want := []string{"Ubuntu 22.04", "Running", "2"}
	if !equalStrings(got, want) {
		t.Fatalf("splitColumns: got=%v want=%v", got, want)
	}
}

func TestIsNoDistrosMessage(t *testing.T) {
	// "no distros installed" must surface as empty-slice + nil; an
	// arbitrary wsl.exe failure (vmcompute down, kernel out of date)
	// must NOT be misclassified as "no distros" because that would
	// land the user on the picker's empty-state without a useful
	// error.
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "empty", in: "", want: false},
		{name: "english_no_installed", in: "Windows Subsystem for Linux has no installed distributions.", want: true},
		{name: "english_caps", in: "NO INSTALLED DISTRIBUTIONS.", want: true},
		{name: "raw_error_code", in: "WSL_E_DEFAULT_DISTRO_NOT_FOUND\r\n", want: true},
		{name: "loose_localized", in: "There are no Linux distributions installed.", want: true},
		{name: "vmcompute_broken", in: "Error code: WSL_E_VM_MODE_INVALID_STATE", want: false},
		{name: "kernel_outdated", in: "Please update the WSL kernel by running 'wsl --update'.", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isNoDistrosMessage(tc.in)
			if got != tc.want {
				t.Errorf("isNoDistrosMessage(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseInt(t *testing.T) {
	cases := []struct {
		in  string
		n   int
		ok  bool
	}{
		{"2", 2, true},
		{"42", 42, true},
		{"", 0, false},
		{"abc", 0, false},
		{"1a", 0, false},
	}
	for _, c := range cases {
		n, ok := parseInt(c.in)
		if n != c.n || ok != c.ok {
			t.Errorf("parseInt(%q) = (%d,%v); want (%d,%v)", c.in, n, ok, c.n, c.ok)
		}
	}
}

func equalDistros(a, b []Distro) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
