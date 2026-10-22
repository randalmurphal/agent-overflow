package provideraccountapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"

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
func refusedDictationManager(t *testing.T) (*Manager, *provideraccounts.Credentials, <-chan struct{}) {
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
	return manager, credentials, refused
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

// A transcription abandoned while it waits for the reconcile lock starts no
// refresh once it gets the lock.
func TestAnAbandonedTranscriptionWaitingForTheLockStartsNoRefresh(t *testing.T) {
	manager, _, refused := refusedDictationManager(t)
	lock := manager.reconcileMutex(string(provider.Codex))
	lock.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	finished := transcribeInBackground(ctx, manager)
	<-refused
	cancel()
	lock.Unlock()

	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("TranscribeWithCodexLogin err = %v, want context.Canceled", err)
	}
}

// A transcription abandoned while promoting the selected slot waits on that
// slot's lock (another process holds it) starts no refresh once the lock is
// free.
func TestAnAbandonedTranscriptionWaitingForTheSlotStartsNoRefresh(t *testing.T) {
	manager, credentials, refused := refusedDictationManager(t)
	releaseSlot, err := credentials.LockSlotForTest(string(provider.Codex), "selected")
	if err != nil {
		t.Fatal(err)
	}
	lock := manager.reconcileMutex(string(provider.Codex))
	lock.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	finished := transcribeInBackground(ctx, manager)
	<-refused
	lock.Unlock()
	// Once the transcription holds the reconcile lock it is past everything
	// before the promotion, which waits on the slot.
	for lock.TryLock() {
		lock.Unlock()
		runtime.Gosched()
	}
	cancel()
	if err := releaseSlot(); err != nil {
		t.Fatal(err)
	}

	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("TranscribeWithCodexLogin err = %v, want context.Canceled", err)
	}
}
