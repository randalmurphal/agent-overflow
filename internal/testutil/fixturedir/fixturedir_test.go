package fixturedir

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/helperproc"
)

// roleEnv makes TestFixtureRole a process of the tests below, since a
// process runs Guard once: "build" builds a fixture under Guard after
// pointing TMPDIR at a directory of its own, and "twice" runs Guard after
// Guard.
const roleEnv = "FIXTUREDIR_TEST_ROLE"

// TestFixtureRole is not a test of its own.
func TestFixtureRole(t *testing.T) {
	switch os.Getenv(roleEnv) {
	case "build":
		builds := 0
		fixture := New("built", func(tb testing.TB, path string) {
			builds++
			if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
				tb.Fatal(err)
			}
		})
		var path string
		code := Guard(func() int {
			own := filepath.Join(t.TempDir(), "own")
			if err := os.Mkdir(own, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", own)
			path = fixture.Path(t)
			if again := fixture.Path(t); again != path {
				t.Errorf("second Path %q; want %q", again, path)
			}
			return 0
		})
		fmt.Printf("guard %d builds %d path %s\n", code, builds, path)
	case "twice":
		first := Guard(func() int { return 0 })
		ran := false
		second := Guard(func() int { ran = true; return 0 })
		fmt.Printf("first %d second %d ran %v\n", first, second, ran)
	default:
		t.Skip("runs only as a process of this file's tests")
	}
}

// fixtureRole runs TestFixtureRole as role in a process of its own, with
// TMPDIR at tmp, and returns what it printed.
func fixtureRole(t *testing.T, role, tmp string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFixtureRole$")
	cmd.Env = append(os.Environ(), roleEnv+"="+role)
	helperproc.Tie(t, cmd)
	cmd.Env = append(cmd.Env, "TMPDIR="+tmp)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the %s process: %v\n%s", role, err, out)
	}
	return string(out)
}

// A fixture built first in a test that points TMPDIR at a directory of its
// own, as a test whose cleanup removes that directory does, is built once,
// in the directory the run made under the TMPDIR it started with, and is
// gone once the run ends.
func TestAFixtureIsBuiltOnceWhereTheRunStarted(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	out := fixtureRole(t, "build", base)
	var code, builds int
	var path string
	if _, err := fmt.Sscanf(out, "guard %d builds %d path %s", &code, &builds, &path); err != nil || code != 0 || builds != 1 {
		t.Fatalf("the run printed %q (%v); want Guard 0 and one build", out, err)
	}
	if got := filepath.Dir(filepath.Dir(path)); got != base {
		t.Fatalf("the fixture was built at %s, under %s; want it under %s, the TMPDIR the run started with", path, got, base)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the fixture directory after the run: %v; want it removed", err)
	}
}

// A second Guard in one process would hand out fixture paths in the
// directory the first removed; it fails the run, saying why, and runs
// nothing.
func TestASecondGuardInOneProcessFailsTheRun(t *testing.T) {
	t.Parallel()
	out := fixtureRole(t, "twice", t.TempDir())
	if !strings.Contains(out, "first 0 second 1 ran false") || !strings.Contains(out, "Guard ran a second time in this process") {
		t.Fatalf("Guard after Guard printed %q; want the second to fail the run, say why and run nothing", out)
	}
}
