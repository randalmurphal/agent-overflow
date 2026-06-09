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
		{"/mnt/c/Users/rmurphy/AppData/Local/Programs/Agent Overflow", true},
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
		{"/home/rmurphy/repos/agent-overflow", false},
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
	const winCwd = "/mnt/c/Users/rmurphy/AppData/Local/Programs/Agent Overflow"
	const linuxCwd = "/home/rmurphy/repos/agent-overflow"
	const home = "/home/rmurphy"

	ok := func(s string) func() (string, error) { return func() (string, error) { return s, nil } }
	fail := func(msg string) func() (string, error) {
		return func() (string, error) { return "", errors.New(msg) }
	}

	t.Run("not WSL: never chdirs", func(t *testing.T) {
		called := false
		relocateCwd(false, ok(winCwd), ok(home), func(string) error { called = true; return nil })
		if called {
			t.Fatal("chdir called off WSL")
		}
	})

	t.Run("WSL but Linux-native cwd: no chdir", func(t *testing.T) {
		called := false
		relocateCwd(true, ok(linuxCwd), ok(home), func(string) error { called = true; return nil })
		if called {
			t.Fatal("chdir called for a Linux-native cwd")
		}
	})

	t.Run("WSL on a Windows mount: chdirs to home", func(t *testing.T) {
		got := ""
		relocateCwd(true, ok(winCwd), ok(home), func(dir string) error { got = dir; return nil })
		if got != home {
			t.Fatalf("chdir target = %q, want %q", got, home)
		}
	})

	t.Run("getwd error: no chdir, no panic", func(t *testing.T) {
		called := false
		relocateCwd(true, fail("getwd boom"), ok(home), func(string) error { called = true; return nil })
		if called {
			t.Fatal("chdir called despite getwd error")
		}
	})

	t.Run("home unavailable: no chdir, degrades", func(t *testing.T) {
		called := false
		relocateCwd(true, ok(winCwd), fail("no home"), func(string) error { called = true; return nil })
		if called {
			t.Fatal("chdir called despite unavailable home")
		}
	})

	t.Run("empty home (no error): no chdir", func(t *testing.T) {
		called := false
		relocateCwd(true, ok(winCwd), ok(""), func(string) error { called = true; return nil })
		if called {
			t.Fatal("chdir called despite empty home")
		}
	})

	t.Run("chdir error: attempted, degrades without panic", func(t *testing.T) {
		attempted := false
		relocateCwd(true, ok(winCwd), ok(home), func(string) error {
			attempted = true
			return errors.New("chdir denied")
		})
		if !attempted {
			t.Fatal("expected a chdir attempt")
		}
	})
}
