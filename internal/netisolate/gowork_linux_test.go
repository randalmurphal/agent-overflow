//go:build linux

package netisolate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/ownedtemp"
)

// goWorkTest points the host temp directory and the developer's Go
// environment file at fresh directories, with userEnv as that file's
// content (none when nil).
func goWorkTest(t *testing.T, userEnv []byte) (parent string) {
	t.Helper()
	parent = t.TempDir()
	t.Setenv("TMPDIR", parent)
	t.Setenv("GOTMPDIR", "")
	userFile := filepath.Join(t.TempDir(), "env")
	if userEnv != nil {
		if err := os.WriteFile(userFile, userEnv, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GOENV", userFile)
	return parent
}

func TestGoWorkDirIsPerRunAndRemovedOnRelease(t *testing.T) {
	goWorkTest(t, []byte("GOPROXY=off\n"))
	parent := diskParent()
	dir, goTmpDir, err := newGoWorkDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir.Dir) != parent || !strings.HasPrefix(filepath.Base(dir.Dir), goWorkPrefix) || !goTmpDir {
		t.Fatalf("dir = %q, GOTMPDIR %v; want %s* under %s, as GOTMPDIR", dir.Dir, goTmpDir, goWorkPrefix, parent)
	}
	if info, err := os.Stat(filepath.Join(dir.Dir, diskDirName)); err != nil || !info.IsDir() {
		t.Fatalf("the run's disk directory in %s: %v", dir.Dir, err)
	}
	// A concurrent run's sweep leaves it alone while it is in use.
	if err := ownedtemp.Sweep(parent, goWorkPrefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir.Dir); err != nil {
		t.Fatalf("a sweep removed a directory in use: %v", err)
	}
	second, _, err := newGoWorkDir()
	if err != nil {
		t.Fatal(err)
	}
	if second.Dir == dir.Dir {
		t.Fatal("two runs share one Go work directory")
	}
	if err := dir.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("released directory still there: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestGoWorkDirKeepsAGivenGOTMPDIR: a GOTMPDIR the developer set, in the
// environment or the Go environment file, stays; the run still gets its
// directory on disk, for DiskDir.
func TestGoWorkDirKeepsAGivenGOTMPDIR(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     string
		userEnv []byte
		want    bool
	}{
		{name: "environment", env: "/somewhere/chosen"},
		{name: "Go environment file", userEnv: []byte("GOFLAGS=-mod=mod\nGOTMPDIR=/somewhere/chosen\n")},
		// The go command ignores a line that does not start with the key,
		// so the run's directory is its GOTMPDIR.
		{name: "line the go command ignores", userEnv: []byte(" GOTMPDIR=/somewhere/chosen\nGOTMPDIR =/somewhere/chosen\n"), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			goWorkTest(t, tc.userEnv)
			if tc.env != "" {
				t.Setenv("GOTMPDIR", tc.env)
			}
			dir, goTmpDir, err := newGoWorkDir()
			if err != nil || dir == nil || goTmpDir != tc.want {
				t.Fatalf("newGoWorkDir = %v, %v, %v; want a directory, as GOTMPDIR %v", dir, goTmpDir, err, tc.want)
			}
			if err := dir.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestHostTempDirLooksPastANamespacesOwn: a namespace started from inside
// another puts its Go work directory where the private temp directory it
// mounts cannot hide it.
func TestHostTempDirLooksPastANamespacesOwn(t *testing.T) {
	t.Setenv("TMPDIR", "/var/tmp/x")
	if got := hostTempDir(); got != "/var/tmp/x" {
		t.Fatalf("hostTempDir = %q outside a namespace", got)
	}
	t.Setenv("TMPDIR", TempDir())
	t.Setenv(EnvHostTempDir, "/var/tmp/host")
	if got := hostTempDir(); got != "/var/tmp/host" {
		t.Fatalf("hostTempDir = %q inside a namespace, want the recorded host directory", got)
	}
	t.Setenv(EnvHostTempDir, "")
	if got := hostTempDir(); got != "/tmp" {
		t.Fatalf("hostTempDir = %q with nothing recorded, want /tmp", got)
	}
}

// TestDiskParentIsNeverATmpfs: a host whose temp directory is a tmpfs gets
// its run directories in /var/tmp, out of memory.
func TestDiskParentIsNeverATmpfs(t *testing.T) {
	disk := t.TempDir()
	if isTmpfs(t, disk) {
		// Under make go-test t.TempDir is the namespace's tmpfs; the host's
		// temp directory is recorded beside it.
		disk = hostTempDir()
	}
	if isTmpfs(t, disk) {
		t.Skip("no temp directory on disk to compare with")
	}
	t.Setenv("TMPDIR", disk)
	if got := diskParent(); got != disk {
		t.Fatalf("diskParent = %q with TMPDIR on disk at %s", got, disk)
	}
	if _, err := os.Stat("/dev/shm"); err != nil || !isTmpfs(t, "/dev/shm") {
		t.Skip("no tmpfs at /dev/shm")
	}
	t.Setenv("TMPDIR", "/dev/shm")
	if got := diskParent(); got != "/var/tmp" {
		t.Fatalf("diskParent = %q with TMPDIR on a tmpfs, want /var/tmp", got)
	}
}
