package editor

import (
	"errors"
	"os"
	"testing"
)

func TestIsWSLEnv_DetectsMicrosoftKernel(t *testing.T) {
	cases := []struct {
		name    string
		release string
		want    bool
	}{
		{"wsl2 lowercase", "5.15.146.1-microsoft-standard-WSL2", true},
		{"wsl1 mixed case", "4.4.0-19041-Microsoft", true},
		{"native linux", "6.6.10-arch1-1", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFS{
				files: map[string][]byte{wslOSReleasePath: []byte(tc.release)},
			}
			env := newDetectEnv(fs, pathTable{}, nil)
			if got := isWSLEnv(env); got != tc.want {
				t.Fatalf("isWSLEnv = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsWSLEnv_ReturnsFalseOnReadError(t *testing.T) {
	fs := &fakeFS{} // no fixture for /proc/sys/kernel/osrelease
	env := newDetectEnv(fs, pathTable{}, nil)
	if isWSLEnv(env) {
		t.Fatal("expected false when /proc read fails")
	}
}

func TestPathTargetsWindows_DirectMntCPath(t *testing.T) {
	fs := &fakeFS{}
	env := newDetectEnv(fs, pathTable{}, nil)
	if !pathTargetsWindows("/mnt/c/Program Files/Microsoft VS Code/bin/code", env) {
		t.Fatal("absolute /mnt/c path must short-circuit to true")
	}
	if !pathTargetsWindows("/mnt/c", env) {
		t.Fatal("/mnt/c root must register")
	}
}

func TestPathTargetsWindows_ShimContentSniff(t *testing.T) {
	shim := []byte(`#!/bin/bash
# Microsoft VS Code WSL launcher
ELECTRON="/mnt/c/Users/Alice/AppData/Local/Programs/Microsoft VS Code/Code.exe"
exec "$ELECTRON" "$@"
`)
	fs := &fakeFS{
		files: map[string][]byte{"/usr/local/bin/code": shim},
	}
	env := newDetectEnv(fs, pathTable{}, nil)
	if !pathTargetsWindows("/usr/local/bin/code", env) {
		t.Fatal("shim that exec's /mnt/c/... must register as windows-targeting")
	}
}

func TestPathTargetsWindows_LinuxNativeRejected(t *testing.T) {
	linuxOnly := []byte(`#!/bin/sh
exec /usr/share/code-oss/code-oss "$@"
`)
	fs := &fakeFS{
		files: map[string][]byte{"/usr/bin/code-oss": linuxOnly},
	}
	env := newDetectEnv(fs, pathTable{}, nil)
	if pathTargetsWindows("/usr/bin/code-oss", env) {
		t.Fatal("linux-only shim must NOT register as windows-targeting")
	}
}

func TestPathTargetsWindows_ReadErrorIsFalse(t *testing.T) {
	fs := &fakeFS{}
	env := newDetectEnv(fs, pathTable{}, nil)
	if pathTargetsWindows("/no/such/file", env) {
		t.Fatal("missing binary must collapse to not-windows")
	}
}

func TestFindWindowsInstall_SkipsUnknownEditors(t *testing.T) {
	fs := &fakeFS{}
	env := newDetectEnv(fs, pathTable{}, nil)
	if got := findWindowsInstall("nonexistent-editor", env); got != "" {
		t.Fatalf("unknown editor must return empty; got %q", got)
	}
}

func TestFindWindowsInstall_HandlesUsersDirReadError(t *testing.T) {
	// /mnt/c/Users readDir fails → caller falls through to the
	// system-paths list. This covers the "WSL distro hasn't
	// auto-mounted /mnt/c yet" case.
	systemPath := "/mnt/c/Program Files/Microsoft VS Code/bin/code"
	fs := &fakeFS{
		files: map[string][]byte{systemPath: []byte("")},
	}
	env := newDetectEnv(fs, pathTable{}, nil)
	got := findWindowsInstall("code", env)
	if got != systemPath {
		t.Fatalf("expected system fallback %q, got %q", systemPath, got)
	}
}

func TestFindWindowsInstall_StatErrorPropagatesToFalse(t *testing.T) {
	// stat returning a transient error (not nil + not exists) must
	// not crash; the caller treats it as "not found" so the next
	// candidate is probed.
	fs := &statErrorFS{err: errors.New("permission denied")}
	env := detectEnv{
		readDir:  fs.readDir,
		stat:     fs.stat,
		readFile: fs.read,
	}
	if got := findWindowsInstall("code", env); got != "" {
		t.Fatalf("stat error must produce empty result; got %q", got)
	}
}

func TestToWSLUNCPath_NoOpOutsideWSL(t *testing.T) {
	env := detectEnv{
		envValue: func(name string) (string, bool) { return "", false },
	}
	got := toWSLUNCPathWithEnv("/home/alice/project", env)
	if got != "/home/alice/project" {
		t.Fatalf("expected pass-through outside WSL; got %q", got)
	}
}

func TestToWSLUNCPath_TranslatesUsingDistroEnv(t *testing.T) {
	env := detectEnv{
		envValue: func(name string) (string, bool) {
			if name == "WSL_DISTRO_NAME" {
				return "Ubuntu-22.04", true
			}
			return "", false
		},
	}
	got := toWSLUNCPathWithEnv("/home/alice/agent-overflow", env)
	want := `\\wsl.localhost\Ubuntu-22.04\home\alice\agent-overflow`
	if got != want {
		t.Fatalf("toWSLUNCPath:\n got: %q\nwant: %q", got, want)
	}
}

// statErrorFS makes /mnt/c/Users readDir succeed (returning one user)
// while every stat call returns a transient error so we can assert the
// finder keeps walking instead of bailing on the first hit.
type statErrorFS struct {
	err error
}

func (f *statErrorFS) read(string) ([]byte, error)      { return nil, os.ErrNotExist }
func (f *statErrorFS) readDir(string) ([]string, error) { return []string{"alice"}, nil }
func (f *statErrorFS) stat(string) (bool, error)        { return false, f.err }
