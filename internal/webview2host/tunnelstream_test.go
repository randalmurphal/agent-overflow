package webview2host

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// The peer's close of a stream runs after the data the peer sent before
// it is written, and data after the close is refused.
func TestATunnelStreamEndsAfterItsQueuedData(t *testing.T) {
	local, remote := net.Pipe()
	defer func() {
		if err := remote.Close(); err != nil {
			t.Errorf("close the peer's end: %v", err)
		}
	}()
	failed := make(chan error, 1)
	w := NewTunnelStreamWriter(local, DefaultTunnelStreamBounds, func(err error) { failed <- err })
	ran := make(chan struct{})
	go func() { defer close(ran); w.Run() }()

	if err := w.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	w.End(func() {
		close(ended)
		w.Stop()
		_ = local.Close()
	})
	if err := w.Write([]byte("late")); !errors.Is(err, ErrTunnelStreamStopped) {
		t.Fatalf("data after the close was %v, want refused", err)
	}
	select {
	case <-ended:
		t.Fatal("the close ran before the data queued before it was written")
	case <-time.After(20 * time.Millisecond):
	}
	_ = remote.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(remote)
	if err != nil || string(got) != "first" {
		t.Fatalf("the socket read %q (%v), want the data before the close", got, err)
	}
	<-ended
	<-ran
	select {
	case err := <-failed:
		t.Fatalf("an ended stream failed: %v", err)
	default:
	}
}
