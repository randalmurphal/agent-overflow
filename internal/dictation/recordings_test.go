package dictation

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRecordingJoinsItsPiecesInOrder(t *testing.T) {
	recordings := New(0)
	id, err := recordings.Begin("audio/webm")
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
	recordings := New(0)
	if _, err := recordings.Begin("audio/wav"); err == nil {
		t.Fatal("a container MediaRecorder does not produce was accepted")
	}
	id, err := recordings.Begin("audio/mp4")
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
	recordings := New(0)
	var ids []string
	for range MaxOpenRecordings {
		id, err := recordings.Begin("audio/webm")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := recordings.Begin("audio/webm"); err == nil {
		t.Fatal("a recording past the open cap was begun")
	}
	recordings.Cancel(ids[0])
	if _, err := recordings.Begin("audio/webm"); err != nil {
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

// A piece's bytes are reserved before it is read, so uploads in flight at
// once cannot pass the caps: a recording reads one piece at a time, and
// every reservation counts against the bytes held across recordings.
func TestPiecesInFlightCountAgainstTheCaps(t *testing.T) {
	recordings := New(0)
	id, err := recordings.Begin("audio/webm")
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
		other, err := recordings.Begin("audio/webm")
		if err != nil {
			t.Fatal(err)
		}
		if err := recordings.Append(other, 0, MaxRecordingBytes, bytes.NewReader(make([]byte, MaxRecordingBytes))); err != nil {
			t.Fatal(err)
		}
		others = append(others, other)
	}
	last, err := recordings.Begin("audio/webm")
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
	recordings := New(0)
	id, _ := recordings.Begin("audio/webm")
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
	recordings := New(0)
	id, _ := recordings.Begin("audio/webm")
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
	recordings := New(20 * time.Millisecond)
	id, _ := recordings.Begin("audio/webm")
	if err := recordings.Append(id, 0, 3, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	expired := make(chan struct{})
	if !recordings.OnExpire(id, func() { close(expired) }) {
		t.Fatal("OnExpire found no recording")
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

// Neither a piece being read nor a transcription expires.
func TestWorkInProgressDoesNotExpire(t *testing.T) {
	recordings := New(20 * time.Millisecond)
	id, _ := recordings.Begin("audio/webm")
	body := &blockedBody{release: make(chan struct{}), data: strings.NewReader("abc")}
	done := make(chan error, 1)
	go func() { done <- recordings.Append(id, 0, 3, body) }()
	time.Sleep(100 * time.Millisecond)
	close(body.release)
	if err := <-done; err != nil {
		t.Fatalf("a piece read past the idle timeout was dropped: %v", err)
	}
	job, err := recordings.Finish(context.Background(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if recordings.Open() != 1 || job.Context.Err() != nil {
		t.Fatal("a transcription expired")
	}
	job.Done()
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
