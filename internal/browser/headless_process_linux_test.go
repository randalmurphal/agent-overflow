package browser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// On Linux Chromium leads a process group of its own, which stop kills
// whole, and the kernel kills it when the backend dies, so a crashed or
// killed backend leaves no browser running.
func TestChromiumCommandRunsInItsOwnGroupAndDiesWithTheBackend(t *testing.T) {
	cmd := chromiumCommand("/usr/bin/chromium", chromiumArgs("/data/chromium"), chromiumEnv("/data/chromium"), os.Stderr)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("SysProcAttr = %+v, want Setpgid and Pdeathsig SIGKILL", cmd.SysProcAttr)
	}
}

// processRunning reports whether pid names a process that has not exited.
// A zombie has exited: it holds no files and runs no code.
func processRunning(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	end := bytes.LastIndexByte(stat, ')')
	return end >= 0 && len(stat) > end+2 && stat[end+2] != 'Z'
}

// readPIDFile reads a pid a script recorded before printing its endpoint.
func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatalf("%s holds %q: %v", path, body, err)
	}
	return pid
}

// stopWithin runs stop and fails the test if it does not return in time.
func stopWithin(t *testing.T, process *chromiumProcess, timeout time.Duration) error {
	t.Helper()
	stopped := make(chan error, 1)
	go func() { stopped <- process.stop() }()
	select {
	case err := <-stopped:
		return err
	case <-time.After(timeout):
		t.Fatalf("stop did not return within %s", timeout)
		return nil
	}
}

// Chromium's renderers, GPU process and network and storage services are
// its children, and they write into the user-data directory until they
// die. stop kills them with the browser and returns only once they are
// gone.
func TestStopKillsEveryProcessInChromiumsGroup(t *testing.T) {
	helperFile := filepath.Join(t.TempDir(), "helper")
	binary := writeChromiumScript(t,
		"sleep 300 &\n"+
			"echo $! > "+shellQuote(helperFile)+"\n"+
			"echo 'DevTools listening on "+fakeDevToolsURL+"' >&2\n"+
			"exec sleep 300\n")
	process, _, err := startChromium(testLaunchContext(t, headlessTestDeadline), binary, nil, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	helper := readPIDFile(t, helperFile)
	t.Cleanup(func() { killTestProcess(t, helper, "sleep\x00300\x00") })
	if !processRunning(helper) {
		t.Fatalf("the helper %d died on its own", helper)
	}
	if err := stopWithin(t, process, headlessTestDeadline); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if processRunning(helper) {
		t.Fatalf("Chromium's helper %d outlived stop", helper)
	}
}

// A process of the group that is still exiting when the browser process has
// been reaped can still write. stop waits for it.
//
// The test holds a member at its exit with ptrace, which models a helper
// still closing its files without depending on how long a real one takes:
// a traced process that is killed stops at PTRACE_EVENT_EXIT, before it
// closes anything, until its tracer lets it go.
func TestStopWaitsForAProcessOfTheGroupThatIsStillExiting(t *testing.T) {
	process := startListeningChromium(t)
	leader := process.cmd.Process.Pid

	// Every ptrace request must come from the thread that started the
	// tracee.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	helper := exec.Command("sleep", "300")
	helper.SysProcAttr = &syscall.SysProcAttr{Ptrace: true, Setpgid: true, Pgid: leader}
	if err := helper.Start(); err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("ptrace is not permitted here: %v", err)
		}
		t.Fatalf("start the helper: %v", err)
	}
	pid := helper.Process.Pid
	defer helper.Process.Release()
	defer releaseTracee(t, pid)
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &status, syscall.WALL, nil); err != nil || !status.Stopped() {
		t.Fatalf("the helper did not stop at exec: %v (%v)", status, err)
	}
	if err := syscall.PtraceSetOptions(pid, unix.PTRACE_O_TRACEEXIT); err != nil {
		t.Fatalf("trace the helper's exit: %v", err)
	}
	if err := syscall.PtraceCont(pid, 0); err != nil {
		t.Fatalf("resume the helper: %v", err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- process.stop() }()
	if _, err := syscall.Wait4(pid, &status, syscall.WALL, nil); err != nil {
		t.Fatalf("wait for the killed helper: %v", err)
	}
	if !status.Stopped() || status.TrapCause() != syscall.PTRACE_EVENT_EXIT {
		t.Skipf("this kernel does not hold a killed tracee at its exit (status %#x)", uint32(status))
	}
	<-process.exited
	select {
	case err := <-stopped:
		t.Fatalf("stop returned (%v) while a process of the group was still exiting", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := syscall.PtraceCont(pid, 0); err != nil {
		t.Fatalf("let the helper exit: %v", err)
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("stop did not return once the group had exited")
	}
}

// releaseTracee kills a traced helper and reaps it.
func releaseTracee(t *testing.T, pid int) {
	t.Helper()
	for range 10 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = syscall.PtraceCont(pid, 0)
		var status syscall.WaitStatus
		if _, err := syscall.Wait4(pid, &status, syscall.WALL, nil); err != nil || status.Exited() || status.Signaled() {
			return
		}
	}
	t.Errorf("the traced helper %d would not exit", pid)
}

// Chromium's crash handlers leave its group and keep its output. stop does
// not wait for a process outside the group, and it closes its own end of
// the output, so one that holds the other end cannot hold stop.
func TestStopIsNotHeldByAProcessThatLeftTheGroup(t *testing.T) {
	escapeeFile := filepath.Join(t.TempDir(), "escapee")
	binary := writeChromiumScript(t,
		"setsid sleep 300 &\n"+
			"echo $! > "+shellQuote(escapeeFile)+"\n"+
			"echo 'DevTools listening on "+fakeDevToolsURL+"' >&2\n"+
			"exec sleep 300\n")
	process, _, err := startChromium(testLaunchContext(t, headlessTestDeadline), binary, nil, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	escapee := readPIDFile(t, escapeeFile)
	t.Cleanup(func() { killTestProcess(t, escapee, "sleep\x00300\x00") })
	eventually(t, "the escapee to leave the group", func() bool {
		group, _, ok := readProcess(escapee)
		return ok && group == escapee
	})

	if err := stopWithin(t, process, headlessTestDeadline); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !processRunning(escapee) {
		t.Fatal("the escapee died, so the test proved nothing")
	}
}

// killTestProcess kills a process a test script started, if it is still
// running, after checking that the pid still names it by its command line.
func killTestProcess(t *testing.T, pid int, cmdline string) {
	t.Helper()
	if !processRunning(pid) {
		return
	}
	current, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || !strings.HasPrefix(string(current), cmdline) {
		t.Errorf("pid %d is now %q (%v); not killing it", pid, current, err)
		return
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Errorf("kill the test's process %d: %v", pid, err)
	}
}

// The wait for a killed group names what is still running when it gives up,
// counts a zombie as exited, and returns as soon as the last member exits.
// Both the pidfd wait and the polling one a kernel without pidfd uses.
func TestWaitGroupExitedWaitsForEveryMember(t *testing.T) {
	for _, tc := range []struct {
		name string
		wait func(pgid int, timeout time.Duration) error
	}{
		{name: "pidfd", wait: waitGroupExited},
		{name: "polling", wait: func(pgid int, timeout time.Duration) error {
			members, err := groupMembers(pgid)
			if err != nil {
				return err
			}
			return pollGroupExited(pgid, members, timeout)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leader := startGroupSleep(t, 0)
			member := startGroupSleep(t, leader.Process.Pid)
			pgid := leader.Process.Pid
			// The leader exits and stays unreaped, as Chromium's browser
			// process does until stop reaps it.
			if err := leader.Process.Signal(syscall.SIGKILL); err != nil {
				t.Fatalf("kill the leader: %v", err)
			}
			if err := awaitExit(pgid); err != nil {
				t.Fatalf("wait for the leader: %v", err)
			}

			err := tc.wait(pgid, 50*time.Millisecond)
			if want := fmt.Sprintf("processes [%d] were still running", member.Process.Pid); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("wait = %v, want an error naming only %d", err, member.Process.Pid)
			}

			done := make(chan error, 1)
			go func() { done <- tc.wait(pgid, headlessTestDeadline) }()
			if err := member.Process.Signal(syscall.SIGKILL); err != nil {
				t.Fatalf("kill the member: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("wait: %v", err)
				}
			case <-time.After(headlessTestDeadline):
				t.Fatal("the wait outlived the last member")
			}
		})
	}
}

// A member that is gone by the time the wait opens it is not waited for,
// however the kernel refuses the open: ESRCH for a pid nothing holds, EINVAL
// for one that still names a process group whose leader was reaped.
func TestOpenMemberSkipsAMemberThatIsGone(t *testing.T) {
	leader := startGroupSleep(t, 0)
	member := startGroupSleep(t, leader.Process.Pid)
	pgid := leader.Process.Pid
	if err := leader.Process.Kill(); err != nil {
		t.Fatalf("kill the leader: %v", err)
	}
	if err := leader.Wait(); err == nil {
		t.Fatal("the killed leader exited cleanly")
	}
	if fd, err := openMember(pgid, pgid); fd != -1 || err != nil {
		t.Fatalf("openMember(reaped leader, live group) = %d, %v; want -1, nil", fd, err)
	}
	fd, err := openMember(member.Process.Pid, pgid)
	if err != nil || fd < 0 {
		t.Fatalf("openMember(running member) = %d, %v", fd, err)
	}
	unix.Close(fd)
	if err := member.Process.Kill(); err != nil {
		t.Fatalf("kill the member: %v", err)
	}
	if err := member.Wait(); err == nil {
		t.Fatal("the killed member exited cleanly")
	}
	for _, pid := range []int{pgid, member.Process.Pid} {
		if fd, err := openMember(pid, pgid); fd != -1 || err != nil {
			t.Fatalf("openMember(%d, gone) = %d, %v; want -1, nil", pid, fd, err)
		}
	}
}

// startGroupSleep starts `sleep 300` leading a new process group, or joining
// group pgid when it is not zero. The test reaps it.
func startGroupSleep(t *testing.T, pgid int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd
}

// A process is exited once it is a zombie with no other thread still
// exiting: the first thread to finish makes it a zombie while the rest may
// still hold the files they share.
func TestParseProcStatReadsTheGroupAndWhetherTheProcessExited(t *testing.T) {
	stat := func(comm, state string, threads int) []byte {
		return fmt.Appendf(nil, "4242 (%s) %s 1 777 777 0 -1 4194560 100 0 0 0 5 3 0 0 20 0 %d 0 123 0 0", comm, state, threads)
	}
	for _, tc := range []struct {
		name       string
		stat       []byte
		wantGroup  int
		wantExited bool
		wantOK     bool
	}{
		{name: "running", stat: stat("chrome", "S", 30), wantGroup: 777, wantOK: true},
		{name: "exited", stat: stat("chrome", "Z", 1), wantGroup: 777, wantExited: true, wantOK: true},
		{name: "a zombie whose threads are still exiting", stat: stat("chrome", "Z", 3), wantGroup: 777, wantOK: true},
		{name: "a name with spaces and parentheses", stat: stat("a) Z 9 9 (b", "R", 1), wantGroup: 777, wantOK: true},
		{name: "truncated", stat: []byte("4242 (chrome) S 1 777"), wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group, exited, ok := parseProcStat(tc.stat)
			if group != tc.wantGroup || exited != tc.wantExited || ok != tc.wantOK {
				t.Fatalf("parseProcStat = %d, %v, %v; want %d, %v, %v", group, exited, ok, tc.wantGroup, tc.wantExited, tc.wantOK)
			}
		})
	}
}

// An ephemeral profile's directory is removed only once nothing Chromium
// started can write into it, so nothing recreates it afterwards. The
// helper here keeps recreating a file in the profile, as Chromium's network
// service flushes its cache index as it exits.
func TestAnEphemeralProfileIsRemovedAfterItsLastWriter(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	writerFile := filepath.Join(t.TempDir(), "writer")
	script, err := os.ReadFile(browser.path)
	if err != nil {
		t.Fatalf("read the fake Chromium: %v", err)
	}
	writer := "for arg in \"$@\"; do case $arg in --user-data-dir=*) profile=${arg#--user-data-dir=};; esac; done\n" +
		"(while :; do mkdir -p \"$profile/Default\" && echo x > \"$profile/Default/index\"; sleep 0.01; done) &\n" +
		"echo $! > " + shellQuote(writerFile) + "\n"
	script = []byte(strings.Replace(string(script), "#!/bin/sh\n", "#!/bin/sh\n"+writer, 1))
	if err := os.WriteFile(browser.path, script, 0o700); err != nil {
		t.Fatalf("write the fake Chromium: %v", err)
	}
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	writerPID := readPIDFile(t, writerFile)
	t.Cleanup(func() { killTestProcess(t, writerPID, "/bin/sh\x00"+browser.path+"\x00") })
	eventually(t, "the writer to write into the profile", func() bool {
		_, err := os.Stat(filepath.Join(profile.userDataDir, "Default", "index"))
		return err == nil
	})

	disposed := make(chan error, 1)
	go func() { disposed <- profile.Dispose(context.Background()) }()
	select {
	case err := <-disposed:
		if err != nil {
			t.Fatalf("dispose: %v", err)
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("Dispose did not return")
	}
	if processRunning(writerPID) {
		t.Fatalf("the writer %d outlived Dispose", writerPID)
	}
	if _, err := os.Stat(profile.ephemeralRoot); !os.IsNotExist(err) {
		t.Fatalf("the ephemeral root survives Dispose: %v", err)
	}
}

// chromiumLeftovers lists the processes still running in Chromium's group,
// or using anything under root through their command line, working
// directory or open files.
func chromiumLeftovers(t *testing.T, group int, root string) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("list processes: %v", err)
	}
	under := func(path string) bool { return path == root || strings.HasPrefix(path, root+"/") }
	var left []string
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		pgid, exited, ok := readProcess(pid)
		if !ok || exited {
			continue
		}
		dir := "/proc/" + entry.Name()
		cmdline, _ := os.ReadFile(dir + "/cmdline")
		cwd, _ := os.Readlink(dir + "/cwd")
		switch {
		case pgid == group:
			left = append(left, fmt.Sprintf("%d in the group: %q", pid, cmdline))
		case bytes.Contains(cmdline, []byte(root)):
			left = append(left, fmt.Sprintf("%d names it: %q", pid, cmdline))
		case under(cwd):
			left = append(left, fmt.Sprintf("%d works in it: %q", pid, cmdline))
		default:
			fds, _ := os.ReadDir(dir + "/fd")
			for _, fd := range fds {
				if target, err := os.Readlink(dir + "/fd/" + fd.Name()); err == nil && under(target) {
					left = append(left, fmt.Sprintf("%d holds %s: %q", pid, target, cmdline))
					break
				}
			}
		}
	}
	return left
}
