package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/logging"
	"agent-overflow/internal/provider"
)

// TranscribeURL is ChatGPT's speech-to-text endpoint, the one the Codex
// desktop app's dictation uses. It takes the recording as one multipart
// `file` part and answers `{"text": ...}`, authorized by a ChatGPT sign-in's
// access token and workspace like every other ChatGPT backend call. OpenAI
// keeps the uploaded recording for 30 days.
const TranscribeURL = "https://chatgpt.com/backend-api/transcribe"

// transcribeTimeout bounds one transcription: the upload and the model's
// pass over at most a few minutes of speech.
const transcribeTimeout = 90 * time.Second

// maxTranscriptBytes bounds the response read. A transcript of the longest
// recording AO sends is tens of kilobytes.
const maxTranscriptBytes = 1 << 20

// ErrTranscribeUnauthorized means ChatGPT refused the sign-in's access
// token. A token refresh may fix it.
var ErrTranscribeUnauthorized = errors.New("codex: ChatGPT refused the sign-in for transcription")

// ErrNoChatGPTLogin means the credential is not a ChatGPT sign-in (an API
// key, another auth mode, or no tokens), so it cannot authorize ChatGPT's
// backend.
var ErrNoChatGPTLogin = errors.New("codex: the Codex sign-in is not a ChatGPT login")

// TranscribeRateLimitedError is a 429 from the endpoint.
type TranscribeRateLimitedError struct {
	RetryAfter time.Duration
}

func (e *TranscribeRateLimitedError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("codex: ChatGPT is limiting transcription requests (retry after %s)", e.RetryAfter.Round(time.Second))
	}
	return "codex: ChatGPT is limiting transcription requests"
}

// HasChatGPTLogin reports whether Codex `auth.json` bytes hold a ChatGPT
// sign-in that can authorize Transcribe.
func HasChatGPTLogin(credential []byte) bool {
	accessToken, accountID, ok := chatGPTRequestAuth(credential)
	return ok && accessToken != "" && accountID != ""
}

// chatGPTRequestAuth reads the access token and workspace a ChatGPT backend
// request carries. The token stays a local value of its caller.
func chatGPTRequestAuth(credential []byte) (accessToken, accountID string, ok bool) {
	var authFile struct {
		Tokens *struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(credential, &authFile); err != nil || authFile.Tokens == nil {
		return "", "", false
	}
	accountID, ok = CredentialOrgID(credential)
	accessToken = strings.TrimSpace(authFile.Tokens.AccessToken)
	return accessToken, accountID, ok && accessToken != ""
}

// TranscribeRequest is one recording to transcribe.
type TranscribeRequest struct {
	// Endpoint is TranscribeURL in production; tests point it at a fake.
	Endpoint string
	// Credential is the Codex `auth.json` whose sign-in authorizes the call.
	Credential []byte
	Audio      []byte
	// MimeType is the recording's container type as the browser's
	// MediaRecorder reported it (audio/webm or audio/mp4), parameters
	// stripped.
	MimeType string
	// UserAgent identifies the caller, as Codex's own requests do.
	UserAgent string
}

// Transcribe sends one recording to ChatGPT and returns its text.
func Transcribe(ctx context.Context, client *http.Client, request TranscribeRequest) (string, error) {
	accessToken, accountID, ok := chatGPTRequestAuth(request.Credential)
	if !ok {
		return "", ErrNoChatGPTLogin
	}
	extension, err := audioFileExtension(request.MimeType)
	if err != nil {
		return "", err
	}
	// The form's framing is built around the audio, not copied with it, so
	// a transcription holds one copy of the recording.
	var framing bytes.Buffer
	form := multipart.NewWriter(&framing)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="audio.`+extension+`"`)
	header.Set("Content-Type", request.MimeType)
	if _, err := form.CreatePart(header); err != nil {
		return "", fmt.Errorf("codex: build transcription request: %w", err)
	}
	headLen := framing.Len()
	if err := form.Close(); err != nil {
		return "", fmt.Errorf("codex: build transcription request: %w", err)
	}
	head, tail := framing.Bytes()[:headLen], framing.Bytes()[headLen:]
	newBody := func() io.Reader {
		return io.MultiReader(bytes.NewReader(head), bytes.NewReader(request.Audio), bytes.NewReader(tail))
	}

	ctx, cancel := context.WithTimeout(ctx, transcribeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, request.Endpoint, newBody())
	if err != nil {
		return "", fmt.Errorf("codex: build transcription request: %w", err)
	}
	req.ContentLength = int64(len(head) + len(request.Audio) + len(tail))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(newBody()), nil }
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("ChatGPT-Account-Id", accountID)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("originator", provider.CodexClientOrigin)
	if request.UserAgent != "" {
		req.Header.Set("User-Agent", request.UserAgent)
	}
	resp, err := client.Do(logging.TraceHTTPRequest(req, "codex-transcribe"))
	if err != nil {
		return "", fmt.Errorf("codex: transcription request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTranscriptBytes+1))
	if err != nil {
		return "", fmt.Errorf("codex: read transcription response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", ErrTranscribeUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", &TranscribeRateLimitedError{RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return "", fmt.Errorf("codex: transcription failed with HTTP %d", resp.StatusCode)
	case len(data) > maxTranscriptBytes:
		return "", errors.New("codex: transcription response exceeds the size limit")
	}
	var answer struct {
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(data, &answer); err != nil || answer.Text == nil {
		return "", errors.New("codex: transcription response has no text")
	}
	return strings.TrimSpace(*answer.Text), nil
}

// audioFileExtension names the upload the way the endpoint expects: the
// container decides how it is decoded.
func audioFileExtension(mimeType string) (string, error) {
	switch mimeType {
	case "audio/webm":
		return "webm", nil
	case "audio/mp4":
		return "mp4", nil
	case "audio/ogg":
		return "ogg", nil
	case "audio/wav":
		return "wav", nil
	default:
		return "", fmt.Errorf("codex: unsupported recording type %q", mimeType)
	}
}

func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if wait := time.Until(at); wait > 0 {
			return wait
		}
	}
	return 0
}
