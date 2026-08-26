package instanceinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDataRootForMatchesTheMakefileRule(t *testing.T) {
	// The Makefile computes HARNESS_DATA_DIR as
	// $(TMPDIR)/agent-overflow-harness$(subst /,-,$(CURDIR)); the backend's
	// flag default and ao-harness must resolve the same instance.
	got := DataRootFor("/home/dev/repos/agent-overflow")
	want := filepath.Join(os.TempDir(), "agent-overflow-harness-home-dev-repos-agent-overflow")
	if got != want {
		t.Fatalf("DataRootFor = %q, want %q", got, want)
	}
	if a, b := DataRootFor("/a/b"), DataRootFor("/a/c"); a == b {
		t.Fatalf("two checkouts collapsed onto one data root: %q", a)
	}
	// Trailing separators are a spelling, not a different checkout.
	if a, b := DataRootFor("/a/b"), DataRootFor("/a/b/"); a != b {
		t.Fatalf("trailing separator changed the root: %q vs %q", a, b)
	}
}

func TestDataRootForFlattensWindowsSpellings(t *testing.T) {
	got := DataRootFor(`C:\repos\agent-overflow`)
	if strings.ContainsAny(strings.TrimPrefix(got, os.TempDir()), `:\`) {
		t.Fatalf("DataRootFor left a separator or drive colon in the component: %q", got)
	}
}

func TestSoakRootIsDistinctFromTheHarnessRoot(t *testing.T) {
	// The soak autopilot refuses a data dir holding threads it did not
	// seed, so a checkout's soak and harness instances cannot share one.
	harness := DataRootFor("/a/b")
	soak := SoakDataRootFor("/a/b")
	if harness == soak {
		t.Fatal("soak root equals the harness root")
	}
	if soak != harness+SoakSuffix {
		t.Fatalf("SoakDataRootFor = %q, want %q", soak, harness+SoakSuffix)
	}
	if DefaultSoakDataRoot() != DefaultDataRoot()+SoakSuffix {
		t.Fatalf("DefaultSoakDataRoot = %q, want %q", DefaultSoakDataRoot(), DefaultDataRoot()+SoakSuffix)
	}
}

func TestDefaultDataRootFollowsTheWorkingDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if got, want := DefaultDataRoot(), DataRootFor(cwd); got != want {
		t.Fatalf("DefaultDataRoot = %q, want %q", got, want)
	}
}
