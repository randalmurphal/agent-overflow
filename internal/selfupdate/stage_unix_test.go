//go:build !windows

package selfupdate

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStageCopyCleanupFailureJoinsTheError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("directory write permissions do not bind root")
	}
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	src := filepath.Join(srcDir, "payload.fifo")
	if err := syscall.Mkfifo(src, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	wrong := sha256.Sum256([]byte("different bytes"))

	// A FIFO source makes the freeze deterministic: StageCopy's io.Copy blocks
	// until the writer closes, so once the temp file exists it is provably
	// parked mid-copy. The destination dir loses its write bit there, the
	// writer then closes, and the digest mismatch's deferred unlink fails with
	// EACCES. The write bit comes back in Cleanup so t.TempDir can remove the
	// tree.
	done := make(chan error, 1)
	go func() {
		_, err := StageCopy(src, dstDir, "agent-overflow-wsl-amd64.exe", wrong[:])
		done <- err
	}()

	w, err := os.OpenFile(src, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open fifo writer: %v", err)
	}
	if _, err := w.Write([]byte("staged bytes")); err != nil {
		t.Fatalf("write fifo: %v", err)
	}
	for {
		if entries := dirEntries(t, dstDir); len(entries) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.Chmod(dstDir, 0o555); err != nil {
		t.Fatalf("freeze staging dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dstDir, 0o755) })
	if err := w.Close(); err != nil {
		t.Fatalf("close fifo writer: %v", err)
	}

	err = <-done
	if err == nil {
		t.Fatal("StageCopy with a wrong digest = nil error")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("primary digest failure missing from error: %v", err)
	}
	if !strings.Contains(err.Error(), "remove temp") {
		t.Fatalf("cleanup failure was dropped from the error: %v", err)
	}
	if entries := dirEntries(t, dstDir); len(entries) != 1 {
		t.Fatalf("expected exactly the stuck temp file in %s, got %v", dstDir, entries)
	}
}
