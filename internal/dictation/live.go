package dictation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Live sessions: a recording streamed to a live transcriber while it is
// spoken (internal/openai), rather than uploaded and transcribed at the end.
// The client appends 16-bit PCM pieces in order, each append answers with
// the text heard so far, and Finish ends the turn and returns its
// transcript. Until Finish returns the session still belongs to its client:
// Cancel, or the idle timer of a client that went away, closes it.
//
// Bounded like recordings: sessions open at once, bytes per piece, bytes per
// session, and idle time. Each open session holds a socket to the
// transcriber, so the idle bound is short; a client appends several times a
// second.

// LiveBytesPerSecond is the PCM rate a session takes: 24 kHz, 16-bit mono.
const LiveBytesPerSecond = 24000 * 2

// MaxLivePieceBytes caps one append: five seconds of audio, far more than a
// client buffers between appends on a slow link.
const MaxLivePieceBytes = 5 * LiveBytesPerSecond

// MaxLiveSessionBytes caps one session at the ten minutes a recording may
// last.
const MaxLiveSessionBytes = 10 * 60 * LiveBytesPerSecond

// MaxOpenLiveSessions caps sessions open at once on this backend.
const MaxOpenLiveSessions = 4

// LiveIdleTimeout closes a session no piece has reached for this long.
const LiveIdleTimeout = 30 * time.Second

// ErrUnknownLiveSession means the session was finished, abandoned or never
// opened here.
var ErrUnknownLiveSession = errors.New("dictation: live session is not open")

// LiveSwitchedOffCode is the wire code of a live recording refused or
// ended because live dictation is off, so a client can tell the switch's
// stop from a failure.
const LiveSwitchedOffCode = "live_dictation_switched_off"

// ErrLiveOff refuses a live recording while live dictation is off.
var ErrLiveOff error = liveOff{}

type liveOff struct{}

func (liveOff) Error() string {
	return "dictation: live dictation with the OpenAI API key is off on this computer"
}

func (liveOff) PublicDetails() (code, message string) {
	return LiveSwitchedOffCode, "Live dictation with the OpenAI API key is off on this computer; turn it on in Settings > Voice."
}

// ErrLiveSwitchedOff means live dictation was switched off after the
// session was admitted, which ended it.
var ErrLiveSwitchedOff error = liveSwitchedOff{}

type liveSwitchedOff struct{}

func (liveSwitchedOff) Error() string {
	return "dictation: live dictation was switched off, so this recording ended"
}

func (liveSwitchedOff) PublicDetails() (code, message string) {
	return LiveSwitchedOffCode, "Live dictation was switched off, so this recording ended."
}

// ErrLiveSessionFull means the session reached its ten minutes.
var ErrLiveSessionFull = errors.New("dictation: the recording reached its ten-minute limit")

// LiveTranscriber is one open transcription session.
type LiveTranscriber interface {
	Append(ctx context.Context, pcm []byte) error
	// Heard is the text transcribed so far, partial text included.
	Heard() string
	Finish(ctx context.Context) (string, error)
	Close()
}

// Live is the set of open live sessions.
//
// Switching live dictation off revokes the sessions admitted before the
// switch. Each session carries the settings revision it was admitted at,
// read before the switch was found on, and RevokeBefore ends every session
// admitted before the switch's write. Revisions order the writes however
// late their observers run: a quick off and on cannot spare an older
// session, and a session admitted after the switch came back on survives.
type Live struct {
	idle time.Duration

	mu   sync.Mutex
	open map[string]*liveSession
	// revokedBefore refuses sessions admitted before it, including one
	// still on its way into open.
	revokedBefore uint64
	// revoked holds what a revoked session still owes its client: the
	// release to run once the client hears why the session ended, or once
	// it has been idle as long as a session may be.
	revoked map[string]*revokedSession
}

type revokedSession struct {
	onRelease func()
	expiry    *time.Timer
}

type liveSession struct {
	// admitted is the settings revision read before the session was let in.
	admitted uint64
	// revoked marks a session RevokeBefore ended.
	revoked bool
	// transcriber is nil while the session is still connecting.
	transcriber LiveTranscriber
	// busy serializes appends and the finish, in the order they arrive.
	busy      sync.Mutex
	next      int
	bytes     int
	finishing bool
	stop      context.CancelFunc
	dropped   bool
	idle      *time.Timer
	onRelease func()
}

// NewLive returns an empty set whose sessions close after idle without a
// piece; zero means LiveIdleTimeout.
func NewLive(idle time.Duration) *Live {
	if idle <= 0 {
		idle = LiveIdleTimeout
	}
	return &Live{idle: idle, open: make(map[string]*liveSession), revoked: make(map[string]*revokedSession)}
}

// Open reserves a session admitted at settings revision admitted, connects
// it with dial outside the lock, and returns its id. A session cancelled or
// revoked while it connects is closed as soon as dial returns.
func (l *Live) Open(ctx context.Context, admitted uint64, dial func(context.Context) (LiveTranscriber, error)) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("dictation: mint live session id: %w", err)
	}
	id := hex.EncodeToString(raw[:])
	connecting, stop := context.WithCancel(ctx)
	defer stop()
	session := &liveSession{admitted: admitted, stop: stop}
	l.mu.Lock()
	if admitted < l.revokedBefore {
		l.mu.Unlock()
		return "", ErrLiveSwitchedOff
	}
	if len(l.open) >= MaxOpenLiveSessions {
		l.mu.Unlock()
		return "", errors.New("dictation: too many live recordings in progress on this computer")
	}
	l.open[id] = session
	l.mu.Unlock()

	transcriber, err := dial(connecting)
	l.mu.Lock()
	if err != nil || session.dropped {
		revoked := session.revoked
		release := l.dropLocked(id, session)
		l.mu.Unlock()
		runRelease(release)
		if transcriber != nil {
			transcriber.Close()
		}
		switch {
		case revoked:
			err = ErrLiveSwitchedOff
		case err == nil:
			err = ErrUnknownLiveSession
		}
		return "", err
	}
	session.transcriber = transcriber
	session.stop = nil
	session.idle = time.AfterFunc(l.idle, func() { l.expire(id, session) })
	l.mu.Unlock()
	return id, nil
}

// OnRelease sets what runs once the session leaves the set, however it
// does. When it is already gone it says why: ErrLiveSwitchedOff for one
// revoked meanwhile, which it then forgets, as its client now knows.
func (l *Live) OnRelease(id string, f func()) error {
	l.mu.Lock()
	if session, ok := l.open[id]; ok {
		session.onRelease = f
		l.mu.Unlock()
		return nil
	}
	_, revoked := l.revoked[id]
	release := l.forgetRevokedLocked(id)
	l.mu.Unlock()
	runRelease(release)
	if revoked {
		return ErrLiveSwitchedOff
	}
	return ErrUnknownLiveSession
}

// Append streams piece seq of the recording and returns the text heard so
// far. Pieces must arrive in order, as whole 16-bit samples.
func (l *Live) Append(ctx context.Context, id string, seq int, pcm []byte) (string, error) {
	switch {
	case len(pcm) == 0 || len(pcm)%2 != 0:
		return "", errors.New("dictation: a live piece must be whole 16-bit samples")
	case len(pcm) > MaxLivePieceBytes:
		return "", fmt.Errorf("dictation: a live piece may be at most %d bytes", MaxLivePieceBytes)
	}
	session, err := l.ready(id)
	if err != nil {
		return "", err
	}
	session.busy.Lock()
	defer session.busy.Unlock()
	l.mu.Lock()
	switch {
	case session.revoked:
		l.mu.Unlock()
		return "", ErrLiveSwitchedOff
	case session.dropped || session.finishing:
		l.mu.Unlock()
		return "", ErrUnknownLiveSession
	case seq != session.next:
		l.mu.Unlock()
		return "", fmt.Errorf("dictation: live piece %d arrived, expected %d", seq, session.next)
	case session.bytes+len(pcm) > MaxLiveSessionBytes:
		l.mu.Unlock()
		return "", ErrLiveSessionFull
	}
	session.next++
	session.bytes += len(pcm)
	session.idle.Reset(l.idle)
	l.mu.Unlock()
	if err := session.transcriber.Append(ctx, pcm); err != nil {
		l.mu.Lock()
		revoked := session.revoked
		l.mu.Unlock()
		if revoked {
			// Revoked mid-append: the closed socket is the revocation's.
			return "", ErrLiveSwitchedOff
		}
		l.Cancel(id)
		return "", err
	}
	return session.transcriber.Heard(), nil
}

// Finish ends the recording's turn and returns its transcript. Cancel
// stops it until it returns; either way the session then leaves the set.
func (l *Live) Finish(ctx context.Context, id string) (string, error) {
	session, err := l.ready(id)
	if err != nil {
		return "", err
	}
	session.busy.Lock()
	defer session.busy.Unlock()
	finishing, stop := context.WithCancel(ctx)
	defer stop()
	l.mu.Lock()
	if session.revoked {
		l.mu.Unlock()
		return "", ErrLiveSwitchedOff
	}
	if session.dropped || session.finishing {
		l.mu.Unlock()
		return "", ErrUnknownLiveSession
	}
	session.finishing = true
	session.stop = stop
	session.idle.Stop()
	l.mu.Unlock()

	transcript, err := session.transcriber.Finish(finishing)
	l.mu.Lock()
	cancelled, revoked := session.dropped, session.revoked
	release := l.dropLocked(id, session)
	l.mu.Unlock()
	session.transcriber.Close()
	runRelease(release)
	switch {
	case revoked:
		return "", ErrLiveSwitchedOff
	case cancelled:
		return "", ErrUnknownLiveSession
	}
	return transcript, err
}

// Cancel closes a session, stopping a connect or finish in progress.
// Cancelling one that is gone does nothing.
func (l *Live) Cancel(id string) {
	l.mu.Lock()
	session, ok := l.open[id]
	if !ok {
		release := l.forgetRevokedLocked(id)
		l.mu.Unlock()
		runRelease(release)
		return
	}
	if session.stop != nil {
		// Connecting or finishing: that call drops the session and closes
		// its transcriber when it returns.
		session.dropped = true
		session.stop()
		l.mu.Unlock()
		return
	}
	release := l.dropLocked(id, session)
	l.mu.Unlock()
	session.transcriber.Close()
	runRelease(release)
}

// RevokeBefore ends every session admitted before revision, the settings
// write that switched live dictation, and refuses any still to come in
// with an older admission. A session connecting or finishing ends when
// that call returns; the rest close now and answer their client's next
// call with ErrLiveSwitchedOff.
func (l *Live) RevokeBefore(revision uint64) {
	var closing []LiveTranscriber
	l.mu.Lock()
	if revision > l.revokedBefore {
		l.revokedBefore = revision
	}
	for id, session := range l.open {
		if session.admitted >= revision || session.dropped {
			continue
		}
		session.revoked = true
		if session.stop != nil {
			session.dropped = true
			session.stop()
			continue
		}
		release := l.dropLocked(id, session)
		l.revoked[id] = &revokedSession{
			onRelease: release,
			expiry:    time.AfterFunc(l.idle, func() { l.Cancel(id) }),
		}
		closing = append(closing, session.transcriber)
	}
	l.mu.Unlock()
	for _, transcriber := range closing {
		transcriber.Close()
	}
}

// forgetRevokedLocked removes a revoked session's record and returns its
// release. Held under mu.
func (l *Live) forgetRevokedLocked(id string) func() {
	revoked, ok := l.revoked[id]
	if !ok {
		return nil
	}
	delete(l.revoked, id)
	revoked.expiry.Stop()
	return revoked.onRelease
}

// ready returns an open, connected session that is not finishing, so a
// call refused here never waits behind the finish. A revoked session's
// client hears why it ended, once.
func (l *Live) ready(id string) (*liveSession, error) {
	l.mu.Lock()
	session, ok := l.open[id]
	if !ok {
		if _, revoked := l.revoked[id]; revoked {
			release := l.forgetRevokedLocked(id)
			l.mu.Unlock()
			runRelease(release)
			return nil, ErrLiveSwitchedOff
		}
		l.mu.Unlock()
		return nil, ErrUnknownLiveSession
	}
	defer l.mu.Unlock()
	if session.transcriber == nil || session.dropped || session.finishing {
		return nil, ErrUnknownLiveSession
	}
	return session, nil
}

func (l *Live) expire(id string, session *liveSession) {
	l.mu.Lock()
	if l.open[id] != session || session.finishing {
		l.mu.Unlock()
		return
	}
	release := l.dropLocked(id, session)
	l.mu.Unlock()
	session.transcriber.Close()
	runRelease(release)
}

// dropLocked removes the session and returns its release hook. Held under
// mu; the caller closes the transcriber and runs the hook outside it.
func (l *Live) dropLocked(id string, session *liveSession) func() {
	if l.open[id] == session {
		delete(l.open, id)
	}
	session.dropped = true
	if session.idle != nil {
		session.idle.Stop()
	}
	release := session.onRelease
	session.onRelease = nil
	return release
}
