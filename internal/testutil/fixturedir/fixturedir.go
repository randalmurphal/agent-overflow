// Package fixturedir owns the one directory in which a test process keeps
// the fixtures it builds once and shares across its tests: a migrated store
// template, a repository snapshot, a seeded database. The directory is made
// before any test runs (Guard), so its place cannot follow a test that
// points TMPDIR at its own t.TempDir, whose cleanup would remove what later
// tests read. It is removed when the run ends, and a later run removes one
// a killed process left (ownedtemp). A fixture is built in it on first use
// (Lazy), so a run that uses none, such as `go test -list`, builds nothing.
//
// It imports only ownedtemp, so package store's own tests can use it.
package fixturedir

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"agent-overflow/internal/ownedtemp"
)

// prefix names the directories, one per test process.
const prefix = "agent-overflow-test-fixtures-"

var current struct {
	sync.Mutex
	// dir is set while Guard's run runs.
	dir string
	// guarded is set by the process's one Guard.
	guarded bool
}

// Guard runs run, a TestMain's test run such as m.Run, with the process's
// fixture directory in place, and removes the directory after it, with
// those of test processes that exited without removing theirs. A directory
// it cannot make or remove fails the run. A process has one Guard: a
// fixture built under it keeps its path, so a second Guard, nested or
// after it, would hand out paths in a directory the first removed. A second
// one fails the run without running it.
func Guard(run func() int) int {
	current.Lock()
	again := current.guarded
	current.guarded = true
	current.Unlock()
	if again {
		fmt.Fprintln(os.Stderr, "fixturedir: Guard ran a second time in this process; a process has one fixture directory, so its TestMain runs one Guard")
		return 1
	}
	sweep()
	dir, release, err := ownedtemp.Make("", prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixturedir: make the fixture directory: %v\n", err)
		return 1
	}
	current.Lock()
	current.dir = dir
	current.Unlock()
	code := run()
	current.Lock()
	current.dir = ""
	current.Unlock()
	if err := release(); err != nil {
		fmt.Fprintf(os.Stderr, "fixturedir: remove the fixture directory: %v\n", err)
		code = max(code, 1)
	}
	sweep()
	return code
}

// sweep removes the directories of exited test processes. A failure leaves
// a directory, not a wrong result, so it is reported, not failed.
func sweep() {
	if err := ownedtemp.Sweep("", prefix); err != nil {
		fmt.Fprintf(os.Stderr, "fixturedir: remove the fixture directories of exited test processes: %v\n", err)
	}
}

// Dir is the process's fixture directory, for a fixture that makes several
// entries in it. It fails tb when no Guard runs.
func Dir(tb testing.TB) string {
	tb.Helper()
	current.Lock()
	defer current.Unlock()
	if current.dir == "" {
		tb.Fatal("fixturedir: no fixture directory; the package's TestMain must run its tests under fixturedir.Guard, as storetest.Run does")
	}
	return current.dir
}

// Lazy is a fixture a test process builds once, on first use, in its
// fixture directory.
type Lazy struct {
	name  string
	build func(tb testing.TB, path string)
	once  sync.Once
	// path is set once build has returned.
	path string
}

// New declares a fixture: build makes it at path, a file or directory
// named name in the fixture directory, and fails tb when it cannot.
func New(name string, build func(tb testing.TB, path string)) *Lazy {
	return &Lazy{name: name, build: build}
}

// Path builds the fixture on its first call and returns its path. A build
// that failed fails every later caller too, naming the fixture.
func (l *Lazy) Path(tb testing.TB) string {
	tb.Helper()
	dir := Dir(tb)
	l.once.Do(func() {
		path := filepath.Join(dir, l.name)
		l.build(tb, path)
		l.path = path
	})
	if l.path == "" {
		tb.Fatalf("fixturedir: the %s fixture's build failed in an earlier test", l.name)
	}
	return l.path
}
