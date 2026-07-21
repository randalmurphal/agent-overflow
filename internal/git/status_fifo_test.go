//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

// The build constraint matches exactly the platforms where syscall.Mkfifo is
// defined. It is narrower than the `unix` tag on purpose: solaris and aix are
// `unix` but lack syscall.Mkfifo, so tagging this `unix` would fail to compile
// there.

package git

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// countUntrackedWithTimeout runs countUntrackedFileLines in a goroutine and
// fails if it does not return within d. The bug it guards is a permanent hang:
// opening a FIFO with no writer blocks forever, so a regression that os.Open'd a
// FIFO (directly or by following a symlink to one) would stall the gitwatch hot
// path indefinitely. The timeout turns that hang into a clean test failure.
func countUntrackedWithTimeout(t *testing.T, path string, budget int, d time.Duration) (insertions, bytesRead int) {
	t.Helper()
	// Lstat outside the goroutine: it never blocks on a FIFO, and the
	// caller contract is that untrackedStats has already stat'ed the path.
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	type result struct{ ins, read int }
	done := make(chan result, 1)
	go func() {
		ins, read := countUntrackedFileLines(info, path, budget)
		done <- result{ins, read}
	}()
	select {
	case r := <-done:
		return r.ins, r.read
	case <-time.After(d):
		t.Fatalf("countUntrackedFileLines(%q) did not return within %s — it likely opened a FIFO and blocked", path, d)
		return 0, 0
	}
}

// TestCountUntrackedFileLinesSkipsFifo proves the counter never opens a FIFO.
// Two paths reach it in practice: a bare FIFO (defense in depth — `git ls-files
// --others` does not enumerate bare FIFOs, but the guard must hold regardless)
// and, the real danger, an untracked *symlink* pointing at a FIFO (ls-files DOES
// list symlinks, so untrackedStats hands this path straight to the counter).
// Both must return immediately, the symlink counted as its 1-line link text.
func TestCountUntrackedFileLinesSkipsFifo(t *testing.T) {
	dir := t.TempDir()

	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unsupported on this platform: %v", err)
	}

	// Bare FIFO: non-regular, skipped as (0, 0) without opening it.
	if ins, read := countUntrackedWithTimeout(t, fifo, maxUntrackedScanBytes, 2*time.Second); ins != 0 || read != 0 {
		t.Fatalf("bare FIFO: got (%d, %d), want (0, 0)", ins, read)
	}

	// Symlink → FIFO: counted as the link text (1 line) via Readlink, the FIFO
	// itself never opened. This is the production-dangerous path.
	link := filepath.Join(dir, "link-to-pipe")
	if err := os.Symlink("pipe", link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	if ins, read := countUntrackedWithTimeout(t, link, maxUntrackedScanBytes, 2*time.Second); ins != 1 || read != 0 {
		t.Fatalf("symlink→FIFO: got (%d, %d), want (1, 0) — link text counted, FIFO not opened", ins, read)
	}
}
