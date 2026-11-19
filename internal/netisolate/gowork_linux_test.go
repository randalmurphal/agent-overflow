//go:build linux

package netisolate

import (
	"bytes"
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
	parent := goWorkTest(t, []byte("GOPROXY=off\n"))
	var stderr bytes.Buffer
	dir, release, err := newGoWorkDir(&stderr)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != parent || !strings.HasPrefix(filepath.Base(dir), goWorkPrefix) {
		t.Fatalf("dir = %q, want %s* under %s", dir, goWorkPrefix, parent)
	}
	// A concurrent run's sweep leaves it alone while it is in use.
	if err := ownedtemp.Sweep(parent, goWorkPrefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a sweep removed a directory in use: %v", err)
	}
	second, releaseSecond, err := newGoWorkDir(&stderr)
	if err != nil {
		t.Fatal(err)
	}
	if second == dir {
		t.Fatal("two runs share one Go work directory")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("released directory still there: %v", err)
	}
	if err := releaseSecond(); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() > 0 {
		t.Fatalf("stderr: %s", stderr.String())
	}
}

// TestGoWorkDirKeepsAGivenGOTMPDIR: a GOTMPDIR the developer set, in the
// environment or the Go environment file, stays.
func TestGoWorkDirKeepsAGivenGOTMPDIR(t *testing.T) {
	t.Run("environment", func(t *testing.T) {
		goWorkTest(t, nil)
		t.Setenv("GOTMPDIR", "/somewhere/chosen")
		dir, _, err := newGoWorkDir(&bytes.Buffer{})
		if err != nil || dir != "" {
			t.Fatalf("newGoWorkDir = %q, %v; want none while GOTMPDIR is set", dir, err)
		}
	})
	t.Run("Go environment file", func(t *testing.T) {
		parent := goWorkTest(t, []byte("GOFLAGS=-mod=mod\n GOTMPDIR = /somewhere/chosen\n"))
		dir, _, err := newGoWorkDir(&bytes.Buffer{})
		if err != nil || dir != "" {
			t.Fatalf("newGoWorkDir = %q, %v; want none while the Go environment file sets GOTMPDIR", dir, err)
		}
		if matches, _ := filepath.Glob(filepath.Join(parent, goWorkPrefix+"*")); len(matches) > 0 {
			t.Fatalf("created %v anyway", matches)
		}
	})
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
