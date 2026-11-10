package openai_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/openai"
	"agent-overflow/internal/openai/openaifake"
	"agent-overflow/internal/secret"
	"agent-overflow/internal/settings"
)

const testKey = "sk-proj-live-test-key"

func fakeClient() *http.Client { return &http.Client{Transport: openaifake.Transport{}} }

func dial(t *testing.T, key string, options openai.LiveTranscriptionOptions) (*openai.LiveTranscription, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return openai.DialLiveTranscription(ctx, fakeClient(), secret.New([]byte(key)), options)
}

// tenth is 100 ms of 24 kHz 16-bit audio.
var tenth = make([]byte, openai.LiveSampleRate*2/10)

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestALiveTranscriptionShowsWhatItHasHeardAndFinishesWithTheTurn(t *testing.T) {
	t.Parallel()
	live, err := dial(t, testKey, openai.LiveTranscriptionOptions{Delay: "low", Languages: []string{"en"}, Keywords: []string{"Agent Overflow"}, Prompt: "A developer dictating."})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := live.Append(ctx, tenth); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first partial text", func() bool { return live.Heard() == "Hearing" })
	for range 2 {
		if err := live.Append(ctx, tenth); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "partial text for every piece", func() bool { return live.Heard() == "Hearing.." })

	transcript, err := live.Finish(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("Heard %d bytes of live audio", 3*len(tenth)); transcript != want {
		t.Fatalf("Finish = %q, want %q", transcript, want)
	}
	if heard := live.Heard(); heard != transcript {
		t.Fatalf("Heard after Finish = %q; the transcript replaces the partial text", heard)
	}
}

func TestARefusedKeyIsSaidWithoutTheKey(t *testing.T) {
	t.Parallel()
	_, err := dial(t, openaifake.RefusedKey, openai.LiveTranscriptionOptions{})
	if !errors.Is(err, openai.ErrKeyRefused) {
		t.Fatalf("dial = %v, want ErrKeyRefused", err)
	}
	if strings.Contains(err.Error(), openaifake.RefusedKey) {
		t.Fatalf("refusal %q carries the key", err)
	}
}

func TestATurnTooShortToTranscribeFailsTheSession(t *testing.T) {
	t.Parallel()
	live, err := dial(t, testKey, openai.LiveTranscriptionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := live.Append(ctx, tenth[:100]); err != nil {
		t.Fatal(err)
	}
	_, err = live.Finish(ctx)
	if err == nil || !strings.Contains(err.Error(), "buffer too small") {
		t.Fatalf("Finish = %v, want OpenAI's refusal", err)
	}
	if err := live.Append(ctx, tenth); err == nil {
		t.Fatal("Append after the session ended succeeded")
	}
}

func TestClosingASessionTwiceIsHarmless(t *testing.T) {
	t.Parallel()
	live, err := dial(t, testKey, openai.LiveTranscriptionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	live.Close()
	live.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := live.Finish(ctx); err == nil {
		t.Fatal("Finish on a closed session succeeded")
	}
}

func TestDialHonorsItsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := openai.DialLiveTranscription(ctx, fakeClient(), secret.New([]byte(testKey)), openai.LiveTranscriptionOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("dial = %v, want context.Canceled", err)
	}
}

// The settings package restates OpenAI's delay vocabulary to stay
// dependency-free; this pins the two together.
func TestTheDelaySettingsMatchTheModel(t *testing.T) {
	t.Parallel()
	if !slices.Equal(settings.DictationAPIDelays, openai.LiveDelays) {
		t.Fatalf("settings offers delays %v, the model takes %v", settings.DictationAPIDelays, openai.LiveDelays)
	}
}

// keyedTransport answers every request with answer and counts the requests
// that carried a key.
type keyedTransport struct {
	mu     sync.Mutex
	keyed  int
	answer func(*http.Request) *http.Response
}

func (k *keyedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	k.mu.Lock()
	if req.Header.Get("Authorization") != "" {
		k.keyed++
	}
	k.mu.Unlock()
	return k.answer(req), nil
}

func TestARedirectedHandshakeIsNotFollowed(t *testing.T) {
	t.Parallel()
	transport := &keyedTransport{answer: func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Status:     "307 Temporary Redirect",
			Header:     http.Header{"Location": {"http://api.openai.com/v1/realtime?intent=transcription"}},
			Body:       http.NoBody,
			Request:    req,
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := openai.DialLiveTranscription(ctx, &http.Client{Transport: transport}, secret.New([]byte(testKey)), openai.LiveTranscriptionOptions{})
	if err == nil {
		t.Fatal("a redirected handshake connected")
	}
	if transport.keyed != 1 {
		t.Fatalf("the key was sent %d times; the redirect must not be followed", transport.keyed)
	}
}

func TestOpenAIsStatusTextIsNotRepeated(t *testing.T) {
	t.Parallel()
	transport := &keyedTransport{answer: func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Status:     "400 invalid key " + testKey,
			Header:     http.Header{},
			Body:       http.NoBody,
			Request:    req,
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := openai.DialLiveTranscription(ctx, &http.Client{Transport: transport}, secret.New([]byte(testKey)), openai.LiveTranscriptionOptions{})
	if err == nil || strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "400 Bad Request") {
		t.Fatalf("dial = %v; want the status in Go's words without the key", err)
	}
}

// blockedWrites counts goroutines blocked writing to the fake's pipe from
// inside a session's send. It sees the whole process, so the tests that use
// it run serially, never beside a parallel test's writes.
func blockedWrites() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	count := 0
	for _, goroutine := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(goroutine, "(*LiveTranscription).send") && strings.Contains(goroutine, "net.(*pipe).write") {
			count++
		}
	}
	return count
}

// stalledAppend starts an Append the StallsKey fake never reads, and
// returns once it is blocked in the pipe write.
func stalledAppend(t *testing.T, live *openai.LiveTranscription, ctx context.Context) <-chan error {
	t.Helper()
	before := blockedWrites()
	done := make(chan error, 1)
	go func() { done <- live.Append(ctx, make([]byte, 240000)) }()
	eventually(t, "the append to block in its write", func() bool { return blockedWrites() > before })
	return done
}

func within(t *testing.T, what string, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not return", what)
		return nil
	}
}

func TestCloseInterruptsAStalledWrite(t *testing.T) {
	live, err := dial(t, openaifake.StallsKey, openai.LiveTranscriptionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stalled := stalledAppend(t, live, context.Background())
	closed := make(chan error, 1)
	go func() { live.Close(); closed <- nil }()
	if err := within(t, "Close", closed); err != nil {
		t.Fatal(err)
	}
	if err := within(t, "the stalled write", stalled); err == nil {
		t.Fatal("the stalled write succeeded after Close")
	}
}

func TestCancellingABlockedWriteReportsTheCancellation(t *testing.T) {
	for range 10 {
		live, err := dial(t, openaifake.StallsKey, openai.LiveTranscriptionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		stalled := stalledAppend(t, live, ctx)
		cancel()
		if err := within(t, "the cancelled write", stalled); !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled blocked write = %v, want context.Canceled", err)
		}
		live.Close()
	}
}

// A Finish that gives up waiting for the gate leaves the session to whoever
// holds it.
func TestAFinishThatGivesUpWaitingDoesNotCloseTheSession(t *testing.T) {
	live, err := dial(t, openaifake.StallsKey, openai.LiveTranscriptionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	stalled := stalledAppend(t, live, context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := live.Finish(ctx)
		finished <- err
	}()
	if err := within(t, "the waiting Finish", finished); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Finish behind a stalled write = %v, want its deadline", err)
	}
	select {
	case err := <-stalled:
		t.Fatalf("the waiting Finish closed the session under the stalled write: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestFinishSealsTheTurn(t *testing.T) {
	t.Parallel()
	live, err := dial(t, testKey, openai.LiveTranscriptionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := live.Append(ctx, tenth); err != nil {
		t.Fatal(err)
	}
	appended := make(chan int, 1)
	go func() {
		pieces := 1
		for live.Append(ctx, tenth) == nil {
			pieces++
		}
		appended <- pieces
	}()
	transcript, err := live.Finish(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pieces := <-appended
	if want := fmt.Sprintf("Heard %d bytes of live audio", pieces*len(tenth)); transcript != want {
		t.Fatalf("Finish = %q after %d accepted pieces, want %q: every accepted piece, and only those, is in the turn", transcript, pieces, want)
	}
}

func TestOnlyOneFinishCommits(t *testing.T) {
	t.Parallel()
	const key = "sk-fake-held-only-one-finish"
	release := openaifake.Hold(key)
	defer release()
	live, err := dial(t, key, openai.LiveTranscriptionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := live.Append(ctx, tenth); err != nil {
		t.Fatal(err)
	}
	type result struct {
		transcript string
		err        error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			transcript, err := live.Finish(ctx)
			results <- result{transcript, err}
		}()
	}
	// The committing Finish waits for the withheld transcript, so the first
	// to return is the refused one.
	if first := <-results; !errors.Is(first.err, openai.ErrFinished) {
		t.Fatalf("the first Finish to return = %+v, want ErrFinished", first)
	}
	release()
	second := <-results
	if want := fmt.Sprintf("Heard %d bytes of live audio", len(tenth)); second.err != nil || second.transcript != want {
		t.Fatalf("the committing Finish = %+v, want %q", second, want)
	}
}

// A second Finish, even one already cancelled, leaves the owner's
// transcript to arrive.
func TestACancelledSecondFinishLeavesTheOwnersTranscript(t *testing.T) {
	t.Parallel()
	const key = "sk-fake-held-cancelled-second-finish"
	release := openaifake.Hold(key)
	defer release()
	live, err := dial(t, key, openai.LiveTranscriptionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := live.Append(ctx, tenth); err != nil {
		t.Fatal(err)
	}
	owner := make(chan error, 1)
	var transcript string
	go func() {
		var err error
		transcript, err = live.Finish(ctx)
		owner <- err
	}()
	// Once audio is refused, the first Finish owns the session; audio
	// accepted before then is part of its turn.
	pieces := 1
	eventually(t, "the first Finish to commit", func() bool {
		err := live.Append(ctx, tenth)
		if err == nil {
			pieces++
		}
		return errors.Is(err, openai.ErrFinished)
	})
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := live.Finish(cancelled); !errors.Is(err, openai.ErrFinished) && !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled second Finish = %v", err)
	}
	// The owner is still waiting; its transcript arrives only now.
	release()
	if err := <-owner; err != nil || transcript != fmt.Sprintf("Heard %d bytes of live audio", pieces*len(tenth)) {
		t.Fatalf("the owner's Finish = %q, %v; the second Finish must not end its session", transcript, err)
	}
}
