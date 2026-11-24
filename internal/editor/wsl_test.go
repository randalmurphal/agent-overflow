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

func TestPathTargetsWindows(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		files map[string][]byte
		want  bool
	}{
		// Absolute /mnt/c paths short-circuit without a read.
		{"absolute mnt/c path", "/mnt/c/Program Files/Microsoft VS Code/bin/code", nil, true},
		{"mnt/c root", "/mnt/c", nil, true},
		{
			"shim that execs mnt/c", "/usr/local/bin/code",
			map[string][]byte{"/usr/local/bin/code": []byte(`#!/bin/bash
# Microsoft VS Code WSL launcher
ELECTRON="/mnt/c/Users/Alice/AppData/Local/Programs/Microsoft VS Code/Code.exe"
exec "$ELECTRON" "$@"
`)},
			true,
		},
		{
			"linux-only shim rejected", "/usr/bin/code-oss",
			map[string][]byte{"/usr/bin/code-oss": []byte(`#!/bin/sh
exec /usr/share/code-oss/code-oss "$@"
`)},
			false,
		},
		{"missing binary collapses to not-windows", "/no/such/file", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDetectEnv(&fakeFS{files: tc.files}, pathTable{}, nil)
			if got := pathTargetsWindows(tc.path, env); got != tc.want {
				t.Fatalf("pathTargetsWindows(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestFindWindowsInstall_SkipsUnknownEditors(t *testing.T) {
	fs := &fakeFS{}
	env := newDetectEnv(fs, pathTable{}, nil)
	if got := findWindowsInstall("nonexistent-editor", env); got != "" {
		t.Fatalf("unknown editor must return empty; got %q", got)
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

// TestValidateWindowsCodeShim pins the shim check detection runs on
// every WSL candidate. A half-uninstalled VS Code keeps bin/code but
// loses the cli.js its VERSIONFOLDER references; without rejection,
// detection would hand back that candidate and the silently-failing
// spawn would leave click-to-open doing nothing. A shim without the
// VERSIONFOLDER pattern (Sublime, Zed, a custom $EDITOR target) has no
// cheap validation, so it passes and the spawn step decides. An
// unreadable shim can't be inspected, so it is rejected and detection
// falls through to the next candidate.
func TestValidateWindowsCodeShim(t *testing.T) {
	const (
		systemShim = "/mnt/c/Program Files/Microsoft VS Code/bin/code"
		userShim   = "/mnt/c/Users/alice/AppData/Local/Programs/Microsoft VS Code/bin/code"
	)
	cases := []struct {
		name  string
		path  string
		files map[string][]byte
		want  bool
	}{
		{
			"broken install rejected", systemShim,
			// cli.js intentionally absent.
			map[string][]byte{systemShim: []byte(`#!/usr/bin/env sh
COMMIT="072586267e68ece9a47aa43f8c108e0dcbf44622"
APP_NAME="code"
QUALITY="stable"
NAME="Code"
SERVERDATAFOLDER=".vscode-server"
VERSIONFOLDER="072586267e"
VSCODE_PATH="$(dirname "$(dirname "$(realpath "$0")")")"
ELECTRON="$VSCODE_PATH/$NAME.exe"
`)},
			false,
		},
		{
			"working install accepted", userShim,
			map[string][]byte{
				userShim: []byte(`#!/usr/bin/env sh
VERSIONFOLDER="034f571df5"
ELECTRON="something"
`),
				"/mnt/c/Users/alice/AppData/Local/Programs/Microsoft VS Code/034f571df5/resources/app/out/cli.js": []byte("// cli.js"),
			},
			true,
		},
		{
			"unrecognized format passes through", "/usr/local/bin/subl",
			map[string][]byte{"/usr/local/bin/subl": []byte(`#!/bin/sh
exec /opt/sublime-text/sublime_text "$@"
`)},
			true,
		},
		{"read error rejects", "/no/such/path", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDetectEnv(&fakeFS{files: tc.files}, pathTable{}, nil)
			if got := validateWindowsCodeShim(tc.path, env); got != tc.want {
				t.Fatalf("validateWindowsCodeShim(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
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
			"/mnt/c/Users/alice/AppData/Local/Programs/Microsoft VS Code/bin/code": brokenShim,
			// alice's cli.js intentionally absent
			"/mnt/c/Users/bob/AppData/Local/Programs/Microsoft VS Code/bin/code":                            workingShim,
			"/mnt/c/Users/bob/AppData/Local/Programs/Microsoft VS Code/034f571df5/resources/app/out/cli.js": []byte("// cli.js"),
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
			"/mnt/c/Users/alice/AppData/Local/Programs/Microsoft VS Code/bin/code":       brokenShim,
			"/mnt/c/Program Files/Microsoft VS Code/bin/code":                            workingShim,
			"/mnt/c/Program Files/Microsoft VS Code/034f571df5/resources/app/out/cli.js": []byte("// cli.js"),
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
