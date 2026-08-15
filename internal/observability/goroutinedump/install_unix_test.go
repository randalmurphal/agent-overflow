//go:build !windows

package goroutinedump

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestInstallDumpsOnSignal(t *testing.T) {
	dir := t.TempDir()

	var (
		mu    sync.Mutex
		lines []string
	)
	stop := Install(dir, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, format)
	})
	defer stop()

	if err := syscall.Kill(os.Getpid(), Signal); err != nil {
		t.Fatalf("send %v: %v", Signal, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dump dir: %v", err)
		}
		if len(entries) == 1 && strings.HasPrefix(entries[0].Name(), FilePrefix) {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("no dump written after %v; dir = %v", Signal, entries)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The operator has to be able to find the file they just asked for.
	deadline = time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := append([]string(nil), lines...)
		mu.Unlock()
		if len(got) == 1 && strings.Contains(got[0], "wrote") {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("log lines = %v, want one naming the dump path", got)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Anyone able to signal this process can ask for a dump, and a dump on a wedged
// process is exactly when the listing is largest. A signal loop must not be able
// to fill the log directory or starve the process it is meant to diagnose, so
// signals inside MinInterval are COALESCED — and the suppression is stated, so
// an operator who sees no new file knows which one is theirs.
func TestInstallCoalescesSignalsInsideTheMinimumInterval(t *testing.T) {
	dir := t.TempDir()

	var (
		mu    sync.Mutex
		lines []string
	)
	stop := Install(dir, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	defer stop()

	logged := func(want int) []string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			got := append([]string(nil), lines...)
			mu.Unlock()
			if len(got) >= want {
				return got
			}
			if !time.Now().Before(deadline) {
				t.Fatalf("log lines = %v, want at least %d", got, want)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	// The signals are sent one at a time rather than as a burst: `signal.Notify`
	// drops onto a full buffer, so a burst can collapse before the handler sees
	// it and would prove nothing about the throttle. Waiting for the first dump's
	// line means the handler is back at its select and the second signal is
	// certain to be delivered.
	if err := syscall.Kill(os.Getpid(), Signal); err != nil {
		t.Fatalf("send %v: %v", Signal, err)
	}
	if first := logged(1); !strings.Contains(first[0], "wrote") {
		t.Fatalf("first line = %q, want the dump the operator asked for", first[0])
	}
	if err := syscall.Kill(os.Getpid(), Signal); err != nil {
		t.Fatalf("send %v: %v", Signal, err)
	}
	second := logged(2)[1]
	if !strings.Contains(second, "ignored") || !strings.Contains(second, MinInterval.String()) {
		t.Fatalf("second line = %q, want a suppression naming the interval it was refused under", second)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dump dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("a second signal inside %s wrote %d dumps, want exactly one", MinInterval, len(entries))
	}
}

func TestInstallStopUnregistersHandler(t *testing.T) {
	dir := t.TempDir()
	stop := Install(dir, func(string, ...any) {})
	stop()

	// After Stop the signal is no longer delivered to our channel. Go's
	// runtime restores the default disposition for SIGUSR1 once nothing is
	// listening, which would kill the test process — so re-arm a plain
	// listener to swallow it before proving no dump is written.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, Signal)
	defer signal.Stop(guard)

	if err := syscall.Kill(os.Getpid(), Signal); err != nil {
		t.Fatalf("send %v: %v", Signal, err)
	}
	<-guard

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dump dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("dump written after stop: %v", entries)
	}
}
