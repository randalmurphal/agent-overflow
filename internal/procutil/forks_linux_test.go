package procutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// A child that has an executable's write descriptor, as one forked while
// the file is open for writing has until its own exec, makes running the
// file fail with "text file busy". InstallExecutable's hold keeps a child
// from being forked in that window. The write here is deliberately raw, and
// TestEveryTestExecutableIsWrittenWithForksHeldOff requires that it find it,
// and only it.
func TestAChildHoldingTheWriteDescriptorMakesTheExecutableBusy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tool")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("#!/bin/sh\nexit 0\n"); err != nil {
		t.Fatal(errors.Join(err, file.Close()))
	}
	holder := exec.Command("sleep", "60")
	holder.ExtraFiles = []*os.File{file}
	startErr := holder.Start()
	if err := errors.Join(startErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	stopHolder := func() {
		if err := holder.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill holder: %v", err)
		}
		_ = holder.Wait() // killed: the exit status is the signal
	}
	if err := exec.Command(path).Run(); !errors.Is(err, syscall.ETXTBSY) {
		stopHolder()
		t.Fatalf("run while a child holds the write descriptor: %v, want %v", err, syscall.ETXTBSY)
	}
	stopHolder()
	if err := exec.Command(path).Run(); err != nil {
		t.Fatalf("run once no process holds the write descriptor: %v", err)
	}
}
