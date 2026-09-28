package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Chunk bounds for Diff.Read. The ceiling keeps one chunk's JSON frame
// under the transport's frame limit even when every byte needs a six-byte
// escape; the floor keeps a caller from splitting a large diff into
// millions of reads.
const (
	MinChunkBytes = 64 << 10
	MaxChunkBytes = 8 << 20
)

// streamIdleTimeout ends a git process nobody has read from for this long.
// The process only spares the next sequential read a restart, and a paused
// one can hold a large file's blobs in git's memory.
var streamIdleTimeout = 30 * time.Second

// maxStreamStderrBytes bounds what a streaming git keeps of its stderr for
// the error message.
const maxStreamStderrBytes = 64 << 10

var (
	// ErrDiffChanged is returned when re-running a diff's git command
	// produces different bytes than the reads it already served: the
	// endpoints are fixed, so only git configuration or a damaged object
	// store can cause it, and the caller has to open the diff again.
	ErrDiffChanged = errors.New("gitdiff: the diff changed since it was opened")
	// ErrDiffClosed is returned by a read on a closed Diff.
	ErrDiffClosed = errors.New("gitdiff: diff is closed")
)

// Chunk is the patch bytes [Offset, NextOffset) of a Diff. EOF reports
// that NextOffset is the end of the patch.
type Chunk struct {
	Data       string `json:"data"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	EOF        bool   `json:"eof"`
}

// Diff is a patch between fixed endpoints, read in chunks by byte offset.
// Nothing holds the patch: a sequential read continues one git process,
// and a read anywhere else re-runs git and skips to the offset, verifying
// the skipped bytes against the reads that first returned them. A worktree
// diff also holds its snapshot's temporary objects until Close.
type Diff struct {
	workspace string
	env       []string
	args      []string
	release   func() error

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	seed   maphash.Seed
	// marks holds, for every offset a read has ended at, the hash of the
	// patch bytes before it. Only these offsets can start a read, which is
	// what makes every re-read verifiable.
	marks map[int64]uint64
	// size is the patch length once a read has reached the end, else -1.
	size    int64
	live    *diffStream
	idle    *time.Timer
	idleGen uint64
}

func newDiff(workspace string, env []string, release func() error, args []string) *Diff {
	ctx, cancel := context.WithCancel(context.Background())
	return &Diff{
		workspace: workspace,
		env:       env,
		args:      args,
		release:   release,
		ctx:       ctx,
		cancel:    cancel,
		seed:      maphash.MakeSeed(),
		marks:     map[int64]uint64{},
		size:      -1,
	}
}

// Read returns up to maxBytes of the patch from offset, which must be 0 or
// a NextOffset an earlier read returned. A chunk ends after its last
// newline; a line longer than the chunk splits at a UTF-8 character
// boundary. Reads with the same offset and maxBytes return the same chunk.
func (d *Diff) Read(ctx context.Context, offset int64, maxBytes int) (Chunk, error) {
	maxBytes = min(max(maxBytes, MinChunkBytes), MaxChunkBytes)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return Chunk{}, ErrDiffClosed
	}
	want, known := d.marks[offset]
	if offset != 0 && !known {
		return Chunk{}, fmt.Errorf("gitdiff: offset %d is not a chunk boundary", offset)
	}
	if offset == d.size {
		return Chunk{Offset: offset, NextOffset: offset, EOF: true}, nil
	}
	d.idleGen++
	if d.live == nil || d.live.pos != offset {
		d.endLive()
		stream, err := d.start()
		if err != nil {
			if d.ctx.Err() != nil {
				return Chunk{}, ErrDiffClosed
			}
			return Chunk{}, err
		}
		d.live = stream
	}
	stream := d.live
	stop := context.AfterFunc(ctx, stream.cancel)
	defer stop()
	fail := func(err error) (Chunk, error) {
		d.endLive()
		switch {
		case ctx.Err() != nil:
			return Chunk{}, ctx.Err()
		case d.ctx.Err() != nil:
			return Chunk{}, ErrDiffClosed
		}
		return Chunk{}, err
	}
	if stream.pos < offset {
		if err := stream.skip(offset); err != nil {
			return fail(err)
		}
		if stream.hash.Sum64() != want {
			return fail(ErrDiffChanged)
		}
	}
	data, eof, err := stream.next(maxBytes)
	if err != nil {
		return fail(err)
	}
	next := stream.pos
	sum := stream.hash.Sum64()
	if prev, ok := d.marks[next]; ok && prev != sum {
		return fail(ErrDiffChanged)
	}
	if eof && d.size >= 0 && next != d.size {
		return fail(ErrDiffChanged)
	}
	d.marks[next] = sum
	if eof {
		d.size = next
		d.endLive()
	} else {
		d.armIdle(stream)
	}
	return Chunk{Data: data, Offset: offset, NextOffset: next, EOF: eof}, nil
}

// Close ends any running git process and removes the diff's snapshot. It
// interrupts a read in progress rather than waiting for git to produce its
// next bytes. Closing twice is a no-op.
func (d *Diff) Close() error {
	d.cancel()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	if d.idle != nil {
		d.idle.Stop()
		d.idle = nil
	}
	d.endLive()
	if d.release == nil {
		return nil
	}
	return d.release()
}

func (d *Diff) start() (*diffStream, error) {
	ctx, cancel := context.WithCancel(d.ctx)
	cmd := exec.CommandContext(ctx, "git", d.args...)
	cmd.Dir = d.workspace
	cmd.Env = gitEnv(d.env)
	cmd.WaitDelay = gitPipeWaitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gitdiff: git %s: stdout pipe: %w", strings.Join(d.args, " "), err)
	}
	stderr := &boundedBuffer{limit: maxStreamStderrBytes}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("gitdiff: git %s: start: %w", strings.Join(d.args, " "), err)
	}
	stream := &diffStream{args: d.args, cmd: cmd, cancel: cancel, stdout: stdout, stderr: stderr}
	stream.hash.SetSeed(d.seed)
	return stream, nil
}

// endLive stops the running git process, if any. Caller holds d.mu.
func (d *Diff) endLive() {
	if d.live != nil {
		d.live.stop()
		d.live = nil
	}
}

// armIdle schedules stream's end if no read continues it in time. Caller
// holds d.mu. A read bumps idleGen first, so a timer that fires while that
// read waits for the lock sees a newer generation and leaves the stream.
func (d *Diff) armIdle(stream *diffStream) {
	if d.idle != nil {
		d.idle.Stop()
	}
	gen := d.idleGen
	d.idle = time.AfterFunc(streamIdleTimeout, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.idleGen == gen && d.live == stream {
			d.endLive()
		}
	})
}

// diffStream is one running git process and how far its output has been
// read. pos counts bytes returned to readers; buf holds bytes read from
// the pipe but not yet returned.
type diffStream struct {
	args   []string
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stdout io.ReadCloser
	stderr *boundedBuffer
	hash   maphash.Hash
	pos    int64
	buf    []byte
	ended  bool
	waited bool
}

// skip discards and hashes output until pos reaches offset.
func (s *diffStream) skip(offset int64) error {
	n, err := io.CopyN(&s.hash, s.stdout, offset-s.pos)
	s.pos += n
	if errors.Is(err, io.EOF) {
		if err := s.finish(); err != nil {
			return err
		}
		return ErrDiffChanged
	}
	if err != nil {
		return fmt.Errorf("gitdiff: read git output: %w", err)
	}
	return nil
}

// next returns up to maxBytes of output from pos and whether it reaches
// the end of the patch.
func (s *diffStream) next(maxBytes int) (string, bool, error) {
	// One byte past the window tells a full window at the end of the
	// output apart from one with more to come.
	if err := s.fill(maxBytes + 1); err != nil {
		return "", false, err
	}
	window := s.buf
	eof := s.ended && len(window) <= maxBytes
	if !eof {
		window = window[:ChunkCut(window[:maxBytes])]
	}
	data := string(window)
	_, _ = s.hash.Write(window)
	s.pos += int64(len(window))
	s.buf = s.buf[:copy(s.buf, s.buf[len(window):])]
	return data, eof, nil
}

// fill reads from the pipe until buf holds n bytes or the output ends.
func (s *diffStream) fill(n int) error {
	if cap(s.buf) < n {
		grown := make([]byte, len(s.buf), n)
		copy(grown, s.buf)
		s.buf = grown
	}
	for len(s.buf) < n && !s.ended {
		read, err := s.stdout.Read(s.buf[len(s.buf):n])
		s.buf = s.buf[:len(s.buf)+read]
		if errors.Is(err, io.EOF) {
			return s.finish()
		}
		if err != nil {
			return fmt.Errorf("gitdiff: read git output: %w", err)
		}
	}
	return nil
}

// finish reaps git after its output ended and reports a failed exit, so a
// patch cut short by an error is never served as complete.
func (s *diffStream) finish() error {
	s.ended = true
	s.waited = true
	err := s.cmd.Wait()
	s.cancel()
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return fmt.Errorf("gitdiff: git %s: output pipes did not close before wait delay: %w",
			strings.Join(s.args, " "), err)
	}
	code := -1
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		code = exitErr.ExitCode()
	}
	return fmt.Errorf("gitdiff: git %s: exit=%d: %s",
		strings.Join(s.args, " "), code, strings.TrimSpace(s.stderr.String()))
}

// stop kills git if it is still running and reaps it. The exit status of
// a process stopped on purpose carries no information.
func (s *diffStream) stop() {
	s.cancel()
	if !s.waited {
		s.waited = true
		_ = s.cmd.Wait()
	}
}

// ChunkCut returns where a full window ends: after its last newline, or,
// for a window inside one long line, before a trailing incomplete UTF-8
// sequence so the split falls between characters. Every review diff reader
// cuts its chunks here.
func ChunkCut(window []byte) int {
	for i := len(window) - 1; i >= 0; i-- {
		if window[i] == '\n' {
			return i + 1
		}
	}
	for back := 1; back <= utf8.UTFMax && back < len(window); back++ {
		start := len(window) - back
		if utf8.RuneStart(window[start]) {
			if !utf8.FullRune(window[start:]) {
				return start
			}
			break
		}
	}
	return len(window)
}

// boundedBuffer keeps the first limit bytes written to it and drops the
// rest, so a chatty git cannot grow the backend's memory.
type boundedBuffer struct {
	limit int
	buf   []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return string(b.buf) }
