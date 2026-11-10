// Package readaloud admits sentences to be read aloud in an OpenAI voice
// and runs their synthesis.
//
// A bound method admits a sentence and gets an id; the client fetches the
// audio for that id once, and only then is the sentence spoken. Admission
// is bounded (MaxPending sentences waiting, each single-use and expiring),
// synthesis is capped (MaxSpeaking at once; another is refused, not
// queued), and a synthesis whose transcript is not a reading of the text
// (openai.IsReading) is retried once and then refused. A clip keeps its
// synthesis slot until it is closed, so the audio held for delivery is
// bounded by the same cap.
//
// Every sentence carries the settings revision it was admitted at.
// RevokeBefore, called with the revision of a write to the switch, drops
// the sentences admitted before it and ends their syntheses, so switching
// read aloud off stops the spending at once and a quick off and on again
// cannot spare an older admission.
//
// The caller supplies the speak function, so the key and the HTTP client
// stay with it.
package readaloud

import (
	"bytes"
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

// MaxSpeaking caps syntheses running, or holding a clip not yet closed, at
// once.
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

// OffCode is the wire code of ErrOff, so a client tells the switch from a
// failure whatever the switch says by the time it asks again.
const OffCode = "read_aloud_api_off"

// ErrOff refuses a sentence while the Settings > Voice switch is off, or
// once a write to the switch revoked it.
var ErrOff error = off{}

type off struct{}

func (off) Error() string {
	return "read aloud in OpenAI voices is off on this computer; turn it on in Settings > Voice"
}

func (o off) PublicDetails() (code, message string) { return OffCode, o.Error() }

// SpeakFunc reads one request aloud.
type SpeakFunc func(ctx context.Context, request openai.SpeechRequest) (openai.Speech, error)

// Sentence is one admitted sentence.
type Sentence struct {
	Request openai.SpeechRequest
	// Admitted is the settings revision the sentence was admitted at.
	Admitted uint64
}

// Clip is one sentence's audio as a WAV file. It holds its synthesis slot
// until closed.
type Clip struct {
	*bytes.Reader
	release func()
	once    sync.Once
}

// Close frees the clip's synthesis slot. Closing a closed clip does
// nothing.
func (c *Clip) Close() error {
	c.once.Do(c.release)
	return nil
}

// Sentences holds admitted sentences and the synthesis slots.
type Sentences struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	pending map[string]pendingSentence
	// speaking are the syntheses in progress.
	speaking map[*synthesis]struct{}
	// revokedBefore refuses every sentence admitted before it.
	revokedBefore uint64

	slots chan struct{}

	// beforeHandOver, when set by a test, runs once a clip is made and
	// before it is handed over.
	beforeHandOver func()
}

type pendingSentence struct {
	sentence Sentence
	expires  time.Time
}

type synthesis struct {
	admitted uint64
	stop     context.CancelFunc
	// revoked is set, under mu, by the RevokeBefore that stopped it.
	revoked bool
}

// New returns an empty set whose sentences expire ttl after admission.
func New(ttl time.Duration) *Sentences {
	return newWithClock(ttl, time.Now)
}

func newWithClock(ttl time.Duration, now func() time.Time) *Sentences {
	return &Sentences{
		ttl:      ttl,
		now:      now,
		pending:  map[string]pendingSentence{},
		speaking: map[*synthesis]struct{}{},
		slots:    make(chan struct{}, MaxSpeaking),
	}
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

// Admit holds a sentence to be read in voice, admitted at settings
// revision admitted, and returns its id.
func (s *Sentences) Admit(text, voice string, admitted uint64) (string, error) {
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
	if admitted < s.revokedBefore {
		return "", ErrOff
	}
	s.dropExpired(now)
	if len(s.pending) >= MaxPending {
		return "", ErrBacklog
	}
	s.pending[id] = pendingSentence{
		sentence: Sentence{Request: openai.SpeechRequest{Text: text, Voice: voice}, Admitted: admitted},
		expires:  now.Add(s.ttl),
	}
	return id, nil
}

// Take spends id and returns its sentence.
func (s *Sentences) Take(id string) (Sentence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExpired(s.now())
	held, ok := s.pending[id]
	if !ok {
		return Sentence{}, ErrUnknown
	}
	delete(s.pending, id)
	return held.sentence, nil
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
	for id, held := range s.pending {
		if !now.Before(held.expires) {
			delete(s.pending, id)
		}
	}
}

// RevokeBefore drops every waiting sentence admitted before revision, the
// settings write that changed the switch, ends every synthesis of one, and
// refuses any still to come in with an older admission. A synthesis it
// ends returns ErrOff, up to the moment its clip is handed over; a clip
// already handed over is not taken back.
func (s *Sentences) RevokeBefore(revision uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision > s.revokedBefore {
		s.revokedBefore = revision
	}
	for id, held := range s.pending {
		if held.sentence.Admitted < revision {
			delete(s.pending, id)
		}
	}
	for work := range s.speaking {
		if work.admitted < revision {
			work.revoked = true
			work.stop()
		}
	}
}

// Synthesize reads sentence aloud with speak, in a synthesis slot, and
// returns the clip once its transcript is a reading of the text; the
// caller closes the clip to free the slot. One result that is not a
// reading is retried; a second is ErrMisread. A sentence revoked before or
// during the synthesis is ErrOff.
func (s *Sentences) Synthesize(ctx context.Context, sentence Sentence, speak SpeakFunc) (*Clip, error) {
	select {
	case s.slots <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	release := func() { <-s.slots }
	clip, err := s.synthesize(ctx, sentence, speak, release)
	if err != nil {
		release()
		return nil, err
	}
	return clip, nil
}

func (s *Sentences) synthesize(ctx context.Context, sentence Sentence, speak SpeakFunc, release func()) (*Clip, error) {
	running, stop := context.WithCancel(ctx)
	defer stop()
	work := &synthesis{admitted: sentence.Admitted, stop: stop}
	s.mu.Lock()
	if sentence.Admitted < s.revokedBefore {
		s.mu.Unlock()
		return nil, ErrOff
	}
	s.speaking[work] = struct{}{}
	s.mu.Unlock()
	// finish takes the synthesis out of the set and reports whether a
	// RevokeBefore reached it first. Until it runs, a revocation still
	// counts.
	finish := func() (revoked bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.speaking, work)
		return work.revoked
	}
	for attempt := 0; attempt < 2; attempt++ {
		speech, err := speak(running, sentence.Request)
		// Revoked while speaking: its error is the cancellation, and a
		// result that is not a reading is not retried.
		reading := err == nil && openai.IsReading(sentence.Request.Text, speech.Transcript)
		if !reading {
			s.mu.Lock()
			revoked := work.revoked
			s.mu.Unlock()
			switch {
			case revoked:
				finish()
				return nil, ErrOff
			case err != nil:
				finish()
				return nil, err
			}
			continue
		}
		wav, err := WAV(speech.PCM, openai.SpeechSampleRate)
		if err != nil {
			finish()
			return nil, err
		}
		if s.beforeHandOver != nil {
			s.beforeHandOver()
		}
		if finish() {
			return nil, ErrOff
		}
		return &Clip{Reader: bytes.NewReader(wav), release: release}, nil
	}
	finish()
	return nil, ErrMisread
}
