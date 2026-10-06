//go:build !windows

package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/procutil"

	"agent-overflow/internal/testutil/mockexec"
)

// startChromium against shell scripts standing in for the ways a Chromium
// starts or fails to. No endpoint is dialed here, so the DevTools URL a
// script prints needs nothing behind it.

const fakeDevToolsURL = "ws://127.0.0.1:9/devtools/browser/fake"

// writeChromiumScript installs a fake Chromium that runs body.
func writeChromiumScript(t *testing.T, body string) string {
	t.Helper()
	return mockexec.WriteIn(t, t.TempDir(), "chromium", "#!/bin/sh\n"+body)
}

func testLaunchContext(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

// startListeningChromium starts a fake Chromium that prints its endpoint and
// then sleeps until it is killed.
func startListeningChromium(t *testing.T) *chromiumProcess {
	t.Helper()
	binary := writeChromiumScript(t, "echo 'DevTools listening on "+fakeDevToolsURL+"' >&2\nexec sleep 300\n")
	process, wsURL, err := startChromium(testLaunchContext(t, headlessTestDeadline), binary, nil, nil)
	if err != nil {
		t.Fatalf("start the fake Chromium: %v", err)
	}
	t.Cleanup(func() {
		if err := process.stop(); err != nil {
			t.Errorf("stop the fake Chromium: %v", err)
		}
	})
	if wsURL != fakeDevToolsURL {
		t.Fatalf("endpoint = %q, want %q", wsURL, fakeDevToolsURL)
	}
	return process
}

// Chromium writes for its whole life, and a pipe nobody reads fills and
// blocks it. Everything after the DevTools line is read and dropped, so the
// browser keeps running and none of it is kept.
func TestStartChromiumKeepsReadingAfterTheEndpoint(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "wrote-everything")
	binary := writeChromiumScript(t,
		"echo 'DevTools listening on "+fakeDevToolsURL+"' >&2\n"+
			// Sixteen times a Linux pipe's default capacity.
			"head -c 1048576 /dev/zero\n"+
			"head -c 1048576 /dev/zero >&2\n"+
			"echo done > "+shellQuote(marker)+"\n"+
			"exec sleep 300\n")
	process, wsURL, err := startChromium(testLaunchContext(t, headlessTestDeadline), binary, nil, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := process.cmd.Process.Pid
	if wsURL != fakeDevToolsURL {
		t.Fatalf("endpoint = %q, want %q", wsURL, fakeDevToolsURL)
	}
	eventually(t, "the browser to finish writing", func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})
	if got := process.said.String(); got != "" {
		t.Fatalf("kept %d bytes printed after the endpoint", len(got))
	}
	if err := process.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if alive(pid) {
		t.Fatalf("Chromium %d outlived stop", pid)
	}
}

// Chromium runs in the backend's own environment: the display, proxy and
// locale settings an operator gave the service reach the browser too.
func TestStartChromiumInheritsTheEnvironment(t *testing.T) {
	t.Setenv("AO_HEADLESS_TEST_MARKER", "from the backend")
	seen := filepath.Join(t.TempDir(), "env")
	binary := writeChromiumScript(t,
		"printf '%s' \"$AO_HEADLESS_TEST_MARKER\" > "+shellQuote(seen)+"\n"+
			"echo 'DevTools listening on "+fakeDevToolsURL+"' >&2\n"+
			"exec sleep 300\n")
	process, _, err := startChromium(testLaunchContext(t, headlessTestDeadline), binary, nil, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := process.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	body, err := os.ReadFile(seen)
	if err != nil || string(body) != "from the backend" {
		t.Fatalf("the browser saw %q (%v), want the backend's environment", body, err)
	}
}

// A browser that closes its output without printing an endpoint will never
// print one. The launch fails at once with what it said, rather than
// waiting out the launch bound, and the process is killed and reaped.
func TestStartChromiumFailsWhenTheOutputEndsWithoutAnEndpoint(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	binary := writeChromiumScript(t,
		"echo $$ > "+shellQuote(pidFile)+"\n"+
			"echo 'Missing X server or $DISPLAY' >&2\n"+
			"exec >&- 2>&-\n"+
			"exec sleep 300\n")
	started := time.Now()
	_, _, err := startChromium(testLaunchContext(t, headlessTestDeadline), binary, nil, nil)
	if err == nil {
		t.Fatal("a browser with no endpoint reported one")
	}
	if elapsed := time.Since(started); elapsed > headlessTestDeadline/2 {
		t.Fatalf("the launch took %s: it waited for a line that could no longer come", elapsed)
	}
	if !strings.Contains(err.Error(), "Missing X server") {
		t.Fatalf("error %v drops what the browser said", err)
	}
	if pid := waitForPID(t, pidFile); alive(pid) {
		t.Fatalf("Chromium %d outlived its failed launch", pid)
	}
}

// launchBound is a launch context whose deadline passes when the test calls
// expire, so the launch ends after the browser has printed however long the
// machine takes to start it. macOS assesses a newly written executable on
// its first run, which alone can outlast a short fixed bound.
type launchBound struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newLaunchBound(t *testing.T) *launchBound {
	b := &launchBound{Context: context.Background(), done: make(chan struct{})}
	t.Cleanup(b.expire)
	return b
}

func (b *launchBound) Done() <-chan struct{} { return b.done }

func (b *launchBound) Err() error {
	select {
	case <-b.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (b *launchBound) expire() { b.once.Do(func() { close(b.done) }) }

// A browser that never prints its endpoint fails the launch when the launch
// bound ends, with what it said, and is killed and reaped before the launch
// returns.
func TestStartChromiumTimesOutAndReapsASilentBrowser(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	binary := writeChromiumScript(t,
		"echo 'still starting' >&2\n"+
			"echo $$ > "+shellQuote(pidFile)+"\n"+
			"exec sleep 300\n")
	bound := newLaunchBound(t)
	launched := make(chan error, 1)
	go func() {
		_, _, err := startChromium(bound, binary, nil, nil)
		launched <- err
	}()
	pid := waitForPID(t, pidFile)
	bound.expire()
	err := <-launched
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v, want the launch bound", err)
	}
	if !strings.Contains(err.Error(), "still starting") {
		t.Fatalf("error %v drops what the browser said", err)
	}
	if alive(pid) {
		t.Fatalf("Chromium %d outlived the launch bound", pid)
	}
}

// Before the DevTools line, what the browser printed is the launch's error.
// stop keeps output the reader had not taken when the group died rather
// than discard it by closing the pipe.
func TestStopKeepsOutputTheReaderHasNotTakenYet(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	binary := writeChromiumScript(t,
		"echo 'Failed to move to new namespace' >&2\n"+
			"echo $$ > "+shellQuote(pidFile)+"\n"+
			"exec sleep 300\n")
	output, input, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := chromiumCommand(binary, nil, nil, input)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	c := &chromiumProcess{
		cmd:      cmd,
		output:   output,
		said:     procutil.NewTailBuffer(headlessOutputTail),
		endpoint: make(chan string, 1),
		drained:  make(chan struct{}),
		exited:   make(chan struct{}),
	}
	go c.watchExit()
	pid := waitForPID(t, pidFile)
	// The reader starts only once stop has reaped the browser, so the line is
	// still in the pipe when the group is gone.
	go func() {
		deadline := time.Now().Add(headlessTestDeadline)
		for alive(pid) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		c.read()
	}()
	if err := c.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := c.failure(errors.New("launch failed")).Error(); !strings.Contains(got, "Failed to move to new namespace") {
		t.Fatalf("error %q drops what the browser printed before stop", got)
	}
}

// A browser that prints a megabyte and then its reason costs the error no
// more than the tail: the reason and how it exited survive, the noise does
// not.
func TestStartChromiumKeepsOnlyTheTailOfALongFailure(t *testing.T) {
	binary := writeChromiumScript(t,
		"head -c 1048576 /dev/zero | tr '\\0' x\n"+
			"echo\n"+
			"echo 'FATAL: libnss3.so: cannot open shared object file' >&2\n"+
			"exit 1\n")
	_, _, err := startChromium(testLaunchContext(t, headlessTestDeadline), binary, nil, nil)
	if err == nil {
		t.Fatal("a browser that exited 1 reported an endpoint")
	}
	message := err.Error()
	for _, want := range []string{"exit status 1", "FATAL: libnss3.so", "..."} {
		if !strings.Contains(message, want) {
			t.Fatalf("error is missing %q: %.200s", want, message)
		}
	}
	if len(message) > headlessOutputTail+200 {
		t.Fatalf("error is %d bytes; the output tail is %d", len(message), headlessOutputTail)
	}
}
