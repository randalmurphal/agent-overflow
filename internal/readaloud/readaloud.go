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
// Every sentence carries the settings revision it was admitted at, and its
// id leads with it. RevokeBefore, called with the revision of a write to
// the switch, drops the sentences admitted before it and ends their
// syntheses, so switching read aloud off stops the spending at once and a
// quick off and on again cannot spare an older admission. Each of those
// answers ErrOff, a dropped one from its id alone.
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
	"strconv"
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
	// never admitted here. One dropped by RevokeBefore is ErrOff.
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

	// beforeFinish, when set by a test, runs once a synthesis has its
	// result and before that result is decided against a revocation.
	beforeFinish func()
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
	id, err := newID(admitted)
	if err != nil {
		return "", err
	}
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

// idRevisionDigits and idRandomDigits are the two parts of an id, in hex:
// the admission revision, then random bytes.
const (
	idRevisionDigits = 16
	idRandomDigits   = 32
)

func newID(admitted uint64) (string, error) {
	var raw [idRandomDigits / 2]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("make a read-aloud id: %w", err)
	}
	return fmt.Sprintf("%0*x%s", idRevisionDigits, admitted, hex.EncodeToString(raw[:])), nil
}

// admittedAt reads the admission revision id leads with.
func admittedAt(id string) (uint64, bool) {
	if len(id) != idRevisionDigits+idRandomDigits {
		return 0, false
	}
	admitted, err := strconv.ParseUint(id[:idRevisionDigits], 16, 64)
	return admitted, err == nil
}

// Take spends id and returns its sentence. An id admitted before a
// RevokeBefore is ErrOff, known from the id, so the dropped sentence need
// not be kept to say why it is gone.
func (s *Sentences) Take(id string) (Sentence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if admitted, ok := admittedAt(id); ok && admitted < s.revokedBefore {
		return Sentence{}, ErrOff
	}
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
// reaches returns ErrOff whatever its own result, up to the moment that
// result is returned; a clip already handed over is not taken back.
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
// during the synthesis is ErrOff, also when every slot is taken.
func (s *Sentences) Synthesize(ctx context.Context, sentence Sentence, speak SpeakFunc) (*Clip, error) {
	running, stop := context.WithCancel(ctx)
	defer stop()
	work := &synthesis{admitted: sentence.Admitted, stop: stop}
	if err := s.begin(work); err != nil {
		return nil, err
	}
	release := func() { <-s.slots }

	wav, err := s.read(running, work, sentence.Request, speak)
	if s.beforeFinish != nil {
		s.beforeFinish()
	}
	// Leaving the set and learning whether a RevokeBefore got there first
	// are one step, so a revocation up to here decides every result, a
	// clip and a failure alike.
	s.mu.Lock()
	delete(s.speaking, work)
	revoked := work.revoked
	s.mu.Unlock()
	if revoked {
		err = ErrOff
	}
	if err != nil {
		release()
		return nil, err
	}
	return &Clip{Reader: bytes.NewReader(wav), release: release}, nil
}

// begin takes a synthesis slot for work and adds it to the set. The
// revocation is checked first, so a revoked admission is ErrOff and not
// ErrBusy when every slot is taken.
func (s *Sentences) begin(work *synthesis) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if work.admitted < s.revokedBefore {
		return ErrOff
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return ErrBusy
	}
	s.speaking[work] = struct{}{}
	return nil
}

// read asks speak for the request until the transcript is a reading of its
// text, twice at most, and returns the reading as a WAV file.
func (s *Sentences) read(ctx context.Context, work *synthesis, request openai.SpeechRequest, speak SpeakFunc) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		speech, err := speak(ctx, request)
		if err != nil {
			return nil, err
		}
		if openai.IsReading(request.Text, speech.Transcript) {
			return WAV(speech.PCM, openai.SpeechSampleRate)
		}
		// A revoked sentence is not asked for a second time.
		s.mu.Lock()
		revoked := work.revoked
		s.mu.Unlock()
		if revoked {
			return nil, ErrOff
		}
	}
	return nil, ErrMisread
}
