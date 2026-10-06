//go:build !windows

package procutil

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"
)

// RunDrained runs cmd with its stdout and stderr copied to the given writers
// (nil discards) and returns once the process has exited and everything it
// wrote has reached them.
//
// exec.Cmd copies a non-file writer through a pipe and, WaitDelay after the
// process exits, closes that pipe whether the copy is blocked reading a pipe
// a leftover descendant holds open or writing to a slow writer with output
// still buffered; ErrWaitDelay does not say which. RunDrained owns the pipes
// instead. Once the process has exited, all of its output is already
// buffered, so each pipe is read until EOF or until one read waits linger
// with nothing to return, which only a descendant holding the pipe can
// cause. A slow writer delays the drain and never truncates it. After a
// writer fails, the rest of its pipe is read and discarded so the process
// cannot block on it, and the failure is returned.
//
// cmd.Stdin stays the caller's. A non-file reader is copied by exec, bounded
// by WaitDelay (linger when unset), so an ErrWaitDelay from Wait concerns
// only input the exited process did not read, and the process's own
// successful result is returned.
//
// ctx is the context cmd was created with. exec stops watching it when the
// process exits, so RunDrained watches it while draining: a descendant that
// keeps writing would otherwise hold the run open. On cancellation it calls
// cmd.Cancel (the group kill ConfigureGroup installs, or exec's default),
// stops reading and returns ctx's error.
func RunDrained(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.Writer, linger time.Duration) error {
	if cmd.Stdout != nil || cmd.Stderr != nil {
		return errors.New("procutil: RunDrained owns the command's stdout and stderr")
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return errors.Join(err, outR.Close(), outW.Close())
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = linger
	}
	startErr := cmd.Start()
	// The child has its own copies; these would keep EOF from arriving.
	closeErr := errors.Join(outW.Close(), errW.Close())
	if startErr != nil {
		return errors.Join(startErr, closeErr, outR.Close(), errR.Close())
	}

	exited := make(chan struct{})
	stop := make(chan struct{})
	copied := make(chan error, 2)
	go func() { copied <- drainPipe(outR, stdout, exited, stop, linger) }()
	go func() { copied <- drainPipe(errR, stderr, exited, stop, linger) }()

	waitErr := cmd.Wait()
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		waitErr = nil
	}
	close(exited)
	// A read already blocked when the process exited gets the same bound.
	// The pipes stay open until both copies return, so this cannot race a
	// close.
	setDeadlines := func(at time.Time) {
		for _, r := range []*os.File{outR, errR} {
			if err := r.SetReadDeadline(at); err != nil {
				closeErr = errors.Join(closeErr, err)
			}
		}
	}
	setDeadlines(time.Now().Add(linger))

	var copyErr, ctxErr error
	done := ctx.Done()
	for pending := 2; pending > 0; {
		select {
		case err := <-copied:
			copyErr = errors.Join(copyErr, err)
			pending--
		case <-done:
			done = nil
			ctxErr = ctx.Err()
			close(stop)
			// Interrupt blocked reads; each copy then sees stop.
			setDeadlines(time.Now())
			if cmd.Cancel != nil {
				if err := cmd.Cancel(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					closeErr = errors.Join(closeErr, err)
				}
			}
		}
	}
	return errors.Join(ctxErr, waitErr, copyErr, closeErr, outR.Close(), errR.Close())
}

// drainPipe copies src to dst until EOF, until stop closes or, after exited
// closes, until a read waits linger for data. It returns the first write
// failure, discarding what follows it, or a read failure.
func drainPipe(src *os.File, dst io.Writer, exited, stop <-chan struct{}, linger time.Duration) error {
	if dst == nil {
		dst = io.Discard
	}
	buf := make([]byte, 32<<10)
	var writeErr error
	for {
		select {
		case <-stop:
			return writeErr
		default:
		}
		select {
		case <-exited:
			if err := src.SetReadDeadline(time.Now().Add(linger)); err != nil {
				return err
			}
		default:
		}
		n, readErr := src.Read(buf)
		if n > 0 && writeErr == nil {
			_, writeErr = dst.Write(buf[:n])
		}
		switch {
		case readErr == nil:
		case readErr == io.EOF, errors.Is(readErr, os.ErrDeadlineExceeded):
			return writeErr
		default:
			return errors.Join(writeErr, readErr)
		}
	}
}
