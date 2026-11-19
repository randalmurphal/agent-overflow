//go:build unix

package agenthome

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO in the home is refused without waiting for a writer, by every
// reader.
func TestAFIFOInTheHomeIsRefusedWithoutBlocking(t *testing.T) {
	t.Parallel()
	home := writeHome(t, nil)
	for _, name := range []string{InstructionsFile, "verification.md"} {
		if err := syscall.Mkfifo(filepath.Join(home, name), 0o600); err != nil {
			t.Skip("no FIFOs here:", err)
		}
	}
	within(t, "RoleLine", func() {
		if _, err := RoleLine(home); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("RoleLine error = %v", err)
		}
	})
	within(t, "ReadAttached", func() {
		if _, err := ReadAttached(home, []string{"verification.md"}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("ReadAttached error = %v", err)
		}
	})
}

// The open itself does not wait on a FIFO, which is what lets OpenRegular
// check the mode on the opened file.
func TestOpenRegularDoesNotBlockOnAFIFO(t *testing.T) {
	t.Parallel()
	home := writeHome(t, nil)
	if err := syscall.Mkfifo(filepath.Join(home, "pipe"), 0o600); err != nil {
		t.Skip("no FIFOs here:", err)
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	within(t, "the open", func() {
		file, err := root.OpenFile("pipe", os.O_RDONLY|openNonblock, 0)
		if err != nil {
			t.Errorf("open: %v", err)
			return
		}
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
}

func within(t *testing.T, name string, call func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		call()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked on a FIFO", name)
	}
}
