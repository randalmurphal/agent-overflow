//go:build !windows

package main

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReadWorkspaceFileRejectsFIFOWithoutBlocking(t *testing.T) {
	// The checks run on the O_NONBLOCK-opened descriptor, so a path
	// swapped to a FIFO after any pre-open validation is rejected
	// instead of hanging the RPC goroutine on open or read.
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatalf("Mkfifo() error = %v", err)
	}
	if _, err := readWorkspaceFile(fifo, 64); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("FIFO read error = %v, want not a regular file", err)
	}
}
