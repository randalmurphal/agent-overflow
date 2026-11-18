//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/procutil"
)

// runUnderHeadlessWeston starts a headless weston on a socket of the run's
// own, runs this test binary again inside it with headlessSmokeEnv, stops
// the weston, and answers the inner run's exit code. The weston and the
// inner run each lead a process group a reaper sidecar watches, so a kill
// of this process, which stops neither, still ends both.
func runUnderHeadlessWeston(weston string) (code int) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		fmt.Fprintln(os.Stderr, "native browser smoke: weston needs XDG_RUNTIME_DIR")
		return 1
	}
	sidecar, err := orphanreaper.Spawn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "native browser smoke: %v\n", err)
		return 1
	}
	defer func() {
		// A sidecar that died watched nothing: the run fails.
		if err := sidecar.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "native browser smoke: the reaper sidecar ended early: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}()
	socket := fmt.Sprintf("ao-native-smoke-%d", os.Getpid())
	// A context that never ends: ConfigureGroup needs one to set the group.
	// --debug lets the run's own clients capture the output
	// (westonScreenshot); the compositor serves nothing else.
	compositor := exec.CommandContext(context.Background(), weston, "--backend=headless", "--renderer=pixman", "--socket="+socket,
		"--width=1280", "--height=800", "--no-config", "--debug")
	// The compositor itself sees no display: it must not nest in the
	// session's.
	compositor.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "DISPLAY=") || strings.HasPrefix(kv, "WAYLAND_DISPLAY=")
	})
	compositor.Stdout, compositor.Stderr = os.Stderr, os.Stderr
	procutil.ConfigureGroup(compositor)
	if err := compositor.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "native browser smoke: start weston: %v\n", err)
		return 1
	}
	sidecar.Watch(compositor.Process.Pid)
	exited := make(chan error, 1)
	go func() { exited <- compositor.Wait() }()
	stop := func() {
		_ = compositor.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = compositor.Process.Kill()
			<-exited
		}
		sidecar.Release(compositor.Process.Pid)
	}
	ready := false
	for deadline := time.Now().Add(30 * time.Second); !ready && time.Now().Before(deadline); {
		select {
		case err := <-exited:
			sidecar.Release(compositor.Process.Pid)
			fmt.Fprintf(os.Stderr, "native browser smoke: weston exited before its socket was ready: %v\n", err)
			return 1
		case <-time.After(50 * time.Millisecond):
		}
		_, err := os.Stat(filepath.Join(runtimeDir, socket))
		ready = err == nil
	}
	if !ready {
		stop()
		fmt.Fprintln(os.Stderr, "native browser smoke: weston's socket never appeared")
		return 1
	}
	inner := exec.CommandContext(context.Background(), os.Args[0], os.Args[1:]...)
	inner.Env = headlessSmokeEnv(os.Environ(), socket)
	inner.Stdin, inner.Stdout, inner.Stderr = os.Stdin, os.Stdout, os.Stderr
	procutil.ConfigureGroup(inner)
	err = inner.Start()
	if err == nil {
		sidecar.Watch(inner.Process.Pid)
		err = inner.Wait()
		sidecar.Release(inner.Process.Pid)
	}
	stop()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	default:
		fmt.Fprintf(os.Stderr, "native browser smoke: run inside weston: %v\n", err)
		return 1
	}
}
