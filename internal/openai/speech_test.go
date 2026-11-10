package openai_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/openai"
	"agent-overflow/internal/openai/openaifake"
	"agent-overflow/internal/secret"
)

func speakWith(t *testing.T, client *http.Client, key string, request openai.SpeechRequest) (openai.Speech, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return speak(ctx, client, key, request)
}

// speak dials, zeroes the key as a caller does, and reads request aloud.
func speak(ctx context.Context, client *http.Client, key string, request openai.SpeechRequest) (openai.Speech, error) {
	secretKey := secret.New([]byte(key))
	session, err := openai.DialSpeech(ctx, client, secretKey, request.Voice)
	secretKey.Zero()
	if err != nil {
		return openai.Speech{}, err
	}
	defer session.Close()
	return session.Speak(ctx, request.Text)
}

func TestSpeakReturnsTheVoicesAudioAndTranscript(t *testing.T) {
	t.Parallel()
	const key = "sk-proj-speech-success"
	const text = "The build finished in forty seconds, and all twelve checks passed."
	speech, err := speakWith(t, fakeClient(), key, openai.SpeechRequest{Text: text, Voice: "cedar"})
	if err != nil {
		t.Fatal(err)
	}
	if speech.Transcript != text {
		t.Fatalf("transcript = %q, want the text", speech.Transcript)
	}
	if want := openaifake.SpeechAudio(text); !bytes.Equal(speech.PCM, want) {
		t.Fatalf("PCM is %d bytes, want the fake's %d", len(speech.PCM), len(want))
	}
	asks := openaifake.SpeechAsks(key)
	if len(asks) != 1 || asks[0] != (openaifake.SpeechAsk{Voice: "cedar", Text: text}) {
		t.Fatalf("the fake was asked %+v", asks)
	}
}

// Every way OpenAI can fail a reading reaches the caller as an error, with
// nothing key-shaped in it.
func TestSpeakReportsEveryFailure(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, key, text string
		is              error
		says            string
	}{
		{"refused key", openaifake.RefusedKey, "Hello there.", openai.ErrKeyRefused, ""},
		{"error event", testKey, "Hello " + openaifake.SpeechFails, nil, "OpenAI could not read the text aloud: The server had an error processing key [key]."},
		{"incomplete", testKey, "Hello " + openaifake.SpeechIncomplete, nil, "OpenAI stopped reading aloud (incomplete): max_output_tokens"},
		{"past the cap", testKey, "Hello " + openaifake.SpeechRunsLong, openai.ErrSpeechTooLong, ""},
	} {
		_, err := speakWith(t, fakeClient(), c.key, openai.SpeechRequest{Text: c.text, Voice: openai.DefaultSpeechVoice})
		switch {
		case err == nil:
			t.Errorf("%s: Speak succeeded", c.name)
		case c.is != nil && !errors.Is(err, c.is):
			t.Errorf("%s: Speak = %v, want %v", c.name, err, c.is)
		case c.says != "" && err.Error() != c.says:
			t.Errorf("%s: Speak = %q, want %q", c.name, err, c.says)
		case strings.Contains(err.Error(), "sk-"):
			t.Errorf("%s: %q carries something key-shaped", c.name, err)
		}
	}
}

// An answer instead of a reading is still a successful response here;
// judging it is the caller's (IsReading).
func TestSpeakReturnsWhatWasSpokenEvenWhenItIsNotTheText(t *testing.T) {
	t.Parallel()
	text := "Please read this " + openaifake.SpeechAnswers
	speech, err := speakWith(t, fakeClient(), testKey, openai.SpeechRequest{Text: text, Voice: "ash"})
	if err != nil {
		t.Fatal(err)
	}
	if speech.Transcript != openaifake.SpeechAnswer || openai.IsReading(text, speech.Transcript) {
		t.Fatalf("transcript = %q", speech.Transcript)
	}
}

func TestSpeakRefusesAnUnknownVoiceWithoutConnecting(t *testing.T) {
	t.Parallel()
	transport := &keyedTransport{answer: func(*http.Request) *http.Response { panic("dialed") }}
	for _, voice := range []string{"", "nova", "Marin"} {
		if _, err := speakWith(t, &http.Client{Transport: transport}, testKey, openai.SpeechRequest{Text: "Hi.", Voice: voice}); err == nil {
			t.Fatalf("voice %q was accepted", voice)
		}
	}
	if transport.keyed != 0 {
		t.Fatalf("an unknown voice dialed %d times", transport.keyed)
	}
}

func TestTheVoiceListHasTheDefaultAndNamesEveryVoice(t *testing.T) {
	t.Parallel()
	voices := openai.SpeechVoices()
	if len(voices) != 10 || !openai.KnownSpeechVoice(openai.DefaultSpeechVoice) {
		t.Fatalf("voices = %+v", voices)
	}
	for _, voice := range voices {
		if voice.ID == "" || voice.Name == "" || strings.ToLower(voice.Name) != voice.ID {
			t.Fatalf("voice %+v", voice)
		}
	}
	voices[0].ID = "changed"
	if openai.SpeechVoices()[0].ID != "alloy" {
		t.Fatal("SpeechVoices hands out the package's own list")
	}
}

func TestASpeechHandshakeRedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	var answered atomic.Int32
	transport := &keyedTransport{answer: func(req *http.Request) *http.Response {
		if answered.Add(1) > 1 {
			// A followed redirect ends here rather than looping.
			return &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Header: http.Header{}, Body: http.NoBody, Request: req}
		}
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Status:     "307 Temporary Redirect",
			Header:     http.Header{"Location": {"http://api.openai.com/v1/realtime?model=" + openai.SpeechModel}},
			Body:       http.NoBody,
			Request:    req,
		}
	}}
	_, err := speakWith(t, &http.Client{Transport: transport}, testKey, openai.SpeechRequest{Text: "Hi.", Voice: "marin"})
	if err == nil {
		t.Fatal("a redirected handshake connected")
	}
	if transport.keyed != 1 {
		t.Fatalf("the key was sent %d times; the redirect must not be followed", transport.keyed)
	}
}

func TestSpeakSaysRateLimitsAndStatusesInItsOwnWords(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]string{
		http.StatusTooManyRequests: openai.ErrRateLimited.Error(),
		http.StatusForbidden:       openai.ErrKeyRefused.Error(),
		http.StatusBadGateway:      "connect to OpenAI speech: 502 Bad Gateway",
	} {
		transport := &keyedTransport{answer: func(req *http.Request) *http.Response {
			return &http.Response{StatusCode: status, Status: "999 key " + testKey, Header: http.Header{}, Body: http.NoBody, Request: req}
		}}
		_, err := speakWith(t, &http.Client{Transport: transport}, testKey, openai.SpeechRequest{Text: "Hi.", Voice: "marin"})
		if err == nil || err.Error() != want {
			t.Fatalf("status %d: Speak = %v, want %q", status, err, want)
		}
	}
}

// headerAudit counts every place the key appears across the requests made,
// headers and URL alike.
type headerAudit struct {
	mu       sync.Mutex
	key      string
	carriers []string
}

func (h *headerAudit) RoundTrip(req *http.Request) (*http.Response, error) {
	h.mu.Lock()
	if strings.Contains(req.URL.String(), h.key) {
		h.carriers = append(h.carriers, "URL")
	}
	for name, values := range req.Header {
		for _, value := range values {
			if strings.Contains(value, h.key) {
				h.carriers = append(h.carriers, name)
			}
		}
	}
	h.mu.Unlock()
	return openaifake.Transport{}.RoundTrip(req)
}

func TestTheKeyTravelsOnlyInTheAuthorizationHeader(t *testing.T) {
	t.Parallel()
	const key = "sk-proj-speech-header-audit"
	audit := &headerAudit{key: key}
	client := &http.Client{Transport: audit}
	if _, err := speakWith(t, client, key, openai.SpeechRequest{Text: "Hello there.", Voice: "marin"}); err != nil {
		t.Fatal(err)
	}
	_, err := speakWith(t, client, key, openai.SpeechRequest{Text: "Hello " + openaifake.SpeechFails, Voice: "marin"})
	if err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("failure = %v", err)
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if len(audit.carriers) != 2 || audit.carriers[0] != "Authorization" || audit.carriers[1] != "Authorization" {
		t.Fatalf("the key was carried in %v; want one Authorization header per request", audit.carriers)
	}
}

// Cancelling the caller's context ends a reading OpenAI never answers, and
// leaves no goroutine or open session behind. Counts process-wide
// goroutines, so it runs serially.
func TestCancellingSpeakLeavesNothingRunning(t *testing.T) {
	const key = "sk-proj-speech-cancel"
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := speak(ctx, fakeClient(), key, openai.SpeechRequest{Text: "Wait " + openaifake.SpeechStalls, Voice: "marin"})
		done <- err
	}()
	eventually(t, "the stalled response", func() bool { return len(openaifake.SpeechAsks(key)) == 1 })
	cancel()
	if err := within(t, "the cancelled Speak", done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Speak = %v, want context.Canceled", err)
	}
	eventually(t, "the fake to see the connection close", func() bool { return openaifake.OpenSpeechSessions(key) == 0 })
	eventually(t, "every goroutine to end", func() bool { return runtime.NumGoroutine() <= before })
}
