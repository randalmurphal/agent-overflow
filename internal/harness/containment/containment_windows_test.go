//go:build windows

package containment

import (
	"os/exec"
	"syscall"
	"testing"

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
