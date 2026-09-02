package devscan

import "testing"

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
