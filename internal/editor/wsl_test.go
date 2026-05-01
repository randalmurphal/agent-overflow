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

// TestValidateWindowsCodeShim_BrokenInstallRejected pins the fix for
// the half-uninstalled VS Code case: bin/code is still present but the
// VERSIONFOLDER it references no longer has resources/app/out/cli.js.
// Without rejection, detection would hand back this candidate and the
// silently-failing spawn would leave the user wondering why click-to-
// open does nothing.
func TestValidateWindowsCodeShim_BrokenInstallRejected(t *testing.T) {
	shim := []byte(`#!/usr/bin/env sh
COMMIT="072586267e68ece9a47aa43f8c108e0dcbf44622"
APP_NAME="code"
QUALITY="stable"
NAME="Code"
SERVERDATAFOLDER=".vscode-server"
VERSIONFOLDER="072586267e"
VSCODE_PATH="$(dirname "$(dirname "$(realpath "$0")")")"
ELECTRON="$VSCODE_PATH/$NAME.exe"
`)
	// Note: cli.js intentionally absent — this is the broken install.
	fs := &fakeFS{
		files: map[string][]byte{
			"/mnt/c/Program Files/Microsoft VS Code/bin/code": shim,
		},
	}
	env := newDetectEnv(fs, pathTable{}, nil)
	if validateWindowsCodeShim("/mnt/c/Program Files/Microsoft VS Code/bin/code", env) {
		t.Fatal("broken shim must be rejected (cli.js missing under VERSIONFOLDER)")
	}
}

// TestValidateWindowsCodeShim_WorkingInstallAccepted confirms the
// happy path: the shim's VERSIONFOLDER points at a cli.js that exists.
func TestValidateWindowsCodeShim_WorkingInstallAccepted(t *testing.T) {
	shim := []byte(`#!/usr/bin/env sh
VERSIONFOLDER="034f571df5"
ELECTRON="something"
`)
	fs := &fakeFS{
		files: map[string][]byte{
			"/mnt/c/Users/alice/AppData/Local/Programs/Microsoft VS Code/bin/code":                                 shim,
			"/mnt/c/Users/alice/AppData/Local/Programs/Microsoft VS Code/034f571df5/resources/app/out/cli.js":      []byte("// cli.js"),
		},
	}
	env := newDetectEnv(fs, pathTable{}, nil)
	if !validateWindowsCodeShim("/mnt/c/Users/alice/AppData/Local/Programs/Microsoft VS Code/bin/code", env) {
		t.Fatal("working shim with matching cli.js must be accepted")
	}
}

// TestValidateWindowsCodeShim_UnrecognizedFormatPassesThrough covers
// editors whose shim doesn't follow the Microsoft VERSIONFOLDER pattern
// (Sublime, Zed, a custom $EDITOR target). We have no cheap way to
// validate them, so detection should defer to the spawn step rather
// than rejecting them outright. Returning true here keeps existing
// non-Microsoft editors working unchanged.
func TestValidateWindowsCodeShim_UnrecognizedFormatPassesThrough(t *testing.T) {
	shim := []byte(`#!/bin/sh
exec /opt/sublime-text/sublime_text "$@"
`)
	fs := &fakeFS{
		files: map[string][]byte{"/usr/local/bin/subl": shim},
	}
	env := newDetectEnv(fs, pathTable{}, nil)
	if !validateWindowsCodeShim("/usr/local/bin/subl", env) {
		t.Fatal("unrecognized shim format must pass validation (defer to spawn)")
	}
}

// TestValidateWindowsCodeShim_ReadErrorRejects ensures we don't
// silently treat an unreadable shim as valid. If we can't read it, we
// can't know what it points at; better to fall through to the next
// candidate than to hand back a path we can't inspect.
func TestValidateWindowsCodeShim_ReadErrorRejects(t *testing.T) {
	fs := &fakeFS{} // no fixture for the path
	env := newDetectEnv(fs, pathTable{}, nil)
	if validateWindowsCodeShim("/no/such/path", env) {
		t.Fatal("unreadable shim must be rejected")
	}
}

// TestFindWindowsInstall_SkipsBrokenUserInstallContinuesWalk pins the
// multi-install fall-through: the first user-install hit has the bin
// shim but a missing cli.js, the second user has a complete install.
// Without the validation pass, findWindowsInstall would return the
// first hit and detection would point at the broken candidate.
func TestFindWindowsInstall_SkipsBrokenUserInstallContinuesWalk(t *testing.T) {
	brokenShim := []byte(`VERSIONFOLDER="072586267e"`)
	workingShim := []byte(`VERSIONFOLDER="034f571df5"`)
	fs := &fakeFS{
		files: map[string][]byte{
			"/mnt/c/Users/alice/AppData/Local/Programs/Microsoft VS Code/bin/code":                                  brokenShim,
			// alice's cli.js intentionally absent
			"/mnt/c/Users/bob/AppData/Local/Programs/Microsoft VS Code/bin/code":                                    workingShim,
			"/mnt/c/Users/bob/AppData/Local/Programs/Microsoft VS Code/034f571df5/resources/app/out/cli.js":         []byte("// cli.js"),
		},
		dirs: map[string][]string{"/mnt/c/Users": {"alice", "bob"}},
	}
	env := newDetectEnv(fs, pathTable{}, nil)
	got := findWindowsInstall("code", env)
	want := "/mnt/c/Users/bob/AppData/Local/Programs/Microsoft VS Code/bin/code"
	if got != want {
		t.Fatalf("findWindowsInstall: got %q, want %q", got, want)
	}
}

// TestFindWindowsInstall_FallsThroughToSystemWhenAllUsersBroken covers
// the case where every user-install candidate is broken but the
// system install is intact. Detection should reach the system path
// rather than reporting unavailable.
func TestFindWindowsInstall_FallsThroughToSystemWhenAllUsersBroken(t *testing.T) {
	brokenShim := []byte(`VERSIONFOLDER="aaaaaaaa"`)
	workingShim := []byte(`VERSIONFOLDER="034f571df5"`)
	fs := &fakeFS{
		files: map[string][]byte{
			"/mnt/c/Users/alice/AppData/Local/Programs/Microsoft VS Code/bin/code":           brokenShim,
			"/mnt/c/Program Files/Microsoft VS Code/bin/code":                                workingShim,
			"/mnt/c/Program Files/Microsoft VS Code/034f571df5/resources/app/out/cli.js":     []byte("// cli.js"),
		},
		dirs: map[string][]string{"/mnt/c/Users": {"alice"}},
	}
	env := newDetectEnv(fs, pathTable{}, nil)
	got := findWindowsInstall("code", env)
	want := "/mnt/c/Program Files/Microsoft VS Code/bin/code"
	if got != want {
		t.Fatalf("findWindowsInstall: got %q, want %q", got, want)
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
