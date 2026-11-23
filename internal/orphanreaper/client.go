package orphanreaper

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SidecarName is the argument that names the sidecar in its command line.
const SidecarName = "agent-overflow-reaper"

// Client is the parent-side handle to a reaper sidecar. It owns the write
// end of the control pipe: writing watch/release commands updates the
// sidecar's watched set, and closing the client (or the parent dying)
// sends EOF, which triggers the sidecar's cleanup. Every method is
// nil-safe so callers on Windows, which starts no reaper, and in tests can
// invoke them unconditionally.
type Client struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	w    *os.File
	done bool
}

// Spawn launches the sidecar with the read end of a fresh control pipe on
// fd 3 and returns a Client holding the write end. Go marks the write end
// close-on-exec, so it never leaks into the provider subprocesses spawned
// later — the parent stays the pipe's sole writer, which is what makes EOF
// a reliable parent-death signal.
func Spawn() (*Client, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("orphanreaper: pipe: %w", err)
	}
	cmd := sidecarCommand(r, killGracePeriod)
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, fmt.Errorf("orphanreaper: start sidecar: %w", err)
	}
	// The child holds its own copy of the read end now; drop ours so the
	// pipe has exactly one reader (child) and one writer (this parent).
	r.Close()
	return &Client{cmd: cmd, w: w}, nil
}

// sidecarCommand builds the sidecar process: sidecarScript under /bin/sh,
// with control on fd 3 and the SIGTERM→SIGKILL grace in whole seconds. The
// script arrives on stdin, so the command line reads
// `/bin/sh -s agent-overflow-reaper 2`.
func sidecarCommand(control *os.File, grace time.Duration) *exec.Cmd {
	seconds := strconv.Itoa(int(grace / time.Second))
	cmd := exec.Command("/bin/sh", "-s", SidecarName, seconds)
	cmd.Stdin = strings.NewReader(sidecarScript)
	cmd.ExtraFiles = []*os.File{control} // lands as fd 3, which sidecarScript reads
	cmd.Stderr = os.Stderr               // surface the sidecar's diagnostics
	// Only PATH, which finds sleep: a shell that imports functions from its
	// environment (bash as /bin/sh) would run an exported kill or sleep in
	// place of the script's.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	applySidecarProcessAttrs(cmd)
	return cmd
}

// Watch tells the sidecar to kill process group pgid if the parent dies.
func (c *Client) Watch(pgid int) { c.send(formatWatch(pgid)) }

// Release tells the sidecar to stop tracking pgid (its session ended
// cleanly and was already torn down).
func (c *Client) Release(pgid int) { c.send(formatRelease(pgid)) }

func (c *Client) send(msg string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done || c.w == nil {
		return
	}
	if _, err := c.w.WriteString(msg); err != nil {
		// A broken pipe means the sidecar already exited; nothing to do
		// from here, and the startup sweep is the backstop.
		log.Printf("orphanreaper: control write failed: %v", err)
	}
}

// Close stops the sidecar. Closing the write end sends EOF, so the
// sidecar kills any still-watched groups (none, if every session was
// released first) and exits; we then reap it to avoid a lingering
// zombie. Idempotent and nil-safe.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return nil
	}
	c.done = true
	w := c.w
	c.w = nil
	cmd := c.cmd
	c.mu.Unlock()

	if w != nil {
		w.Close()
	}
	if cmd != nil {
		return cmd.Wait()
	}
	return nil
}
