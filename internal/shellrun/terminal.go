package shellrun

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/procutil"
)

const (
	// terminalTailBytes bounds the text of a command's terminal a client is
	// shown: a prompt and what led up to it.
	terminalTailBytes = 4 << 10
	// AnswerLimitBytes bounds one typed answer. A terminal's input line is
	// about a kilobyte on macOS, and what does not fit is dropped silently.
	AnswerLimitBytes = 512
)

// ErrAnswerRejected is returned by Answer for text the terminal would not
// take as one line. It never carries the text.
var ErrAnswerRejected = errors.New("the answer must be one line of at most 512 bytes with no control characters")

// runTerminal is a run's private terminal (procutil.Terminal) and the view of
// what the command wrote to it.
type runTerminal struct {
	device *procutil.Terminal
	view   *terminalView
}

// terminalView is what the command wrote to its terminal: a prompt, and the
// echo of an answer when the program left echo on. It is never part of the
// command's output or of the message the output becomes. Every frame carries
// the whole retained text, so the newest by sequence is the state.
type terminalView struct {
	run  *run
	emit func(Event)

	mu   sync.Mutex
	tail *procutil.TailBuffer
	seq  uint64
	// armed paces frames: the first write is sent at once, and writes that
	// follow within flushInterval go out together when the timer fires.
	armed  bool
	dirty  bool
	timer  *time.Timer
	closed bool
}

func newTerminalView(current *run, emit func(Event)) *terminalView {
	return &terminalView{run: current, emit: emit, tail: procutil.NewTailBuffer(terminalTailBytes)}
}

func (v *terminalView) Write(chunk []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || len(chunk) == 0 {
		return len(chunk), nil
	}
	_, _ = v.tail.Write(chunk)
	if v.armed {
		v.dirty = true
		return len(chunk), nil
	}
	v.emitLocked()
	v.armLocked()
	return len(chunk), nil
}

func (v *terminalView) armLocked() {
	v.armed = true
	if v.timer == nil {
		v.timer = time.AfterFunc(flushInterval, v.flushAfterInterval)
		return
	}
	v.timer.Reset(flushInterval)
}

func (v *terminalView) flushAfterInterval() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.armed = false
	if v.closed || !v.dirty {
		return
	}
	v.dirty = false
	v.emitLocked()
	v.armLocked()
}

func (v *terminalView) emitLocked() {
	v.seq++
	request := v.run.request
	v.emit(Event{
		Phase: PhaseTerminal, ThreadID: request.ThreadID, RunID: request.RunID,
		Terminal: v.textLocked(), TerminalSeq: v.seq,
	})
}

// textLocked is the retained text as a client shows it: whole runes, and
// the line ends a terminal writes as CR LF reduced to LF. Dropping invalid
// bytes drops a rune cut by the bound and one the terminal has only begun.
func (v *terminalView) textLocked() string {
	return strings.ReplaceAll(strings.ToValidUTF8(v.tail.String(), ""), "\r\n", "\n")
}

// snapshot is the text with the sequence of the last frame. It may hold more
// than that frame did; the next frame carries all of it again.
func (v *terminalView) snapshot() (string, uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.textLocked(), v.seq
}

// close ends the view: no frame follows it.
func (v *terminalView) close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.closed = true
	if v.timer != nil {
		v.timer.Stop()
	}
}

// openTerminal gives the run its terminal and starts reading it. The reader
// ends when the terminal is closed or, where a close does not wake a read
// (procutil.PollableMaster), when no process holds the terminal any more, so
// nothing waits for it.
func (s *Service) openTerminal(current *run) (*runTerminal, error) {
	device, err := s.config.OpenTerminal()
	if err != nil {
		return nil, err
	}
	terminal := &runTerminal{device: device, view: newTerminalView(current, s.config.Emit)}
	current.terminal.Store(terminal)
	go func() { _, _ = io.Copy(terminal.view, device) }()
	return terminal, nil
}

// close ends the view first, so the terminal's last bytes cannot follow the
// run's finished frame.
func (t *runTerminal) close(runID string) {
	t.view.close()
	if err := t.device.Close(); err != nil {
		log.Printf("shellrun: close the terminal of run %s: %v", runID, err)
	}
}

// Answer types one line into the running command's terminal, followed by a
// newline. It is how a person answers a prompt the command wrote there, such
// as sudo's for a password. The text reaches the command and nothing else: it
// is not output, it is not logged, and no error names it.
func (s *Service) Answer(threadID, runID, text string) error {
	if len(text) > AnswerLimitBytes || strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ErrAnswerRejected
	}
	s.mu.Lock()
	current := s.runs[threadID]
	if current == nil || current.request.RunID != runID {
		s.mu.Unlock()
		return ErrNotRunning
	}
	s.mu.Unlock()
	terminal := current.terminal.Load()
	if terminal == nil {
		return ErrNotRunning
	}
	if _, err := terminal.device.Write([]byte(text + "\n")); err != nil {
		// The terminal closes as soon as the command is gone, before the
		// run settles.
		if errors.Is(err, os.ErrClosed) {
			return ErrNotRunning
		}
		return fmt.Errorf("type into the command's terminal: %w", err)
	}
	return nil
}
