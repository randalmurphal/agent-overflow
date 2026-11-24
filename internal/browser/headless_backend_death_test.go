//go:build linux || darwin

package browser

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/orphanreaper/orphanreapertest"
	"agent-overflow/internal/testutil/helperproc"
)

// chromiumBackendHelperEnv makes TestChromiumBackendHelper the backend that
// starts a fake Chromium and is then killed. Its value is the directory the
// fake records its pids in.
const chromiumBackendHelperEnv = "BROWSER_CHROMIUM_BACKEND_HELPER"

// TestChromiumBackendHelper is not a test of its own: the test below runs
// this binary as a backend that launches a browser through the headless
// engine's launch path and is then killed.
func TestChromiumBackendHelper(t *testing.T) {
	dir := os.Getenv(chromiumBackendHelperEnv)
	if dir == "" {
		t.Skip("runs only as the backend TestABackendsDeathEndsEveryProcessOfItsChromium kills")
	}
	// A browser process with a child of its own, as Chromium starts its
	// renderer, GPU and utility processes.
	binary := writeChromiumScript(t,
		"sleep 300 &\n"+
			"echo $! > "+shellQuote(filepath.Join(dir, "child"))+"\n"+
			"echo $$ > "+shellQuote(filepath.Join(dir, "leader"))+"\n"+
			"echo 'DevTools listening on "+fakeDevToolsURL+"' >&2\n"+
			"exec sleep 300\n")
	// The sidecar the app starts at boot (app_orphan_reaper.go).
	sidecar, err := orphanreaper.Spawn()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), headlessTestDeadline)
	defer cancel()
	if _, _, err := startChromium(ctx, sidecar, binary, nil, nil); err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	helperproc.Hold()
}

// A backend that dies without stopping its browser takes every process of
// the browser's group with it, not only the browser process.
func TestABackendsDeathEndsEveryProcessOfItsChromium(t *testing.T) {
	dir := t.TempDir()
	backend := exec.Command(os.Args[0], "-test.run=^TestChromiumBackendHelper$")
	backend.Env = append(os.Environ(), chromiumBackendHelperEnv+"="+dir)
	stdout, err := backend.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	helperproc.Tie(t, backend)
	if err := backend.Start(); err != nil {
		t.Fatal(err)
	}
	ready := false
	scanner := bufio.NewScanner(stdout)
	for !ready && scanner.Scan() {
		ready = strings.TrimSpace(scanner.Text()) == "ready"
	}
	if !ready {
		_ = backend.Process.Kill()
		_ = backend.Wait()
		t.Fatalf("the backend never started its browser: %v", scanner.Err())
	}
	leader := readPIDFile(t, filepath.Join(dir, "leader"))
	child := readPIDFile(t, filepath.Join(dir, "child"))
	t.Cleanup(func() { _ = syscall.Kill(-leader, syscall.SIGKILL) })
	// The sidecar waits out its grace after the kill it sends.
	sidecar := orphanreapertest.Of(t, backend.Process.Pid)
	t.Cleanup(func() { helperproc.EndGroup(t, sidecar) })
	if err := backend.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = backend.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for (processAlive(leader) || processAlive(child)) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(leader) || processAlive(child) {
		t.Fatalf("after the backend died: browser process running %v, its child running %v",
			processAlive(leader), processAlive(child))
	}
}

// processAlive reports whether pid can still run: it exists and has not
// exited.
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
