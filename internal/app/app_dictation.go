package app

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/dictation"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/transport"
)

// Dictation: the composer records speech with MediaRecorder, uploads it in
// pieces to the computer it sends to while it records, and on stop has
// that computer transcribe the recording with its ChatGPT sign-in for
// Codex. The text comes back to the composer; nothing is stored.

// DictationSources reports what this computer can dictate with. With
// either source on, the composer offers a mic.
type DictationSources struct {
	// Subscription is true when dictation through the Codex sign-in is on
	// here and Codex is signed in with ChatGPT.
	Subscription bool `json:"subscription"`
	// Live is true when live dictation through the OpenAI API key is on
	// here and the key file holds a usable key (app_live_dictation.go).
	Live bool `json:"live"`
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
	return DictationSources{
		Subscription:  signedIn && a.dictationSubscriptionOn(),
		Live:          a.liveDictationAvailable(),
		ChatGPTSignIn: signedIn,
	}, nil
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
	conn, err := callerConnection(ctx)
	if err != nil {
		return "", err
	}
	if a.settings == nil {
		return "", ErrDictationOff
	}
	// Read before the switch, so a write that lands between the two
	// revokes this recording rather than slipping past it.
	admitted := a.settings.Revision()
	if !a.dictationSubscriptionOn() {
		return "", ErrDictationOff
	}
	id, err := a.dictationRecordings().Begin(mimeType, admitted)
	if err != nil {
		return "", dictationSwitchedOff(err)
	}
	key := dictationCleanupKey(id)
	if err := conn.bindRelease(key, func() { a.dictationRecordings().Cancel(id) }); err != nil {
		return "", fmt.Errorf("dictation: %w", err)
	}
	// The connection owns the recording until the recording is released,
	// however that happens.
	if !a.dictationRecordings().OnRelease(id, func() { conn.state.UnbindCleanup(key) }) {
		conn.state.UnbindCleanup(key)
		return "", dictation.ErrUnknownRecording
	}
	return id, nil
}

// MintDictationChunkTicket returns the URL piece seq of an open recording
// is uploaded to.
//
//ao:scope threads:operate
//ao:route selected
//ao:callerbound
func (a *App) MintDictationChunkTicket(ctx context.Context, recordingID string, seq int, size int64) (string, error) {
	if err := ownsConnectionResource(ctx, dictationCleanupKey(recordingID), dictation.ErrUnknownRecording); err != nil {
		return "", err
	}
	if err := a.dictationRecordings().Admits(recordingID, seq, size); err != nil {
		return "", dictationSwitchedOff(err)
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
// connection's end stops it, and it counts against the recording caps. It
// returns within dictation.TranscribeBudget.
//
//ao:scope threads:operate
//ao:route selected
//ao:budget 3m
func (a *App) FinishDictation(ctx context.Context, recordingID string, pieces int) (string, error) {
	if err := ownsConnectionResource(ctx, dictationCleanupKey(recordingID), dictation.ErrUnknownRecording); err != nil {
		return "", err
	}
	job, err := a.dictationRecordings().Finish(ctx, recordingID, pieces)
	if err != nil {
		return "", dictationSwitchedOff(err)
	}
	defer job.Done()
	// Switched off while recording: the audio is dropped, never sent. A
	// write to the switch from here on ends the transcription and its retry
	// (app_settings_broadcast.go).
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
		return "", dictationFailure(job.Context, err)
	}
	return text, nil
}

// The wire codes of the transcription failures the mic shows in words of
// their own.
const (
	dictationTranscriptionRefusedCode = "dictation_transcription_refused"
	dictationTranscriptionTimeoutCode = "dictation_transcription_timeout"
)

// dictationFailure is a failed transcription as the mic shows it. job is
// the transcription's context, to tell its budget running out from the
// other ways it ends.
func dictationFailure(job context.Context, err error) error {
	switch {
	case errors.Is(context.Cause(job), dictation.ErrSwitchedOff):
		return ErrDictationOff
	case errors.Is(context.Cause(job), dictation.ErrTranscribeBudget):
		return errorsx.Public(dictationTranscriptionTimeoutCode,
			"ChatGPT took too long to transcribe the recording, so it was stopped. Try again, or dictate in shorter parts.", err)
	case errors.Is(err, codex.ErrTranscribeForbidden):
		return errorsx.Public(dictationTranscriptionRefusedCode,
			"ChatGPT refused to transcribe the recording (HTTP 403). Signing in again will not change that; dictation through ChatGPT may be unavailable for now.", err)
	}
	return fmt.Errorf("transcribe: %w", err)
}

// dictationSwitchedOff answers a recording a write to the switch ended as
// the switch being off.
func dictationSwitchedOff(err error) error {
	if errors.Is(err, dictation.ErrSwitchedOff) {
		return ErrDictationOff
	}
	return err
}

// CancelDictation drops an open recording or stops its transcription.
// Cancelling one that is gone does nothing.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) CancelDictation(ctx context.Context, recordingID string) error {
	if err := ownsConnectionResource(ctx, dictationCleanupKey(recordingID), dictation.ErrUnknownRecording); err != nil {
		return err
	}
	a.dictationRecordings().Cancel(recordingID)
	return nil
}

func (a *App) dictationRecordings() *dictation.Recordings {
	a.dictationOnce.Do(func() { a.dictation = dictation.New(0, 0) })
	return a.dictation
}

func dictationCleanupKey(recordingID string) string {
	return "dictation:" + recordingID
}

// ownsConnectionResource refuses a recording the calling connection did not
// open, answering unknown as if it were gone. A call that came over no
// connection owns none: one cannot open a recording.
func ownsConnectionResource(ctx context.Context, cleanupKey string, unknown error) error {
	if transport.ConnStateFromContext(ctx).HasCleanup(cleanupKey) {
		return nil
	}
	return unknown
}

// DictationTransfer adapts the open recordings onto the transport's upload
// route. A package function, not a method, so it never becomes an RPC.
func DictationTransfer(a *App) transport.DictationTransfer { return dictationTransfer{app: a} }

type dictationTransfer struct{ app *App }

func (t dictationTransfer) AppendDictationChunk(chunk transport.DictationChunk) error {
	return t.app.dictationRecordings().Append(chunk.RecordingID, chunk.Seq, chunk.Size, chunk.Body)
}
