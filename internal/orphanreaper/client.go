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
// a reliable parent-death signal. The sidecar holds its whole script when
// Spawn returns (startSidecar), so it guards the parent from that moment.
func Spawn() (*Client, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("orphanreaper: pipe: %w", err)
	}
	cmd := sidecarCommand(r, killGracePeriod)
	startErr := startSidecar(cmd, sidecarScript)
	// The child holds its own copy of the read end now, or never started;
	// drop ours so the pipe has exactly one reader (child) and one writer
	// (this parent).
	closeErr := r.Close()
	if startErr != nil {
		return nil, errors.Join(fmt.Errorf("orphanreaper: start sidecar: %w", startErr), closeErr, w.Close())
	}
	if closeErr != nil {
		// The sidecar runs and guards this process; only the fd leaks.
		log.Printf("orphanreaper: close this process's copy of the control pipe's read end: %v", closeErr)
	}
	return &Client{cmd: cmd, w: w}, nil
}

// scriptHandoff bounds startSidecar's write of the script. The shell reads
// its script as soon as it runs, so only a shell that never runs waits it out.
const scriptHandoff = 10 * time.Second

// startSidecar starts cmd, a sidecarCommand, and returns once the shell's
// stdin holds all of script and has ended. The write is made here, by the
// caller's own goroutine: a Stdin that is not a file would be copied to the
// shell by a goroutine exec starts after the shell, and a parent killed
// before that goroutine ran would leave the sidecar an empty script, so it
// would exit at once and reap nothing. The shell reads while this writes,
// so the script's size is not bounded by the pipe's. A parent that dies
// during the write leaves a script cut short, which the shell does not run:
// sidecarScript is one compound command. When the handoff fails, the
// sidecar is ended and waited for.
func startSidecar(cmd *exec.Cmd, script string) error {
	stdin, feed, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("script pipe: %w", err)
	}
	cmd.Stdin = stdin
	if err := cmd.Start(); err != nil {
		return errors.Join(err, stdin.Close(), feed.Close())
	}
	err = stdin.Close()
	if err == nil {
		err = feed.SetWriteDeadline(time.Now().Add(scriptHandoff))
	}
	if err == nil {
		_, err = feed.WriteString(script)
	}
	if err = errors.Join(err, feed.Close()); err == nil {
		return nil
	}
	err = errors.Join(fmt.Errorf("hand the sidecar its script: %w", err), cmd.Process.Kill())
	// Its exit by that kill is the expected outcome, not a failure.
	if waitErr := cmd.Wait(); waitErr != nil {
		if _, exited := errors.AsType[*exec.ExitError](waitErr); !exited {
			err = errors.Join(err, waitErr)
		}
	}
	return err
}

// sidecarCommand builds the sidecar process, which startSidecar starts: the
// shell reads its script from stdin, with control on fd 3 and the
// SIGTERM→SIGKILL grace in whole seconds, so the command line reads
// `/bin/sh -s agent-overflow-reaper 2`.
func sidecarCommand(control *os.File, grace time.Duration) *exec.Cmd {
	seconds := strconv.Itoa(int(grace / time.Second))
	cmd := exec.Command("/bin/sh", "-s", SidecarName, seconds)
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
