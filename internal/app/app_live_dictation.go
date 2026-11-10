package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"agent-overflow/internal/dictation"
	"agent-overflow/internal/openai"
	"agent-overflow/internal/openaikey"
	"agent-overflow/internal/transport"
)

// Live dictation: with Settings > Voice's OpenAI API switch on and a key in
// its file, the composer streams the recording as 24 kHz PCM to the
// computer it sends to while the user speaks. That computer relays it to
// OpenAI with the key (internal/openai) and answers each piece with the
// text heard so far; stopping returns the whole transcript. Nothing is
// stored, and the key is read only to open each session.

const (
	// liveDictationConnectTimeout bounds opening a session with OpenAI.
	liveDictationConnectTimeout = 15 * time.Second
	// liveDictationFinishTimeout bounds waiting for a turn's transcript.
	liveDictationFinishTimeout = 30 * time.Second
)

// openAIClient is the client keyed OpenAI requests use. Its proxy comes
// from the environment, like every other provider request.
var openAIClient = &http.Client{}

func (a *App) openAIHTTPClient() *http.Client {
	if a.openAIClientOverride != nil {
		return a.openAIClientOverride
	}
	return openAIClient
}

// liveDictationOn is the Settings > Voice switch. Off by default: it sends
// recordings to OpenAI with this computer's key.
func (a *App) liveDictationOn() bool {
	return a.settings != nil && a.settings.BackendScreen().Get().DictationAPIEnabled
}

// liveDictationAvailable is whether the composer should offer live
// dictation: the switch is on and the key file holds a usable key.
func (a *App) liveDictationAvailable() bool {
	if !a.liveDictationOn() {
		return false
	}
	file, err := a.openAIKeyFile()
	return err == nil && file.State == openaikey.StatePresent
}

func (a *App) liveDictationOptions() openai.LiveTranscriptionOptions {
	current := a.settings.BackendScreen().Get()
	return openai.LiveTranscriptionOptions{
		Delay:     current.DictationAPIDelay,
		Languages: current.DictationAPILanguages,
		Keywords:  current.DictationAPIKeywords,
		Prompt:    current.DictationAPIPrompt,
	}
}

// BeginLiveDictation opens a live recording for the calling connection and
// returns its id. The recording is dropped when the connection ends.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) BeginLiveDictation(ctx context.Context) (string, error) {
	// The revision is read before the switch, so any write that switches
	// it after this check carries a later one and revokes this session
	// (dictation.Live).
	if a.settings == nil {
		return "", dictation.ErrLiveOff
	}
	admitted := a.settings.Revision()
	if !a.liveDictationOn() {
		return "", dictation.ErrLiveOff
	}
	options := a.liveDictationOptions()
	sessions := a.liveDictations()
	id, err := sessions.Open(ctx, admitted, func(connecting context.Context) (dictation.LiveTranscriber, error) {
		key, err := a.readOpenAIKey()
		if err != nil {
			return nil, err
		}
		defer key.Zero()
		connecting, cancel := context.WithTimeout(connecting, liveDictationConnectTimeout)
		defer cancel()
		session, err := openai.DialLiveTranscription(connecting, a.openAIHTTPClient(), key, options)
		if err != nil {
			return nil, err
		}
		return session, nil
	})
	if err != nil {
		return "", err
	}
	if state := transport.ConnStateFromContext(ctx); state != nil {
		key := liveDictationCleanupKey(id)
		if !state.BindCleanup(key, func() { sessions.Cancel(id) }) {
			sessions.Cancel(id)
			return "", errors.New("dictation: connection closing")
		}
		// The connection owns the session until it is released, however
		// that happens.
		if err := sessions.OnRelease(id, func() { state.UnbindCleanup(key) }); err != nil {
			state.UnbindCleanup(key)
			return "", err
		}
	}
	return id, nil
}

// AppendLiveDictation streams piece seq of a live recording, 16-bit PCM at
// 24 kHz, and returns the text heard so far.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) AppendLiveDictation(ctx context.Context, recordingID string, seq int, audio []byte) (string, error) {
	if err := ownsConnectionResource(ctx, liveDictationCleanupKey(recordingID), dictation.ErrUnknownLiveSession); err != nil {
		return "", err
	}
	// Switching live off revokes the session (the settings observer). The
	// switch is also read here, so a write the observer never heard of (a
	// hand edit, a write that failed after the host file) still stops it.
	if !a.liveDictationOn() {
		a.liveDictations().Cancel(recordingID)
		return "", dictation.ErrLiveSwitchedOff
	}
	return a.liveDictations().Append(ctx, recordingID, seq, audio)
}

// FinishLiveDictation ends a live recording and returns its transcript.
// Until it returns the recording still belongs to the caller's connection:
// CancelLiveDictation or the connection's end stops it.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) FinishLiveDictation(ctx context.Context, recordingID string) (string, error) {
	if err := ownsConnectionResource(ctx, liveDictationCleanupKey(recordingID), dictation.ErrUnknownLiveSession); err != nil {
		return "", err
	}
	if !a.liveDictationOn() {
		a.liveDictations().Cancel(recordingID)
		return "", dictation.ErrLiveSwitchedOff
	}
	finishing, cancel := context.WithTimeout(ctx, liveDictationFinishTimeout)
	defer cancel()
	return a.liveDictations().Finish(finishing, recordingID)
}

// CancelLiveDictation drops a live recording. Cancelling one that is gone
// does nothing.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) CancelLiveDictation(ctx context.Context, recordingID string) error {
	if err := ownsConnectionResource(ctx, liveDictationCleanupKey(recordingID), dictation.ErrUnknownLiveSession); err != nil {
		return err
	}
	a.liveDictations().Cancel(recordingID)
	return nil
}

func (a *App) liveDictations() *dictation.Live {
	a.liveDictationOnce.Do(func() { a.liveDictation = dictation.NewLive(0) })
	return a.liveDictation
}

func liveDictationCleanupKey(recordingID string) string {
	return "live-dictation:" + recordingID
}
