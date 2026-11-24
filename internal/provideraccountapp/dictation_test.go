package provideraccountapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
)

// refusingTranscription answers every transcription as an expired sign-in
// and says when it has.
type refusingTranscription struct{ refused chan<- struct{} }

func (r refusingTranscription) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	r.refused <- struct{}{}
	return &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"detail":"token expired"}`)),
		Request:    req,
	}, nil
}

// refusedDictationManager is newTestManager with a selected Codex ChatGPT
// sign-in that the transcription endpoint refuses. Its poisoned binary
// fails the test if the Codex CLI is spawned for a refresh.
func refusedDictationManager(t *testing.T) (*Manager, *provideraccounts.Store, <-chan struct{}) {
	t.Helper()
	manager, store, credentials := newTestManager(t)
	claims, err := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "ws-1"}})
	if err != nil {
		t.Fatal(err)
	}
	segment := base64.RawURLEncoding.EncodeToString
	login, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token":      segment([]byte(`{"alg":"none"}`)) + "." + segment(claims) + "." + segment([]byte("sig")),
			"access_token":  "expired",
			"refresh_token": "rt-expired",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(provideraccounts.Account{
		ID: "selected", Provider: string(provider.Codex), Email: "selected@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteNativeCredentialForTest(string(provider.Codex), login); err != nil {
		t.Fatal(err)
	}
	refused := make(chan struct{}, 1)
	manager.deps.HTTPClient = func() *http.Client {
		return &http.Client{Transport: refusingTranscription{refused: refused}}
	}
	return manager, store, refused
}

func transcribeInBackground(ctx context.Context, manager *Manager) <-chan error {
	finished := make(chan error, 1)
	go func() {
		_, err := manager.TranscribeWithCodexLogin(ctx, CodexTranscription{
			Audio: []byte("frames"), MimeType: "audio/webm", Endpoint: codex.TranscribeURL,
		})
		finished <- err
	}()
	return finished
}

// A transcription abandoned while it waits for the reconcile lock returns
// at once, not when the lock frees: whatever holds the lock, the caller's
// bound on the transcription holds. The refresh it left behind still
// starts nothing and ends inside its own work admission; it takes the
// lock's file in the provider home, so the test joins that work before its
// home is removed.
func TestAnAbandonedTranscriptionDoesNotWaitForTheLock(t *testing.T) {
	manager, _, refused := refusedDictationManager(t)
	var work sync.WaitGroup
	manager.deps.BeginWork = func(ctx context.Context) (func(), error) {
		work.Add(1)
		return work.Done, ctx.Err()
	}
	lock := manager.reconcileMutex(string(provider.Codex))
	lock.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	finished := transcribeInBackground(ctx, manager)
	<-refused
	cancel()

	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("TranscribeWithCodexLogin err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an abandoned transcription waited for the reconcile lock")
	}
	lock.Unlock()
	work.Wait()
}

// A transcription abandoned after the promotion, as late as the refresh's
// configuration, starts no refresh: every wait before it (the reconcile
// lock, the slot promotion) is behind the one check.
func TestATranscriptionAbandonedJustBeforeTheRefreshStartsNone(t *testing.T) {
	manager, _, _ := refusedDictationManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	resolve := manager.deps.ProviderBinary
	manager.deps.ProviderBinary = func(providerName string) string {
		cancel()
		return resolve(providerName)
	}

	if err := <-transcribeInBackground(ctx, manager); !errors.Is(err, context.Canceled) {
		t.Fatalf("TranscribeWithCodexLogin err = %v, want context.Canceled", err)
	}
}
