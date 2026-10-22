package codex

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// transcribeEndpoint answers in-process, so no request leaves the test.
type transcribeEndpoint func(*http.Request) *http.Response

func (f transcribeEndpoint) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

func transcribeAnswer(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func chatGPTAuthFile(t *testing.T, accessToken string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"tokens": map[string]any{
		"access_token": accessToken,
		"id_token":     encodeIDToken(t, map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "ws-1"}}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The request is the one ChatGPT's own clients send: the recording as one
// multipart `file` part named for its container, authorized by the
// sign-in's access token and workspace, and identified as AO.
func TestTranscribeSendsTheRecordingAsChatGPTExpects(t *testing.T) {
	t.Parallel()
	var seen *http.Request
	var part struct {
		name, filename, contentType string
		body                        []byte
	}
	client := &http.Client{Transport: transcribeEndpoint(func(req *http.Request) *http.Response {
		seen = req
		_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("content type: %v", err)
			return transcribeAnswer(http.StatusBadRequest, "", nil)
		}
		reader := multipart.NewReader(req.Body, params["boundary"])
		p, err := reader.NextPart()
		if err != nil {
			t.Errorf("first part: %v", err)
			return transcribeAnswer(http.StatusBadRequest, "", nil)
		}
		part.name, part.filename, part.contentType = p.FormName(), p.FileName(), p.Header.Get("Content-Type")
		part.body, _ = io.ReadAll(p)
		if _, err := reader.NextPart(); !errors.Is(err, io.EOF) {
			t.Errorf("a second part was sent: %v", err)
		}
		return transcribeAnswer(http.StatusOK, `{"text":"  hello there \n"}`, nil)
	})}

	text, err := Transcribe(t.Context(), client, TranscribeRequest{
		Endpoint:   TranscribeURL,
		Credential: chatGPTAuthFile(t, "access-1"),
		Audio:      []byte("webm-bytes"),
		MimeType:   "audio/webm",
		UserAgent:  "agent_overflow/1.2.3",
	})
	if err != nil || text != "hello there" {
		t.Fatalf("Transcribe = %q, %v", text, err)
	}
	if seen.Method != http.MethodPost || seen.URL.String() != TranscribeURL {
		t.Fatalf("request = %s %s", seen.Method, seen.URL)
	}
	for header, want := range map[string]string{
		"Authorization":      "Bearer access-1",
		"ChatGPT-Account-Id": "ws-1",
		"originator":         provider.CodexClientOrigin,
		"User-Agent":         "agent_overflow/1.2.3",
	} {
		if got := seen.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if part.name != "file" || part.filename != "audio.webm" || part.contentType != "audio/webm" || string(part.body) != "webm-bytes" {
		t.Fatalf("part = %q %q %q %q", part.name, part.filename, part.contentType, part.body)
	}
}

func TestTranscribeAnswers(t *testing.T) {
	t.Parallel()
	credential := chatGPTAuthFile(t, "secret-access-token")
	for _, tc := range []struct {
		name   string
		answer *http.Response
		check  func(t *testing.T, err error)
	}{
		{name: "token refused", answer: transcribeAnswer(http.StatusUnauthorized, `{}`, nil), check: func(t *testing.T, err error) {
			if !errors.Is(err, ErrTranscribeUnauthorized) {
				t.Fatalf("err = %v, want ErrTranscribeUnauthorized", err)
			}
		}},
		{name: "workspace forbidden", answer: transcribeAnswer(http.StatusForbidden, `{}`, nil), check: func(t *testing.T, err error) {
			if !errors.Is(err, ErrTranscribeUnauthorized) {
				t.Fatalf("err = %v, want ErrTranscribeUnauthorized", err)
			}
		}},
		{name: "rate limited", answer: transcribeAnswer(http.StatusTooManyRequests, `{}`, http.Header{"Retry-After": {"7"}}), check: func(t *testing.T, err error) {
			var limited *TranscribeRateLimitedError
			if !errors.As(err, &limited) || limited.RetryAfter != 7*time.Second {
				t.Fatalf("err = %v, want a 7s rate limit", err)
			}
		}},
		{name: "server error", answer: transcribeAnswer(http.StatusInternalServerError, `secret-access-token`, nil), check: func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
				t.Fatalf("err = %v, want the status", err)
			}
		}},
		{name: "no text", answer: transcribeAnswer(http.StatusOK, `{"detail":"x"}`, nil), check: func(t *testing.T, err error) {
			if err == nil {
				t.Fatal("an answer without text was accepted")
			}
		}},
		{name: "oversized", answer: transcribeAnswer(http.StatusOK, `{"text":"`+strings.Repeat("a", maxTranscriptBytes)+`"}`, nil), check: func(t *testing.T, err error) {
			if err == nil {
				t.Fatal("an oversized answer was accepted")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &http.Client{Transport: transcribeEndpoint(func(*http.Request) *http.Response { return tc.answer })}
			_, err := Transcribe(t.Context(), client, TranscribeRequest{
				Endpoint: TranscribeURL, Credential: credential, Audio: []byte("a"), MimeType: "audio/mp4",
			})
			tc.check(t, err)
			if err != nil && strings.Contains(err.Error(), "secret-access-token") {
				t.Fatalf("error carries the access token: %v", err)
			}
		})
	}
}

// Nothing is sent without a ChatGPT sign-in to authorize it or a container
// the endpoint decodes.
func TestTranscribeSendsNothingItCannotAuthorize(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: transcribeEndpoint(func(*http.Request) *http.Response {
		t.Error("a request was sent")
		return transcribeAnswer(http.StatusOK, `{"text":"x"}`, nil)
	})}
	apiKey := []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-test"}`)
	if HasChatGPTLogin(apiKey) {
		t.Fatal("an API-key credential reads as a ChatGPT sign-in")
	}
	if _, err := Transcribe(t.Context(), client, TranscribeRequest{Endpoint: TranscribeURL, Credential: apiKey, Audio: []byte("a"), MimeType: "audio/webm"}); !errors.Is(err, ErrNoChatGPTLogin) {
		t.Fatalf("API-key credential err = %v, want ErrNoChatGPTLogin", err)
	}
	if _, err := Transcribe(t.Context(), client, TranscribeRequest{Endpoint: TranscribeURL, Credential: chatGPTAuthFile(t, "a"), Audio: []byte("a"), MimeType: "video/x-flv"}); err == nil {
		t.Fatal("an unsupported container was sent")
	}
	if !HasChatGPTLogin(chatGPTAuthFile(t, "a")) {
		t.Fatal("a ChatGPT sign-in does not read as one")
	}
}
