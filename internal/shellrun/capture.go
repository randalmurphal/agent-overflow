package shellrun

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/stringsx"
)

// capture is a run's single output sink: the bounded file the result is built
// from, the in-memory tail a snapshot reads, and the coalesced live frames.
// One lock orders all three, so a frame's sequence, its position on the wire
// and the snapshot that names it cannot disagree.
type capture struct {
	run  *run
	emit func(Event)

	mu       sync.Mutex
	file     *os.File
	written  int64
	dropped  int64
	writeErr error
	pending  []byte
	timer    *time.Timer
	armed    bool
	closed   bool
}

func newCapture(path string, current *run, emit func(Event)) (*capture, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &capture{run: current, emit: emit, file: file}, nil
}

func (c *capture) Write(chunk []byte) (int, error) {
	if len(chunk) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// The tail takes every byte: the snapshot reads it, and it is what a
	// capped capture ends with.
	_, _ = c.run.tail.Write(chunk)
	if c.closed {
		return len(chunk), nil
	}
	c.writeHeadLocked(chunk)
	c.pending = append(c.pending, chunk...)
	if c.dropped > 0 {
		// Past the capture cap the stream is a flood. Frames are paced by the
		// timer alone and each carries only the newest bytes.
		if len(c.pending) > flushBytes {
			newest := c.pending[len(c.pending)-flushBytes:]
			// The cut can land inside a rune; start the frame on the next one.
			for len(newest) > 0 && !utf8.RuneStart(newest[0]) {
				newest = newest[1:]
			}
			c.pending = append(c.pending[:0], newest...)
		}
		if !c.armed {
			c.armLocked()
		}
		return len(chunk), nil
	}
	if bytes.IndexByte(chunk, '\n') >= 0 || len(c.pending) >= flushBytes {
		c.flushLocked(false)
		return len(chunk), nil
	}
	// Output that never emits a newline (a progress bar redrawing on \r)
	// still has to reach the client, so a partial buffer is time-bounded:
	// the timer runs from the first buffered byte, not the latest.
	if !c.armed {
		c.armLocked()
	}
	return len(chunk), nil
}

func (c *capture) armLocked() {
	c.armed = true
	if c.timer == nil {
		c.timer = time.AfterFunc(flushInterval, c.flushAfterTimeout)
		return
	}
	c.timer.Reset(flushInterval)
}

// writeHeadLocked appends to the capture file up to CaptureLimitBytes and
// counts the rest. A write failure stops the file, not the command: the
// process keeps draining and close reports the failure.
func (c *capture) writeHeadLocked(chunk []byte) {
	if c.writeErr != nil {
		return
	}
	room := CaptureLimitBytes - c.written
	if room <= 0 {
		c.dropped += int64(len(chunk))
		return
	}
	head := chunk
	if int64(len(head)) > room {
		head = head[:room]
		c.dropped += int64(len(chunk)) - room
	}
	n, err := c.file.Write(head)
	c.written += int64(n)
	if err != nil {
		c.writeErr = err
	}
}

func (c *capture) flushAfterTimeout() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.flushLocked(false)
	}
}

// flushLocked emits what is buffered. Unless final, a rune the stream has only
// begun stays buffered so a frame never ends inside one.
func (c *capture) flushLocked(final bool) {
	if c.timer != nil {
		c.timer.Stop()
	}
	c.armed = false
	cut := len(c.pending)
	if !final {
		cut = stringsx.CompleteRunePrefix(c.pending)
	}
	if cut == 0 {
		return
	}
	chunk := string(c.pending[:cut])
	c.pending = append(c.pending[:0], c.pending[cut:]...)
	request := c.run.request
	c.emit(Event{
		Phase: PhaseOutput, ThreadID: request.ThreadID, RunID: request.RunID,
		Seq: c.run.seq.Add(1), Chunk: chunk,
	})
}

// emitted is the retained output up to the last frame, with that frame's
// sequence. Bytes buffered for the next frame are left out: the tail holds
// them already, and a snapshot that included them would deliver them twice.
func (c *capture) emitted() (seq uint64, output string, truncated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	output = c.run.tail.String()
	output = output[:max(0, len(output)-len(c.pending))]
	return c.run.seq.Load(), output, c.run.tail.Truncated()
}

// note appends a line of the app's own to the output, on a line of its own.
func (c *capture) note(line string) {
	if tail := c.run.tail.String(); tail != "" && !strings.HasSuffix(tail, "\n") {
		line = "\n" + line
	}
	_, _ = c.Write([]byte(line))
}

func (c *capture) truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped > 0
}

// close flushes the last frame and settles the file. A capped capture ends
// with an omission marker and the part of the retained tail the head does not
// already hold.
func (c *capture) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.flushLocked(true)
	c.closed = true
	err := c.writeErr
	if err == nil && c.dropped > 0 {
		tail := []byte(c.run.tail.String())
		if int64(len(tail)) > c.dropped {
			tail = tail[int64(len(tail))-c.dropped:]
		}
		omitted := c.dropped - int64(len(tail))
		marker := fmt.Sprintf("\n[... %d bytes of output omitted ...]\n", omitted)
		if omitted <= 0 {
			marker = ""
		}
		if _, werr := c.file.Write(append([]byte(marker), tail...)); werr != nil {
			err = werr
		}
	}
	if cerr := c.file.Close(); err == nil {
		err = cerr
	}
	return err
}
