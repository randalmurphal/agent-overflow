package browser

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The native smoke maps a real window. On Linux that window goes to a
// headless weston the run starts for itself, never to the session display
// the user works in, unless the run is explicitly opted in to it.

// nativeSmokeSessionDisplayEnv opts a Linux smoke run in to the session's
// own display, where its window appears on the user's desktop.
const nativeSmokeSessionDisplayEnv = "AO_NATIVE_BROWSER_SMOKE_SESSION_DISPLAY"

// nativeSmokeWestonEnv names the headless weston socket a smoke run started
// for the run it re-executes inside it.
const nativeSmokeWestonEnv = "AO_NATIVE_BROWSER_SMOKE_WESTON"

// nativeSmokePlan is where a smoke run puts its window.
type nativeSmokePlan int

const (
	// smokeHere runs in this process, on the display it already has.
	smokeHere nativeSmokePlan = iota
	// smokeUnderWeston starts a headless weston and runs again inside it.
	smokeUnderWeston
)

var errNoHeadlessDisplay = errors.New("native browser smoke: no headless weston is installed, and the session display is the user's; install weston, or set " +
	nativeSmokeSessionDisplayEnv + "=1 to run on the session display")

// nativeSmokeDisplay decides where a smoke run puts its window, from the
// environment, the weston binary found (or ""), and the OS. macOS has no
// headless compositor and runs in its logged-in session. On Linux a run
// re-executed inside its own weston runs there; one opted in to the session
// display runs on it; any other starts a weston, or is refused without one.
func nativeSmokeDisplay(getenv func(string) string, weston, goos string) (nativeSmokePlan, error) {
	if goos != "linux" {
		return smokeHere, nil
	}
	if socket := getenv(nativeSmokeWestonEnv); socket != "" {
		if getenv("WAYLAND_DISPLAY") != socket || getenv("DISPLAY") != "" {
			return smokeHere, errors.New("native browser smoke: re-executed for weston " + socket + " with another display")
		}
		return smokeHere, nil
	}
	if getenv(nativeSmokeSessionDisplayEnv) == "1" {
		return smokeHere, nil
	}
	if weston == "" {
		return smokeHere, errNoHeadlessDisplay
	}
	return smokeUnderWeston, nil
}

// headlessSmokeEnv is the environment of a run re-executed inside the
// weston at socket: no X display to fall back to, Wayland on that socket
// only, and the socket recorded so the run knows it is inside it.
func headlessSmokeEnv(environ []string, socket string) []string {
	out := make([]string, 0, len(environ)+3)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "DISPLAY", "WAYLAND_DISPLAY", "GDK_BACKEND", nativeSmokeWestonEnv, nativeSmokeSessionDisplayEnv:
			continue
		}
		out = append(out, kv)
	}
	return append(out, "WAYLAND_DISPLAY="+socket, "GDK_BACKEND=wayland", nativeSmokeWestonEnv+"="+socket)
}

// runUnderHeadlessWeston starts a headless weston on a socket of the run's
// own, runs this test binary again inside it with headlessSmokeEnv, stops
// the weston, and answers the inner run's exit code.
func runUnderHeadlessWeston(weston string) int {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		fmt.Fprintln(os.Stderr, "native browser smoke: weston needs XDG_RUNTIME_DIR")
		return 1
	}
	socket := fmt.Sprintf("ao-native-smoke-%d", os.Getpid())
	compositor := exec.Command(weston, "--backend=headless", "--renderer=pixman", "--socket="+socket,
		"--width=1280", "--height=800", "--no-config")
	// The compositor itself sees no display: it must not nest in the
	// session's.
	compositor.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "DISPLAY=") || strings.HasPrefix(kv, "WAYLAND_DISPLAY=")
	})
	compositor.Stdout, compositor.Stderr = os.Stderr, os.Stderr
	if err := compositor.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "native browser smoke: start weston: %v\n", err)
		return 1
	}
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
	}
	ready := false
	for deadline := time.Now().Add(30 * time.Second); !ready && time.Now().Before(deadline); {
		select {
		case err := <-exited:
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
	inner := exec.Command(os.Args[0], os.Args[1:]...)
	inner.Env = headlessSmokeEnv(os.Environ(), socket)
	inner.Stdin, inner.Stdout, inner.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := inner.Run()
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

func TestNativeSmokeDisplayRefusesTheSessionDisplay(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(name string) string { return vars[name] }
	}
	session := map[string]string{"DISPLAY": ":0", "WAYLAND_DISPLAY": "wayland-0"}
	for _, tc := range []struct {
		name   string
		vars   map[string]string
		weston string
		goos   string
		want   nativeSmokePlan
		err    bool
	}{
		{"linux with weston starts its own", session, "/usr/bin/weston", "linux", smokeUnderWeston, false},
		{"linux without weston is refused", session, "", "linux", smokeHere, true},
		{"linux with no display and no weston is refused", map[string]string{}, "", "linux", smokeHere, true},
		{"the session display only when opted in", map[string]string{"DISPLAY": ":0", nativeSmokeSessionDisplayEnv: "1"}, "", "linux", smokeHere, false},
		{"an opt-in other than 1 is no opt-in", map[string]string{"DISPLAY": ":0", nativeSmokeSessionDisplayEnv: "yes"}, "", "linux", smokeHere, true},
		{"inside its weston it runs there", map[string]string{"WAYLAND_DISPLAY": "ao-smoke-1", nativeSmokeWestonEnv: "ao-smoke-1"}, "/usr/bin/weston", "linux", smokeHere, false},
		{"inside its weston with an X display left is refused", map[string]string{"DISPLAY": ":0", "WAYLAND_DISPLAY": "ao-smoke-1", nativeSmokeWestonEnv: "ao-smoke-1"}, "/usr/bin/weston", "linux", smokeHere, true},
		{"inside its weston on another socket is refused", map[string]string{"WAYLAND_DISPLAY": "wayland-0", nativeSmokeWestonEnv: "ao-smoke-1"}, "/usr/bin/weston", "linux", smokeHere, true},
		{"macOS runs in its session", session, "", "darwin", smokeHere, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nativeSmokeDisplay(env(tc.vars), tc.weston, tc.goos)
			if got != tc.want || (err != nil) != tc.err {
				t.Fatalf("nativeSmokeDisplay = %v, %v; want %v, error %v", got, err, tc.want, tc.err)
			}
		})
	}
}

func TestHeadlessSmokeEnvLeavesOnlyItsWeston(t *testing.T) {
	got := headlessSmokeEnv([]string{
		"HOME=/home/u", "DISPLAY=:0", "WAYLAND_DISPLAY=wayland-0", "GDK_BACKEND=x11",
		nativeSmokeSessionDisplayEnv + "=1", "PATH=/bin", "DISPLAYX=kept",
	}, "ao-smoke-7")
	want := []string{
		"HOME=/home/u", "PATH=/bin", "DISPLAYX=kept",
		"WAYLAND_DISPLAY=ao-smoke-7", "GDK_BACKEND=wayland", nativeSmokeWestonEnv + "=ao-smoke-7",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("headlessSmokeEnv = %q, want %q", got, want)
	}
}
