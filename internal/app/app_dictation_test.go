package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/dictation"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/transport"
)

// dictationAuth is a ChatGPT-mode Codex auth.json carrying accessToken.
func dictationAuth(t *testing.T, accessToken string) []byte {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "ws-1"}})
	if err != nil {
		t.Fatal(err)
	}
	segment := base64.RawURLEncoding.EncodeToString
	data, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token":      segment([]byte(`{"alg":"none"}`)) + "." + segment(claims) + "." + segment([]byte("sig")),
			"access_token":  accessToken,
			"refresh_token": "rt-" + accessToken,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// newDictationTestApp signs Codex in with ChatGPT and answers the
// transcription endpoint the way an isolated boot does.
func newDictationTestApp(t *testing.T, accessToken string) *App {
	t.Helper()
	app := newTestAppWithStore(t)
	app.rateLimitProbeClientOverride = isolatedHTTPClient
	installIdentityTestAccount(t, app, string(provider.Codex), "selected", "selected@example.com", dictationAuth(t, accessToken))
	enableDictation(t, app, true)
	return app
}

func enableDictation(t *testing.T, app *App, on bool) {
	t.Helper()
	if _, err := app.settings.Update(map[string]any{"dictationSubscriptionEnabled": on}); err != nil {
		t.Fatal(err)
	}
}

func uploadDictation(t *testing.T, app *App, recordingID string, pieces ...string) {
	t.Helper()
	for seq, piece := range pieces {
		if err := DictationTransfer(app).AppendDictationChunk(transport.DictationChunk{
			RecordingID: recordingID, Seq: seq, Size: int64(len(piece)), Body: strings.NewReader(piece),
		}); err != nil {
			t.Fatalf("piece %d: %v", seq, err)
		}
	}
}

// A recording uploaded in pieces is transcribed whole with this computer's
// Codex ChatGPT sign-in, and nothing of it is kept.
func TestDictationTranscribesWithTheCodexSignIn(t *testing.T) {
	t.Parallel()
	app := newDictationTestApp(t, "fresh")
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})

	sources, err := app.DictationSources()
	if err != nil || !sources.Subscription {
		t.Fatalf("DictationSources = %+v, %v; want the subscription", sources, err)
	}
	id, err := app.BeginDictation(ctx, "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	uploadDictation(t, app, id, "header+", "frames")
	text, err := app.FinishDictation(ctx, id, 2)
	if err != nil || text != "Heard 13 bytes of audio/webm" {
		t.Fatalf("FinishDictation = %q, %v", text, err)
	}
	if open := app.dictationRecordings().Open(); open != 0 {
		t.Fatalf("open recordings after finishing = %d", open)
	}
}

// Without a ChatGPT sign-in for Codex there is no dictation source, and
// finishing a recording says why.
func TestDictationNeedsAChatGPTSignIn(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.rateLimitProbeClientOverride = isolatedHTTPClient
	installIdentityTestAccount(t, app, string(provider.Codex), "selected", "selected@example.com",
		[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-test"}`))
	enableDictation(t, app, true)

	sources, err := app.DictationSources()
	if err != nil || sources.Subscription || sources.ChatGPTSignIn {
		t.Fatalf("DictationSources = %+v, %v; want none", sources, err)
	}
	id, err := app.BeginDictation(connCtx(t), "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	uploadDictation(t, app, id, "frames")
	if _, err := app.FinishDictation(connCtx(t), id, 1); !errors.Is(err, provideraccountapp.ErrNoCodexChatGPTLogin) {
		t.Fatalf("FinishDictation err = %v, want ErrNoCodexChatGPTLogin", err)
	}
}

// A recording belongs to the connection that opened it: another connection
// can neither mint its tickets, finish it nor cancel it, and the owner's
// disconnect drops it.
func TestDictationRecordingsBelongToTheirConnection(t *testing.T) {
	t.Parallel()
	app := newDictationTestApp(t, "fresh")
	owner, ownerState := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	other, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})

	id, err := app.BeginDictation(owner, "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.MintDictationChunkTicket(other, id, 0, 4); !errors.Is(err, dictation.ErrUnknownRecording) {
		t.Fatalf("another connection minted a ticket: %v", err)
	}
	if _, err := app.FinishDictation(other, id, 0); !errors.Is(err, dictation.ErrUnknownRecording) {
		t.Fatalf("another connection finished the recording: %v", err)
	}
	if err := app.CancelDictation(other, id); !errors.Is(err, dictation.ErrUnknownRecording) {
		t.Fatalf("another connection cancelled the recording: %v", err)
	}
	// A call that came over no connection cannot have opened it either.
	if _, err := app.MintDictationChunkTicket(context.Background(), id, 0, 4); !errors.Is(err, dictation.ErrUnknownRecording) {
		t.Fatalf("a call with no connection minted a ticket: %v", err)
	}
	if _, err := app.FinishDictation(context.Background(), id, 0); !errors.Is(err, dictation.ErrUnknownRecording) {
		t.Fatalf("a call with no connection finished the recording: %v", err)
	}
	if err := app.CancelDictation(context.Background(), id); !errors.Is(err, dictation.ErrUnknownRecording) {
		t.Fatalf("a call with no connection cancelled the recording: %v", err)
	}
	if app.dictationRecordings().Open() != 1 {
		t.Fatal("another connection's calls changed the recording")
	}
	ownerState.RunCleanups()
	if open := app.dictationRecordings().Open(); open != 0 {
		t.Fatalf("open recordings after the owner disconnected = %d", open)
	}

	cancelled, err := app.BeginDictation(owner, "audio/webm")
	if err == nil {
		t.Fatalf("a closing connection opened recording %s", cancelled)
	}
}

// Cancelling unbinds the recording from its connection, so a long-lived
// connection keeps no callback per recording it ever made.
func TestCancelDictationReleasesTheRecording(t *testing.T) {
	t.Parallel()
	app := newDictationTestApp(t, "fresh")
	ctx, state := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	id, err := app.BeginDictation(ctx, "audio/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.CancelDictation(ctx, id); err != nil {
		t.Fatal(err)
	}
	if app.dictationRecordings().Open() != 0 || state.Bound(dictationCleanupKey(id)) {
		t.Fatal("a cancelled recording is still held")
	}
	if _, err := app.FinishDictation(ctx, id, 0); !errors.Is(err, dictation.ErrUnknownRecording) {
		t.Fatalf("finishing a cancelled recording = %v", err)
	}
}

// ChatGPT refusing the access token has the Codex CLI refresh the sign-in
// in the canonical home, and the recording is sent once more with the
// refreshed token. A refresh that changes nothing ends in a sign-in error.
func TestDictationRefreshesARefusedSignInOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		refreshed bool
	}{
		{name: "refresh fixes it", refreshed: true},
		{name: "refresh changes nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newDictationTestApp(t, "refused")
			paths, err := providerCredentialsForTest(t, app).Paths(string(provider.Codex))
			if err != nil {
				t.Fatal(err)
			}
			refresh := ""
			if tc.refreshed {
				fresh, err := json.Marshal(string(dictationAuth(t, "fresh")))
				if err != nil {
					t.Fatal(err)
				}
				var quoted string
				if err := json.Unmarshal(fresh, &quoted); err != nil {
					t.Fatal(err)
				}
				refresh = "printf '%s' '" + quoted + "' > '" + filepath.Join(paths.SharedHome, paths.CredentialFile) + "'\n"
			}
			binary := filepath.Join(t.TempDir(), "mock-codex")
			mockexec.Write(t, binary, "#!/bin/bash\n"+
				"read -r _ || true\n"+
				`printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"agent_overflow/0.200.0 (Linux 6.6.0; x86_64) xterm-256color"}}'`+"\n"+
				"read -r _ || true\nread -r _ || true\n"+
				refresh+
				`printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"account":{"type":"chatgpt","email":"selected@example.com","planType":"pro"},"requiresOpenaiAuth":true}}'`+"\n")
			if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
				t.Fatal(err)
			}

			id, err := app.BeginDictation(connCtx(t), "audio/webm")
			if err != nil {
				t.Fatal(err)
			}
			uploadDictation(t, app, id, "frames")
			text, err := app.FinishDictation(connCtx(t), id, 1)
			if tc.refreshed {
				if err != nil || text != "Heard 6 bytes of audio/webm" {
					t.Fatalf("FinishDictation = %q, %v", text, err)
				}
				return
			}
			if !errors.Is(err, provideraccountapp.ErrCodexLoginRefused) {
				t.Fatalf("FinishDictation err = %v, want ErrCodexLoginRefused", err)
			}
		})
	}
}

// Dictation is off until Settings > Voice turns it on: no source is offered,
// no recording opens, and one switched off mid-recording is never sent.
func TestDictationIsOffUntilTurnedOn(t *testing.T) {
	t.Parallel()
	app := newDictationTestApp(t, "fresh")
	enableDictation(t, app, false)
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})

	sources, err := app.DictationSources()
	if err != nil || sources.Subscription || !sources.ChatGPTSignIn {
		t.Fatalf("DictationSources = %+v, %v; want signed in but off", sources, err)
	}
	if _, err := app.BeginDictation(ctx, "audio/webm"); !errors.Is(err, ErrDictationOff) {
		t.Fatalf("BeginDictation err = %v, want ErrDictationOff", err)
	}

	enableDictation(t, app, true)
	id, err := app.BeginDictation(ctx, "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	uploadDictation(t, app, id, "frames")
	enableDictation(t, app, false)
	if _, err := app.FinishDictation(ctx, id, 1); !errors.Is(err, ErrDictationOff) {
		t.Fatalf("FinishDictation err = %v, want ErrDictationOff", err)
	}
	if open := app.dictationRecordings().Open(); open != 0 {
		t.Fatalf("open recordings after a refused finish = %d", open)
	}
}

// blockingTranscription holds every request until its context ends.
type blockingTranscription struct{ started chan struct{} }

func (b blockingTranscription) RoundTrip(req *http.Request) (*http.Response, error) {
	close(b.started)
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// A transcription still belongs to the recording's connection: it counts
// against the caps while it runs, and CancelDictation or the connection's
// end stops it.
func TestDictationCancelStopsATranscriptionInFlight(t *testing.T) {
	t.Parallel()
	for _, stop := range []string{"cancel", "disconnect"} {
		t.Run(stop, func(t *testing.T) {
			app := newDictationTestApp(t, "fresh")
			started := make(chan struct{})
			app.rateLimitProbeClientOverride = &http.Client{Transport: blockingTranscription{started: started}}
			ctx, state := transport.WithConnState(context.Background(), transport.ConnPrincipal{})

			id, err := app.BeginDictation(ctx, "audio/webm")
			if err != nil {
				t.Fatal(err)
			}
			uploadDictation(t, app, id, "frames")
			finished := make(chan error, 1)
			go func() {
				_, err := app.FinishDictation(ctx, id, 1)
				finished <- err
			}()
			<-started
			if open := app.dictationRecordings().Open(); open != 1 {
				t.Fatalf("open recordings while transcribing = %d, want 1", open)
			}
			if stop == "cancel" {
				if err := app.CancelDictation(ctx, id); err != nil {
					t.Fatal(err)
				}
			} else {
				state.RunCleanups()
			}
			if err := <-finished; !errors.Is(err, context.Canceled) {
				t.Fatalf("FinishDictation err = %v, want context.Canceled", err)
			}
			if open := app.dictationRecordings().Open(); open != 0 {
				t.Fatalf("open recordings after the stop = %d", open)
			}
		})
	}
}

// A Finish the recording refuses leaves it with its connection: the owner
// can still cancel a transcription in flight, and a connection's end still
// drops a recording whose piece was uploading.
func TestARefusedFinishKeepsTheRecordingWithItsConnection(t *testing.T) {
	t.Parallel()
	t.Run("finished again while transcribing", func(t *testing.T) {
		t.Parallel()
		app := newDictationTestApp(t, "fresh")
		started := make(chan struct{})
		app.rateLimitProbeClientOverride = &http.Client{Transport: blockingTranscription{started: started}}
		ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
		id, err := app.BeginDictation(ctx, "audio/webm")
		if err != nil {
			t.Fatal(err)
		}
		uploadDictation(t, app, id, "frames")
		finished := make(chan error, 1)
		go func() {
			_, err := app.FinishDictation(ctx, id, 1)
			finished <- err
		}()
		<-started
		if _, err := app.FinishDictation(ctx, id, 1); err == nil {
			t.Fatal("a transcribing recording was finished again")
		}
		if err := app.CancelDictation(ctx, id); err != nil {
			t.Fatalf("CancelDictation after a refused finish = %v", err)
		}
		if err := <-finished; !errors.Is(err, context.Canceled) {
			t.Fatalf("FinishDictation err = %v, want context.Canceled", err)
		}
	})
	t.Run("finished while a piece uploads", func(t *testing.T) {
		t.Parallel()
		app := newDictationTestApp(t, "fresh")
		ctx, state := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
		id, err := app.BeginDictation(ctx, "audio/webm")
		if err != nil {
			t.Fatal(err)
		}
		release := make(chan struct{})
		uploaded := make(chan error, 1)
		go func() {
			uploaded <- DictationTransfer(app).AppendDictationChunk(transport.DictationChunk{
				RecordingID: id, Seq: 0, Size: 6, Body: io.MultiReader(gate(release), strings.NewReader("frames")),
			})
		}()
		for app.dictationRecordings().Held() != 6 {
			runtime.Gosched()
		}
		if _, err := app.FinishDictation(ctx, id, 1); err == nil {
			t.Fatal("a recording was finished while a piece was uploading")
		}
		close(release)
		if err := <-uploaded; err != nil {
			t.Fatal(err)
		}
		state.RunCleanups()
		if open := app.dictationRecordings().Open(); open != 0 {
			t.Fatalf("open recordings after the connection ended = %d", open)
		}
	})
}

// gate is a reader that yields nothing until release closes.
type gate chan struct{}

func (g gate) Read([]byte) (int, error) {
	<-g
	return 0, io.EOF
}

// cancelThenRefuse stops the recording's transcription and answers its
// request the way an expired sign-in is answered.
type cancelThenRefuse struct{ cancel func() }

func (c cancelThenRefuse) RoundTrip(req *http.Request) (*http.Response, error) {
	c.cancel()
	return &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"detail":"token expired"}`)),
		Request:    req,
	}, nil
}

// A transcription abandoned while ChatGPT refuses its sign-in starts no
// refresh: the Codex CLI is never run for it.
func TestAnAbandonedDictationStartsNoRefresh(t *testing.T) {
	t.Parallel()
	app := newDictationTestApp(t, "refused")
	ran := filepath.Join(t.TempDir(), "codex-ran")
	binary := filepath.Join(t.TempDir(), "mock-codex")
	mockexec.Write(t, binary, "#!/bin/bash\ntouch '"+ran+"'\nexit 1\n")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	id, err := app.BeginDictation(ctx, "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	uploadDictation(t, app, id, "frames")
	app.rateLimitProbeClientOverride = &http.Client{Transport: cancelThenRefuse{cancel: func() { app.dictationRecordings().Cancel(id) }}}

	if _, err := app.FinishDictation(ctx, id, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("FinishDictation err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(ran); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the Codex CLI ran a refresh for an abandoned transcription (stat err = %v)", err)
	}
}

// Switching subscription dictation off ends a transcription in flight at
// once, and a recording begun before an off and straight back on is still
// refused: the write's revision decides, not the switch's value later.
func TestSwitchingDictationOffEndsItsTranscriptions(t *testing.T) {
	t.Parallel()
	t.Run("transcribing", func(t *testing.T) {
		t.Parallel()
		app := newDictationTestApp(t, "fresh")
		started := make(chan struct{})
		app.rateLimitProbeClientOverride = &http.Client{Transport: blockingTranscription{started: started}}
		ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
		id, err := app.BeginDictation(ctx, "audio/webm")
		if err != nil {
			t.Fatal(err)
		}
		uploadDictation(t, app, id, "frames")
		finished := make(chan error, 1)
		go func() {
			_, err := app.FinishDictation(ctx, id, 1)
			finished <- err
		}()
		<-started
		enableDictation(t, app, false)
		enableDictation(t, app, true)
		if err := <-finished; !errors.Is(err, ErrDictationOff) {
			t.Fatalf("FinishDictation err = %v, want ErrDictationOff", err)
		}
		if open := app.dictationRecordings().Open(); open != 0 {
			t.Fatalf("open recordings after the switch = %d", open)
		}
	})
	t.Run("recording", func(t *testing.T) {
		t.Parallel()
		app := newDictationTestApp(t, "fresh")
		ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
		id, err := app.BeginDictation(ctx, "audio/webm")
		if err != nil {
			t.Fatal(err)
		}
		uploadDictation(t, app, id, "frames")
		enableDictation(t, app, false)
		enableDictation(t, app, true)
		if held := app.dictationRecordings().Held(); held != 0 {
			t.Fatalf("audio held after the switch = %d", held)
		}
		if _, err := app.FinishDictation(ctx, id, 1); !errors.Is(err, ErrDictationOff) {
			t.Fatalf("FinishDictation err = %v, want ErrDictationOff", err)
		}
		if open := app.dictationRecordings().Open(); open != 0 {
			t.Fatalf("open recordings after the refused finish = %d", open)
		}
	})
}

// countingRefusal answers every transcription as an expired sign-in and
// counts them.
type countingRefusal struct{ sent *atomic.Int32 }

func (c countingRefusal) RoundTrip(req *http.Request) (*http.Response, error) {
	c.sent.Add(1)
	return &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"detail":"token expired"}`)),
		Request:    req,
	}, nil
}

// Switching dictation off while the sign-in refresh runs ends the
// transcription then, and its retry is never sent.
func TestSwitchingDictationOffEndsTheRetry(t *testing.T) {
	t.Parallel()
	app := newDictationTestApp(t, "refused")
	dir := t.TempDir()
	refreshing, release, exited := filepath.Join(dir, "refreshing"), filepath.Join(dir, "release"), filepath.Join(dir, "exited")
	binary := filepath.Join(dir, "mock-codex")
	mockexec.Write(t, binary, "#!/bin/bash\ntouch '"+refreshing+"'\n"+
		"while [ ! -e '"+release+"' ]; do sleep 0.01; done\ntouch '"+exited+"'\nexit 1\n")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	var sent atomic.Int32
	app.rateLimitProbeClientOverride = &http.Client{Transport: countingRefusal{sent: &sent}}
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	id, err := app.BeginDictation(ctx, "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	uploadDictation(t, app, id, "frames")
	finished := make(chan error, 1)
	go func() {
		_, err := app.FinishDictation(ctx, id, 1)
		finished <- err
	}()
	waitForFile(t, refreshing)
	enableDictation(t, app, false)
	if err := <-finished; !errors.Is(err, ErrDictationOff) {
		t.Fatalf("FinishDictation err = %v, want ErrDictationOff", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, exited)
	if n := sent.Load(); n != 1 {
		t.Fatalf("transcription requests = %d, want the first alone", n)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A transcription that outlasts the computer's budget ends with a readable
// error and releases the recording.
func TestDictationEndsAtItsBudget(t *testing.T) {
	t.Parallel()
	app := newDictationTestApp(t, "fresh")
	const budget = 100 * time.Millisecond
	app.dictationRecordings()
	app.dictation = dictation.New(0, budget)
	started := make(chan struct{})
	app.rateLimitProbeClientOverride = &http.Client{Transport: blockingTranscription{started: started}}
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})

	id, err := app.BeginDictation(ctx, "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	uploadDictation(t, app, id, "frames")
	_, err = app.FinishDictation(ctx, id, 1)
	if code, _, _ := errorsx.PublicDetails(err); code != dictationTranscriptionTimeoutCode {
		t.Fatalf("FinishDictation err = %v, want the readable timeout", err)
	}
	if open := app.dictationRecordings().Open(); open != 0 {
		t.Fatalf("open recordings after the budget ran out = %d", open)
	}
}

// answerTranscription answers every transcription with status.
type answerTranscription struct{ status int }

func (a answerTranscription) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: a.status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"detail":"Forbidden"}`)),
		Request:    req,
	}, nil
}

// A 403 is ChatGPT refusing the request, not the token: the Codex CLI is
// never run to refresh the sign-in, and the mic does not ask for a new one.
func TestARefusedTranscriptionRefreshesNothing(t *testing.T) {
	t.Parallel()
	app := newDictationTestApp(t, "fresh")
	ran := filepath.Join(t.TempDir(), "codex-ran")
	binary := filepath.Join(t.TempDir(), "mock-codex")
	mockexec.Write(t, binary, "#!/bin/bash\ntouch '"+ran+"'\nexit 1\n")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	app.rateLimitProbeClientOverride = &http.Client{Transport: answerTranscription{status: http.StatusForbidden}}
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	id, err := app.BeginDictation(ctx, "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	uploadDictation(t, app, id, "frames")

	_, err = app.FinishDictation(ctx, id, 1)
	_, message, public := errorsx.PublicDetails(err)
	if !public || !strings.Contains(message, "HTTP 403") || strings.Contains(message, "sign in to Codex again") {
		t.Fatalf("FinishDictation err = %v (public %q), want its own readable 403", err, message)
	}
	if errors.Is(err, provideraccountapp.ErrCodexLoginRefused) {
		t.Fatalf("FinishDictation err = %v, a 403 is not a refused sign-in", err)
	}
	if _, err := os.Stat(ran); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the Codex CLI ran a refresh for a 403 (stat err = %v)", err)
	}
}

// dictationConnectionTie is BeginDictation's row of connectionTieChecks: a
// recording, which only its own connection cancels.
func dictationConnectionTie(t *testing.T) connectionTie {
	app := newDictationTestApp(t, "fresh")
	return connectionTie{
		open:    func(ctx context.Context) (string, error) { return app.BeginDictation(ctx, "audio/webm") },
		held:    func() int { return app.dictationRecordings().Open() },
		release: app.CancelDictation,
		key:     dictationCleanupKey,
	}
}
