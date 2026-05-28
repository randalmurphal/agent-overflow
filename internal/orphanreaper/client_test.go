package orphanreaper

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"
)

// TestClientNilSafe covers the non-darwin / no-sidecar path: app.go holds
// a nil *Client there and calls these unconditionally.
func TestClientNilSafe(t *testing.T) {
	var c *Client
	c.Watch(100)
	c.Release(100)
	if err := c.Close(); err != nil {
		t.Fatalf("nil Client Close = %v, want nil", err)
	}
}

// TestClientWritesProtocol verifies Watch/Release put the right bytes on
// the control pipe. Built with cmd=nil so it exercises the write path
// without spawning a real sidecar.
func TestClientWritesProtocol(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c := &Client{w: w}

	c.Watch(100)
	c.Release(100)

	br := bufio.NewReader(r)
	for _, want := range []string{"watch 100", "release 100"} {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got := strings.TrimSuffix(line, "\n"); got != want {
			t.Errorf("line = %q, want %q", got, want)
		}
	}
	_ = c.Close()
}

// TestClientCloseIdempotentAndStopsSends proves Close is safe to call
// twice and that sends after Close are dropped (not panicking, not
// written) — the shutdown double-invocation and late-event cases.
func TestClientCloseIdempotentAndStopsSends(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c := &Client{w: w}

	if err := c.Close(); err != nil {
		t.Fatalf("first Close = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil (idempotent)", err)
	}
	c.Watch(200) // must be dropped, not panic

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("read %q after Close, want nothing (sends dropped)", data)
	}
}
