package provider

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// maxLineSize is the max stdout line buffer: 10 MB.
	// Diffs and command outputs can be large.
	maxLineSize = 10 * 1024 * 1024

	// shutdownGrace is how long to wait after closing stdin before sending SIGTERM.
	shutdownGrace = 3 * time.Second

	// killGrace is how long to wait after SIGTERM before sending SIGKILL.
	killGrace = 2 * time.Second
)

// Process manages a subprocess with stdin/stdout pipes.
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	done   chan struct{}
	err    error
	mu     sync.Mutex
}

// SpawnConfig configures subprocess creation.
type SpawnConfig struct {
	Binary string
	Args   []string
	Dir    string
	Env    map[string]string
}

// Spawn starts a subprocess with stdin/stdout pipes and process group isolation.
// The context is associated with the command — canceling it will kill the process.
// Prefer Close() for graceful shutdown.
func Spawn(ctx context.Context, cfg SpawnConfig) (*Process, error) {
	cmd := exec.CommandContext(ctx, cfg.Binary, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Build env: inherit current env + overrides.
	env := os.Environ()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("provider: stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("provider: stdout pipe: %w", err)
	}

	// Forward stderr so provider debug output is visible during development.
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("provider: start %s: %w", cfg.Binary, err)
	}

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	p := &Process{
		cmd:    cmd,
		stdin:  stdin,
		stdout: scanner,
		done:   make(chan struct{}),
	}

	// Wait goroutine: detect process exit.
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()

	return p, nil
}

// WriteLine writes a line to stdin (appends newline).
func (p *Process) WriteLine(data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-p.done:
		return fmt.Errorf("provider: process already exited")
	default:
	}

	buf := make([]byte, len(data)+1)
	copy(buf, data)
	buf[len(data)] = '\n'
	_, err := p.stdin.Write(buf)
	if err != nil {
		return fmt.Errorf("provider: write to stdin: %w", err)
	}
	return nil
}

// ReadLine reads the next line from stdout. Returns io.EOF when process exits.
func (p *Process) ReadLine() ([]byte, error) {
	if p.stdout.Scan() {
		// Return a copy — scanner reuses its buffer.
		line := p.stdout.Bytes()
		out := make([]byte, len(line))
		copy(out, line)
		return out, nil
	}
	if err := p.stdout.Err(); err != nil {
		// cmd.Wait() closes the stdout pipe, which can race with Scan().
		// Treat a closed-pipe error as EOF — the process is gone either way.
		if isClosedPipeErr(err) {
			return nil, io.EOF
		}
		return nil, err
	}
	return nil, io.EOF
}

// isClosedPipeErr returns true if err indicates a closed pipe or file descriptor.
// This happens when cmd.Wait() closes stdout before the scanner finishes reading.
func isClosedPipeErr(err error) bool {
	return strings.Contains(err.Error(), "file already closed") ||
		strings.Contains(err.Error(), "broken pipe")
}

// Done returns a channel that closes when the process exits.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// Err returns the process exit error. Only valid after Done() is closed.
func (p *Process) Err() error {
	return p.err
}

// Close performs graceful shutdown:
// 1. Close stdin
// 2. Wait shutdownGrace for natural exit
// 3. SIGTERM the process group
// 4. Wait killGrace
// 5. SIGKILL the process group
func (p *Process) Close() error {
	// Close stdin to signal the process to exit.
	p.stdin.Close()

	select {
	case <-p.done:
		return p.err
	case <-time.After(shutdownGrace):
	}

	// SIGTERM the process group.
	p.signalGroup(syscall.SIGTERM)

	select {
	case <-p.done:
		return p.err
	case <-time.After(killGrace):
	}

	// SIGKILL the process group.
	return p.Kill()
}

// Kill immediately kills the process group.
func (p *Process) Kill() error {
	p.signalGroup(syscall.SIGKILL)
	<-p.done
	return p.err
}

// signalGroup sends a signal to the process group.
func (p *Process) signalGroup(sig syscall.Signal) {
	if p.cmd.Process == nil {
		return
	}
	// Negative PID sends to the process group.
	_ = syscall.Kill(-p.cmd.Process.Pid, sig)
}
