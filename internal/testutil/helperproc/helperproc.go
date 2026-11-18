// Package helperproc ties a helper process a test starts to the test binary
// that started it. A helper that stands in for a backend runs until its test
// kills it; a test binary that is itself killed, or panics at its timeout,
// never does, and the helper and everything it started would run on. A
// killed helper removes nothing it created either, so its temporary files go
// in a directory of the test that started it.
//
// Standard library only, so any test package can use it.
package helperproc

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// Tie gives command a stdin that stays open for as long as this process
// runs, and a temporary directory of t's for os.TempDir and for its own
// tests' t.TempDir. Call it before command starts and after setting
// command.Env; the helper ends with Hold.
func Tie(t testing.TB, command *exec.Cmd) {
	t.Helper()
	// The write end is never written to and never closed here: the kernel
	// closes it when this process exits, however it exits.
	if _, err := command.StdinPipe(); err != nil {
		t.Fatalf("tie the helper process to this one: %v", err)
	}
	if command.Env == nil {
		command.Env = os.Environ()
	}
	dir := t.TempDir()
	// os.TempDir reads TMPDIR, or TMP then TEMP on Windows; t.TempDir reads
	// GOTMPDIR first.
	names := []string{"TMPDIR", "GOTMPDIR"}
	if runtime.GOOS == "windows" {
		names = []string{"TMP", "TEMP", "GOTMPDIR"}
	}
	for _, name := range names {
		command.Env = append(command.Env, name+"="+dir)
	}
}

// Hold blocks the helper until the process that started it (Tie) is gone,
// then exits the helper. It returns only by exiting.
func Hold() {
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
