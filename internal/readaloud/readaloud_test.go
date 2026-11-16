package readaloud

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/openai"
)

const text = "The build finished in forty seconds."

const answer = "Sure thing. Why don't penguins use Facebook? They already have plenty of ice to break."

// admit admits text at revision admitted and returns its id.
func admit(t *testing.T, sentences *Sentences, admitted uint64) string {
	t.Helper()
	id, err := sentences.Admit(text, "marin", admitted)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// echo speaks every request as written.
func echo(_ context.Context, request openai.SpeechRequest) (openai.Speech, error) {
	return openai.Speech{PCM: []byte{1, 2}, Transcript: request.Text}, nil
}

// scripted answers each call with the next transcript and counts calls.
type scripted struct {
	mu          sync.Mutex
	transcripts []string
	calls       int
	requests    []openai.SpeechRequest
	// during runs inside each call, before it answers.
	during func()
}

func (s *scripted) speak(_ context.Context, request openai.SpeechRequest) (openai.Speech, error) {
	s.mu.Lock()
	transcript := s.transcripts[min(s.calls, len(s.transcripts)-1)]
	s.calls++
	s.requests = append(s.requests, request)
	during := s.during
	s.mu.Unlock()
	if during != nil {
		during()
	}
	return openai.Speech{PCM: []byte(transcript), Transcript: transcript}, nil
}

func (s *scripted) called() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// synthesized runs one synthesis and closes its clip.
func synthesized(t *testing.T, sentences *Sentences, id string, speak SpeakFunc) error {
	t.Helper()
	clip, err := sentences.Synthesize(context.Background(), id, speak)
	if err != nil {
		return err
	}
	return clip.Close()
}

// everySlotIsFree fails the test unless MaxSpeaking clips can be held at
// once, which one slot not given back prevents.
func everySlotIsFree(t *testing.T, sentences *Sentences, admitted uint64) {
	t.Helper()
	clips := make([]*Clip, 0, MaxSpeaking)
	for range MaxSpeaking {
		clip, err := sentences.Synthesize(context.Background(), admit(t, sentences, admitted), echo)
		if err != nil {
			t.Fatalf("a slot was not given back: %v", err)
		}
		clips = append(clips, clip)
	}
	for _, clip := range clips {
		if err := clip.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAnAdmittedSentenceIsSpokenOnce(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	id, err := sentences.Admit("  "+text+"\n", "coral", 7)
	if err != nil {
		t.Fatal(err)
	}
	speaker := &scripted{transcripts: []string{text}}
	if err := synthesized(t, sentences, id, speaker.speak); err != nil {
		t.Fatal(err)
	}
	if want := (openai.SpeechRequest{Text: text, Voice: "coral"}); len(speaker.requests) != 1 || speaker.requests[0] != want {
		t.Fatalf("spoke %+v; want the trimmed text in its voice, once", speaker.requests)
	}
	if err := synthesized(t, sentences, id, speaker.speak); !errors.Is(err, ErrUnknown) {
		t.Fatalf("a second synthesis = %v, want ErrUnknown", err)
	}
	if err := synthesized(t, sentences, "never-admitted", speaker.speak); !errors.Is(err, ErrUnknown) {
		t.Fatalf("an unknown id = %v, want ErrUnknown", err)
	}
	if speaker.called() != 1 {
		t.Fatalf("OpenAI was asked %d times, want 1", speaker.called())
	}
	// Repeated admissions get distinct ids, each its own sentence.
	first, err := sentences.Admit("One.", "marin", 7)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sentences.Admit("Two.", "marin", 7)
	if err != nil || first == second {
		t.Fatalf("ids %q and %q, %v", first, second, err)
	}
	two := &scripted{transcripts: []string{"Two."}}
	if err := synthesized(t, sentences, second, two.speak); err != nil || two.requests[0].Text != "Two." {
		t.Fatalf("second id spoke %+v, %v", two.requests, err)
	}
}

func TestAdmitRefusesWhatCannotBeRead(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	for _, c := range []struct {
		text, voice string
		is          error
	}{
		{"", "marin", ErrNoText},
		{" \n\t ", "marin", ErrNoText},
		{strings.Repeat("é", MaxTextCharacters+1), "marin", ErrTextTooLong},
		{text, "nova", nil},
		{text, "", nil},
	} {
		_, err := sentences.Admit(c.text, c.voice, 0)
		if err == nil || (c.is != nil && !errors.Is(err, c.is)) {
			t.Fatalf("Admit(%.20q, %q) = %v, want %v", c.text, c.voice, err, c.is)
		}
	}
	if _, err := sentences.Admit(" "+strings.Repeat("é", MaxTextCharacters)+" ", "marin", 0); err != nil {
		t.Fatalf("a sentence of exactly the cap, in characters, was refused: %v", err)
	}
	if n := sentences.Pending(); n != 1 {
		t.Fatalf("pending = %d after refusals, want 1", n)
	}
}

// The client admits a reply one section at a time (several sentences up to
// MaxTextCharacters), so a refusal the user reads names a section.
func TestRefusalsNameTheSectionTheUserAskedFor(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]string{
		ErrUnknown:     "this section was already read aloud or has expired",
		ErrBacklog:     "8 sections are already waiting to be read aloud; try again shortly",
		ErrBusy:        "this computer is already reading 4 sections aloud; try again shortly",
		ErrTextTooLong: "a section to read aloud is at most 1000 characters",
	} {
		if err.Error() != want {
			t.Errorf("refusal = %q, want %q", err, want)
		}
	}
}

func TestAdmissionIsBoundedAndExpires(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(by time.Duration) {
		mu.Lock()
		now = now.Add(by)
		mu.Unlock()
	}
	sentences := newWithClock(30*time.Second, clock)
	var ids []string
	for range MaxPending {
		id, err := sentences.Admit(text, "marin", 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := sentences.Admit(text, "marin", 0); !errors.Is(err, ErrBacklog) {
		t.Fatalf("admission past the bound = %v, want ErrBacklog", err)
	}
	// Synthesizing one frees its place.
	if err := synthesized(t, sentences, ids[0], echo); err != nil {
		t.Fatal(err)
	}
	if _, err := sentences.Admit(text, "marin", 0); err != nil {
		t.Fatalf("admission after a synthesis: %v", err)
	}
	advance(29 * time.Second)
	if err := synthesized(t, sentences, ids[1], echo); err != nil {
		t.Fatalf("a sentence inside its lifetime: %v", err)
	}
	advance(time.Second)
	if err := synthesized(t, sentences, ids[2], echo); !errors.Is(err, ErrUnknown) {
		t.Fatalf("an expired sentence = %v, want ErrUnknown", err)
	}
	if n := sentences.Pending(); n != 0 {
		t.Fatalf("pending = %d after every sentence expired", n)
	}
	if _, err := sentences.Admit(text, "marin", 0); err != nil {
		t.Fatalf("admission after expiry: %v", err)
	}
}

func TestSynthesizeReturnsAReadingAsAWAVFile(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	clip, err := sentences.Synthesize(context.Background(), admit(t, sentences, 0), echo)
	if err != nil {
		t.Fatal(err)
	}
	wav, err := io.ReadAll(clip)
	if err != nil || len(wav) != WAVHeaderBytes+2 || string(wav[:4]) != "RIFF" || string(wav[WAVHeaderBytes:]) != "\x01\x02" {
		t.Fatalf("clip = %q, %v; want a WAV header and the PCM", wav, err)
	}
	if err := clip.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAnAnswerIsRetriedOnce(t *testing.T) {
	t.Parallel()
	once := &scripted{transcripts: []string{answer, text}}
	sentences := New(time.Minute)
	if err := synthesized(t, sentences, admit(t, sentences, 0), once.speak); err != nil || once.called() != 2 {
		t.Fatalf("answered then read: %v after %d calls", err, once.called())
	}

	always := &scripted{transcripts: []string{answer}}
	if err := synthesized(t, sentences, admit(t, sentences, 0), always.speak); !errors.Is(err, ErrMisread) {
		t.Fatalf("always answered: %v, want ErrMisread", err)
	}
	if always.called() != 2 {
		t.Fatalf("always answered: %d calls, want 2", always.called())
	}
}

func TestASpeakFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	calls := 0
	failure := errors.New("OpenAI is down")
	sentences := New(time.Minute)
	err := synthesized(t, sentences, admit(t, sentences, 0), func(context.Context, openai.SpeechRequest) (openai.Speech, error) {
		calls++
		return openai.Speech{}, failure
	})
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("Synthesize = %v after %d calls", err, calls)
	}
	everySlotIsFree(t, sentences, 0)
}

// blocking speaks only when released or cancelled.
type blocking struct {
	entered chan struct{}
	release chan struct{}
}

func (b blocking) speak(ctx context.Context, request openai.SpeechRequest) (openai.Speech, error) {
	b.entered <- struct{}{}
	select {
	case <-b.release:
		return openai.Speech{Transcript: request.Text}, nil
	case <-ctx.Done():
		return openai.Speech{}, ctx.Err()
	}
}

// refusedAtOnce fails the test unless a synthesis is refused with want
// without waiting.
func refusedAtOnce(t *testing.T, sentences *Sentences, want error) {
	t.Helper()
	refused := make(chan error, 1)
	id := admit(t, sentences, 0)
	go func() { refused <- synthesized(t, sentences, id, echo) }()
	select {
	case err := <-refused:
		if !errors.Is(err, want) {
			t.Fatalf("a synthesis past the cap = %v, want %v", err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a synthesis past the cap waited for a slot")
	}
}

func TestSynthesesAreCappedAndTheCapFrees(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	held := blocking{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, MaxSpeaking)
	for range MaxSpeaking {
		id := admit(t, sentences, 0)
		go func() { done <- synthesized(t, sentences, id, held.speak) }()
		<-held.entered
	}
	// Past the cap is refused at once, not queued behind the four.
	refusedAtOnce(t, sentences, ErrBusy)
	close(held.release)
	for range MaxSpeaking {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	everySlotIsFree(t, sentences, 0)
}

// A clip not yet delivered still counts against the cap, so the audio held
// for slow readers is bounded.
func TestAClipHoldsItsSlotUntilClosed(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	var clips []*Clip
	for range MaxSpeaking {
		clip, err := sentences.Synthesize(context.Background(), admit(t, sentences, 0), echo)
		if err != nil {
			t.Fatal(err)
		}
		clips = append(clips, clip)
	}
	refusedAtOnce(t, sentences, ErrBusy)
	// Closing one clip, twice, frees one slot.
	for range 2 {
		if err := clips[0].Close(); err != nil {
			t.Fatal(err)
		}
	}
	second, err := sentences.Synthesize(context.Background(), admit(t, sentences, 0), echo)
	if err != nil {
		t.Fatalf("a synthesis after a clip closed: %v", err)
	}
	refusedAtOnce(t, sentences, ErrBusy)
	for _, clip := range append(clips[1:], second) {
		if err := clip.Close(); err != nil {
			t.Fatal(err)
		}
	}
	everySlotIsFree(t, sentences, 0)
}

// stubborn holds every request until release closes, whatever its context
// says: a synthesis that has not yet noticed its fetch went away.
type stubborn struct {
	entered chan struct{}
	release chan struct{}
}

func (b stubborn) speak(_ context.Context, request openai.SpeechRequest) (openai.Speech, error) {
	b.entered <- struct{}{}
	<-b.release
	return openai.Speech{Transcript: request.Text}, nil
}

// Release drops a section still waiting, and ends a synthesis of one and
// gives its slot back before it returns, before the synthesis has noticed:
// with every slot taken, a reader's next synthesis is not refused for the
// one it gave up.
func TestReleaseFreesWhatItsReaderGaveUp(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	waiting, err := sentences.Admit(text, "marin", 0)
	if err != nil {
		t.Fatal(err)
	}
	sentences.Release(waiting)
	if err := synthesized(t, sentences, waiting, echo); !errors.Is(err, ErrUnknown) || sentences.Pending() != 0 {
		t.Fatalf("a released section was still waiting: %v", err)
	}

	held := stubborn{entered: make(chan struct{}), release: make(chan struct{})}
	ids := make([]string, MaxSpeaking)
	done := make(chan error, MaxSpeaking)
	for i := range MaxSpeaking {
		id, err := sentences.Admit(text, "marin", 0)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		go func() {
			clip, err := sentences.Synthesize(context.Background(), id, held.speak)
			if err == nil {
				err = clip.Close()
			}
			done <- err
		}()
		<-held.entered
	}
	refusedAtOnce(t, sentences, ErrBusy)
	sentences.Release(ids[0])
	clip, err := sentences.Synthesize(context.Background(), admit(t, sentences, 0), echo)
	if err != nil {
		t.Fatalf("a synthesis after a release: %v", err)
	}
	refusedAtOnce(t, sentences, ErrBusy)
	if err := clip.Close(); err != nil {
		t.Fatal(err)
	}

	close(held.release)
	released := 0
	for range MaxSpeaking {
		switch err := <-done; {
		case errors.Is(err, ErrUnknown):
			released++
		case err != nil:
			t.Fatal(err)
		}
	}
	if released != 1 {
		t.Fatalf("released syntheses answered ErrUnknown %d times, want 1", released)
	}
	everySlotIsFree(t, sentences, 0)
}

func TestCancellingASynthesisEndsItAndFreesItsSlot(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	held := blocking{entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, MaxSpeaking)
	for range MaxSpeaking {
		id := admit(t, sentences, 0)
		go func() {
			_, err := sentences.Synthesize(ctx, id, held.speak)
			done <- err
		}()
		<-held.entered
	}
	cancel()
	for range MaxSpeaking {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("a cancelled synthesis = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a cancelled synthesis did not return")
		}
	}
	everySlotIsFree(t, sentences, 0)
}

// A write to the switch at revision 5 ends what was admitted before it,
// waiting or speaking, and spares what was admitted at or after it.
func TestRevokingEndsOlderAdmissions(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	older, err := sentences.Admit(text, "marin", 4)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := sentences.Admit(text, "marin", 5)
	if err != nil {
		t.Fatal(err)
	}
	olderSpoken, newerSpoken := admit(t, sentences, 4), admit(t, sentences, 5)
	held := blocking{entered: make(chan struct{}), release: make(chan struct{})}
	speakingOlder := make(chan error, 1)
	go func() { speakingOlder <- synthesized(t, sentences, olderSpoken, held.speak) }()
	<-held.entered
	speakingNewer := make(chan error, 1)
	go func() { speakingNewer <- synthesized(t, sentences, newerSpoken, held.speak) }()
	<-held.entered

	sentences.RevokeBefore(5)

	select {
	case err := <-speakingOlder:
		if !errors.Is(err, ErrOff) {
			t.Fatalf("a revoked synthesis = %v, want ErrOff", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a revoked synthesis kept speaking")
	}
	// The waiting one is dropped, and its id still says why.
	if waiting := sentences.Pending(); waiting != 1 {
		t.Fatalf("%d sentences wait after the revocation, want only the newer", waiting)
	}
	for range 2 {
		if err := synthesized(t, sentences, older, echo); !errors.Is(err, ErrOff) {
			t.Fatalf("a revoked waiting sentence = %v, want ErrOff", err)
		}
	}
	if err := synthesized(t, sentences, strings.Repeat("z", len(older)), echo); !errors.Is(err, ErrUnknown) {
		t.Fatalf("an id that is not one of ours = %v, want ErrUnknown", err)
	}
	if _, err := sentences.Admit(text, "marin", 4); !errors.Is(err, ErrOff) {
		t.Fatalf("an admission older than the revocation = %v, want ErrOff", err)
	}
	// A lower revision later does not bring the older ones back.
	sentences.RevokeBefore(2)
	if _, err := sentences.Admit(text, "marin", 4); !errors.Is(err, ErrOff) {
		t.Fatalf("an older admission after a lower revocation = %v, want ErrOff", err)
	}

	if err := synthesized(t, sentences, newer, echo); err != nil {
		t.Fatalf("a sentence admitted at the revocation's revision: %v", err)
	}
	close(held.release)
	if err := <-speakingNewer; err != nil {
		t.Fatalf("a synthesis admitted at the revocation's revision: %v", err)
	}
	// The revoked synthesis gave its slot back.
	everySlotIsFree(t, sentences, 5)
}

// A sentence revoked before its synthesis begins is off, not busy, when
// every slot is taken by clips still being delivered.
func TestARevokedSentenceIsOffWhenEverySlotIsTaken(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	revoked := admit(t, sentences, 0)
	clips := make([]*Clip, 0, MaxSpeaking)
	for range MaxSpeaking {
		clip, err := sentences.Synthesize(context.Background(), admit(t, sentences, 0), echo)
		if err != nil {
			t.Fatal(err)
		}
		clips = append(clips, clip)
	}
	sentences.RevokeBefore(1)
	if err := synthesized(t, sentences, revoked, echo); !errors.Is(err, ErrOff) {
		t.Fatalf("a revoked sentence with every slot taken = %v, want ErrOff", err)
	}
	if err := synthesized(t, sentences, admit(t, sentences, 1), echo); !errors.Is(err, ErrBusy) {
		t.Fatalf("a sentence admitted since, with every slot taken = %v, want ErrBusy", err)
	}
	for _, clip := range clips {
		if err := clip.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Neither refusal took a slot.
	everySlotIsFree(t, sentences, 1)
}

// A revocation that lands while the first attempt answers instead of
// reading stops the retry.
func TestRevokingBetweenAttemptsStopsTheRetry(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	speaker := &scripted{transcripts: []string{answer, text}}
	speaker.during = func() { sentences.RevokeBefore(1) }
	if err := synthesized(t, sentences, admit(t, sentences, 0), speaker.speak); !errors.Is(err, ErrOff) {
		t.Fatalf("Synthesize = %v, want ErrOff", err)
	}
	if speaker.called() != 1 {
		t.Fatalf("OpenAI was asked %d times after the revocation, want 1", speaker.called())
	}
}

// A reading that arrives as the revocation lands is not handed over.
func TestRevokingAsTheReadingArrivesWithholdsIt(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	speaker := &scripted{transcripts: []string{text}}
	speaker.during = func() { sentences.RevokeBefore(1) }
	if err := synthesized(t, sentences, admit(t, sentences, 0), speaker.speak); !errors.Is(err, ErrOff) {
		t.Fatalf("Synthesize = %v, want ErrOff", err)
	}
}

// A revocation that lands once the synthesis has its result, whatever that
// is, still decides it: the result is settled against revocations as the
// synthesis leaves the set.
func TestRevokingOnceTheResultIsKnownIsOff(t *testing.T) {
	t.Parallel()
	failure := errors.New("OpenAI is down")
	for name, speak := range map[string]SpeakFunc{
		"a clip":          echo,
		"two misreadings": (&scripted{transcripts: []string{answer}}).speak,
		"a failure": func(context.Context, openai.SpeechRequest) (openai.Speech, error) {
			return openai.Speech{}, failure
		},
	} {
		sentences := New(time.Minute)
		id := admit(t, sentences, 0)
		sentences.beforeFinish = func() { sentences.RevokeBefore(1) }
		if err := synthesized(t, sentences, id, speak); !errors.Is(err, ErrOff) {
			t.Fatalf("%s: Synthesize = %v, want ErrOff", name, err)
		}
		// Its slot is free, and sentences admitted since are read.
		sentences.beforeFinish = nil
		everySlotIsFree(t, sentences, 1)
	}
}

// A Release or RevokeBefore that lands once a synthesis has taken its
// section and before it first speaks finds it: OpenAI is not asked, the
// synthesis answers as for a section dropped while waiting, and its slot
// is back. Releasing the spent id again is harmless.
func TestAStopBeforeTheFirstSpeakAsksNothing(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		stop func(sentences *Sentences, id string)
		want error
	}{
		"released": {func(sentences *Sentences, id string) { sentences.Release(id); sentences.Release(id) }, ErrUnknown},
		"revoked":  {func(sentences *Sentences, _ string) { sentences.RevokeBefore(1) }, ErrOff},
	} {
		sentences := New(time.Minute)
		id := admit(t, sentences, 0)
		speaker := &scripted{transcripts: []string{text}}
		sentences.started = func() { c.stop(sentences, id) }
		if err := synthesized(t, sentences, id, speaker.speak); !errors.Is(err, c.want) {
			t.Fatalf("%s: Synthesize = %v, want %v", name, err, c.want)
		}
		if speaker.called() != 0 {
			t.Fatalf("%s: OpenAI was asked %d times after the stop, want 0", name, speaker.called())
		}
		if held := len(sentences.slots); held != 0 {
			t.Fatalf("%s: %d slots held after the stop, want 0", name, held)
		}
		sentences.Release(id)
		if err := synthesized(t, sentences, id, speaker.speak); !errors.Is(err, c.want) || speaker.called() != 0 {
			t.Fatalf("%s: the spent id again = %v after %d calls, want %v", name, err, speaker.called(), c.want)
		}
		sentences.started = nil
		everySlotIsFree(t, sentences, 1)
	}
}

// With every slot held, a section is refused at once without asking
// OpenAI and without disturbing the syntheses that hold the slots: a
// revoked one is ErrOff, any other ErrBusy, and the refused one is spent,
// so releasing it afterwards is harmless.
func TestEverySlotHeldRefusesWithoutSpeakingOrDisturbingTheHolders(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	revoked := admit(t, sentences, 0)
	held := blocking{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, MaxSpeaking)
	for range MaxSpeaking {
		id := admit(t, sentences, 1)
		go func() { done <- synthesized(t, sentences, id, held.speak) }()
		<-held.entered
	}
	sentences.RevokeBefore(1)

	speaker := &scripted{transcripts: []string{text}}
	if err := synthesized(t, sentences, revoked, speaker.speak); !errors.Is(err, ErrOff) {
		t.Fatalf("a revoked section with every slot held = %v, want ErrOff", err)
	}
	busy := admit(t, sentences, 1)
	if err := synthesized(t, sentences, busy, speaker.speak); !errors.Is(err, ErrBusy) {
		t.Fatalf("a section with every slot held = %v, want ErrBusy", err)
	}
	sentences.Release(busy)
	sentences.Release(busy)
	if err := synthesized(t, sentences, busy, speaker.speak); !errors.Is(err, ErrUnknown) {
		t.Fatalf("a section refused for a slot, again = %v, want ErrUnknown", err)
	}
	if speaker.called() != 0 {
		t.Fatalf("OpenAI was asked %d times for refused sections, want 0", speaker.called())
	}
	if n := len(sentences.slots); n != MaxSpeaking {
		t.Fatalf("%d slots held, want the %d holders'", n, MaxSpeaking)
	}

	close(held.release)
	for range MaxSpeaking {
		if err := <-done; err != nil {
			t.Fatalf("a holder was disturbed: %v", err)
		}
	}
	if n := len(sentences.slots); n != 0 {
		t.Fatalf("%d slots held once every holder closed, want 0", n)
	}
	everySlotIsFree(t, sentences, 1)
}
