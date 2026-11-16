package orphanreaper

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"
)

// TestClientNilSafe covers the no-sidecar path (Windows, tests): app.go
// holds a nil *Client there and calls these unconditionally.
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

// TestSidecarCommandRunsOneP pins the sidecar's runtime to one P even when
// the app was started with its own GOMAXPROCS, and keeps the rest of the
// app's environment for the sidecar's startup and diagnostics.
func TestSidecarCommandRunsOneP(t *testing.T) {
	t.Setenv("GOMAXPROCS", "8")
	t.Setenv("ORPHANREAPER_TEST_INHERITED", "kept")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	cmd := sidecarCommand("agent-overflow", r)

	var maxprocs []string
	inherited := false
	for _, kv := range cmd.Environ() {
		if v, ok := strings.CutPrefix(kv, "GOMAXPROCS="); ok {
			maxprocs = append(maxprocs, v)
		}
		if kv == "ORPHANREAPER_TEST_INHERITED=kept" {
			inherited = true
		}
	}
	if len(maxprocs) != 1 || maxprocs[0] != "1" {
		t.Errorf("sidecar GOMAXPROCS = %v, want [1]", maxprocs)
	}
	if !inherited {
		t.Error("sidecar environment dropped the parent's variables")
	}
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != r {
		t.Errorf("sidecar ExtraFiles = %v, want the control pipe as fd 3", cmd.ExtraFiles)
	}
}
