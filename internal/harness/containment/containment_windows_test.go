//go:build windows

package containment

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsConfigurePassesAnInheritedJobHandle(t *testing.T) {
	group, err := Prepare(64 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	job := group.(*windowsGroup)
	cmd := exec.Command(`C:\backend.exe`)
	if err := group.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_SUSPENDED == 0 {
		t.Fatal("Configure did not suspend the child before job adoption")
	}
	if len(cmd.SysProcAttr.AdditionalInheritedHandles) != 1 || cmd.SysProcAttr.AdditionalInheritedHandles[0] != syscall.Handle(job.handle) {
		t.Fatalf("inherited handles = %v, want the job handle", cmd.SysProcAttr.AdditionalInheritedHandles)
	}
}

func TestWindowsAdoptFailureTerminatesTheSuspendedProcess(t *testing.T) {
	previous := assignProcessToJob
	assignProcessToJob = func(windows.Handle, windows.Handle) error { return errors.New("injected assign failure") }
	t.Cleanup(func() { assignProcessToJob = previous })
	group, err := Prepare(64 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), "/c", "exit", "0")
	if err := group.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	adoptErr := group.Adopt(cmd)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	if adoptErr == nil || !strings.Contains(adoptErr.Error(), "injected assign failure") {
		t.Errorf("Adopt error = %v, want the assignment failure", adoptErr)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("the unadopted process stayed suspended and blocked Wait")
	}
}
