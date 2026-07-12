package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"agent-overflow/internal/observability/replay"
)

// Replayer re-emits a recorded event stream (the NDJSON files
// internal/observability/replay writes: {ts, threadId, kind, data})
// onto the live event bus with original inter-event timing — the
// wire-level replay tool for reproducing streaming/rendering bugs
// frame-accurately. Pair with store.RestoreFrom of the recording's DB
// snapshot so lazy loads resolve like the original session.
//
// One replay runs at a time per Replayer; starting a new one while a
// prior is active fails (predictability beats queueing in a test
// harness).
type Replayer struct {
	// emit publishes one event to the bus. Injected so the engine stays
	// free of App/transport types.
	emit func(kind string, data json.RawMessage)
	// progress publishes replay-state events (kind "harness:replay").
	progress func(status ReplayStatus)

	mu      sync.Mutex
	status  ReplayStatus
	pause   chan struct{} // non-nil while paused; closed on resume
	stop    chan struct{} // closed to abort the run goroutine
	stepped chan struct{} // step-mode ticket channel
	// pausing wakes the run goroutine out of an inter-event gap wait so
	// a Pause() issued mid-gap holds the next emission instead of
	// racing the timer. Buffered(1): Pause never blocks, and a stale
	// signal (pause + resume before the gap wait sees it) is harmless —
	// awaitRunnable just returns immediately.
	pausing chan struct{}
	// done is closed when the run goroutine exits; Stop waits on it so
	// callers get a hard no-emissions-after-return guarantee (a reset
	// that starts deleting state while a stale run emits one last event
	// is exactly the bug this prevents).
	done chan struct{}
}

// ReplayOptions tunes one replay run.
type ReplayOptions struct {
	// Speed multiplies playback rate: 2 = twice as fast. 0 defaults
	// to 1. Applied to recorded inter-event gaps.
	Speed float64 `json:"speed,omitempty"`
	// MaxGapMs caps any single recorded gap (post-speed) so a
	// recording with a long think pause doesn't stall the replay.
	// 0 defaults to 5000; negative means uncapped.
	MaxGapMs int `json:"maxGapMs,omitempty"`
	// StartPaused begins in the paused state; each Step() then releases
	// exactly one event — the frame-by-frame mode.
	StartPaused bool `json:"startPaused,omitempty"`
	// ThreadFilter, when non-empty, drops records for other threads.
	ThreadFilter string `json:"threadFilter,omitempty"`
}

// ReplayStatus is the state surface reported to RPC callers and pushed
// as harness:replay events on every transition.
type ReplayStatus struct {
	// State: "idle", "running", "paused", "done", "stopped", "failed".
	State string `json:"state"`
	File  string `json:"file,omitempty"`
	// Position is the count of events already emitted; Total the
	// number loaded.
	Position int    `json:"position"`
	Total    int    `json:"total"`
	Error    string `json:"error,omitempty"`
}

// NewReplayer wires the two sinks.
func NewReplayer(emit func(kind string, data json.RawMessage), progress func(ReplayStatus)) *Replayer {
	return &Replayer{
		emit:     emit,
		progress: progress,
		status:   ReplayStatus{State: "idle"},
	}
}

// loadRecords parses one replay NDJSON file. Unparseable lines fail the
// load — a mid-write truncated tail is the one exception (rotation or a
// crash can cut the final line), tolerated only when the file actually
// lacks a terminating newline. A malformed but newline-complete final
// line is corruption and fails like any other line.
func loadRecords(path, threadFilter string) ([]replay.Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("harness: open recording: %w", err)
	}
	defer f.Close()

	var records []replay.Record
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	lineNo := 0
	var pendingErr error
	for scanner.Scan() {
		lineNo++
		if pendingErr != nil {
			// The malformed line wasn't the final one — real corruption.
			return nil, pendingErr
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec replay.Record
		if err := json.Unmarshal(line, &rec); err != nil {
			pendingErr = fmt.Errorf("harness: recording %s line %d: %w", path, lineNo, err)
			continue
		}
		if rec.Kind == "" {
			pendingErr = fmt.Errorf("harness: recording %s line %d: missing kind", path, lineNo)
			continue
		}
		if threadFilter != "" && rec.ThreadID != threadFilter {
			continue
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("harness: read recording: %w", err)
	}
	if pendingErr != nil {
		complete, err := endsWithNewline(f)
		if err != nil {
			return nil, fmt.Errorf("harness: inspect recording tail: %w", err)
		}
		if complete {
			return nil, pendingErr
		}
		// Truncated mid-write tail — tolerated.
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("harness: recording %s has no replayable events", path)
	}
	return records, nil
}

// endsWithNewline reports whether the file's final byte is '\n' — the
// discriminator between a complete (corrupt) final line and a
// truncated mid-write tail.
func endsWithNewline(f *os.File) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
		return false, err
	}
	return last[0] == '\n', nil
}

// ValidateRecording parses a replay NDJSON file and returns the error a
// subsequent Start would return, without touching any replayer state.
// Callers with destructive pre-replay work (stopping sessions,
// restoring a DB snapshot) preflight with this so a corrupt or empty
// recording is rejected before anything is torn down.
func ValidateRecording(path, threadFilter string) error {
	_, err := loadRecords(path, threadFilter)
	return err
}

// Start loads the recording and begins playback on a new goroutine.
func (r *Replayer) Start(path string, opts ReplayOptions) (ReplayStatus, error) {
	records, err := loadRecords(path, opts.ThreadFilter)
	if err != nil {
		return ReplayStatus{}, err
	}

	r.mu.Lock()
	if r.status.State == "running" || r.status.State == "paused" {
		st := r.status
		r.mu.Unlock()
		return st, fmt.Errorf("harness: replay already active (%s at %d/%d); stop it first", st.State, st.Position, st.Total)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	r.stop = stop
	r.done = done
	r.stepped = make(chan struct{}, 1)
	r.pausing = make(chan struct{}, 1)
	r.pause = nil
	r.status = ReplayStatus{State: "running", File: path, Total: len(records)}
	if opts.StartPaused {
		r.pause = make(chan struct{})
		r.status.State = "paused"
	}
	st := r.status
	r.mu.Unlock()
	r.progress(st)

	go func() {
		defer close(done)
		r.run(records, opts, stop)
	}()
	return st, nil
}

func (r *Replayer) run(records []replay.Record, opts ReplayOptions, stop chan struct{}) {
	speed := opts.Speed
	if speed <= 0 {
		speed = 1
	}
	maxGap := time.Duration(opts.MaxGapMs) * time.Millisecond
	if opts.MaxGapMs == 0 {
		maxGap = 5 * time.Second
	}

	for i, rec := range records {
		// Honor pause/step before the inter-event delay so Step()
		// releases exactly one emission.
		viaStep, ok := r.awaitRunnable(stop)
		if !ok {
			r.finish("stopped", i, len(records), "")
			return
		}
		// A step drives the frame directly — the recorded gap belongs
		// to free-running playback, not to operator-paced stepping.
		if i > 0 && !viaStep {
			gap := time.Duration(rec.Timestamp-records[i-1].Timestamp) * time.Millisecond
			if gap > 0 {
				gap = time.Duration(float64(gap) / speed)
				if maxGap > 0 && gap > maxGap {
					gap = maxGap
				}
				if !r.sleepUnlessPaused(gap, stop) {
					r.finish("stopped", i, len(records), "")
					return
				}
			}
		}
		r.emit(rec.Kind, rec.Data)
		r.setPosition(i + 1)
	}
	r.finish("done", len(records), len(records), "")
}

// sleepUnlessPaused waits out an inter-event gap, but re-enters the
// pause gate if Pause() lands mid-gap — a pause acknowledgement must
// guarantee no further emission until resume/step. A step during the
// pause skips the remainder of the gap (operator-paced). Returns false
// when the run must abort.
func (r *Replayer) sleepUnlessPaused(gap time.Duration, stop chan struct{}) bool {
	deadline := time.Now().Add(gap)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true
		}
		r.mu.Lock()
		pausing := r.pausing
		r.mu.Unlock()
		select {
		case <-time.After(remaining):
			return true
		case <-pausing:
			viaStep, ok := r.awaitRunnable(stop)
			if !ok {
				return false
			}
			if viaStep {
				return true
			}
			// Resumed: keep waiting out the remainder of the gap.
		case <-stop:
			return false
		}
	}
}

// awaitRunnable blocks while paused, honouring step tickets and stop.
// viaStep reports that progress was granted by a Step() ticket rather
// than free-running/resumed playback; ok=false means the run must
// abort.
func (r *Replayer) awaitRunnable(stop chan struct{}) (viaStep, ok bool) {
	for {
		r.mu.Lock()
		pause := r.pause
		stepped := r.stepped
		r.mu.Unlock()
		if pause == nil {
			return false, true
		}
		select {
		case <-pause:
			// Resumed.
		case <-stepped:
			return true, true // one event's worth of progress
		case <-stop:
			return false, false
		}
	}
}

// Pause suspends playback after the in-flight event. Effective even
// mid-gap: the run goroutine's inter-event wait watches for it.
func (r *Replayer) Pause() ReplayStatus {
	r.mu.Lock()
	if r.status.State == "running" && r.pause == nil {
		r.pause = make(chan struct{})
		r.status.State = "paused"
		select {
		case r.pausing <- struct{}{}:
		default: // a wake-up is already pending
		}
	}
	st := r.status
	r.mu.Unlock()
	r.progress(st)
	return st
}

// Resume continues playback.
func (r *Replayer) Resume() ReplayStatus {
	r.mu.Lock()
	if r.status.State == "paused" && r.pause != nil {
		close(r.pause)
		r.pause = nil
		r.status.State = "running"
	}
	st := r.status
	r.mu.Unlock()
	r.progress(st)
	return st
}

// Step releases exactly one event while paused. Success means this
// call's ticket was accepted; a still-unconsumed prior ticket is an
// error so frame-by-frame drivers can't silently lose steps.
func (r *Replayer) Step() (ReplayStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.State != "paused" {
		return r.status, fmt.Errorf("harness: step requires a paused replay (state %s)", r.status.State)
	}
	select {
	case r.stepped <- struct{}{}:
	default:
		return r.status, fmt.Errorf("harness: a step is already pending; wait for the position to advance before stepping again")
	}
	return r.status, nil
}

// Stop aborts the active replay and waits for the run goroutine to
// exit — when Stop returns, no further event will be emitted and the
// status is terminal. Idempotent: a Stop racing another Stop (or a
// natural finish) waits on the same done channel and returns the same
// terminal status.
func (r *Replayer) Stop() ReplayStatus {
	r.mu.Lock()
	if r.stop != nil && (r.status.State == "running" || r.status.State == "paused") {
		close(r.stop)
		r.stop = nil
	}
	done := r.done
	r.mu.Unlock()
	if done != nil {
		<-done
	}
	return r.Status()
}

// Status reports the current state.
func (r *Replayer) Status() ReplayStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *Replayer) setPosition(pos int) {
	r.mu.Lock()
	r.status.Position = pos
	r.mu.Unlock()
}

func (r *Replayer) finish(state string, pos, total int, errMsg string) {
	r.mu.Lock()
	r.status.State = state
	r.status.Position = pos
	r.status.Total = total
	r.status.Error = errMsg
	r.stop = nil
	r.pause = nil
	st := r.status
	r.mu.Unlock()
	r.progress(st)
}
