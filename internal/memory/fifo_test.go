//go:build unix

package memory

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO where a fact, instructions.md or a scope folder belongs is passed
// over or refused without waiting for a writer.
func TestAFIFOIsRefusedWithoutBlocking(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	if err := os.RemoveAll(HomeDir(s.plan.Home)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(s.plan.User); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(s.project, "pipe.md"),
		filepath.Join(s.plan.Home, "instructions.md"),
		HomeDir(s.plan.Home),
		s.plan.User,
	} {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skip("no FIFOs here:", err)
		}
	}
	done := make(chan struct{})
	var outputs []Output
	var problems []error
	go func() {
		defer close(done)
		outputs, problems = Build(s.plan, day)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Build blocked on a FIFO")
	}
	if len(problems) != 3 {
		t.Fatalf("problems: %v", problems)
	}
	text := joined(outputs)
	// A FIFO in a folder is not a fact.
	if strings.Contains(text, "pipe.md") {
		t.Errorf("listed a FIFO:\n%s", text)
	}
	for _, want := range []string{
		"[instructions.md could not be read: not a regular file. Read it yourself.]",
		"could not be read: open " + s.plan.User + ": not a folder.]",
		"could not be read: open memory: not a folder.]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in:\n%s", want, text)
		}
	}
}

// A .project-path that is a FIFO, or that cannot be opened, is a marker
// that could not be read.
func TestAProjectPathMarkerThatCannotBeRead(t *testing.T) {
	t.Parallel()
	t.Run("fifo", func(t *testing.T) {
		t.Parallel()
		s := newScopes(t)
		marker := filepath.Join(s.project, ProjectPathFile)
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(marker, 0o600); err != nil {
			t.Skip("no FIFOs here:", err)
		}
		requireProjectOff(t, s)
	})
	t.Run("unreadable", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root reads any file")
		}
		s := newScopes(t)
		if err := os.Chmod(filepath.Join(s.project, ProjectPathFile), 0); err != nil {
			t.Fatal(err)
		}
		requireProjectOff(t, s)
	})
}
