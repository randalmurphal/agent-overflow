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
	home := writeHome(t, map[string]string{MemoryIndexFile: "index"})
	for _, name := range []string{InstructionsFile, "verification.md", memoryDir} {
		if err := syscall.Mkfifo(filepath.Join(home, name), 0o600); err != nil {
			t.Skip("no FIFOs here:", err)
		}
	}
	within(t, "ReadContext", func() {
		text, problem := readContext(t, home, InstructionsFile)
		if problem == nil || !strings.Contains(problem.Error(), "not a regular file") || !strings.HasPrefix(text, instructionsHeading) {
			t.Errorf("ReadContext = %q, %v", text, problem)
		}
	})
	within(t, "the index check", func() {
		if notices, problem := indexCheck(t, home); problem == nil || !strings.HasPrefix(notices, "[Index check: memory/ could not be read: ") {
			t.Errorf("index check = %q, %v", notices, problem)
		}
	})
	within(t, "RunHook", func() {
		var stdout, stderr strings.Builder
		if code := RunHook([]string{InstructionsFile, home}, strings.NewReader("{}"), &stdout, &stderr); code != 0 ||
			!strings.Contains(stdout.String(), "not a regular file") || !strings.Contains(stderr.String(), "not a regular file") {
			t.Errorf("RunHook = %d, %q, %q", code, stdout.String(), stderr.String())
		}
	})
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

// The open itself does not wait on a FIFO, which is what lets openRegular
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
