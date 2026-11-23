package devscan

import (
	"runtime"
	"testing"
)

// Attribution is exercised on the shapes it exists for, with hand-built
// listeners and owners: no process is spawned and no /proc is read.

func TestAttributeWalksTheAncestorChain(t *testing.T) {
	owners := []Owner{{ThreadID: "thread-a", PID: 300, PGID: 300}}
	// vite(500) → npm(400) → the provider session(300).
	parents := map[int]int{500: 400, 400: 300, 300: 1}
	// PGID 0 so only the chain can answer.
	l := listener{Port: 5173, PID: 500, PGID: 0}

	threadID, ok := attribute(l, owners, parents)
	if !ok || threadID != "thread-a" {
		t.Fatalf("attribute = %q/%v, want thread-a/true", threadID, ok)
	}
}

// A dev server that daemonised has reparented to init, so the chain no
// longer reaches anything of ours. The group id it inherited does.
func TestAttributeFindsADaemonisedServerByItsGroup(t *testing.T) {
	owners := []Owner{{ThreadID: "thread-b", PID: 700, PGID: 700}}
	parents := map[int]int{900: 1}
	l := listener{Port: 3000, PID: 900, PPID: 1, PGID: 700}

	threadID, ok := attribute(l, owners, parents)
	if !ok || threadID != "thread-b" {
		t.Fatalf("attribute = %q/%v, want thread-b/true", threadID, ok)
	}
}

// The person's own dev server, started in their own shell before the app
// existed, belongs to no thread. Claiming it would put a link under a
// thread that never started it.
func TestAttributeClaimsNothingItDoesNotOwn(t *testing.T) {
	owners := []Owner{{ThreadID: "thread-c", PID: 300, PGID: 300}}
	parents := map[int]int{800: 1}
	l := listener{Port: 8080, PID: 800, PPID: 1, PGID: 800, Comm: "node"}

	if threadID, ok := attribute(l, owners, parents); ok {
		t.Fatalf("attribute claimed %q for a listener nothing of ours started", threadID)
	}
}

// A pid the fd walk could not read is 0, and 0 is not a process: it must
// not fall through to whatever the maps happen to hold.
func TestAttributeRefusesAnUnknownPID(t *testing.T) {
	owners := []Owner{{ThreadID: "thread-d", PID: 300, PGID: 300}}
	if _, ok := attribute(listener{Port: 4000}, owners, map[int]int{}); ok {
		t.Fatal("a listener with no readable pid was attributed")
	}
	if _, ok := attribute(listener{Port: 4000, PID: 500}, nil, map[int]int{500: 300}); ok {
		t.Fatal("a listener was attributed with no owners at all")
	}
}

// A parent map with a cycle in it is not a shape /proc produces, but the
// walk must terminate on one anyway: this is the bound, not a guess about
// how deep a real tree gets.
func TestAttributeTerminatesOnACycle(t *testing.T) {
	owners := []Owner{{ThreadID: "thread-e", PID: 300, PGID: 300}}
	parents := map[int]int{500: 600, 600: 500}
	if _, ok := attribute(listener{Port: 5173, PID: 500}, owners, parents); ok {
		t.Fatal("a cycle produced an attribution")
	}
}

// Multiple threads sharing one multiplexed provider process (same PID/PGID)
// break ties by the deepest matching WorkDir prefix of the listener's CWD.
func TestAttributeBreaksSharedProcessTiesByWorkDir(t *testing.T) {
	owners := []Owner{
		{ThreadID: "thread-root", PID: 300, PGID: 300, WorkDir: "/work/repo"},
		{ThreadID: "thread-nested", PID: 300, PGID: 300, WorkDir: "/work/repo/sub"},
		{ThreadID: "thread-other", PID: 300, PGID: 300, WorkDir: "/work/other"},
	}
	parents := map[int]int{500: 400, 400: 300, 300: 1}

	got, ok := attribute(listener{Port: 5173, PID: 500, PGID: 300, CWD: "/work/repo/sub/frontend"}, owners, parents)
	if !ok || got != "thread-nested" {
		t.Fatalf("nested CWD attribute = %q/%v, want thread-nested/true", got, ok)
	}

	got, ok = attribute(listener{Port: 3000, PID: 500, PGID: 300, CWD: "/work/other"}, owners, parents)
	if !ok || got != "thread-other" {
		t.Fatalf("other CWD attribute = %q/%v, want thread-other/true", got, ok)
	}

	// macOS /private/tmp prefix normalizes to match /tmp WorkDir.
	if runtime.GOOS == "darwin" {
		tmpOwners := []Owner{
			{ThreadID: "thread-a", PID: 300, PGID: 300, WorkDir: "/tmp/ws-a"},
			{ThreadID: "thread-b", PID: 300, PGID: 300, WorkDir: "/tmp/ws-b"},
		}
		got, ok = attribute(listener{Port: 5174, PID: 500, PGID: 300, CWD: "/private/tmp/ws-b/app"}, tmpOwners, parents)
		if !ok || got != "thread-b" {
			t.Fatalf("macOS /private/tmp CWD attribute = %q/%v, want thread-b/true", got, ok)
		}
	}

	// Without a listener CWD or when two distinct threads share the exact same
	// WorkDir, attribution declines instead of picking a random thread.
	if got, ok := attribute(listener{Port: 5175, PID: 500, PGID: 300}, owners, parents); ok {
		t.Fatalf("expected ambiguous shared-process attribution without CWD to decline, got %q", got)
	}
	sameDir := []Owner{
		{ThreadID: "thread-1", PID: 300, PGID: 300, WorkDir: "/work/repo"},
		{ThreadID: "thread-2", PID: 300, PGID: 300, WorkDir: "/work/repo"},
	}
	if got, ok := attribute(listener{Port: 5176, PID: 500, PGID: 300, CWD: "/work/repo"}, sameDir, parents); ok {
		t.Fatalf("expected tied WorkDir attribution to decline, got %q", got)
	}
}

// When two threads share a PGID (e.g. sibling terminals under a shared shell
// group) but have distinct PIDs, an unresolved PGID tie-break must fall through
// to the PID ancestor walk rather than failing outright.
func TestAttributeFallsThroughFromAmbiguousPGIDToPIDWalk(t *testing.T) {
	owners := []Owner{
		{ThreadID: "thread-a", PID: 301, PGID: 300, WorkDir: "/work/repo"},
		{ThreadID: "thread-b", PID: 302, PGID: 300, WorkDir: "/work/repo"},
	}
	parents := map[int]int{500: 400, 400: 302, 302: 300}

	got, ok := attribute(listener{Port: 5177, PID: 500, PGID: 300}, owners, parents)
	if !ok || got != "thread-b" {
		t.Fatalf("attribute = %q/%v, want thread-b/true", got, ok)
	}
}

func TestNormalizePathForMatchPreservesPrefixBoundaries(t *testing.T) {
	if got := normalizePathForOS("/private/tmp/work", "darwin"); got != "/tmp/work" {
		t.Fatalf("normalizePathForOS(/private/tmp/work, darwin) = %q, want /tmp/work", got)
	}
	if got := normalizePathForOS("/private/tmp", "darwin"); got != "/tmp" {
		t.Fatalf("normalizePathForOS(/private/tmp, darwin) = %q, want /tmp", got)
	}
	if got := normalizePathForOS("/private/tmp/work", "linux"); got != "/private/tmp/work" {
		t.Fatalf("normalizePathForOS(/private/tmp/work, linux) = %q, want unchanged", got)
	}
	if got := normalizePathForOS("/private/tmpfoo/bar", "darwin"); got != "/private/tmpfoo/bar" {
		t.Fatalf("normalizePathForOS(/private/tmpfoo/bar, darwin) = %q, want unchanged", got)
	}
	if got := normalizePathForOS("/private/varfoo/bar", "darwin"); got != "/private/varfoo/bar" {
		t.Fatalf("normalizePathForOS(/private/varfoo/bar, darwin) = %q, want unchanged", got)
	}
}
