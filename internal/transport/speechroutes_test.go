package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubSpeech answers every sentence with wav or err and records the ids
// and contexts it was asked with, and how many clips were closed.
type stubSpeech struct {
	mu     sync.Mutex
	wav    []byte
	err    error
	ids    []string
	wait   chan struct{}
	ctxs   []context.Context
	closed int
}

// stubClip counts its close on the stub that made it.
type stubClip struct {
	*bytes.Reader
	stub *stubSpeech
}

func (c stubClip) Close() error {
	c.stub.mu.Lock()
	c.stub.closed++
	c.stub.mu.Unlock()
	return nil
}

func (s *stubSpeech) closes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *stubSpeech) SpeakSentence(ctx context.Context, speechID string) (io.ReadSeekCloser, error) {
	s.mu.Lock()
	s.ids = append(s.ids, speechID)
	s.ctxs = append(s.ctxs, ctx)
	wait := s.wait
	s.mu.Unlock()
	if wait != nil {
		close(wait)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	return stubClip{Reader: bytes.NewReader(s.wav), stub: s}, nil
}

func (s *stubSpeech) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

func speechFixture(t *testing.T, stub SpeechTransfer) *serverFixture {
	t.Helper()
	return newServerFixtureWith(t, func(cfg *Config) {
		cfg.SpeechTransfer = stub
		cfg.AttachmentTransfer = &stubTransfer{content: []byte("x"), mime: "image/png"}
	})
}

func mintSpeech(t *testing.T, f *serverFixture, speechID string) string {
	t.Helper()
	relative, err := f.srv.MintSpeechTicket(speechID)
	if err != nil {
		t.Fatalf("mint speech ticket: %v", err)
	}
	return relative
}

func TestSpeechServesTheSentencesAudioOnce(t *testing.T) {
	stub := &stubSpeech{wav: []byte("RIFF....WAVE")}
	f := speechFixture(t, stub)
	relative := mintSpeech(t, f, "speech-1")
	if !strings.HasPrefix(relative, "/attachments/speech/speech-1?ticket=") {
		t.Fatalf("minted URL %q does not name the sentence it admits", relative)
	}
	resp := get(t, f.srv.Addr(), relative, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for header, want := range map[string]string{
		"Content-Type":           "audio/wav",
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "RIFF....WAVE" {
		t.Fatalf("body = %q, %v", body, err)
	}
	if again := get(t, f.srv.Addr(), relative, nil); again.StatusCode != http.StatusNotFound {
		t.Fatalf("reused ticket status = %d, want 404", again.StatusCode)
	}
	if asked := stub.asked(); len(asked) != 1 || asked[0] != "speech-1" {
		t.Fatalf("the seam was asked for %v", asked)
	}
	// The clip is released once its response is written.
	deadline := time.Now().Add(5 * time.Second)
	for stub.closes() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the served clip was closed %d times, want 1", stub.closes())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A ticket admits its own sentence at this route and nothing else.
func TestSpeechRefusesTicketsItDidNotMint(t *testing.T) {
	stub := &stubSpeech{wav: []byte("wav")}
	f := speechFixture(t, stub)
	ticket := mintSpeech(t, f, "speech-1")
	if resp := get(t, f.srv.Addr(), "/attachments/speech/speech-2"+ticket[strings.Index(ticket, "?"):], nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another id's ticket status = %d, want 404", resp.StatusCode)
	}
	if resp := get(t, f.srv.Addr(), "/attachments/speech/speech-1?ticket=forged", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("forged ticket status = %d, want 404", resp.StatusCode)
	}
	image, err := f.srv.MintLocalImageTicket("speech-1")
	if err != nil {
		t.Fatal(err)
	}
	if resp := get(t, f.srv.Addr(), "/attachments/speech/speech-1"+image[strings.Index(image, "?"):], nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a local image ticket status = %d, want 404", resp.StatusCode)
	}
	speech := mintSpeech(t, f, "img-1")
	if resp := get(t, f.srv.Addr(), "/attachments/image/img-1"+speech[strings.Index(speech, "?"):], nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a speech ticket at the image route status = %d, want 404", resp.StatusCode)
	}
	if len(stub.asked()) != 0 {
		t.Fatalf("a refused ticket reached the seam: %v", stub.asked())
	}
	if _, err := f.srv.MintSpeechTicket(""); err == nil {
		t.Fatal("minted a ticket for no id")
	}

	unwired := newServerFixtureWith(t, func(*Config) {})
	if resp := get(t, unwired.srv.Addr(), mintSpeech(t, unwired, "speech-1"), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no speech transfer status = %d, want 404", resp.StatusCode)
	}
}

// A failed synthesis answers its status with its message as plain text.
func TestSpeechAnswersAFailureForAPerson(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		body   string
	}{
		{&SpeechError{Status: http.StatusConflict, Err: errors.New("Read aloud is off on this computer.")}, http.StatusConflict, "Read aloud is off on this computer."},
		{fmt.Errorf("wrapped: %w", &SpeechError{Status: http.StatusBadGateway, Err: errors.New("OpenAI did not read the text as written")}), http.StatusBadGateway, "wrapped: OpenAI did not read the text as written"},
		{&SpeechError{Status: http.StatusOK, Err: errors.New("not a failure status")}, http.StatusInternalServerError, "not a failure status"},
		{errors.New("unclassified"), http.StatusInternalServerError, "unclassified"},
	} {
		f := speechFixture(t, &stubSpeech{err: c.err})
		resp := get(t, f.srv.Addr(), mintSpeech(t, f, "speech-1"), nil)
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != c.status || string(body) != c.body {
			t.Fatalf("%v: answered %d %q, want %d %q", c.err, resp.StatusCode, body, c.status, c.body)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Fatalf("failure Content-Type = %q", got)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("failure Cache-Control = %q", got)
		}
	}
}

// The synthesis runs under the request's context: a client that gives up
// cancels it.
func TestAnAbandonedSpeechFetchCancelsTheSynthesis(t *testing.T) {
	stub := &stubSpeech{wait: make(chan struct{})}
	f := speechFixture(t, stub)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+f.srv.Addr()+mintSpeech(t, f, "speech-1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fetched := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			err = resp.Body.Close()
		}
		fetched <- err
	}()
	<-stub.wait
	cancel()
	if err := <-fetched; err == nil {
		t.Fatal("the abandoned fetch got an answer")
	}
	stub.mu.Lock()
	synthesis := stub.ctxs[0]
	stub.mu.Unlock()
	select {
	case <-synthesis.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the synthesis outlived its request")
	}
}

func TestSpeechPreflight(t *testing.T) {
	f := speechFixture(t, &stubSpeech{})
	for origin, want := range map[string]int{ShellOrigin: http.StatusNoContent, "https://evil.example": http.StatusNotFound} {
		req, err := http.NewRequest(http.MethodOptions, "http://"+f.srv.Addr()+"/attachments/speech/speech-1", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", http.MethodGet)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("preflight: %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("preflight body: %v", err)
		}
		if resp.StatusCode != want {
			t.Fatalf("preflight from %s = %d, want %d", origin, resp.StatusCode, want)
		}
	}
}
