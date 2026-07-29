package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnsureCLISymlinkCreatesTheCanonicalName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows launcher spawns no provider children and publishes no command")
	}
	configDir := t.TempDir()
	executable := filepath.Join(t.TempDir(), "Agent Overflow")
	writeStubBinary(t, executable)

	binDir, err := ensureCLISymlink(configDir, executable)
	if err != nil {
		t.Fatalf("ensureCLISymlink: %v", err)
	}
	if binDir != filepath.Join(configDir, cliBinDirName) {
		t.Fatalf("bin dir = %q", binDir)
	}
	// The published name is the canonical one no matter what the running
	// executable is called — that is the whole reason the link exists.
	link := filepath.Join(binDir, cliCommandName)
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink %s: %v", link, err)
	}
	if target != executable {
		t.Fatalf("link target = %q, want %q", target, executable)
	}
	// Nothing but the link lives in the directory: prepending it to a session's
	// PATH must expose this command and no other.
	entries, err := os.ReadDir(binDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != cliCommandName {
		t.Fatalf("bin dir holds %d entries: %v", len(entries), entries)
	}
}

func TestEnsureCLISymlinkRepointsAStaleLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows launcher spawns no provider children and publishes no command")
	}
	configDir := t.TempDir()
	previous := filepath.Join(t.TempDir(), "old-build")
	current := filepath.Join(t.TempDir(), "new-build")
	writeStubBinary(t, previous)
	writeStubBinary(t, current)

	if _, err := ensureCLISymlink(configDir, previous); err != nil {
		t.Fatalf("first ensureCLISymlink: %v", err)
	}
	binDir, err := ensureCLISymlink(configDir, current)
	if err != nil {
		t.Fatalf("second ensureCLISymlink: %v", err)
	}
	link := filepath.Join(binDir, cliCommandName)
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink %s: %v", link, err)
	}
	if target != current {
		t.Fatalf("a stale link survived: target = %q, want %q", target, current)
	}
	// The atomic replace leaves no staging entry behind for the next boot to
	// trip over.
	entries, err := os.ReadDir(binDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("bin dir holds %d entries after a repoint: %v", len(entries), entries)
	}

	// Re-publishing an already-correct link is a no-op rather than a churn of
	// unlink/relink under a session that may be resolving the name right now.
	before, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ensureCLISymlink(configDir, current); err != nil {
		t.Fatalf("third ensureCLISymlink: %v", err)
	}
	after, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("an unchanged target replaced the link anyway")
	}
}

// A path already occupied by a regular file (a stray copy, a botched install)
// is replaced, not reported: the name has to resolve to this executable.
func TestEnsureCLISymlinkReplacesAPlainFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows launcher spawns no provider children and publishes no command")
	}
	configDir := t.TempDir()
	binDir := filepath.Join(configDir, cliBinDirName)
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, cliCommandName), []byte("stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "agent-overflow")
	writeStubBinary(t, executable)

	if _, err := ensureCLISymlink(configDir, executable); err != nil {
		t.Fatalf("ensureCLISymlink: %v", err)
	}
	target, err := os.Readlink(filepath.Join(binDir, cliCommandName))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != executable {
		t.Fatalf("link target = %q, want %q", target, executable)
	}
}

func TestEnsureCLISymlinkRejectsUnusableInputs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows launcher spawns no provider children and publishes no command")
	}
	if _, err := ensureCLISymlink("", "/usr/bin/agent-overflow"); err == nil {
		t.Fatal("an empty config dir was accepted")
	}
	if _, err := ensureCLISymlink(t.TempDir(), "  "); err == nil {
		t.Fatal("an empty executable path was accepted")
	}
}

// A boot that cannot publish the command keeps booting: the App records an
// empty bin dir, logs why, and the composer block reports it. Anything else
// would trade a degraded feature for no product.
func TestEnsureCLIBinDirToleratesAnUnwritableConfigDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows launcher spawns no provider children and publishes no command")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the failure cannot be provoked")
	}
	configDir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(configDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(configDir, 0o700); err != nil {
			t.Fatalf("restore %s: %v", configDir, err)
		}
	})

	app := &App{}
	if binDir := app.ensureCLIBinDir(configDir); binDir != "" {
		t.Fatalf("a failed publish returned %q, want the empty no-command answer", binDir)
	}
	// The failure is not silent: the message names the operation and the path.
	if _, err := ensureCLISymlink(configDir, filepath.Join(t.TempDir(), "agent-overflow")); err == nil {
		t.Fatal("an unwritable config dir was accepted")
	} else if !strings.Contains(err.Error(), "publish CLI command") {
		t.Fatalf("error does not name the operation: %v", err)
	}
}

func TestPrependCLIBinDir(t *testing.T) {
	separator := string(os.PathListSeparator)
	tests := []struct {
		name string
		env  map[string]string
		want map[string]string
	}{
		{
			name: "an env with no PATH gets one",
			env:  map[string]string{"HOME": "/home/agent"},
			want: map[string]string{"HOME": "/home/agent", "PATH": "/cfg/bin"},
		},
		{
			name: "an existing PATH keeps its entries behind ours",
			env:  map[string]string{"PATH": "/usr/bin" + separator + "/bin"},
			want: map[string]string{"PATH": "/cfg/bin" + separator + "/usr/bin" + separator + "/bin"},
		},
		{
			// provider.BuildEnvironment matches PATH case-insensitively; if this
			// helper did not, a `Path` override would survive and our entry would
			// land on a variable nothing reads.
			name: "a differently cased PATH is the one extended",
			env:  map[string]string{"Path": "/usr/bin"},
			want: map[string]string{"Path": "/cfg/bin" + separator + "/usr/bin"},
		},
		{
			name: "an empty PATH is replaced rather than left with a leading separator",
			env:  map[string]string{"PATH": ""},
			want: map[string]string{"PATH": "/cfg/bin"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prependCLIBinDir(test.env, "/cfg/bin")
			if len(test.env) != len(test.want) {
				t.Fatalf("env = %#v, want %#v", test.env, test.want)
			}
			for key, want := range test.want {
				if test.env[key] != want {
					t.Fatalf("env[%q] = %q, want %q", key, test.env[key], want)
				}
			}
		})
	}
}

func writeStubBinary(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
