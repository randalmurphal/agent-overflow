package main

import (
	"errors"
	"testing"
)

func TestIsUnderWindowsDriveMount(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Real Windows drive mounts (9p) — relocate away from these.
		{"/mnt/c", true},
		{"/mnt/c/", true},
		{"/mnt/c/Users/user/AppData/Local/Programs/Agent Overflow", true},
		{"/mnt/d/projects", true},
		{"/mnt/z", true},
		{"/mnt/C/Windows", true}, // uppercase drive (custom automount casing)

		// WSL's own Linux-backed tmpfs mounts under /mnt — must NOT relocate.
		{"/mnt/wsl", false},
		{"/mnt/wsl/some/path", false},
		{"/mnt/wslg", false},
		{"/mnt/wslg/distro", false},

		// Single non-letter segment: not a drive (pins the ASCII-letter check).
		{"/mnt/1", false},
		{"/mnt/-", false},

		// Malformed or non-drive paths under /mnt.
		{"/mnt", false},
		{"/mnt/", false},
		{"/mnt//c", false}, // empty first segment
		{"/mnt/cd", false}, // two-letter segment is not a drive

		// Linux-native paths and edge inputs.
		{"/home/user/repos/agent-overflow", false},
		{"/", false},
		{"", false},
		{"mnt/c", false},      // not rooted at /mnt/
		{"/tmp/mnt/c", false}, // /mnt not at the root
	}
	for _, tc := range cases {
		if got := isUnderWindowsDriveMount(tc.path); got != tc.want {
			t.Errorf("isUnderWindowsDriveMount(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestRelocateCwd(t *testing.T) {
	const winCwd = "/mnt/c/Users/user/AppData/Local/Programs/Agent Overflow"
	const linuxCwd = "/home/user/repos/agent-overflow"
	const home = "/home/user"

	ok := func(s string) func() (string, error) { return func() (string, error) { return s, nil } }
	fail := func(msg string) func() (string, error) {
		return func() (string, error) { return "", errors.New(msg) }
	}

	for _, tc := range []struct {
		name     string
		wsl      bool
		getwd    func() (string, error)
		home     func() (string, error)
		chdirErr error
		wantDir  string // "" when chdir must not run
	}{
		{"not WSL: never chdirs", false, ok(winCwd), ok(home), nil, ""},
		{"WSL but Linux-native cwd: no chdir", true, ok(linuxCwd), ok(home), nil, ""},
		{"WSL on a Windows mount: chdirs to home", true, ok(winCwd), ok(home), nil, home},
		{"getwd error: no chdir, no panic", true, fail("getwd boom"), ok(home), nil, ""},
		{"home unavailable: no chdir, degrades", true, ok(winCwd), fail("no home"), nil, ""},
		{"empty home (no error): no chdir", true, ok(winCwd), ok(""), nil, ""},
		{"chdir error: attempted, degrades without panic", true, ok(winCwd), ok(home), errors.New("chdir denied"), home},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dirs []string
			relocateCwd(tc.wsl, tc.getwd, tc.home, func(dir string) error {
				dirs = append(dirs, dir)
				return tc.chdirErr
			})
			switch {
			case tc.wantDir == "" && len(dirs) != 0:
				t.Fatalf("chdir called with %q, want no chdir", dirs)
			case tc.wantDir != "" && (len(dirs) != 1 || dirs[0] != tc.wantDir):
				t.Fatalf("chdir calls = %q, want one to %q", dirs, tc.wantDir)
			}
		})
	}
}
