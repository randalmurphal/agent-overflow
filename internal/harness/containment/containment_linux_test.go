//go:build linux

package containment

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnescapeMountField(t *testing.T) {
	if got := unescapeMountField(`/sys/fs/cgroup\040with\011tab`); got != "/sys/fs/cgroup with\ttab" {
		t.Fatalf("unescapeMountField() = %q", got)
	}
}

func TestPrepareWithFallbackInstallsInheritedDataLimit(t *testing.T) {
	group, mode, err := PrepareWithFallback(64 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	if mode == "cgroup-v2" {
		t.Skip("host delegated cgroup v2; fallback is covered only on non-delegated hosts")
	}
	cmd := exec.Command("/bin/sh", "-c", "ulimit -d")
	if err := group.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "65536" {
		t.Fatalf("inherited data limit = %q, want 65536 KiB", out)
	}
}

func TestFallbackAdoptReturnsAfterTheLauncherExecsTheCommand(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(sleep)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sleep, "30")
	group := &linuxFallbackGroup{limit: 64 << 20}
	if err := group.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if err := group.Adopt(cmd); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	// The caller records process identity next; it must name the command.
	got, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("executable after Adopt = %q, want %q", got, want)
	}
}

func TestFallbackAdoptReturnsWhenTheLauncherExitsBeforeExec(t *testing.T) {
	cmd := exec.Command(filepath.Join(t.TempDir(), "missing"))
	group := &linuxFallbackGroup{limit: 64 << 20}
	if err := group.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := group.Adopt(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("Adopt: %v", err)
	}
	var exitErr *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exitErr) {
		t.Fatalf("launcher exit = %v, want its exec failure", err)
	}
}

func TestFallbackAdoptBoundsTheWaitForTheLauncher(t *testing.T) {
	previous := launcherExecTimeout
	launcherExecTimeout = 50 * time.Millisecond
	t.Cleanup(func() { launcherExecTimeout = previous })
	// A process that keeps the recorded launcher cmdline stands in for a
	// launcher that never execs.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	group := &linuxFallbackGroup{limit: 64 << 20, configured: true, launcherCmdline: strings.Join(cmd.Args, "\x00") + "\x00"}
	if err := group.Adopt(cmd); err == nil || !strings.Contains(err.Error(), "did not exec") {
		t.Fatalf("Adopt error = %v, want the launcher bound", err)
	}
}

func TestFallbackLauncherDoneReadsExecAndExit(t *testing.T) {
	group := &linuxFallbackGroup{launcherCmdline: "sh\x00-c\x00script\x00"}
	for _, tc := range []struct {
		name    string
		cmdline string
		hasExe  bool
		done    bool
	}{
		{name: "launcher running", cmdline: group.launcherCmdline, hasExe: true, done: false},
		{name: "exec publishing its arguments", cmdline: "", hasExe: true, done: false},
		{name: "command running", cmdline: "/bin/backend\x00--harness\x00", hasExe: true, done: true},
		{name: "exited", cmdline: "", hasExe: false, done: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proc := t.TempDir()
			if err := os.WriteFile(filepath.Join(proc, "cmdline"), []byte(tc.cmdline), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.hasExe {
				if err := os.Symlink("/bin/sh", filepath.Join(proc, "exe")); err != nil {
					t.Fatal(err)
				}
			}
			done, err := group.launcherDone(proc)
			if err != nil {
				t.Fatal(err)
			}
			if done != tc.done {
				t.Fatalf("launcherDone = %v, want %v", done, tc.done)
			}
		})
	}
}

// The fallback has no kernel handle on its processes, so it must not claim
// to kill them: a caller that finds a Killer trusts it to end the tree.
func TestFallbackIsNotAKiller(t *testing.T) {
	if _, ok := any(&linuxFallbackGroup{}).(Killer); ok {
		t.Fatal("the RLIMIT fallback implements Killer without owning a kill handle")
	}
}
