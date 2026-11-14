package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/dictation"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/openai"
	"agent-overflow/internal/openai/openaifake"
	"agent-overflow/internal/openaikey"
	"agent-overflow/internal/transport"
)

const liveTestKey = "sk-proj-live-dictation-test"

// tenthOfASecond is 100 ms of the PCM live dictation takes.
var tenthOfASecond = make([]byte, dictation.LiveBytesPerSecond/10)

// newLiveDictationTestApp turns live dictation on, puts key in the key
// file under its own config folder (none when key is ""), and answers
// OpenAI the way an isolated boot does.
func newLiveDictationTestApp(t *testing.T, key string) *App {
	t.Helper()
	app := newTestAppWithStore(t)
	app.configDir = t.TempDir()
	app.openAIClientOverride = isolatedHTTPClient
	if key != "" {
		writeOpenAIKey(t, app, key)
	}
	enableLiveDictation(t, app, true)
	return app
}

func writeOpenAIKey(t *testing.T, app *App, key string) {
	t.Helper()
	path := openaikey.Path(app.configDir)
	if _, err := openaikey.Create(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func enableLiveDictation(t *testing.T, app *App, on bool) {
	t.Helper()
	if _, err := app.settings.Update(map[string]any{"dictationAPIEnabled": on}); err != nil {
		t.Fatal(err)
	}
}

// Live dictation streams the recording to OpenAI with the key file's key,
// answers each piece with the text heard so far, and finishes with the
// turn's transcript.
func TestLiveDictationStreamsWithTheKeyFile(t *testing.T) {
	t.Parallel()
	app := newLiveDictationTestApp(t, liveTestKey)
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})

	sources, err := app.DictationSources()
	if err != nil || !sources.Live {
		t.Fatalf("DictationSources = %+v, %v; want live", sources, err)
	}
	id, err := app.BeginLiveDictation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var heard string
	for seq := range 2 {
		heard, err = app.AppendLiveDictation(ctx, id, seq, tenthOfASecond)
		if err != nil {
			t.Fatalf("piece %d: %v", seq, err)
		}
	}
	// The fake's partial text arrives asynchronously; the answer is
	// whatever had arrived, a prefix of the whole.
	if !strings.HasPrefix("Hearing.", heard) {
		t.Fatalf("heard %q, want a prefix of the partial text", heard)
	}
	text, err := app.FinishLiveDictation(ctx, id)
	if err != nil || text != "Heard 9600 bytes of live audio" {
		t.Fatalf("FinishLiveDictation = %q, %v", text, err)
	}
	if _, err := app.AppendLiveDictation(ctx, id, 2, tenthOfASecond); !errors.Is(err, dictation.ErrUnknownLiveSession) {
		t.Fatalf("append after finishing = %v", err)
	}
}

// Without a usable key there is no live source, and starting one says
// where the key goes. The key never appears in what a client sees.
func TestLiveDictationNeedsAUsableKey(t *testing.T) {
	t.Parallel()
	missing := newLiveDictationTestApp(t, "")
	sources, err := missing.DictationSources()
	if err != nil || sources.Live {
		t.Fatalf("DictationSources without a key = %+v, %v", sources, err)
	}
	_, err = missing.BeginLiveDictation(context.Background())
	if err == nil || !strings.Contains(err.Error(), openaikey.Path(missing.configDir)) {
		t.Fatalf("BeginLiveDictation without a key = %v; want where the key goes", err)
	}

	refused := newLiveDictationTestApp(t, openaifake.RefusedKey)
	_, err = refused.BeginLiveDictation(context.Background())
	if !errors.Is(err, openai.ErrKeyRefused) || strings.Contains(err.Error(), openaifake.RefusedKey) {
		t.Fatalf("BeginLiveDictation with a refused key = %v", err)
	}
	file, err := refused.OpenAIKeyFile()
	if err != nil || file.State != openaikey.StatePresent {
		t.Fatalf("OpenAIKeyFile = %+v, %v", file, err)
	}
	wire, err := json.Marshal(file)
	if err != nil || strings.Contains(string(wire), openaifake.RefusedKey) {
		t.Fatalf("the key file's status carries the key: %s", wire)
	}
}

func TestLiveDictationRefusesASymlinkedKeyFile(t *testing.T) {
	t.Parallel()
	app := newLiveDictationTestApp(t, "")
	elsewhere := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(elsewhere, []byte(liveTestKey), 0o600); err != nil {
		t.Fatal(err)
	}
	path := openaikey.Path(app.configDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	var refusal *openaikey.RefusedError
	if _, err := app.BeginLiveDictation(context.Background()); !errors.As(err, &refusal) {
		t.Fatalf("BeginLiveDictation with a symlinked key file = %v", err)
	}
	if sources, _ := app.DictationSources(); sources.Live {
		t.Fatal("a symlinked key file was offered as a live source")
	}
}

// The switch decides: off refuses a start, and turned off mid-recording
// drops what was heard rather than finishing it.
func TestLiveDictationFollowsItsSwitch(t *testing.T) {
	t.Parallel()
	app := newLiveDictationTestApp(t, liveTestKey)
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	id, err := app.BeginLiveDictation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.AppendLiveDictation(ctx, id, 0, tenthOfASecond); err != nil {
		t.Fatal(err)
	}
	enableLiveDictation(t, app, false)
	if _, err := app.FinishLiveDictation(ctx, id); !errors.Is(err, dictation.ErrLiveSwitchedOff) {
		t.Fatalf("FinishLiveDictation after the switch went off = %v", err)
	}
	if _, err := app.BeginLiveDictation(ctx); !errors.Is(err, dictation.ErrLiveOff) {
		t.Fatalf("BeginLiveDictation with the switch off = %v", err)
	}
	if sources, _ := app.DictationSources(); sources.Live {
		t.Fatal("live dictation offered with its switch off")
	}
}

// Switching live dictation off ends every live recording at once, so
// switching it straight back on cannot let one carry on.
func TestSwitchingLiveDictationOffAndOnEndsTheRecording(t *testing.T) {
	t.Parallel()
	app := newLiveDictationTestApp(t, liveTestKey)
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	id, err := app.BeginLiveDictation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.AppendLiveDictation(ctx, id, 0, tenthOfASecond); err != nil {
		t.Fatal(err)
	}
	enableLiveDictation(t, app, false)
	enableLiveDictation(t, app, true)
	if _, err := app.AppendLiveDictation(ctx, id, 1, tenthOfASecond); !errors.Is(err, dictation.ErrLiveSwitchedOff) {
		t.Fatalf("a recording outlived the switch going off: %v", err)
	}
	// One admitted after the switch came back on carries on.
	fresh, err := app.BeginLiveDictation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.AppendLiveDictation(ctx, fresh, 0, tenthOfASecond); err != nil {
		t.Fatalf("a recording begun after the switch came back on: %v", err)
	}
	if err := app.CancelLiveDictation(ctx, fresh); err != nil {
		t.Fatal(err)
	}
}

// The switch is also read from the settings themselves, so a change no
// write announced (a hand edit of the host file, or a write that failed
// after saving it) still ends a recording at its next piece or its finish.
func TestLiveDictationFailsClosedOnAnUnannouncedSwitchOff(t *testing.T) {
	t.Parallel()
	for _, call := range []string{"append", "finish"} {
		app := newLiveDictationTestApp(t, liveTestKey)
		ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
		id, err := app.BeginLiveDictation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := app.AppendLiveDictation(ctx, id, 0, tenthOfASecond); err != nil {
			t.Fatal(err)
		}
		revision := app.settings.Revision()
		path := app.settings.Path()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var file map[string]any
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatal(err)
		}
		delete(file, "dictationAPIEnabled")
		edited, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, edited, 0o600); err != nil {
			t.Fatal(err)
		}
		if app.liveDictationOn() || app.settings.Revision() != revision {
			t.Fatal("the hand edit was not the unannounced switch-off this test needs")
		}
		switch call {
		case "append":
			_, err = app.AppendLiveDictation(ctx, id, 1, tenthOfASecond)
		case "finish":
			_, err = app.FinishLiveDictation(ctx, id)
		}
		if !errors.Is(err, dictation.ErrLiveSwitchedOff) {
			t.Fatalf("%s after an unannounced switch-off = %v", call, err)
		}
		if _, err := app.AppendLiveDictation(ctx, id, 1, tenthOfASecond); !errors.Is(err, dictation.ErrUnknownLiveSession) {
			t.Fatalf("after %s the recording is still open: %v", call, err)
		}
		// Back on by hand: a new recording is admitted, not refused for good.
		enableLiveDictation(t, app, true)
		fresh, err := app.BeginLiveDictation(ctx)
		if err != nil {
			t.Fatalf("after %s, a recording once live is back on: %v", call, err)
		}
		if err := app.CancelLiveDictation(ctx, fresh); err != nil {
			t.Fatal(err)
		}
	}
}

// Begin's refusals for live being off carry the switch's wire code, so a
// client stops quietly however the switch moved around its start.
func TestBeginRefusesForTheSwitchWithItsCode(t *testing.T) {
	t.Parallel()
	app := newLiveDictationTestApp(t, liveTestKey)
	enableLiveDictation(t, app, false)
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	_, err := app.BeginLiveDictation(ctx)
	if code, _, ok := errorsx.PublicDetails(err); !ok || code != dictation.LiveSwitchedOffCode {
		t.Fatalf("Begin with live off = %v (code %q)", err, code)
	}
}

// heldTransport holds each request until released, then answers it as an
// isolated boot does.
type heldTransport struct {
	entered chan struct{}
	release chan struct{}
}

func (h heldTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h.entered <- struct{}{}
	<-h.release
	return isolatedHTTPClient.Transport.RoundTrip(req)
}

// A recording whose connect is under way when live dictation is switched
// off never opens: Begin refuses it once the connect returns, whether or
// not the switch is back on by then.
func TestSwitchingLiveDictationOffWhileConnectingRefusesTheRecording(t *testing.T) {
	t.Parallel()
	for _, backOn := range []bool{false, true} {
		app := newLiveDictationTestApp(t, liveTestKey)
		held := heldTransport{entered: make(chan struct{}), release: make(chan struct{})}
		app.openAIClientOverride = &http.Client{Transport: held}
		ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
		begun := make(chan error, 1)
		go func() {
			_, err := app.BeginLiveDictation(ctx)
			begun <- err
		}()
		<-held.entered
		enableLiveDictation(t, app, false)
		if backOn {
			enableLiveDictation(t, app, true)
		}
		close(held.release)
		err := <-begun
		if !errors.Is(err, dictation.ErrLiveSwitchedOff) {
			t.Fatalf("back on %v: a recording connecting through the switch going off = %v", backOn, err)
		}
		if code, _, _ := errorsx.PublicDetails(err); code != dictation.LiveSwitchedOffCode {
			t.Fatalf("back on %v: the refusal's wire code = %q", backOn, code)
		}
	}
}

// A live recording belongs to the connection that opened it: another
// cannot touch it, and its own connection's end drops it.
func TestLiveDictationBelongsToItsConnection(t *testing.T) {
	t.Parallel()
	app := newLiveDictationTestApp(t, liveTestKey)
	owner, ownerState := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	other, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	id, err := app.BeginLiveDictation(owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.AppendLiveDictation(other, id, 0, tenthOfASecond); !errors.Is(err, dictation.ErrUnknownLiveSession) {
		t.Fatalf("another connection appended: %v", err)
	}
	if _, err := app.FinishLiveDictation(other, id); !errors.Is(err, dictation.ErrUnknownLiveSession) {
		t.Fatalf("another connection finished: %v", err)
	}
	if err := app.CancelLiveDictation(other, id); !errors.Is(err, dictation.ErrUnknownLiveSession) {
		t.Fatalf("another connection cancelled: %v", err)
	}
	if _, err := app.AppendLiveDictation(owner, id, 0, tenthOfASecond); err != nil {
		t.Fatalf("the owner's append after the refusals: %v", err)
	}
	ownerState.RunCleanups()
	if _, err := app.AppendLiveDictation(owner, id, 1, tenthOfASecond); !errors.Is(err, dictation.ErrUnknownLiveSession) {
		t.Fatalf("the session outlived its connection: %v", err)
	}
	if ownerState.HasCleanup(liveDictationCleanupKey(id)) {
		t.Fatal("the dropped session's cleanup is still bound")
	}
}

// Finishing releases the connection's cleanup, so a long-lived connection
// does not collect one per recording.
func TestAFinishedLiveDictationLeavesNoCleanupBehind(t *testing.T) {
	t.Parallel()
	app := newLiveDictationTestApp(t, liveTestKey)
	ctx, state := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	id, err := app.BeginLiveDictation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !state.HasCleanup(liveDictationCleanupKey(id)) {
		t.Fatal("an open session has no cleanup bound")
	}
	if _, err := app.AppendLiveDictation(ctx, id, 0, tenthOfASecond); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FinishLiveDictation(ctx, id); err != nil {
		t.Fatal(err)
	}
	if state.HasCleanup(liveDictationCleanupKey(id)) {
		t.Fatal("a finished session's cleanup is still bound")
	}
}
