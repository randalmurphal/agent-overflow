package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/openai"
	"agent-overflow/internal/openai/openaifake"
	"agent-overflow/internal/openaikey"
	"agent-overflow/internal/readaloud"
	"agent-overflow/internal/secret"
	"agent-overflow/internal/transport"
)

// newReadAloudTestApp turns read aloud on, puts key in the key file (none
// when key is ""), answers OpenAI the way an isolated boot does, and serves
// the real transport. It returns the app and the transport's base URL.
func newReadAloudTestApp(t *testing.T, key string) (*App, string) {
	t.Helper()
	app := newTestAppWithStore(t)
	app.configDir = t.TempDir()
	app.openAIClientOverride = isolatedHTTPClient
	if key != "" {
		writeOpenAIKey(t, app, key)
	}
	enableReadAloud(t, app, true)
	srv, err := transport.New(transport.Config{
		Dispatcher:     transport.NewDispatcher(),
		EventBus:       transport.NewEventBus(8),
		Token:          "read-aloud-test-token",
		SpeechTransfer: ReadAloudTransfer(app),
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("transport.Server.Start: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})
	app.SetTransportServer(srv)
	return app, "http://" + srv.Addr()
}

func enableReadAloud(t *testing.T, app *App, on bool) {
	t.Helper()
	if _, err := app.settings.Update(map[string]any{"readAloudAPIEnabled": on}); err != nil {
		t.Fatal(err)
	}
}

func fetchSpeech(t *testing.T, ctx context.Context, url string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch %s: %v", url, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err := errors.Join(err, resp.Body.Close()); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body, resp.Header
}

func mintAndFetch(t *testing.T, app *App, base, text, voice string) (int, []byte, http.Header) {
	t.Helper()
	url, err := app.MintReadAloudTicket(context.Background(), text, voice)
	if err != nil {
		t.Fatalf("MintReadAloudTicket: %v", err)
	}
	return fetchSpeech(t, context.Background(), base+url)
}

func TestReadAloudSourceFollowsTheSwitchAndTheKey(t *testing.T) {
	t.Parallel()
	app, _ := newReadAloudTestApp(t, "")
	check := func(when string, want bool) {
		t.Helper()
		status, err := app.ReadAloudSource()
		if err != nil || status.Available != want {
			t.Fatalf("%s: ReadAloudSource = %+v, %v; want available %v", when, status, err, want)
		}
		if len(status.Voices) != len(openai.SpeechVoices()) || status.Voices[8] != (ReadAloudVoice{ID: "marin", Name: "Marin"}) {
			t.Fatalf("%s: voices = %+v", when, status.Voices)
		}
	}
	check("on without a key", false)
	writeOpenAIKey(t, app, "sk-proj-read-aloud-source")
	check("on with a key", true)
	enableReadAloud(t, app, false)
	check("off with a key", false)
}

func TestMintReadAloudTicketRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	app, _ := newReadAloudTestApp(t, "sk-proj-read-aloud-refusals")
	for _, c := range []struct {
		text, voice string
		is          error
	}{
		{"", "marin", readaloud.ErrNoText},
		{"   ", "marin", readaloud.ErrNoText},
		{strings.Repeat("a", readaloud.MaxTextCharacters+1), "marin", readaloud.ErrTextTooLong},
		{"Hello.", "nova", nil},
	} {
		if _, err := app.MintReadAloudTicket(context.Background(), c.text, c.voice); err == nil || (c.is != nil && !errors.Is(err, c.is)) {
			t.Fatalf("Mint(%.20q, %q) = %v, want %v", c.text, c.voice, err, c.is)
		}
	}
	enableReadAloud(t, app, false)
	_, err := app.MintReadAloudTicket(context.Background(), "Hello.", "marin")
	if !errors.Is(err, readaloud.ErrOff) {
		t.Fatalf("Mint with the switch off = %v, want ErrOff", err)
	}
}

// The whole path: admit, fetch through the real route, and get a WAV file
// of exactly the fake's audio for the sentence, spoken in the voice asked.
func TestReadAloudFetchesTheSentenceAsAWAVFile(t *testing.T) {
	t.Parallel()
	const key = "sk-proj-read-aloud-wav"
	const text = "The build finished in forty seconds."
	app, base := newReadAloudTestApp(t, key)
	url, err := app.MintReadAloudTicket(context.Background(), "  "+text+" ", "sage")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "/attachments/speech/") {
		t.Fatalf("URL %q is not the speech route", url)
	}
	status, body, header := fetchSpeech(t, context.Background(), base+url)
	if status != http.StatusOK || header.Get("Content-Type") != "audio/wav" || header.Get("Cache-Control") != "no-store" {
		t.Fatalf("fetch = %d %s, %v", status, body, header)
	}
	pcm := openaifake.SpeechAudio(text)
	if len(pcm) != len(text)*openaifake.SpeechBytesPerCharacter {
		t.Fatalf("the fake's audio for %d characters is %d bytes", len(text), len(pcm))
	}
	if len(body) != readaloud.WAVHeaderBytes+len(pcm) || !bytes.Equal(body[readaloud.WAVHeaderBytes:], pcm) {
		t.Fatalf("body is %d bytes, want a 44-byte header and the fake's %d", len(body), len(pcm))
	}
	le := binary.LittleEndian
	if string(body[0:4]) != "RIFF" || string(body[8:12]) != "WAVE" || string(body[12:16]) != "fmt " || string(body[36:40]) != "data" ||
		le.Uint32(body[4:]) != uint32(len(body)-8) || le.Uint16(body[20:]) != 1 || le.Uint16(body[22:]) != 1 ||
		le.Uint32(body[24:]) != 24000 || le.Uint32(body[28:]) != 48000 || le.Uint16(body[32:]) != 2 ||
		le.Uint16(body[34:]) != 16 || le.Uint32(body[40:]) != uint32(len(pcm)) {
		t.Fatalf("not a 24 kHz mono 16-bit PCM WAV header: % x", body[:readaloud.WAVHeaderBytes])
	}
	if asks := openaifake.SpeechAsks(key); len(asks) != 1 || asks[0] != (openaifake.SpeechAsk{Voice: "sage", Text: text}) {
		t.Fatalf("the fake was asked %+v", asks)
	}
	if status, _, _ := fetchSpeech(t, context.Background(), base+url); status != http.StatusNotFound {
		t.Fatalf("a second fetch = %d, want 404", status)
	}
	if asks := openaifake.SpeechAsks(key); len(asks) != 1 {
		t.Fatalf("a second fetch reached OpenAI: %+v", asks)
	}
}

func TestReadAloudSwitchedOffBeforeTheFetchRefusesIt(t *testing.T) {
	t.Parallel()
	const key = "sk-proj-read-aloud-switched-off"
	app, base := newReadAloudTestApp(t, key)
	url, err := app.MintReadAloudTicket(context.Background(), "Hello there.", "marin")
	if err != nil {
		t.Fatal(err)
	}
	enableReadAloud(t, app, false)
	status, body, _ := fetchSpeech(t, context.Background(), base+url)
	if status != http.StatusConflict || string(body) != readaloud.ErrOff.Error() {
		t.Fatalf("fetch after the switch went off = %d %q", status, body)
	}
	if asks := openaifake.SpeechAsks(key); len(asks) != 0 {
		t.Fatalf("OpenAI was asked after the switch went off: %+v", asks)
	}
}

func TestReadAloudRetriesAnAnswerOnceAndThenRefuses(t *testing.T) {
	t.Parallel()
	const key = "sk-proj-read-aloud-misread"
	app, base := newReadAloudTestApp(t, key)

	once := "Read this sentence " + openaifake.SpeechAnswersOnce
	status, body, _ := mintAndFetch(t, app, base, once, "marin")
	if status != http.StatusOK || len(body) != readaloud.WAVHeaderBytes+len(openaifake.SpeechAudio(once)) {
		t.Fatalf("answered once then read = %d, %d bytes", status, len(body))
	}
	if asks := openaifake.SpeechAsks(key); len(asks) != 2 {
		t.Fatalf("answered once: OpenAI was asked %d times, want 2", len(asks))
	}

	always := "Read this one " + openaifake.SpeechAnswers
	status, body, _ = mintAndFetch(t, app, base, always, "marin")
	if status != http.StatusBadGateway || string(body) != readaloud.ErrMisread.Error() {
		t.Fatalf("always answered = %d %q", status, body)
	}
	if asks := openaifake.SpeechAsks(key); len(asks) != 4 {
		t.Fatalf("always answered: OpenAI was asked %d times in all, want 4", len(asks))
	}
}

// Each failure answers a status and a message a person can act on, with
// no key in it.
func TestReadAloudFailuresReachThePerson(t *testing.T) {
	t.Parallel()
	refused, refusedBase := newReadAloudTestApp(t, openaifake.RefusedKey)
	status, body, _ := mintAndFetch(t, refused, refusedBase, "Hello there.", "marin")
	if status != http.StatusBadGateway || string(body) != openai.ErrKeyRefused.Error() {
		t.Fatalf("a refused key = %d %q", status, body)
	}

	missing, missingBase := newReadAloudTestApp(t, "")
	status, body, _ = mintAndFetch(t, missing, missingBase, "Hello there.", "marin")
	if status != http.StatusConflict || !strings.Contains(string(body), openaikey.Path(missing.configDir)) {
		t.Fatalf("no key = %d %q; want where the key goes", status, body)
	}

	app, base := newReadAloudTestApp(t, "sk-proj-read-aloud-failures")
	for marker, want := range map[string]int{
		openaifake.SpeechFails:      http.StatusBadGateway,
		openaifake.SpeechIncomplete: http.StatusBadGateway,
		openaifake.SpeechRunsLong:   http.StatusBadGateway,
	} {
		status, body, _ := mintAndFetch(t, app, base, "Hello "+marker, "marin")
		if status != want || len(body) == 0 || strings.Contains(string(body), "sk-") {
			t.Fatalf("%s = %d %q", marker, status, body)
		}
	}
}

func TestReadAloudFailuresAnswerDeliberateStatuses(t *testing.T) {
	t.Parallel()
	live := context.Background()
	for _, c := range []struct {
		err    error
		status int
	}{
		{readaloud.ErrBusy, http.StatusTooManyRequests},
		{openai.ErrRateLimited, http.StatusTooManyRequests},
		{context.DeadlineExceeded, http.StatusGatewayTimeout},
		{openai.ErrKeyRefused, http.StatusBadGateway},
		{readaloud.ErrMisread, http.StatusBadGateway},
		{openai.ErrSpeechTooLong, http.StatusBadGateway},
	} {
		var failure *transport.SpeechError
		if !errors.As(readAloudFailure(live, c.err), &failure) || failure.Status != c.status {
			t.Fatalf("%v answers %+v, want %d", c.err, failure, c.status)
		}
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	var failure *transport.SpeechError
	if err := readAloudFailure(gone, context.Canceled); errors.As(err, &failure) {
		t.Fatalf("an abandoned fetch was classified: %+v", failure)
	}
}

// A client that abandons the fetch cancels the OpenAI call under it.
func TestAnAbandonedReadAloudFetchClosesTheOpenAICall(t *testing.T) {
	t.Parallel()
	const key = "sk-proj-read-aloud-abandoned"
	app, base := newReadAloudTestApp(t, key)
	url, err := app.MintReadAloudTicket(context.Background(), "Wait "+openaifake.SpeechStalls, "marin")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fetched := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+url, nil)
		if err != nil {
			fetched <- err
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			err = errors.Join(errors.New("the abandoned fetch got an answer"), resp.Body.Close())
		} else {
			err = nil
		}
		fetched <- err
	}()
	waitFor(t, "the stalled OpenAI call", func() bool { return openaifake.OpenSpeechSessions(key) == 1 })
	cancel()
	if err := <-fetched; err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the OpenAI call to close", func() bool { return openaifake.OpenSpeechSessions(key) == 0 })
	// The synthesis slot is free again.
	if status, _, _ := mintAndFetch(t, app, base, "Hello there.", "marin"); status != http.StatusOK {
		t.Fatalf("a fetch after the abandoned one = %d", status)
	}
}

// The key is gone from memory once the handshake is done: while OpenAI has
// yet to accept the session's configuration, the secret the reader handed
// over reveals nothing.
func TestTheReadAloudKeyIsZeroedOnceTheHandshakeIsDone(t *testing.T) {
	t.Parallel()
	const key = openaifake.SpeechConfigStallsKey
	held := secret.New([]byte(key))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := speakOnce(ctx, isolatedHTTPClient, func() (*secret.Bytes, error) { return held, nil },
			openai.SpeechRequest{Text: "Hello there.", Voice: "marin"})
		done <- err
	}()
	waitFor(t, "the session to send its configuration", func() bool { return openaifake.SpeechSessionsConfigured(key) == 1 })
	if held.Reveal() != nil || held.Len() != 0 {
		t.Fatal("the key is still in memory after the handshake")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("speakOnce = %v, want context.Canceled", err)
	}
}

// Turning the switch off ends a synthesis in progress, and one admitted
// before it stays refused when the switch comes back on.
func TestSwitchingReadAloudOffEndsWhatWasAdmitted(t *testing.T) {
	t.Parallel()
	const key = "sk-proj-read-aloud-revoked"
	app, base := newReadAloudTestApp(t, key)
	speaking, err := app.MintReadAloudTicket(context.Background(), "Wait "+openaifake.SpeechStalls, "marin")
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := app.MintReadAloudTicket(context.Background(), "Hello there.", "marin")
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		status int
		body   string
	}
	fetched := make(chan answer, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+speaking, nil)
		if err != nil {
			fetched <- answer{0, err.Error()}
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fetched <- answer{0, err.Error()}
			return
		}
		body, err := io.ReadAll(resp.Body)
		if err := errors.Join(err, resp.Body.Close()); err != nil {
			fetched <- answer{0, err.Error()}
			return
		}
		fetched <- answer{resp.StatusCode, string(body)}
	}()
	waitFor(t, "the stalled OpenAI call", func() bool { return openaifake.OpenSpeechSessions(key) == 1 })

	enableReadAloud(t, app, false)

	select {
	case got := <-fetched:
		if got.status != http.StatusConflict || got.body != readaloud.ErrOff.Error() {
			t.Fatalf("the synthesis in progress answered %d %q, want 409 and the off message", got.status, got.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the synthesis kept running after the switch went off")
	}
	waitFor(t, "the OpenAI call to close", func() bool { return openaifake.OpenSpeechSessions(key) == 0 })

	enableReadAloud(t, app, true)
	status, body, _ := fetchSpeech(t, context.Background(), base+waiting)
	if status != http.StatusNotFound || string(body) != readaloud.ErrUnknown.Error() {
		t.Fatalf("a sentence admitted before the switch went off = %d %q, want 404", status, body)
	}
	if asks := openaifake.SpeechAsks(key); len(asks) != 1 {
		t.Fatalf("OpenAI was asked %d times, want only the stalled one", len(asks))
	}
	// What is admitted now is read.
	if status, _, _ := mintAndFetch(t, app, base, "Hello again.", "marin"); status != http.StatusOK {
		t.Fatalf("a fetch after the switch came back on = %d", status)
	}
}

// Each served clip gives its synthesis slot back, so more sentences than
// the cap can be read one after another.
func TestAServedReadAloudClipFreesItsSlot(t *testing.T) {
	t.Parallel()
	app, base := newReadAloudTestApp(t, "sk-proj-read-aloud-slots")
	for i := range readaloud.MaxSpeaking + 2 {
		if status, body, _ := mintAndFetch(t, app, base, "Hello there.", "marin"); status != http.StatusOK {
			t.Fatalf("fetch %d = %d %q", i+1, status, body)
		}
	}
}
