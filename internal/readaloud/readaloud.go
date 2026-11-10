// Package readaloud admits sentences to be read aloud in an OpenAI voice
// and runs their synthesis.
//
// A bound method admits a sentence and gets an id; the client fetches the
// audio for that id once, and only then is the sentence spoken. Admission
// is bounded (MaxPending sentences waiting, each single-use and expiring),
// synthesis is capped (MaxSpeaking at once; another is refused, not
// queued), and a synthesis whose transcript is not a reading of the text
// (openai.IsReading) is retried once and then refused. The caller supplies
// the speak function, so the key and the HTTP client stay with it.
package readaloud

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/openai"
)

// MaxPending caps sentences admitted and not yet fetched.
const MaxPending = 8

// MaxSpeaking caps syntheses running at once.
const MaxSpeaking = 4

// MaxTextCharacters caps one sentence, counted after trimming.
const MaxTextCharacters = 1000

var (
	// ErrUnknown means the sentence was already fetched, expired, or was
	// never admitted here.
	ErrUnknown = errors.New("this sentence was already read aloud or has expired")
	// ErrBacklog refuses a sentence while MaxPending wait.
	ErrBacklog = fmt.Errorf("%d sentences are already waiting to be read aloud; try again shortly", MaxPending)
	// ErrBusy refuses a synthesis while MaxSpeaking run.
	ErrBusy = fmt.Errorf("this computer is already reading %d sentences aloud; try again shortly", MaxSpeaking)
	// ErrMisread means OpenAI twice spoke something other than the text.
	ErrMisread = errors.New("OpenAI did not read the text as written")
	// ErrNoText refuses an empty sentence.
	ErrNoText = errors.New("there is no text to read aloud")
	// ErrTextTooLong refuses a sentence over MaxTextCharacters.
	ErrTextTooLong = fmt.Errorf("a sentence to read aloud is at most %d characters", MaxTextCharacters)
)

// OffCode is the wire code of a sentence refused because read aloud in
// OpenAI voices is off, so a client can tell the switch from a failure.
const OffCode = "read_aloud_api_off"

// ErrOff refuses a sentence while the Settings > Voice switch is off,
// whether at admission or at the fetch.
var ErrOff error = off{}

type off struct{}

func (off) Error() string {
	return "Read aloud in OpenAI voices is off on this computer; turn it on in Settings > Voice."
}

func (o off) PublicDetails() (code, message string) { return OffCode, o.Error() }

// SpeakFunc reads one request aloud.
type SpeakFunc func(ctx context.Context, request openai.SpeechRequest) (openai.Speech, error)

// Sentences holds admitted sentences and the synthesis slots.
type Sentences struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	pending map[string]pendingSentence

	slots chan struct{}
}

type pendingSentence struct {
	request openai.SpeechRequest
	expires time.Time
}

// New returns an empty set whose sentences expire ttl after admission.
func New(ttl time.Duration) *Sentences {
	return newWithClock(ttl, time.Now)
}

func newWithClock(ttl time.Duration, now func() time.Time) *Sentences {
	return &Sentences{ttl: ttl, now: now, pending: map[string]pendingSentence{}, slots: make(chan struct{}, MaxSpeaking)}
}

// CheckText validates a sentence and returns it trimmed.
func CheckText(text string) (string, error) {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return "", ErrNoText
	case utf8.RuneCountInString(text) > MaxTextCharacters:
		return "", ErrTextTooLong
	}
	return text, nil
}

// Admit holds a sentence to be read in voice and returns its id.
func (s *Sentences) Admit(text, voice string) (string, error) {
	if !openai.KnownSpeechVoice(voice) {
		return "", fmt.Errorf("OpenAI has no voice %q", voice)
	}
	text, err := CheckText(text)
	if err != nil {
		return "", err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("make a read-aloud id: %w", err)
	}
	id := hex.EncodeToString(raw[:])
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExpired(now)
	if len(s.pending) >= MaxPending {
		return "", ErrBacklog
	}
	s.pending[id] = pendingSentence{request: openai.SpeechRequest{Text: text, Voice: voice}, expires: now.Add(s.ttl)}
	return id, nil
}

// Take spends id and returns its sentence.
func (s *Sentences) Take(id string) (openai.SpeechRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExpired(s.now())
	sentence, ok := s.pending[id]
	if !ok {
		return openai.SpeechRequest{}, ErrUnknown
	}
	delete(s.pending, id)
	return sentence.request, nil
}

// Pending counts the sentences waiting.
func (s *Sentences) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExpired(s.now())
	return len(s.pending)
}

// Held under mu.
func (s *Sentences) dropExpired(now time.Time) {
	for id, sentence := range s.pending {
		if !now.Before(sentence.expires) {
			delete(s.pending, id)
		}
	}
}

// Synthesize reads request aloud with speak, in a synthesis slot, and
// returns the speech once its transcript is a reading of the text. One
// result that is not a reading is retried; a second is ErrMisread.
func (s *Sentences) Synthesize(ctx context.Context, request openai.SpeechRequest, speak SpeakFunc) (openai.Speech, error) {
	select {
	case s.slots <- struct{}{}:
	default:
		return openai.Speech{}, ErrBusy
	}
	defer func() { <-s.slots }()
	for attempt := 0; attempt < 2; attempt++ {
		speech, err := speak(ctx, request)
		if err != nil {
			return openai.Speech{}, err
		}
		if openai.IsReading(request.Text, speech.Transcript) {
			return speech, nil
		}
	}
	return openai.Speech{}, ErrMisread
}
