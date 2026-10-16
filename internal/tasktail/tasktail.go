// Package tasktail follows the output file of a running background task
// for the clients that have its tray row expanded, and streams what the
// file gains to them.
//
// One follow per (thread, launch), shared by every watch on it; a follow
// exists only while a watch does, and ends when its task does
// (EndLaunch). The follow keeps the last TailBytes of the file in memory
// and nothing else: 64 KiB is some eight hundred lines of eighty columns,
// well past what the tray's clipped body can show and a bound on the
// text each append makes the client re-diff; the completion row carries
// the whole file once the task ends.
//
// The file is polled: one fstat on the open descriptor per Interval, a
// pread of the new bytes only when the size moved. That is the idle cost
// per expanded task on every platform the backend runs on (Linux, macOS,
// WSL), with no watcher descriptor to own, and no notification API to
// differ by filesystem. A file that shrank is read again from its start;
// one that grew by more than TailBytes between polls is read from
// TailBytes before its end, and the tail restarts at a line boundary.
//
// Reads go through the Resolve guard the caller supplies, the same
// containment rule the completion read applies to the file, so a watch
// never reads outside the roots the CLI writes task output to.
package tasktail

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	// DefaultTailBytes is the tail kept per follow.
	DefaultTailBytes = 64 << 10
	// DefaultInterval is the poll cadence.
	DefaultInterval = 250 * time.Millisecond
	// DefaultMaxWatches bounds the watches alive at once across every
	// connection; a tray has a handful of rows and each expanded one
	// costs a watch.
	DefaultMaxWatches = 64
)

// ErrTooManyWatches is returned by Watch past MaxWatches.
var ErrTooManyWatches = errors.New("tasktail: too many background task output watches")

// ErrClosed is returned by Watch after Close.
var ErrClosed = errors.New("tasktail: closed")

// ErrWatchExists is returned by Watch for a watch id already held.
var ErrWatchExists = errors.New("tasktail: watch id already held")

// Event is one frame of a follow (provider:background_task_output).
// Append is the text the tail gained since the last frame, or with Reset
// the whole tail after it restarted. Truncated says the tail holds less
// than the file. Ended says the task ended or the file became unreadable
// (Error), and no later frame follows for this launch.
type Event struct {
	ThreadID  string `json:"threadId"`
	LaunchID  string `json:"launchId"`
	Seq       uint64 `json:"seq"`
	Append    string `json:"append,omitempty"`
	Reset     bool   `json:"reset,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Size      int64  `json:"size"`
	Ended     bool   `json:"ended,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Snapshot answers Watch: the tail as it stands and the Seq the next
// frame continues from. A watch on a task that already ended carries no
// WatchID and Ended.
type Snapshot struct {
	WatchID   string `json:"watchId"`
	Seq       uint64 `json:"seq"`
	Data      string `json:"data"`
	Truncated bool   `json:"truncated"`
	Size      int64  `json:"size"`
	Ended     bool   `json:"ended"`
	Error     string `json:"error,omitempty"`
}

// Config wires a Tailer. Emit receives every frame; Resolve canonicalises
// an output path and refuses one outside the allowed roots, returning an
// error that satisfies errors.Is(err, fs.ErrNotExist) while the file is
// not yet written. Dropped, when set, receives the ids of the watches the
// tailer let go of on its own (the task ended, the read failed, Close),
// never one a Release ended; the caller releases what it bound to them.
// Called with no tailer lock held. Zero durations and sizes take the
// defaults.
type Config struct {
	Emit       func(Event)
	Resolve    func(path string) (string, error)
	Dropped    func(watchIDs []string)
	Interval   time.Duration
	TailBytes  int
	MaxWatches int
}

// Stats counts the file operations the tailer made, for measuring the
// cost of a follow.
type Stats struct {
	Stats     int64
	Reads     int64
	BytesRead int64
}

type followKey struct{ thread, launch string }

// watchRef is a live watch: the follow it holds and the owner that may
// release it. The owner is the connection that opened the watch, so a
// watch id in another connection's hands releases nothing.
type watchRef struct {
	key   followKey
	owner string
}

// Tailer owns the follows and the goroutine that polls them.
type Tailer struct {
	cfg Config

	mu      sync.Mutex
	follows map[followKey]*follow
	watches map[string]watchRef
	closed  bool

	// beforeSeed, when set by a test, runs between a watch's registration
	// and its seed read, where another Watch can interleave.
	beforeSeed func()

	stop chan struct{}
	done chan struct{}

	stats     atomic.Int64
	reads     atomic.Int64
	bytesRead atomic.Int64
}

type follow struct {
	key  followKey
	path string

	mu        sync.Mutex
	file      *os.File
	offset    int64
	size      int64
	tail      []byte
	truncated bool
	seq       uint64
	watchers  map[string]struct{}
	ended     bool
	err       string
	// dropped says the follow left the tailer's maps (its last watch
	// released, its launch ended, or its read failed); a poll that still
	// holds it reads nothing more.
	dropped bool
}

// New starts a Tailer. Close it to stop polling.
func New(cfg Config) *Tailer {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.TailBytes <= 0 {
		cfg.TailBytes = DefaultTailBytes
	}
	if cfg.MaxWatches <= 0 {
		cfg.MaxWatches = DefaultMaxWatches
	}
	if cfg.Emit == nil {
		cfg.Emit = func(Event) {}
	}
	if cfg.Resolve == nil {
		cfg.Resolve = func(path string) (string, error) { return path, nil }
	}
	if cfg.Dropped == nil {
		cfg.Dropped = func([]string) {}
	}
	t := &Tailer{
		cfg:     cfg,
		follows: make(map[followKey]*follow),
		watches: make(map[string]watchRef),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go t.loop()
	return t
}

// Watch adds a watch on launchID's output file at path for owner and
// answers with the tail as it stands. The watch lives until Release by
// the same owner, EndLaunch or Close. A file not yet written answers an
// empty tail and is picked up when it appears. A seed read that fails
// answers Ended with the error and holds no watch.
//
// watchID names the watch; empty mints one (NewWatchID). A caller that
// binds a release to the id passes one it minted itself, so its record
// exists before the tailer can drop the watch and report it (Dropped).
func (t *Tailer) Watch(threadID, launchID, path, owner, watchID string) (Snapshot, error) {
	key := followKey{thread: threadID, launch: launchID}
	if watchID == "" {
		minted, err := NewWatchID()
		if err != nil {
			return Snapshot{}, err
		}
		watchID = minted
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return Snapshot{}, ErrClosed
	}
	if len(t.watches) >= t.cfg.MaxWatches {
		t.mu.Unlock()
		return Snapshot{}, ErrTooManyWatches
	}
	if _, held := t.watches[watchID]; held {
		t.mu.Unlock()
		return Snapshot{}, ErrWatchExists
	}
	f := t.follows[key]
	if f == nil {
		f = &follow{key: key, path: path, watchers: make(map[string]struct{})}
		t.follows[key] = f
	}
	t.watches[watchID] = watchRef{key: key, owner: owner}
	f.mu.Lock()
	f.watchers[watchID] = struct{}{}
	f.mu.Unlock()
	t.mu.Unlock()

	if t.beforeSeed != nil {
		t.beforeSeed()
	}
	// Seed the follow before answering, so the snapshot carries the tail
	// as it stands rather than waiting a poll for it. Whether the read is
	// anyone's frame is decided here, under the follow's lock: a watch
	// alone on the follow gets its snapshot and no frame; with another
	// watch registered, what the file gained is that watch's frame, which
	// this snapshot also carries (its seq is the frame's, so the new
	// watcher drops it as already seen). Two first watches that overlap
	// each read with the other registered, so neither read is silent.
	f.mu.Lock()
	t.pollLocked(f, len(f.watchers) > 1)
	snapshot := Snapshot{
		WatchID:   watchID,
		Seq:       f.seq,
		Data:      string(f.tail),
		Truncated: f.truncated,
		Size:      f.size,
		Ended:     f.ended,
		Error:     f.err,
	}
	ended := f.ended
	f.mu.Unlock()
	if ended {
		t.dropEnded(f)
		snapshot.WatchID = ""
	}
	return snapshot, nil
}

// NewWatchID mints an id a client cannot guess: the id is the only handle
// a release names, and a counter would let one connection release
// another's watch.
func NewWatchID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("tasktail: watch id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Release ends one watch, if owner is the one that opened it and the
// watch is on threadID, and reports whether it did. The follow ends with
// its last watch. Unknown ids and another owner's ids are no-ops: the
// connection cleanup and the client's own release may both name the same
// watch, and a guessed id must change nothing.
func (t *Tailer) Release(threadID, watchID, owner string) bool {
	t.mu.Lock()
	ref, ok := t.watches[watchID]
	if !ok || ref.owner != owner || ref.key.thread != threadID {
		t.mu.Unlock()
		return false
	}
	delete(t.watches, watchID)
	f := t.follows[ref.key]
	var drop bool
	if f != nil {
		f.mu.Lock()
		delete(f.watchers, watchID)
		drop = len(f.watchers) == 0
		if drop {
			// Marked before the maps let go of it: a poll that already
			// holds the follow must not reopen the file it is closing.
			f.dropped = true
			delete(t.follows, ref.key)
		}
		f.mu.Unlock()
	}
	t.mu.Unlock()
	if drop {
		f.mu.Lock()
		f.closeFileLocked()
		f.mu.Unlock()
	}
	return true
}

// dropEnded removes a follow whose read failed, with every watch on it,
// so the ids do not count against the cap until their clients release
// them. Called with no lock held.
func (t *Tailer) dropEnded(f *follow) {
	t.mu.Lock()
	if t.follows[f.key] != f {
		t.mu.Unlock()
		return
	}
	f.mu.Lock()
	if !f.ended {
		f.mu.Unlock()
		t.mu.Unlock()
		return
	}
	dropped := make([]string, 0, len(f.watchers))
	for watchID := range f.watchers {
		delete(t.watches, watchID)
		dropped = append(dropped, watchID)
	}
	f.dropped = true
	f.mu.Unlock()
	delete(t.follows, f.key)
	t.mu.Unlock()
	if len(dropped) > 0 {
		t.cfg.Dropped(dropped)
	}
}

// EndLaunch ends the follow of launchID: a last read of the file, then
// the Ended frame, then the follow and its watches are dropped. Called
// when the task's end is announced; a launch nobody follows is a no-op.
func (t *Tailer) EndLaunch(threadID, launchID string) {
	key := followKey{thread: threadID, launch: launchID}
	t.mu.Lock()
	f := t.follows[key]
	if f == nil {
		t.mu.Unlock()
		return
	}
	delete(t.follows, key)
	f.mu.Lock()
	dropped := make([]string, 0, len(f.watchers))
	for watchID := range f.watchers {
		delete(t.watches, watchID)
		dropped = append(dropped, watchID)
	}
	f.mu.Unlock()
	t.mu.Unlock()

	f.mu.Lock()
	if !f.ended && !f.dropped {
		t.pollLocked(f, true)
	}
	f.dropped = true
	if !f.ended {
		f.ended = true
		f.seq++
		t.cfg.Emit(Event{
			ThreadID:  key.thread,
			LaunchID:  key.launch,
			Seq:       f.seq,
			Truncated: f.truncated,
			Size:      f.size,
			Ended:     true,
		})
	}
	f.closeFileLocked()
	f.mu.Unlock()
	if len(dropped) > 0 {
		t.cfg.Dropped(dropped)
	}
}

// Close stops polling, joins the poll goroutine and closes every file.
func (t *Tailer) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	close(t.stop)
	follows := make([]*follow, 0, len(t.follows))
	for _, f := range t.follows {
		follows = append(follows, f)
	}
	dropped := make([]string, 0, len(t.watches))
	for watchID := range t.watches {
		dropped = append(dropped, watchID)
	}
	t.follows = make(map[followKey]*follow)
	t.watches = make(map[string]watchRef)
	t.mu.Unlock()
	<-t.done
	for _, f := range follows {
		f.mu.Lock()
		f.dropped = true
		f.closeFileLocked()
		f.mu.Unlock()
	}
	if len(dropped) > 0 {
		t.cfg.Dropped(dropped)
	}
}

// Stats reports the file operations made so far.
func (t *Tailer) Stats() Stats {
	return Stats{Stats: t.stats.Load(), Reads: t.reads.Load(), BytesRead: t.bytesRead.Load()}
}

// Watching reports how many watches are alive.
func (t *Tailer) Watching() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.watches)
}

func (t *Tailer) loop() {
	defer close(t.done)
	ticker := time.NewTicker(t.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
			t.pollAll()
		}
	}
}

// Poll polls every follow now. Tests use it in place of the ticker.
func (t *Tailer) Poll() {
	t.pollAll()
}

func (t *Tailer) pollAll() {
	t.mu.Lock()
	follows := make([]*follow, 0, len(t.follows))
	for _, f := range t.follows {
		follows = append(follows, f)
	}
	t.mu.Unlock()
	var failed []*follow
	for _, f := range follows {
		f.mu.Lock()
		t.pollLocked(f, true)
		if f.ended {
			failed = append(failed, f)
		}
		f.mu.Unlock()
	}
	for _, f := range failed {
		t.dropEnded(f)
	}
}

// pollLocked reads what the file gained since the last poll. With emit
// false (the seed read) the frame is left to the caller's snapshot.
func (t *Tailer) pollLocked(f *follow, emit bool) {
	if f.ended || f.dropped {
		return
	}
	if f.file == nil {
		resolved, err := t.cfg.Resolve(f.path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return
			}
			t.failLocked(f, err, emit)
			return
		}
		file, err := os.Open(resolved)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return
			}
			t.failLocked(f, err, emit)
			return
		}
		f.file = file
		f.offset = 0
	}
	t.stats.Add(1)
	info, err := f.file.Stat()
	if err != nil {
		t.failLocked(f, err, emit)
		return
	}
	size := info.Size()
	reset := false
	if size < f.offset {
		// The file was truncated or replaced under the task: read it
		// again from its start.
		f.offset = 0
		f.tail = nil
		f.truncated = false
		reset = true
	}
	if size == f.offset {
		if reset {
			f.size = size
			t.emitLocked(f, emit, "", true)
		}
		return
	}
	start := f.offset
	if size-start > int64(t.cfg.TailBytes) {
		// More arrived than the tail holds: skip to the last TailBytes
		// and restart the tail there.
		start = size - int64(t.cfg.TailBytes)
		f.tail = nil
		f.truncated = true
		reset = true
	}
	buf := make([]byte, size-start)
	n, err := io.ReadFull(io.NewSectionReader(f.file, start, size-start), buf)
	t.reads.Add(1)
	t.bytesRead.Add(int64(n))
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.failLocked(f, err, emit)
		return
	}
	chunk := buf[:n]
	// A multi-byte character cut by the read waits for its remaining
	// bytes; the frame carries whole characters only.
	held := incompleteRuneTail(chunk)
	chunk = chunk[:len(chunk)-held]
	f.offset = start + int64(len(chunk))
	f.size = size
	if reset && f.truncated {
		chunk = afterFirstNewline(chunk)
	}
	if len(chunk) == 0 && !reset {
		return
	}
	f.tail = append(f.tail, chunk...)
	if len(f.tail) > t.cfg.TailBytes {
		f.tail = boundTail(f.tail, t.cfg.TailBytes)
		f.truncated = true
	}
	if reset {
		t.emitLocked(f, emit, string(f.tail), true)
		return
	}
	t.emitLocked(f, emit, string(chunk), false)
}

func (t *Tailer) emitLocked(f *follow, emit bool, text string, reset bool) {
	if !emit {
		return
	}
	f.seq++
	t.cfg.Emit(Event{
		ThreadID:  f.key.thread,
		LaunchID:  f.key.launch,
		Seq:       f.seq,
		Append:    text,
		Reset:     reset,
		Truncated: f.truncated,
		Size:      f.size,
	})
}

func (t *Tailer) failLocked(f *follow, err error, emit bool) {
	f.ended = true
	f.err = fmt.Sprintf("read task output %s: %v", f.path, err)
	f.closeFileLocked()
	if !emit {
		return
	}
	f.seq++
	t.cfg.Emit(Event{
		ThreadID:  f.key.thread,
		LaunchID:  f.key.launch,
		Seq:       f.seq,
		Truncated: f.truncated,
		Size:      f.size,
		Ended:     true,
		Error:     f.err,
	})
}

func (f *follow) closeFileLocked() {
	if f.file != nil {
		_ = f.file.Close()
		f.file = nil
	}
}

// boundTail keeps the last limit bytes of tail, from the line after the
// cut when the cut lands inside a line (else from the character the cut
// lands in), in a fresh slice so the dropped head is released.
func boundTail(tail []byte, limit int) []byte {
	cut := len(tail) - limit
	if nl := bytes.IndexByte(tail[cut:], '\n'); nl >= 0 && cut+nl+1 < len(tail) {
		cut += nl + 1
	} else {
		cut = runeStart(tail, cut)
	}
	out := make([]byte, len(tail)-cut)
	copy(out, tail[cut:])
	return out
}

// afterFirstNewline drops a partial first line from a chunk read from
// inside the file; a chunk without a newline is kept from its first
// whole character.
func afterFirstNewline(chunk []byte) []byte {
	if nl := bytes.IndexByte(chunk, '\n'); nl >= 0 && nl+1 < len(chunk) {
		return chunk[nl+1:]
	}
	return chunk[runeStart(chunk, 0):]
}

// runeStart is the first index at or after i that begins a UTF-8
// sequence, so a byte cut never lands inside a character.
func runeStart(b []byte, i int) int {
	for i < len(b) && !utf8.RuneStart(b[i]) {
		i++
	}
	return i
}

// incompleteRuneTail reports how many trailing bytes of b begin a UTF-8
// sequence the chunk does not complete.
func incompleteRuneTail(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(b[i]) {
			continue
		}
		if b[i] < utf8.RuneSelf {
			return 0
		}
		if utf8.FullRune(b[i:]) {
			return 0
		}
		return len(b) - i
	}
	return 0
}
