package provideraccountapp

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// ErrNoCodexChatGPTLogin means this computer's Codex is not signed in with
// ChatGPT, so subscription dictation has nothing to authorize it.
var ErrNoCodexChatGPTLogin = errors.New("sign in to Codex with ChatGPT on this computer to dictate")

// ErrCodexLoginRefused means ChatGPT refused the Codex sign-in even after a
// token refresh.
var ErrCodexLoginRefused = errors.New("ChatGPT refused this computer's Codex sign-in; sign in to Codex again")

// CodexTranscription is one recording for TranscribeWithCodexLogin.
type CodexTranscription struct {
	Audio    []byte
	MimeType string
	// Endpoint is codex.TranscribeURL in production.
	Endpoint  string
	UserAgent string
}

// CodexDictationAvailable reports whether the canonical Codex credential is
// a ChatGPT sign-in that can authorize subscription dictation.
func (m *Manager) CodexDictationAvailable() (bool, error) {
	if !m.available() {
		return false, nil
	}
	credential, present, err := m.ReadCanonicalCredential(string(provider.Codex))
	if err != nil || !present {
		return false, err
	}
	return codex.HasChatGPTLogin(credential.Bytes()), nil
}

// TranscribeWithCodexLogin transcribes a recording with the canonical Codex
// ChatGPT sign-in, read fresh for each request. When ChatGPT refuses the
// access token, the Codex CLI refreshes it in the canonical home and the
// request is retried once.
func (m *Manager) TranscribeWithCodexLogin(ctx context.Context, request CodexTranscription) (string, error) {
	if !m.available() {
		return "", errors.New("provider account storage is unavailable")
	}
	endWork, err := m.beginWork(ctx)
	if err != nil {
		return "", err
	}
	defer endWork()

	text, err := m.transcribeOnce(ctx, request)
	if !errors.Is(err, codex.ErrTranscribeUnauthorized) {
		return text, err
	}
	if err := m.refreshCanonicalCodexToken(ctx); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	text, err = m.transcribeOnce(ctx, request)
	if errors.Is(err, codex.ErrTranscribeUnauthorized) {
		return "", ErrCodexLoginRefused
	}
	return text, err
}

func (m *Manager) transcribeOnce(ctx context.Context, request CodexTranscription) (string, error) {
	credential, present, err := m.ReadCanonicalCredential(string(provider.Codex))
	if err != nil {
		return "", fmt.Errorf("read the Codex sign-in: %w", err)
	}
	if !present {
		return "", ErrNoCodexChatGPTLogin
	}
	text, err := codex.Transcribe(ctx, m.providerHTTPClient(), codex.TranscribeRequest{
		Endpoint:   request.Endpoint,
		Credential: credential.Bytes(),
		Audio:      request.Audio,
		MimeType:   request.MimeType,
		UserAgent:  request.UserAgent,
	})
	if errors.Is(err, codex.ErrNoChatGPTLogin) {
		return "", ErrNoCodexChatGPTLogin
	}
	return text, err
}

// refreshCanonicalCodexToken has the Codex CLI refresh the canonical sign-in.
// It holds the Codex reconcile lock so no switch replaces the canonical
// credential while the CLI rotates it. The rotation reaches the selected
// account's slot the way any CLI rotation of the canonical credential does.
//
// A transcription abandoned before the lock is held starts no refresh. One
// that started runs to the end whatever happens to the caller: a refresh
// stopped after the server rotated the token, before the CLI saved it,
// would lose the sign-in.
func (m *Manager) refreshCanonicalCodexToken(ctx context.Context) error {
	mu := m.reconcileMutex(string(provider.Codex))
	mu.Lock()
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Another store's boot sweep can leave the canonical credential on a
	// seed whose rotation it recovered into the selected slot; refreshing
	// that seed would spend a consumed token.
	if err := m.promoteActiveSlot(string(provider.Codex)); err != nil {
		return fmt.Errorf("refresh the Codex sign-in for dictation: %w", err)
	}
	cfg := m.codexProbeConfig(m.providerBinaryPath(string(provider.Codex)), nil)
	if err := codex.RefreshAccountToken(context.WithoutCancel(ctx), cfg); err != nil {
		return fmt.Errorf("refresh the Codex sign-in for dictation: %w", err)
	}
	return nil
}
