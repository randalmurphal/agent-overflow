package browser

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto"
	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"

	"agent-overflow/internal/procutil"
)

// chromiumProcess is one Chromium the headless engine started: the process
// group it leads, the goroutine that waits for it to exit, and the goroutine
// that reads its combined stdout and stderr for its whole life.
//
// The engine starts Chromium itself rather than through chromedp's
// ExecAllocator. That allocator reads the output from exec's StdoutPipe
// while a second goroutine calls Wait, and Wait closes a StdoutPipe as soon
// as the process exits, so a browser that prints why it refused to start
// and exits at once can lose the reason. Here the output is a pipe this
// type creates: Wait leaves a caller-supplied *os.File open, so reaping and
// reading are independent and nothing the process wrote is lost. The
// allocator would also add --no-sandbox when the backend runs as root.
//
// Chromium runs in a process group of its own, and stop kills the group and
// returns only once every process in it has exited. The renderers, the GPU
// process and the network and storage services all stay in that group, and
// the services write into the user-data directory until they die, so the
// group, not the browser process, is what an ephemeral profile's removal
// waits for. The crash handlers Chromium starts leave the group; they write
// only under the user-data directory (chromiumEnv) and exit with the
// browser.
type chromiumProcess struct {
	cmd *exec.Cmd
	// output is the read end of the pipe. stop closes it, which ends the
	// reader even while a process outside the group still holds the write
	// end.
	output *os.File
	// said is the tail of what the process printed before its DevTools line.
	said *procutil.TailBuffer
	// wsURL is the DevTools endpoint, set before startChromium returns.
	wsURL string

	// endpoint receives the DevTools websocket URL, or is closed when the
	// output ends without one. Buffered, so the reader never waits for a
	// launch that already gave up.
	endpoint chan string
	// drained is closed when the reader returns.
	drained chan struct{}
	// exited is closed when the browser process has exited. It is left
	// unreaped until stop: its pid is the group's id, and a pid the kernel
	// could hand to another process would let the group kill reach an
	// unrelated group.
	exited chan struct{}

	stopOnce sync.Once
	stopErr  error
}

// devToolsPrefix starts the line Chromium prints once its DevTools endpoint
// is listening. The rest of the line is the browser websocket URL.
var devToolsPrefix = []byte("DevTools listening on")

// chromiumReadSize is the reader's buffer, and so the longest line the
// DevTools prefix is matched at the start of. Longer lines are read in
// pieces and kept in the tail like any other output.
const chromiumReadSize = 4 << 10

// chromiumExitTimeout bounds each of stop's waits, for the browser process
// and then for the rest of its group. SIGKILL ends a process as soon as it
// leaves the kernel, so only one stuck in uninterruptible I/O can reach it,
// and stop then reports Chromium as still running rather than let its
// caller remove what it may still write.
const chromiumExitTimeout = 5 * time.Second

// chromiumDrainGrace bounds how long a stop before the DevTools line waits,
// once the group is gone, for the reader to take what the group wrote. The
// output ends at once unless a process outside the group, a crash handler,
// still holds the pipe.
const chromiumDrainGrace = 250 * time.Millisecond

// startChromium starts binary with args, and env added to the inherited
// environment, and waits, within ctx, for the DevTools websocket URL it
// prints. On error the process has been killed and reaped, and the error
// carries the tail of what it printed.
func startChromium(ctx context.Context, binary string, args, env []string) (*chromiumProcess, string, error) {
	output, input, err := os.Pipe()
	if err != nil {
		return nil, "", fmt.Errorf("create the output pipe: %w", err)
	}
	cmd := chromiumCommand(binary, args, env, input)
	if err := cmd.Start(); err != nil {
		return nil, "", errors.Join(err, input.Close(), output.Close())
	}
	c := &chromiumProcess{
		cmd:      cmd,
		output:   output,
		said:     procutil.NewTailBuffer(headlessOutputTail),
		endpoint: make(chan string, 1),
		drained:  make(chan struct{}),
		exited:   make(chan struct{}),
	}
	go c.watchExit()
	go c.read()
	// The child has its own copy of the write end. Closing this one is what
	// lets the reader see EOF once the child and anything it started are
	// gone.
	if err := input.Close(); err != nil {
		return nil, "", errors.Join(fmt.Errorf("close the parent's copy of the output pipe: %w", err), c.stop())
	}

	select {
	case wsURL, ok := <-c.endpoint:
		if ok {
			c.wsURL = wsURL
			return c, wsURL, nil
		}
	case <-c.exited:
		// Everything it wrote precedes its exit, so the output ends as soon
		// as no process it started still holds the pipe.
		select {
		case <-c.drained:
		case <-ctx.Done():
		}
	case <-ctx.Done():
		stopErr := c.stop()
		return nil, "", errors.Join(c.failure(fmt.Errorf("no DevTools endpoint: %w", ctx.Err())), stopErr)
	}
	// It stopped, or closed its output, before listening. Stopping it first
	// makes the exit status and the whole output available to the error.
	stopErr := c.stop()
	return nil, "", errors.Join(c.failure(fmt.Errorf("exited before listening for DevTools (%s)", c.exitStatus())), stopErr)
}

// chromiumCommand builds the launch command. The environment, with env
// added, and the working directory are inherited, stdin is the null device,
// and stdout and stderr share one pipe, because Chromium prints its
// DevTools line to stderr and its failures to either.
// configureChromiumProcess gives it its own process group where the
// platform has them.
func chromiumCommand(binary string, args, env []string, output *os.File) *exec.Cmd {
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = output
	cmd.Stderr = output
	configureChromiumProcess(cmd)
	return cmd
}

// watchExit closes exited when the browser process exits. Where the platform
// cannot wait without reaping, it reaps.
func (c *chromiumProcess) watchExit() {
	if err := awaitExit(c.cmd.Process.Pid); err != nil {
		// Reaping still reports the exit; only the reserved pid is lost.
		_ = c.cmd.Wait()
	}
	close(c.exited)
}

// read keeps the tail of the output until the DevTools line, then discards
// the rest until EOF or stop. Chromium writes for its whole life and would
// block on a full pipe, so the output is read until the end even though
// nothing after the DevTools line is kept.
func (c *chromiumProcess) read() {
	defer close(c.drained)
	reader := bufio.NewReaderSize(c.output, chromiumReadSize)
	lineStart := true
	for {
		line, err := reader.ReadSlice('\n')
		if lineStart && err == nil && bytes.HasPrefix(line, devToolsPrefix) {
			c.endpoint <- string(bytes.TrimSpace(line[len(devToolsPrefix):]))
			// The copy ends at EOF or when stop closes the pipe; neither
			// is a failure of the running browser.
			_, _ = io.Copy(io.Discard, reader)
			return
		}
		_, _ = c.said.Write(line)
		if err == nil || errors.Is(err, bufio.ErrBufferFull) {
			lineStart = err == nil
			continue
		}
		// EOF, or stop closed the pipe, before the DevTools line.
		close(c.endpoint)
		return
	}
}

// close asks the browser over DevTools to exit and waits, up to timeout, for
// it to do so. The request goes over a connection of its own: chromedp sends
// Browser.close only for a browser it launched itself. An error means the
// browser did not exit in time; stop kills it either way.
func (c *chromiumProcess) close(timeout time.Duration) error {
	select {
	case <-c.exited:
		return nil
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := chromedp.DialContext(ctx, c.wsURL)
	if err != nil {
		return fmt.Errorf("connect to its DevTools endpoint: %w", err)
	}
	defer conn.Close()
	request := &cdproto.Message{ID: 1, Method: cdproto.MethodType(cdpbrowser.CommandClose)}
	if err := conn.Write(ctx, request); err != nil {
		return fmt.Errorf("send %s: %w", cdpbrowser.CommandClose, err)
	}
	select {
	case <-c.exited:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("still running %s after %s", timeout, cdpbrowser.CommandClose)
	}
}

// stop kills Chromium's process group and returns once nothing in it can
// still write: every process in the group has exited and the browser
// process is reaped. It then ends the reader. Before the DevTools line the
// output is the launch's error, so the reader first gets up to
// chromiumDrainGrace to take everything the group wrote; closing the pipe
// discards what it has not read. It is safe to call more than once and from
// several goroutines; every caller returns after the first stop completes.
// An error means something Chromium started may still be running.
func (c *chromiumProcess) stop() error {
	c.stopOnce.Do(func() {
		c.stopErr = c.kill()
		if c.stopErr == nil && c.wsURL == "" {
			timer := time.NewTimer(chromiumDrainGrace)
			select {
			case <-c.drained:
			case <-timer.C:
			}
			timer.Stop()
		}
		if err := c.output.Close(); err != nil {
			c.stopErr = errors.Join(c.stopErr, fmt.Errorf("close Chromium's output: %w", err))
		}
		<-c.drained
	})
	return c.stopErr
}

// kill kills Chromium's process group, waits for every process in it to
// exit and reaps the browser process.
func (c *chromiumProcess) kill() error {
	pid := c.cmd.Process.Pid
	if err := killChromium(c.cmd); err != nil && !errors.Is(err, os.ErrProcessDone) {
		// Waiting for processes that could not be killed would not return.
		return fmt.Errorf("kill Chromium's process group (%d): %w", pid, err)
	}
	timer := time.NewTimer(chromiumExitTimeout)
	defer timer.Stop()
	select {
	case <-c.exited:
	case <-timer.C:
		return fmt.Errorf("Chromium (pid %d) was still running %s after it was killed", pid, chromiumExitTimeout)
	}
	err := waitGroupExited(pid, chromiumExitTimeout)
	if c.cmd.ProcessState == nil {
		var exitErr *exec.ExitError
		if waitErr := c.cmd.Wait(); waitErr != nil && !errors.As(waitErr, &exitErr) {
			err = errors.Join(err, fmt.Errorf("reap Chromium (pid %d): %w", pid, waitErr))
		}
	}
	return err
}

// exitStatus describes how the process ended. It reads the result only once
// the process is reaped, which stop does not do when the kill failed or the
// process outlived it.
func (c *chromiumProcess) exitStatus() string {
	select {
	case <-c.exited:
	default:
		return "not reaped"
	}
	if c.cmd.ProcessState == nil {
		return "not reaped"
	}
	return c.cmd.ProcessState.String()
}

// failure frames reason with the tail of what the process printed before
// its DevTools line, marked with "..." when older output was dropped.
func (c *chromiumProcess) failure(reason error) error {
	said := strings.TrimRight(c.said.String(), "\r\n")
	if said == "" {
		return reason
	}
	if c.said.Truncated() {
		said = "..." + said
	}
	return fmt.Errorf("%w; output:\n%s", reason, said)
}
