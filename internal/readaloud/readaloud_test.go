package readaloud

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/openai"
)

const sentence = "The build finished in forty seconds."

// echo speaks every request as written.
func echo(_ context.Context, request openai.SpeechRequest) (openai.Speech, error) {
	return openai.Speech{PCM: []byte{1, 2}, Transcript: request.Text}, nil
}

// scripted answers each call with the next transcript and counts calls.
type scripted struct {
	mu          sync.Mutex
	transcripts []string
	calls       int
}

func (s *scripted) speak(_ context.Context, request openai.SpeechRequest) (openai.Speech, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	transcript := s.transcripts[min(s.calls, len(s.transcripts)-1)]
	s.calls++
	return openai.Speech{PCM: []byte(transcript), Transcript: transcript}, nil
}

func TestAnAdmittedSentenceIsTakenOnce(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	id, err := sentences.Admit("  "+sentence+"\n", "coral")
	if err != nil {
		t.Fatal(err)
	}
	request, err := sentences.Take(id)
	if err != nil || request != (openai.SpeechRequest{Text: sentence, Voice: "coral"}) {
		t.Fatalf("Take = %+v, %v; want the trimmed text in its voice", request, err)
	}
	if _, err := sentences.Take(id); !errors.Is(err, ErrUnknown) {
		t.Fatalf("a second Take = %v, want ErrUnknown", err)
	}
	if _, err := sentences.Take("never-admitted"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("an unknown id = %v, want ErrUnknown", err)
	}
	// Repeated admissions get distinct ids, each its own sentence.
	first, err := sentences.Admit("One.", "marin")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sentences.Admit("Two.", "marin")
	if err != nil || first == second {
		t.Fatalf("ids %q and %q, %v", first, second, err)
	}
	if request, _ := sentences.Take(second); request.Text != "Two." {
		t.Fatalf("second id took %+v", request)
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
		{sentence, "nova", nil},
		{sentence, "", nil},
	} {
		_, err := sentences.Admit(c.text, c.voice)
		if err == nil || (c.is != nil && !errors.Is(err, c.is)) {
			t.Fatalf("Admit(%.20q, %q) = %v, want %v", c.text, c.voice, err, c.is)
		}
	}
	if _, err := sentences.Admit(" "+strings.Repeat("é", MaxTextCharacters)+" ", "marin"); err != nil {
		t.Fatalf("a sentence of exactly the cap, in characters, was refused: %v", err)
	}
	if n := sentences.Pending(); n != 1 {
		t.Fatalf("pending = %d after refusals, want 1", n)
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
		id, err := sentences.Admit(sentence, "marin")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := sentences.Admit(sentence, "marin"); !errors.Is(err, ErrBacklog) {
		t.Fatalf("admission past the bound = %v, want ErrBacklog", err)
	}
	// Taking one frees its place.
	if _, err := sentences.Take(ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := sentences.Admit(sentence, "marin"); err != nil {
		t.Fatalf("admission after a take: %v", err)
	}
	advance(29 * time.Second)
	if _, err := sentences.Take(ids[1]); err != nil {
		t.Fatalf("a sentence inside its lifetime: %v", err)
	}
	advance(time.Second)
	if _, err := sentences.Take(ids[2]); !errors.Is(err, ErrUnknown) {
		t.Fatalf("an expired sentence = %v, want ErrUnknown", err)
	}
	if n := sentences.Pending(); n != 0 {
		t.Fatalf("pending = %d after every sentence expired", n)
	}
	if _, err := sentences.Admit(sentence, "marin"); err != nil {
		t.Fatalf("admission after expiry: %v", err)
	}
}

func TestSynthesizeReturnsAReading(t *testing.T) {
	t.Parallel()
	speech, err := New(time.Minute).Synthesize(context.Background(), openai.SpeechRequest{Text: sentence, Voice: "marin"}, echo)
	if err != nil || speech.Transcript != sentence {
		t.Fatalf("Synthesize = %+v, %v", speech, err)
	}
}

func TestAnAnswerIsRetriedOnce(t *testing.T) {
	t.Parallel()
	const answer = "Sure thing. Why don't penguins use Facebook? They already have plenty of ice to break."
	request := openai.SpeechRequest{Text: sentence, Voice: "marin"}

	once := &scripted{transcripts: []string{answer, sentence}}
	speech, err := New(time.Minute).Synthesize(context.Background(), request, once.speak)
	if err != nil || speech.Transcript != sentence || once.calls != 2 {
		t.Fatalf("answered then read: %+v, %v after %d calls", speech, err, once.calls)
	}

	always := &scripted{transcripts: []string{answer}}
	if _, err := New(time.Minute).Synthesize(context.Background(), request, always.speak); !errors.Is(err, ErrMisread) {
		t.Fatalf("always answered: %v, want ErrMisread", err)
	}
	if always.calls != 2 {
		t.Fatalf("always answered: %d calls, want 2", always.calls)
	}
}

func TestASpeakFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	calls := 0
	failure := errors.New("OpenAI is down")
	_, err := New(time.Minute).Synthesize(context.Background(), openai.SpeechRequest{Text: sentence, Voice: "marin"},
		func(context.Context, openai.SpeechRequest) (openai.Speech, error) {
			calls++
			return openai.Speech{}, failure
		})
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("Synthesize = %v after %d calls", err, calls)
	}
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

func TestSynthesesAreCappedAndTheCapFrees(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	held := blocking{entered: make(chan struct{}), release: make(chan struct{})}
	request := openai.SpeechRequest{Text: sentence, Voice: "marin"}
	done := make(chan error, MaxSpeaking)
	for range MaxSpeaking {
		go func() {
			_, err := sentences.Synthesize(context.Background(), request, held.speak)
			done <- err
		}()
		<-held.entered
	}
	// Past the cap is refused at once, not queued behind the four.
	refused := make(chan error, 1)
	go func() {
		_, err := sentences.Synthesize(context.Background(), request, echo)
		refused <- err
	}()
	select {
	case err := <-refused:
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("a synthesis past the cap = %v, want ErrBusy", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a synthesis past the cap waited for a slot")
	}
	close(held.release)
	for range MaxSpeaking {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sentences.Synthesize(context.Background(), request, echo); err != nil {
		t.Fatalf("a synthesis once the slots freed: %v", err)
	}
}

func TestCancellingASynthesisEndsItAndFreesItsSlot(t *testing.T) {
	t.Parallel()
	sentences := New(time.Minute)
	held := blocking{entered: make(chan struct{}), release: make(chan struct{})}
	request := openai.SpeechRequest{Text: sentence, Voice: "marin"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, MaxSpeaking)
	for range MaxSpeaking {
		go func() {
			_, err := sentences.Synthesize(ctx, request, held.speak)
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
	if _, err := sentences.Synthesize(context.Background(), request, echo); err != nil {
		t.Fatalf("cancelled syntheses kept their slots: %v", err)
	}
}
