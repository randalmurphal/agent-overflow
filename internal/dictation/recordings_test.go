package dictation

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

func TestRecordingJoinsItsPiecesInOrder(t *testing.T) {
	recordings := New(0, 0)
	id, err := recordings.Begin("audio/webm", 0)
	if err != nil {
		t.Fatal(err)
	}
	for seq, piece := range []string{"header+", "frames-1+", "frames-2"} {
		if err := recordings.Append(id, seq, int64(len(piece)), strings.NewReader(piece)); err != nil {
			t.Fatalf("piece %d: %v", seq, err)
		}
	}
	job, err := recordings.Finish(context.Background(), id, 3)
	if err != nil || job.MimeType != "audio/webm" || string(job.Audio) != "header+frames-1+frames-2" {
		t.Fatalf("Finish = %+v, %v", job, err)
	}
	if _, err := recordings.Finish(context.Background(), id, 3); err == nil {
		t.Fatal("a recording being transcribed was finished again")
	}
	job.Done()
	job.Done()
	if _, err := recordings.Finish(context.Background(), id, 3); !errors.Is(err, ErrUnknownRecording) {
		t.Fatalf("Finish after Done = %v, want ErrUnknownRecording", err)
	}
	if held := recordings.Held(); held != 0 {
		t.Fatalf("bytes held after Done = %d", held)
	}
}

func TestRecordingRefusesWhatWouldCorruptIt(t *testing.T) {
	recordings := New(0, 0)
	if _, err := recordings.Begin("audio/wav", 0); err == nil {
		t.Fatal("a container MediaRecorder does not produce was accepted")
	}
	id, err := recordings.Begin("audio/mp4", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordings.Append(id, 1, 1, strings.NewReader("x")); err == nil {
		t.Fatal("a piece out of order was accepted")
	}
	if err := recordings.Append(id, 0, 4, strings.NewReader("ab")); err == nil {
		t.Fatal("a short piece was accepted")
	}
	if err := recordings.Append(id, 0, 2, strings.NewReader("abc")); err == nil {
		t.Fatal("a piece longer than declared was accepted")
	}
	if err := recordings.Admits(id, 0, MaxRecordingBytes+1); err == nil {
		t.Fatal("a piece past the size cap was admitted")
	}
	if err := recordings.Append(id, 0, 2, strings.NewReader("ab")); err != nil {
		t.Fatalf("the refused pieces changed the recording: %v", err)
	}
	if held := recordings.Held(); held != 2 {
		t.Fatalf("bytes held = %d, want only the accepted piece", held)
	}
	if _, err := recordings.Finish(context.Background(), id, 2); err == nil {
		t.Fatal("a recording missing a piece was finished")
	}
	if recordings.Open() != 0 || recordings.Held() != 0 {
		t.Fatal("a recording that could not be finished was kept")
	}
}

func TestRecordingsAreBounded(t *testing.T) {
	recordings := New(0, 0)
	var ids []string
	for range MaxOpenRecordings {
		id, err := recordings.Begin("audio/webm", 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := recordings.Begin("audio/webm", 0); err == nil {
		t.Fatal("a recording past the open cap was begun")
	}
	recordings.Cancel(ids[0])
	if _, err := recordings.Begin("audio/webm", 0); err != nil {
		t.Fatalf("a cancelled recording still counts: %v", err)
	}
}

// blockedBody is an upload body whose bytes arrive only when released.
type blockedBody struct {
	release chan struct{}
	data    io.Reader
}

func (b *blockedBody) Read(p []byte) (int, error) {
	<-b.release
	return b.data.Read(p)
}

// No declared size, however large, wraps a cap's sum into admission.
func TestNoPieceSizeOverflowsTheCaps(t *testing.T) {
	recordings := New(0, 0)
	id, _ := recordings.Begin("audio/webm", 0)
	if err := recordings.Append(id, 0, 1, strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{math.MaxInt64, math.MaxInt64 - 1, MaxHeldBytes} {
		if err := recordings.Admits(id, 1, size); err == nil {
			t.Fatalf("a piece of %d bytes was admitted", size)
		}
		if err := recordings.Append(id, 1, size, strings.NewReader("b")); err == nil {
			t.Fatalf("a piece of %d bytes was accepted", size)
		}
	}
	if held := recordings.Held(); held != 1 {
		t.Fatalf("bytes held = %d, want the one stored byte", held)
	}
}

// A recording's release hook runs once when it leaves the set, however it
// leaves, and not while a refused finish leaves it with its owner.
func TestARecordingSaysWhenItIsReleased(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, r *Recordings, id string)
	}{
		{"done", func(t *testing.T, r *Recordings, id string) {
			job, err := r.Finish(context.Background(), id, 1)
			if err != nil {
				t.Fatal(err)
			}
			job.Done()
		}},
		{"cancelled", func(_ *testing.T, r *Recordings, id string) { r.Cancel(id) }},
		{"finished short of its pieces", func(_ *testing.T, r *Recordings, id string) { _, _ = r.Finish(context.Background(), id, 2) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recordings := New(0, 0)
			id, _ := recordings.Begin("audio/webm", 0)
			if err := recordings.Append(id, 0, 3, strings.NewReader("abc")); err != nil {
				t.Fatal(err)
			}
			released := 0
			recordings.OnRelease(id, func() { released++ })
			tc.run(t, recordings, id)
			if released != 1 || recordings.Open() != 0 {
				t.Fatalf("released %d times, %d open; want once and none", released, recordings.Open())
			}
		})
	}

	t.Run("finished again while transcribing", func(t *testing.T) {
		recordings := New(0, 0)
		id, _ := recordings.Begin("audio/webm", 0)
		if err := recordings.Append(id, 0, 3, strings.NewReader("abc")); err != nil {
			t.Fatal(err)
		}
		released := 0
		recordings.OnRelease(id, func() { released++ })
		job, err := recordings.Finish(context.Background(), id, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := recordings.Finish(context.Background(), id, 1); err == nil {
			t.Fatal("a transcribing recording was finished again")
		}
		if released != 0 || recordings.Open() != 1 {
			t.Fatalf("a refused finish released the transcribing recording (released %d, open %d)", released, recordings.Open())
		}
		job.Done()
		if released != 1 {
			t.Fatalf("released %d times after Done, want once", released)
		}
	})

	t.Run("finished while a piece is read", func(t *testing.T) {
		recordings := New(0, 0)
		id, _ := recordings.Begin("audio/webm", 0)
		released := 0
		recordings.OnRelease(id, func() { released++ })
		body := &blockedBody{release: make(chan struct{}), data: strings.NewReader("abcd")}
		done := make(chan error, 1)
		go func() { done <- recordings.Append(id, 0, 4, body) }()
		waitFor(t, func() bool { return recordings.Held() == 4 })
		if _, err := recordings.Finish(context.Background(), id, 1); err == nil {
			t.Fatal("a recording was finished while a piece was being read")
		}
		if released != 0 || recordings.Open() != 1 {
			t.Fatalf("a refused finish released the uploading recording (released %d, open %d)", released, recordings.Open())
		}
		close(body.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// A piece's bytes are reserved before it is read, so uploads in flight at
// once cannot pass the caps: a recording reads one piece at a time, and
// every reservation counts against the bytes held across recordings.
func TestPiecesInFlightCountAgainstTheCaps(t *testing.T) {
	recordings := New(0, 0)
	id, err := recordings.Begin("audio/webm", 0)
	if err != nil {
		t.Fatal(err)
	}
	body := &blockedBody{release: make(chan struct{}), data: strings.NewReader("abcd")}
	done := make(chan error, 1)
	go func() { done <- recordings.Append(id, 0, 4, body) }()
	waitFor(t, func() bool { return recordings.Held() == 4 })

	if err := recordings.Admits(id, 0, 4); err == nil {
		t.Fatal("a second upload of the piece being read was admitted")
	}
	if err := recordings.Append(id, 0, 4, strings.NewReader("abcd")); err == nil {
		t.Fatal("a second upload of the piece being read was accepted")
	}
	if _, err := recordings.Finish(context.Background(), id, 1); err == nil {
		t.Fatal("a recording was finished while a piece was being read")
	}

	// The budget across recordings counts the piece in flight.
	var others []string
	for held := int64(4); held+MaxRecordingBytes <= MaxHeldBytes; held += MaxRecordingBytes {
		other, err := recordings.Begin("audio/webm", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := recordings.Append(other, 0, MaxRecordingBytes, bytes.NewReader(make([]byte, MaxRecordingBytes))); err != nil {
			t.Fatal(err)
		}
		others = append(others, other)
	}
	last, err := recordings.Begin("audio/webm", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordings.Admits(last, 0, MaxRecordingBytes); err == nil {
		t.Fatal("a piece past the bytes held across recordings was admitted")
	}

	close(body.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, other := range others {
		recordings.Cancel(other)
	}
	if held := recordings.Held(); held != 4 {
		t.Fatalf("bytes held = %d, want the one stored piece", held)
	}
}

// A recording dropped while its piece is read keeps that piece's
// reservation until the read ends, and the piece is not stored.
func TestADroppedRecordingReleasesItsPieceWhenTheReadEnds(t *testing.T) {
	recordings := New(0, 0)
	id, _ := recordings.Begin("audio/webm", 0)
	body := &blockedBody{release: make(chan struct{}), data: strings.NewReader("abcd")}
	done := make(chan error, 1)
	go func() { done <- recordings.Append(id, 0, 4, body) }()
	waitFor(t, func() bool { return recordings.Held() == 4 })

	recordings.Cancel(id)
	if held := recordings.Held(); held != 4 {
		t.Fatalf("bytes held while the dropped piece is read = %d, want 4", held)
	}
	close(body.release)
	if err := <-done; !errors.Is(err, ErrUnknownRecording) {
		t.Fatalf("Append into a dropped recording = %v", err)
	}
	if held := recordings.Held(); held != 0 {
		t.Fatalf("bytes held after the read = %d", held)
	}
}

// A transcribing recording keeps its place and bytes until Done; Cancel
// ends its context instead of dropping it out from under the job.
func TestATranscriptionStaysOwnedUntilDone(t *testing.T) {
	recordings := New(0, 0)
	id, _ := recordings.Begin("audio/webm", 0)
	if err := recordings.Append(id, 0, 3, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	job, err := recordings.Finish(context.Background(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	recordings.Cancel(id)
	if job.Context.Err() == nil {
		t.Fatal("Cancel did not stop the transcription")
	}
	if recordings.Open() != 1 || recordings.Held() != 3 {
		t.Fatalf("open = %d, held = %d before Done; want the job counted", recordings.Open(), recordings.Held())
	}
	if err := recordings.Admits(id, 1, 1); err == nil {
		t.Fatal("a finished recording admitted another piece")
	}
	job.Done()
	if recordings.Open() != 0 || recordings.Held() != 0 {
		t.Fatalf("open = %d, held = %d after Done", recordings.Open(), recordings.Held())
	}
}

// An abandoned recording expires on its own timer, with no other activity
// on the backend, and says so to whoever owns its cleanup.
func TestAnIdleRecordingExpiresOnItsOwn(t *testing.T) {
	recordings := New(time.Hour, 0)
	id, _ := recordings.Begin("audio/webm", 0)
	expired := make(chan struct{})
	if !recordings.OnRelease(id, func() { close(expired) }) {
		t.Fatal("OnRelease found no recording")
	}
	// The piece's arrival arms the timer at the idle time set here, so no
	// step of the test has to come before a short timer fires.
	recordings.mu.Lock()
	recordings.idle = 20 * time.Millisecond
	recordings.mu.Unlock()
	if err := recordings.Append(id, 0, 3, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-expired:
	case <-time.After(5 * time.Second):
		t.Fatal("the idle recording never expired")
	}
	if recordings.Open() != 0 || recordings.Held() != 0 {
		t.Fatalf("open = %d, held = %d after expiry", recordings.Open(), recordings.Held())
	}
	if _, err := recordings.Finish(context.Background(), id, 1); !errors.Is(err, ErrUnknownRecording) {
		t.Fatalf("Finish after expiry = %v", err)
	}
}

// Neither a piece being read nor a transcription expires: the idle timer
// firing during either leaves the recording open. The test fires the
// timer's own callback at each point, since a real timer short enough to
// fire there can also fire between the test's steps.
func TestWorkInProgressDoesNotExpire(t *testing.T) {
	recordings := New(time.Hour, 0)
	id, _ := recordings.Begin("audio/webm", 0)
	recordings.mu.Lock()
	rec := recordings.open[id]
	recordings.mu.Unlock()
	body := &blockedBody{release: make(chan struct{}), data: strings.NewReader("abc")}
	done := make(chan error, 1)
	go func() { done <- recordings.Append(id, 0, 3, body) }()
	waitFor(t, func() bool {
		recordings.mu.Lock()
		defer recordings.mu.Unlock()
		return rec.reading > 0
	})
	recordings.expire(id, rec)
	close(body.release)
	if err := <-done; err != nil {
		t.Fatalf("a piece read past the idle timeout was dropped: %v", err)
	}
	job, err := recordings.Finish(context.Background(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	recordings.expire(id, rec)
	if recordings.Open() != 1 || job.Context.Err() != nil {
		t.Fatal("a transcription expired")
	}
	job.Done()
}

// A transcription's context ends at the budget, with a cause that says so.
func TestATranscriptionEndsAtItsBudget(t *testing.T) {
	recordings := New(0, 20*time.Millisecond)
	if New(0, 0).budget != TranscribeBudget {
		t.Fatal("a zero budget is not TranscribeBudget")
	}
	id, _ := recordings.Begin("audio/webm", 0)
	if err := recordings.Append(id, 0, 3, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	job, err := recordings.Finish(context.Background(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer job.Done()
	<-job.Context.Done()
	if cause := context.Cause(job.Context); !errors.Is(cause, ErrTranscribeBudget) {
		t.Fatalf("cause = %v, want ErrTranscribeBudget", cause)
	}
}

// A write to the switch ends every recording admitted before it: a
// transcription's context ends with ErrSwitchedOff, an open recording drops
// its audio at once and answers its client's next call with ErrSwitchedOff,
// and nothing admitted before the write opens after it. A recording
// admitted at or after the write is untouched.
func TestRevokeBeforeEndsRecordingsAdmittedBeforeTheWrite(t *testing.T) {
	recordings := New(0, 0)
	transcribing, _ := recordings.Begin("audio/webm", 4)
	open, _ := recordings.Begin("audio/webm", 4)
	later, _ := recordings.Begin("audio/webm", 5)
	for _, id := range []string{transcribing, open, later} {
		if err := recordings.Append(id, 0, 3, strings.NewReader("abc")); err != nil {
			t.Fatal(err)
		}
	}
	job, err := recordings.Finish(context.Background(), transcribing, 1)
	if err != nil {
		t.Fatal(err)
	}

	recordings.RevokeBefore(5)
	<-job.Context.Done()
	if cause := context.Cause(job.Context); !errors.Is(cause, ErrSwitchedOff) {
		t.Fatalf("transcription cause = %v, want ErrSwitchedOff", cause)
	}
	job.Done()
	if held := recordings.Held(); held != 3 {
		t.Fatalf("bytes held after the revocation = %d, want only the later recording's 3", held)
	}
	if err := recordings.Admits(open, 1, 3); !errors.Is(err, ErrSwitchedOff) {
		t.Fatalf("Admits on a revoked recording = %v, want ErrSwitchedOff", err)
	}
	if err := recordings.Admits(open, 1, 3); !errors.Is(err, ErrUnknownRecording) {
		t.Fatalf("a revoked recording stayed after its client heard why: %v", err)
	}
	if _, err := recordings.Begin("audio/webm", 4); !errors.Is(err, ErrSwitchedOff) {
		t.Fatalf("Begin admitted before the write = %v, want ErrSwitchedOff", err)
	}
	if recordings.Open() != 1 {
		t.Fatalf("open recordings = %d, want the later one", recordings.Open())
	}
	if _, err := recordings.Finish(context.Background(), later, 1); err != nil {
		t.Fatalf("a recording admitted at the write was ended: %v", err)
	}
}

// A revoked recording whose piece was being read answers that upload, and
// a Finish, with ErrSwitchedOff.
func TestRevokeBeforeReachesAPieceBeingRead(t *testing.T) {
	recordings := New(0, 0)
	id, _ := recordings.Begin("audio/webm", 1)
	body := &blockedBody{release: make(chan struct{}), data: strings.NewReader("abc")}
	done := make(chan error, 1)
	go func() { done <- recordings.Append(id, 0, 3, body) }()
	waitFor(t, func() bool { return recordings.Held() == 3 })
	recordings.RevokeBefore(2)
	close(body.release)
	if err := <-done; !errors.Is(err, ErrSwitchedOff) {
		t.Fatalf("Append = %v, want ErrSwitchedOff", err)
	}
	if recordings.Open() != 0 || recordings.Held() != 0 {
		t.Fatalf("open %d, held %d after the revoked upload", recordings.Open(), recordings.Held())
	}
}

// A WebKit recorder sends its one piece only when it stops, up to the
// client's ten-minute cap.
func TestTheIdleTimeoutOutlastsTheClientCap(t *testing.T) {
	if IdleTimeout <= 10*time.Minute {
		t.Fatalf("IdleTimeout = %s, want more than the ten-minute client cap", IdleTimeout)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(time.Millisecond)
	}
}
