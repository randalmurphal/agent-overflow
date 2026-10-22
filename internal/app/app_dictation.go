package app

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/dictation"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/transport"
)

// Dictation: the composer records speech with MediaRecorder, uploads it in
// pieces to the computer it sends to while it records, and on stop has
// that computer transcribe the recording with its ChatGPT sign-in for
// Codex. The text comes back to the composer; nothing is stored.

// DictationSources reports what this computer can dictate with.
type DictationSources struct {
	// Subscription is true when dictation through the Codex sign-in is on
	// here and Codex is signed in with ChatGPT: the composer offers a mic.
	Subscription bool `json:"subscription"`
	// ChatGPTSignIn is true when Codex is signed in with ChatGPT here,
	// whether or not dictation through it is on.
	ChatGPTSignIn bool `json:"chatGPTSignIn"`
}

// ErrDictationOff refuses a recording while no dictation source is on.
var ErrDictationOff = errors.New("dictation is off on this computer; turn it on in Settings > Voice")

// DictationSources answers whether the composer should offer dictation for
// messages sent to this computer.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) DictationSources() (DictationSources, error) {
	signedIn, err := a.providerAccounts.CodexDictationAvailable()
	if err != nil {
		return DictationSources{}, fmt.Errorf("read the Codex sign-in: %w", err)
	}
	return DictationSources{Subscription: signedIn && a.dictationSubscriptionOn(), ChatGPTSignIn: signedIn}, nil
}

// dictationSubscriptionOn is the Settings > Voice switch. Off by default:
// it sends recordings to OpenAI under this computer's Codex sign-in.
func (a *App) dictationSubscriptionOn() bool {
	return a.settings != nil && a.settings.BackendScreen().Get().DictationSubscriptionEnabled
}

// BeginDictation opens a recording of the given container type (audio/webm
// or audio/mp4) for the calling connection and returns its id. The
// recording is dropped when the connection ends.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) BeginDictation(ctx context.Context, mimeType string) (string, error) {
	if !a.dictationSubscriptionOn() {
		return "", ErrDictationOff
	}
	id, err := a.dictationRecordings().Begin(mimeType)
	if err != nil {
		return "", err
	}
	if state := transport.ConnStateFromContext(ctx); state != nil {
		key := dictationCleanupKey(id)
		if !state.BindCleanup(key, func() { a.dictationRecordings().Cancel(id) }) {
			a.dictationRecordings().Cancel(id)
			return "", errors.New("dictation: connection closing")
		}
		// The connection owns the recording until the recording is released,
		// however that happens.
		if !a.dictationRecordings().OnRelease(id, func() { state.UnbindCleanup(key) }) {
			state.UnbindCleanup(key)
			return "", dictation.ErrUnknownRecording
		}
	}
	return id, nil
}

// MintDictationChunkTicket returns the URL piece seq of an open recording
// is uploaded to.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) MintDictationChunkTicket(ctx context.Context, recordingID string, seq int, size int64) (string, error) {
	if err := ownsDictation(ctx, recordingID); err != nil {
		return "", err
	}
	if err := a.dictationRecordings().Admits(recordingID, seq, size); err != nil {
		return "", err
	}
	server := a.transportServer.Load()
	if server == nil {
		return "", errors.New("dictation: transport is not serving")
	}
	return server.MintDictationChunkTicket(recordingID, seq, size)
}

// FinishDictation closes a recording the caller uploaded pieces pieces of
// and returns its transcript. Until the transcription ends the recording
// still belongs to the caller's connection: CancelDictation or the
// connection's end stops it, and it counts against the recording caps.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) FinishDictation(ctx context.Context, recordingID string, pieces int) (string, error) {
	if err := ownsDictation(ctx, recordingID); err != nil {
		return "", err
	}
	job, err := a.dictationRecordings().Finish(ctx, recordingID, pieces)
	if err != nil {
		return "", err
	}
	defer job.Done()
	// Switched off while recording: the audio is dropped, never sent.
	if !a.dictationSubscriptionOn() {
		return "", ErrDictationOff
	}
	text, err := a.providerAccounts.TranscribeWithCodexLogin(job.Context, provideraccountapp.CodexTranscription{
		Audio:     job.Audio,
		MimeType:  job.MimeType,
		Endpoint:  codex.TranscribeURL,
		UserAgent: provider.CodexClientOrigin + "/" + a.version,
	})
	if err != nil {
		return "", fmt.Errorf("transcribe: %w", err)
	}
	return text, nil
}

// CancelDictation drops an open recording or stops its transcription.
// Cancelling one that is gone does nothing.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) CancelDictation(ctx context.Context, recordingID string) error {
	if err := ownsDictation(ctx, recordingID); err != nil {
		return err
	}
	a.dictationRecordings().Cancel(recordingID)
	return nil
}

func (a *App) dictationRecordings() *dictation.Recordings {
	a.dictationOnce.Do(func() { a.dictation = dictation.New(0) })
	return a.dictation
}

func dictationCleanupKey(recordingID string) string {
	return "dictation:" + recordingID
}

// ownsDictation refuses a recording another connection opened. An
// in-process caller has no connection and owns any recording.
func ownsDictation(ctx context.Context, recordingID string) error {
	state := transport.ConnStateFromContext(ctx)
	if state == nil || state.HasCleanup(dictationCleanupKey(recordingID)) {
		return nil
	}
	return dictation.ErrUnknownRecording
}

// DictationTransfer adapts the open recordings onto the transport's upload
// route. A package function, not a method, so it never becomes an RPC.
func DictationTransfer(a *App) transport.DictationTransfer { return dictationTransfer{app: a} }

type dictationTransfer struct{ app *App }

func (t dictationTransfer) AppendDictationChunk(chunk transport.DictationChunk) error {
	return t.app.dictationRecordings().Append(chunk.RecordingID, chunk.Seq, chunk.Size, chunk.Body)
}
