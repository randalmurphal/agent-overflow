// Package dictation holds recordings a client uploads in pieces while it
// records, until their transcription ends or the client abandons them.
//
// A client opens a recording, uploads each MediaRecorder piece in order as
// the recorder produces it, and finishes with the piece count it sent; the
// joined bytes are one valid file of the recorded container. Finishing
// hands the bytes to a transcription Job that still belongs to the
// recording: it counts against the caps and Cancel stops it, until Done.
//
// Everything is bounded: bytes per recording, recordings open or
// transcribing, bytes held across all of them (stored, being read and being
// transcribed), one piece read at a time per recording, and the idle time
// after which an abandoned recording is dropped by its own timer.
package dictation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// MaxRecordingBytes caps one recording. Ten minutes of speech at the 32
// kbps the client records at is about 2.4 MB; the cap leaves room for a
// recorder that ignores the requested bitrate.
const MaxRecordingBytes = 16 << 20

// MaxOpenRecordings caps recordings open or transcribing at once on this
// backend.
const MaxOpenRecordings = 8

// MaxHeldBytes caps the audio held across every recording at once: stored
// pieces, pieces being read, and recordings being transcribed.
const MaxHeldBytes = 64 << 20

// IdleTimeout drops a recording no piece has reached for this long: the
// client recording it went away without saying so. A WebKit recorder sends
// its one piece only when it stops, so this outlasts the client's
// ten-minute cap on a recording.
const IdleTimeout = 12 * time.Minute

// ErrUnknownRecording means the recording was finished, abandoned or never
// opened here.
var ErrUnknownRecording = errors.New("dictation: recording is not open")

// Recordings is the set of open recordings.
type Recordings struct {
	idle time.Duration

	mu   sync.Mutex
	open map[string]*recording
	// held is the bytes reserved across every recording: stored data,
	// pieces being read, and transcribing jobs' audio.
	held int64
}

type recording struct {
	mimeType string
	data     []byte
	next     int
	// reading is the size of the piece being read, 0 when none is.
	reading int64
	// dropped is set when the recording leaves the set; a reader still
	// holding a piece then releases its own reservation and stops.
	dropped bool
	idle    *time.Timer
	// onExpire runs, outside the lock, when the idle timer drops the
	// recording.
	onExpire func()
	// cancelJob is set while the recording is being transcribed.
	cancelJob context.CancelFunc
}

// New returns an empty set whose recordings expire after idle without a
// piece; zero means IdleTimeout.
func New(idle time.Duration) *Recordings {
	if idle <= 0 {
		idle = IdleTimeout
	}
	return &Recordings{idle: idle, open: make(map[string]*recording)}
}

// Begin opens a recording of the given container type and returns its id.
func (r *Recordings) Begin(mimeType string) (string, error) {
	if !SupportedMimeType(mimeType) {
		return "", fmt.Errorf("dictation: unsupported recording type %q", mimeType)
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("dictation: mint recording id: %w", err)
	}
	id := hex.EncodeToString(raw[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.open) >= MaxOpenRecordings {
		return "", errors.New("dictation: too many recordings in progress on this computer")
	}
	rec := &recording{mimeType: mimeType}
	rec.idle = time.AfterFunc(r.idle, func() { r.expire(id, rec) })
	r.open[id] = rec
	return id, nil
}

// OnExpire sets what runs when the idle timer drops the recording, such as
// releasing its connection's cleanup. False when the recording is gone.
func (r *Recordings) OnExpire(id string, f func()) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.open[id]
	if ok {
		rec.onExpire = f
	}
	return ok
}

// SupportedMimeType reports the containers a recording may use: the ones
// MediaRecorder produces in Chromium (WebM) and WebKit (MP4).
func SupportedMimeType(mimeType string) bool {
	return mimeType == "audio/webm" || mimeType == "audio/mp4"
}

// Admits reports whether piece seq of size bytes would be accepted now. A
// ticket is minted only for a piece the recording can take, so a client
// learns of a refusal before it uploads.
func (r *Recordings) Admits(id string, seq int, size int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.open[id]
	if !ok {
		return ErrUnknownRecording
	}
	return r.admitsLocked(rec, seq, size)
}

func (r *Recordings) admitsLocked(rec *recording, seq int, size int64) error {
	switch {
	case rec.cancelJob != nil:
		return errors.New("dictation: recording is already finished")
	case rec.reading > 0:
		return errors.New("dictation: another piece of this recording is being uploaded")
	case seq != rec.next:
		return fmt.Errorf("dictation: piece %d out of order, expected %d", seq, rec.next)
	case size <= 0:
		return errors.New("dictation: empty piece")
	case int64(len(rec.data))+size > MaxRecordingBytes:
		return errors.New("dictation: recording is too long")
	case r.held+size > MaxHeldBytes:
		return errors.New("dictation: this computer is holding too much audio; try again shortly")
	}
	return nil
}

// Append adds piece seq, size bytes read from body, to a recording. The
// piece's bytes are reserved before it is read and released only when the
// read ends, so concurrent uploads cannot pass the caps; a short, long or
// failed piece changes nothing.
func (r *Recordings) Append(id string, seq int, size int64, body io.Reader) error {
	r.mu.Lock()
	rec, ok := r.open[id]
	if !ok {
		r.mu.Unlock()
		return ErrUnknownRecording
	}
	if err := r.admitsLocked(rec, seq, size); err != nil {
		r.mu.Unlock()
		return err
	}
	rec.reading = size
	r.held += size
	r.mu.Unlock()

	piece, err := readPiece(seq, size, body)

	r.mu.Lock()
	defer r.mu.Unlock()
	rec.reading = 0
	r.held -= size
	if rec.dropped {
		return ErrUnknownRecording
	}
	if err != nil {
		return err
	}
	rec.data = append(rec.data, piece...)
	r.held += size
	rec.next++
	rec.idle.Reset(r.idle)
	return nil
}

func readPiece(seq int, size int64, body io.Reader) ([]byte, error) {
	piece := make([]byte, size)
	if _, err := io.ReadFull(body, piece); err != nil {
		return nil, fmt.Errorf("dictation: read piece %d: %w", seq, err)
	}
	// A body longer than its declared size is refused, not cut short: the
	// recording would decode as garbage from there on.
	var extra [1]byte
	if n, err := body.Read(extra[:]); n > 0 {
		return nil, fmt.Errorf("dictation: piece %d is longer than %d bytes", seq, size)
	} else if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("dictation: read piece %d: %w", seq, err)
	}
	return piece, nil
}

// Job is a finished recording being transcribed. It keeps the recording's
// place and bytes under the caps until Done, and its Context ends when the
// recording is cancelled or the caller's context ends.
type Job struct {
	MimeType string
	Audio    []byte
	Context  context.Context
	done     func()
}

// Done releases the recording. Call it once the transcription has ended;
// later calls do nothing.
func (j *Job) Done() { j.done() }

// Finish closes a recording the client sent pieces pieces of to new pieces
// and returns its transcription job. A recording that cannot be finished
// is dropped.
func (r *Recordings) Finish(ctx context.Context, id string, pieces int) (*Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.open[id]
	if !ok {
		return nil, ErrUnknownRecording
	}
	switch {
	case rec.cancelJob != nil:
		return nil, errors.New("dictation: recording is already finished")
	case rec.reading > 0:
		return nil, errors.New("dictation: a piece of this recording is still being uploaded")
	case pieces != rec.next:
		r.dropLocked(id, rec)
		return nil, fmt.Errorf("dictation: recording has %d of %d pieces", rec.next, pieces)
	case len(rec.data) == 0:
		r.dropLocked(id, rec)
		return nil, errors.New("dictation: recording is empty")
	}
	rec.idle.Stop()
	jobCtx, cancel := context.WithCancel(ctx)
	rec.cancelJob = cancel
	var once sync.Once
	return &Job{
		MimeType: rec.mimeType,
		Audio:    rec.data,
		Context:  jobCtx,
		done: func() {
			once.Do(func() {
				cancel()
				r.mu.Lock()
				defer r.mu.Unlock()
				if r.open[id] == rec {
					r.dropLocked(id, rec)
				}
			})
		},
	}, nil
}

// Cancel drops a recording, or ends its transcription's context; the job's
// Done then drops it. Cancelling one that is gone does nothing.
func (r *Recordings) Cancel(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.open[id]
	if !ok {
		return
	}
	if rec.cancelJob != nil {
		rec.cancelJob()
		return
	}
	r.dropLocked(id, rec)
}

// Open reports how many recordings are open or transcribing.
func (r *Recordings) Open() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.open)
}

// Held reports the bytes reserved across every recording.
func (r *Recordings) Held() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held
}

// expire drops rec when its idle timer fires, unless a piece is being read
// (the timer is re-armed) or it is being transcribed.
func (r *Recordings) expire(id string, rec *recording) {
	r.mu.Lock()
	if r.open[id] != rec || rec.cancelJob != nil {
		r.mu.Unlock()
		return
	}
	if rec.reading > 0 {
		rec.idle.Reset(r.idle)
		r.mu.Unlock()
		return
	}
	r.dropLocked(id, rec)
	onExpire := rec.onExpire
	r.mu.Unlock()
	if onExpire != nil {
		onExpire()
	}
}

// dropLocked removes rec and releases its stored bytes. A piece still
// being read keeps its reservation until its reader releases it.
func (r *Recordings) dropLocked(id string, rec *recording) {
	delete(r.open, id)
	rec.idle.Stop()
	rec.dropped = true
	r.held -= int64(len(rec.data))
	rec.data = nil
}
