//go:build !windows

package app

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRemoteArtifactRefusesFIFOWithoutBlocking(t *testing.T) {
	workspace := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(workspace, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readRemoteArtifactChunk(context.Background(), workspace, RemoteArtifactRequest{Path: "pipe"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted FIFO")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("artifact open blocked on FIFO")
	}
}
