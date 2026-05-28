package orphanreaper

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
)

const reapSubcommand = "__reap"

// Subcommand returns the argv token that routes a process into RunChild.
// main() compares os.Args[1] against this before any other startup.
func Subcommand() string { return reapSubcommand }

// Client is the parent-side handle to a reaper sidecar. It owns the write
// end of the control pipe: writing watch/release commands updates the
// sidecar's watched set, and closing the client (or the parent dying)
// sends EOF, which triggers the sidecar's cleanup. Every method is
// nil-safe so callers on platforms that don't start a reaper (Linux,
// Windows) can invoke them unconditionally.
type Client struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	w    *os.File
	done bool
}

// Spawn launches `selfExe __reap` with the read end of a fresh control
// pipe on fd 3 and returns a Client holding the write end. selfExe is
// normally os.Executable(). Go marks the write end close-on-exec, so it
// never leaks into the provider subprocesses spawned later — the parent
// stays the pipe's sole writer, which is what makes EOF a reliable
// parent-death signal.
func Spawn(selfExe string) (*Client, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("orphanreaper: pipe: %w", err)
	}
	cmd := exec.Command(selfExe, reapSubcommand)
	cmd.ExtraFiles = []*os.File{r} // lands as fd 3 (controlFD) in the child
	cmd.Stderr = os.Stderr         // surface the sidecar's diagnostics
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
