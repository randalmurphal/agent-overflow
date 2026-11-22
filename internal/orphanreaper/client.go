package orphanreaper

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
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
// a reliable parent-death signal. The sidecar has its whole script before it
// starts, so it guards the parent from the moment Spawn returns.
func Spawn() (*Client, error) {
	script, err := scriptInput(sidecarScript)
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("orphanreaper: pipe: %w", err), script.Close())
	}
	cmd := sidecarCommand(script, r, killGracePeriod)
	startErr := cmd.Start()
	// The child holds its own copies of both read ends now, or never started;
	// drop ours so the control pipe has exactly one reader (child) and one
	// writer (this parent).
	closeErr := errors.Join(script.Close(), r.Close())
	if startErr != nil {
		return nil, errors.Join(fmt.Errorf("orphanreaper: start sidecar: %w", startErr), closeErr, w.Close())
	}
	if closeErr != nil {
		// The sidecar runs and guards this process; only the fds leak.
		log.Printf("orphanreaper: close this process's copies of the sidecar's pipes: %v", closeErr)
	}
	return &Client{cmd: cmd, w: w}, nil
}

// scriptInput returns the read end of a pipe that already holds all of
// script, its write end closed. A Stdin that is not a file would be copied
// to the shell by a goroutine of this process after the shell starts, and a
// parent killed before that goroutine ran would leave the sidecar an empty
// script: it would exit at once and reap nothing. The script must fit in the
// pipe's buffer, since nothing reads it yet (TestSidecarScriptFitsInAPipe).
func scriptInput(script string) (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("orphanreaper: script pipe: %w", err)
	}
	_, writeErr := w.WriteString(script)
	if err := errors.Join(writeErr, w.Close()); err != nil {
		return nil, errors.Join(fmt.Errorf("orphanreaper: write the sidecar script: %w", err), r.Close())
	}
	return r, nil
}

// sidecarCommand builds the sidecar process: the shell reads script from
// stdin (scriptInput), with control on fd 3 and the SIGTERM→SIGKILL grace in
// whole seconds, so the command line reads
// `/bin/sh -s agent-overflow-reaper 2`. The caller closes its copies of
// script and control once the sidecar has started.
func sidecarCommand(script, control *os.File, grace time.Duration) *exec.Cmd {
	seconds := strconv.Itoa(int(grace / time.Second))
	cmd := exec.Command("/bin/sh", "-s", SidecarName, seconds)
	cmd.Stdin = script
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
