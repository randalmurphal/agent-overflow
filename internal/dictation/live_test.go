package dictation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTranscriber records what reaches it; Finish waits for release when
// it is set, so a test can act while a finish is in progress.
type fakeTranscriber struct {
	mu        sync.Mutex
	heard     int
	appendErr error
	release   chan struct{}
	finishing chan struct{}
	closed    int
}

func (f *fakeTranscriber) Append(_ context.Context, pcm []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendErr != nil {
		return f.appendErr
	}
	f.heard += len(pcm)
	return nil
}

func (f *fakeTranscriber) Heard() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf("%d bytes", f.heard)
}

func (f *fakeTranscriber) Finish(ctx context.Context) (string, error) {
	if f.release != nil {
		close(f.finishing)
		select {
		case <-f.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "transcript of " + f.Heard(), nil
}

func (f *fakeTranscriber) Close() {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
}

func (f *fakeTranscriber) closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func openFake(t *testing.T, live *Live, fake *fakeTranscriber) string {
	t.Helper()
	id, err := live.Open(context.Background(), 0, func(context.Context) (LiveTranscriber, error) { return fake, nil })
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func pcm(n int) []byte { return make([]byte, n) }

func TestALiveSessionAnswersEachPieceAndFinishesWithTheTurn(t *testing.T) {
	t.Parallel()
	live := NewLive(0)
	fake := &fakeTranscriber{}
	id := openFake(t, live, fake)
	released := 0
	if err := live.OnRelease(id, func() { released++ }); err != nil {
		t.Fatalf("OnRelease on an open session = %v", err)
	}
	total := 0
	for seq, size := range []int{4800, 4800, 2400} {
		heard, err := live.Append(context.Background(), id, seq, pcm(size))
		if err != nil {
			t.Fatal(err)
		}
		total += size
		if want := fmt.Sprintf("%d bytes", total); heard != want {
			t.Fatalf("piece %d heard %q, want %q", seq, heard, want)
		}
	}
	transcript, err := live.Finish(context.Background(), id)
	if err != nil || transcript != "transcript of 12000 bytes" {
		t.Fatalf("Finish = %q, %v", transcript, err)
	}
	if released != 1 || fake.closes() == 0 {
		t.Fatalf("after Finish: released %d times, closed %d times", released, fake.closes())
	}
	if _, err := live.Append(context.Background(), id, 3, pcm(2)); !errors.Is(err, ErrUnknownLiveSession) {
		t.Fatalf("Append after Finish = %v", err)
	}
	if _, err := live.Finish(context.Background(), id); !errors.Is(err, ErrUnknownLiveSession) {
		t.Fatalf("second Finish = %v", err)
	}
}

func TestALiveSessionRefusesPiecesThatWouldCorruptIt(t *testing.T) {
	t.Parallel()
	live := NewLive(0)
	id := openFake(t, live, &fakeTranscriber{})
	for name, tc := range map[string]struct {
		seq  int
		size int
	}{
		"empty":           {0, 0},
		"half a sample":   {0, 3},
		"out of order":    {1, 2},
		"larger than cap": {0, MaxLivePieceBytes + 2},
	} {
		if _, err := live.Append(context.Background(), id, tc.seq, pcm(tc.size)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := live.Append(context.Background(), id, 0, pcm(2)); err != nil {
		t.Fatalf("a good piece after the refusals: %v", err)
	}
}

func TestALiveSessionStopsAtTenMinutes(t *testing.T) {
	t.Parallel()
	live := NewLive(0)
	id := openFake(t, live, &fakeTranscriber{})
	piece := pcm(MaxLivePieceBytes)
	seq := 0
	for ; seq < MaxLiveSessionBytes/MaxLivePieceBytes; seq++ {
		if _, err := live.Append(context.Background(), id, seq, piece); err != nil {
			t.Fatalf("piece %d: %v", seq, err)
		}
	}
	if _, err := live.Append(context.Background(), id, seq, pcm(2)); !errors.Is(err, ErrLiveSessionFull) {
		t.Fatalf("a piece past ten minutes = %v, want ErrLiveSessionFull", err)
	}
}

func TestOnlySoManyLiveSessionsOpenAtOnce(t *testing.T) {
	t.Parallel()
	live := NewLive(0)
	var ids []string
	for range MaxOpenLiveSessions {
		ids = append(ids, openFake(t, live, &fakeTranscriber{}))
	}
	dialed := false
	if _, err := live.Open(context.Background(), 0, func(context.Context) (LiveTranscriber, error) {
		dialed = true
		return &fakeTranscriber{}, nil
	}); err == nil || dialed {
		t.Fatalf("a session past the cap opened (dialed %v): %v", dialed, err)
	}
	live.Cancel(ids[0])
	openFake(t, live, &fakeTranscriber{})
}

func TestCancelStopsAFinishInProgress(t *testing.T) {
	t.Parallel()
	live := NewLive(0)
	fake := &fakeTranscriber{release: make(chan struct{}), finishing: make(chan struct{})}
	id := openFake(t, live, fake)
	released := make(chan struct{})
	if err := live.OnRelease(id, func() { close(released) }); err != nil {
		t.Fatal(err)
	}
	if _, err := live.Append(context.Background(), id, 0, pcm(4800)); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := live.Finish(context.Background(), id)
		result <- err
	}()
	<-fake.finishing
	if _, err := live.Append(context.Background(), id, 1, pcm(2)); err == nil {
		t.Fatal("a piece was appended to a finishing session")
	}
	live.Cancel(id)
	if err := <-result; !errors.Is(err, ErrUnknownLiveSession) {
		t.Fatalf("a cancelled Finish = %v", err)
	}
	<-released
	if fake.closes() == 0 {
		t.Fatal("the cancelled session's transcriber was not closed")
	}
}

func TestCancelStopsAConnectInProgress(t *testing.T) {
	t.Parallel()
	live := NewLive(0)
	dialing := make(chan struct{})
	var opened sync.WaitGroup
	opened.Add(1)
	var openErr error
	go func() {
		defer opened.Done()
		_, openErr = live.Open(context.Background(), 0, func(ctx context.Context) (LiveTranscriber, error) {
			close(dialing)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}()
	<-dialing
	live.mu.Lock()
	var id string
	for open := range live.open {
		id = open
	}
	live.mu.Unlock()
	live.Cancel(id)
	opened.Wait()
	if openErr == nil {
		t.Fatal("a cancelled connect opened a session")
	}
	live.mu.Lock()
	left := len(live.open)
	live.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d sessions left after a cancelled connect", left)
	}
}

func openFakeAt(t *testing.T, live *Live, admitted uint64, fake *fakeTranscriber) (string, *atomic.Int32) {
	t.Helper()
	id, err := live.Open(context.Background(), admitted, func(context.Context) (LiveTranscriber, error) { return fake, nil })
	if err != nil {
		t.Fatal(err)
	}
	released := new(atomic.Int32)
	if err := live.OnRelease(id, func() { released.Add(1) }); err != nil {
		t.Fatalf("OnRelease on a session just opened: %v", err)
	}
	return id, released
}

// Switching live off at revision 5 ends what was admitted before it, open
// or connecting, and refuses anything still to come in from before it;
// what was admitted at or after it carries on.
func TestRevokeBeforeEndsOnlyOlderSessions(t *testing.T) {
	t.Parallel()
	live := NewLive(0)
	older, newer := &fakeTranscriber{}, &fakeTranscriber{}
	olderID, olderReleased := openFakeAt(t, live, 3, older)
	newerID, _ := openFakeAt(t, live, 5, newer)
	dialing := make(chan struct{})
	connected := make(chan error, 1)
	go func() {
		_, err := live.Open(context.Background(), 4, func(ctx context.Context) (LiveTranscriber, error) {
			close(dialing)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		connected <- err
	}()
	<-dialing

	live.RevokeBefore(5)
	if err := <-connected; !errors.Is(err, ErrLiveSwitchedOff) {
		t.Fatalf("a connect admitted before the switch = %v", err)
	}
	if older.closes() != 1 {
		t.Fatalf("the older session closed %d times", older.closes())
	}
	if olderReleased.Load() != 0 {
		t.Fatal("the older session was released before its client heard why it ended")
	}
	if _, err := live.Append(context.Background(), olderID, 0, pcm(2)); !errors.Is(err, ErrLiveSwitchedOff) {
		t.Fatalf("the older session's next append = %v", err)
	}
	if got := olderReleased.Load(); got != 1 {
		t.Fatalf("the older session released %d times once its client heard", got)
	}
	if _, err := live.Append(context.Background(), olderID, 0, pcm(2)); !errors.Is(err, ErrUnknownLiveSession) {
		t.Fatalf("the older session told twice: %v", err)
	}
	if _, err := live.Append(context.Background(), newerID, 0, pcm(2)); err != nil {
		t.Fatalf("a session admitted at the switch's revision was ended: %v", err)
	}
	if _, err := live.Open(context.Background(), 4, func(context.Context) (LiveTranscriber, error) { return &fakeTranscriber{}, nil }); !errors.Is(err, ErrLiveSwitchedOff) {
		t.Fatalf("an admission from before the switch arriving after it = %v", err)
	}
	// A late observer of an older switch revokes nothing newer.
	live.RevokeBefore(2)
	if _, err := live.Append(context.Background(), newerID, 1, pcm(2)); err != nil {
		t.Fatalf("a late, older revocation ended a newer session: %v", err)
	}
}

// A session revoked between opening and its owner attaching says so to
// the owner, once, rather than reading as unknown.
func TestAttachingToARevokedSessionSaysWhy(t *testing.T) {
	t.Parallel()
	live := NewLive(0)
	fake := &fakeTranscriber{}
	id, err := live.Open(context.Background(), 1, func(context.Context) (LiveTranscriber, error) { return fake, nil })
	if err != nil {
		t.Fatal(err)
	}
	live.RevokeBefore(2)
	if err := live.OnRelease(id, func() {}); !errors.Is(err, ErrLiveSwitchedOff) {
		t.Fatalf("attaching to a revoked session = %v", err)
	}
	if err := live.OnRelease(id, func() {}); !errors.Is(err, ErrUnknownLiveSession) {
		t.Fatalf("attaching again = %v", err)
	}
	live.mu.Lock()
	left := len(live.revoked)
	live.mu.Unlock()
	if left != 0 || fake.closes() != 1 {
		t.Fatalf("%d revoked records left, closed %d times", left, fake.closes())
	}
}

func TestARevokedFinishSaysWhy(t *testing.T) {
	t.Parallel()
	live := NewLive(0)
	fake := &fakeTranscriber{release: make(chan struct{}), finishing: make(chan struct{})}
	id, released := openFakeAt(t, live, 1, fake)
	finished := make(chan error, 1)
	go func() {
		_, err := live.Finish(context.Background(), id)
		finished <- err
	}()
	<-fake.finishing
	live.RevokeBefore(2)
	if err := <-finished; !errors.Is(err, ErrLiveSwitchedOff) {
		t.Fatalf("a finish revoked part way = %v", err)
	}
	if released.Load() != 1 || fake.closes() != 1 {
		t.Fatalf("released %d, closed %d; want each once", released.Load(), fake.closes())
	}
}

// What a revoked session still owes goes once: to a cancel, or after it
// has been idle as long as a session may be.
func TestARevokedSessionIsReleasedByCancelOrIdleness(t *testing.T) {
	t.Parallel()
	live := NewLive(20 * time.Millisecond)
	cancelled, cancelledReleased := openFakeAt(t, live, 1, &fakeTranscriber{})
	_, idleReleased := openFakeAt(t, live, 1, &fakeTranscriber{})
	live.RevokeBefore(2)
	live.Cancel(cancelled)
	if got := cancelledReleased.Load(); got != 1 {
		t.Fatalf("a cancelled revoked session released %d times", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for idleReleased.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a revoked session nobody asked about was never released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	live.mu.Lock()
	left := len(live.revoked)
	live.mu.Unlock()
	if left != 0 || idleReleased.Load() != 1 {
		t.Fatalf("%d revoked records left, idle released %d times", left, idleReleased.Load())
	}
}

func TestAnAbandonedLiveSessionIsClosed(t *testing.T) {
	t.Parallel()
	live := NewLive(20 * time.Millisecond)
	fake := &fakeTranscriber{}
	id := openFake(t, live, fake)
	released := make(chan struct{})
	if err := live.OnRelease(id, func() { close(released) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("an idle session was never released")
	}
	if fake.closes() == 0 {
		t.Fatal("an idle session's transcriber was not closed")
	}
}

func TestATranscriberThatFailsAnAppendEndsTheSession(t *testing.T) {
	t.Parallel()
	live := NewLive(0)
	fake := &fakeTranscriber{appendErr: errors.New("socket gone")}
	id := openFake(t, live, fake)
	if _, err := live.Append(context.Background(), id, 0, pcm(2)); err == nil {
		t.Fatal("a failed append succeeded")
	}
	if _, err := live.Finish(context.Background(), id); !errors.Is(err, ErrUnknownLiveSession) {
		t.Fatalf("Finish after a failed append = %v", err)
	}
	if fake.closes() == 0 {
		t.Fatal("the failed session's transcriber was not closed")
	}
}
