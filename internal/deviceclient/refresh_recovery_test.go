package deviceclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type renewalTestTransport func(*http.Request) (*http.Response, error)

func (f renewalTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func recoveryClient(t *testing.T) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	var mu sync.Mutex
	operations := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(RefreshRecoveryHeader, "1")
		if r.URL.Path == "/healthz" {
			json.NewEncoder(w).Encode(map[string]string{"backendId": "backend-a"})
			return
		}
		if r.URL.Path == authTicketPath {
			json.NewEncoder(w).Encode(map[string]string{"ticket": "ticket"})
			return
		}
		if r.URL.Path != authTokenRecoverPath {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var body struct {
			RefreshSecret string `json:"refreshSecret"`
			Next          string `json:"nextRefreshSecret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body.Next == "" {
			t.Error("missing successor")
			http.Error(w, "missing successor", 400)
			return
		}
		mu.Lock()
		old, used := operations[body.RefreshSecret]
		if used && old != body.Next {
			mu.Unlock()
			t.Error("two processes chose different successors")
			http.Error(w, "conflicting renewal", 401)
			return
		}
		operations[body.RefreshSecret] = body.Next
		mu.Unlock()
		json.NewEncoder(w).Encode(grant{SessionID: "session-1", Credential: "credential-" + body.Next, RefreshSecret: body.Next, ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli()})
	}))
	t.Cleanup(server.Close)
	be := &backend{Server: server}
	client, _ := openAgainst(t, be, func(s *Session) { supported := true; s.RefreshRecovery = &supported })
	return client, &calls
}

func TestRecoverableRenewalSurvivesLostReplyAndClientRestart(t *testing.T) {
	client, calls := recoveryClient(t)
	base := client.http.Transport
	client.http.Transport = renewalTestTransport(func(r *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(r)
		if err != nil {
			return response, err
		}
		response.Body.Close()
		return nil, io.ErrUnexpectedEOF
	})
	if err := client.renew(t.Context()); err == nil {
		t.Fatal("lost reply reported success")
	}
	pending, err := LoadSession(client.dir, "backend-a")
	if err != nil {
		t.Fatal(err)
	}
	if pending.PendingNextSecret == "" || pending.RefreshSecret != "refresh-0" {
		t.Fatal("lost durable recovery state")
	}
	restarted, err := Open(client.dir, pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.renew(t.Context()); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadSession(client.dir, "backend-a")
	if err != nil {
		t.Fatal(err)
	}
	if saved.RefreshSecret != pending.PendingNextSecret || saved.PendingNextSecret != "" || calls.Load() != 2 {
		t.Fatal("retry did not finish the same operation")
	}
}

func TestRecoverableRenewalLateReplyCannotOverwriteNewerGeneration(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		t.Run(map[bool]string{false: "grant", true: "refusal"}[refusal], func(t *testing.T) {
			first, calls := recoveryClient(t)
			base := first.http.Transport
			arrived, resume := make(chan struct{}), make(chan struct{})
			first.http.Transport = renewalTestTransport(func(r *http.Request) (*http.Response, error) {
				response, err := base.RoundTrip(r)
				close(arrived)
				select {
				case <-resume:
				case <-r.Context().Done():
				}
				if refusal && response != nil {
					response.Body.Close()
					response.StatusCode = 401
					response.Body = io.NopCloser(strings.NewReader(`{"reason":"refresh_reused"}`))
				}
				return response, err
			})
			done := make(chan error, 1)
			go func() { done <- first.renew(t.Context()) }()
			<-arrived
			pending, err := LoadSession(first.dir, "backend-a")
			if err != nil {
				t.Fatal(err)
			}
			second, err := Open(first.dir, pending)
			if err != nil {
				t.Fatal(err)
			}
			if err := second.renew(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := second.renew(t.Context()); err != nil {
				t.Fatal(err)
			}
			newest := second.Session()
			close(resume)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			saved, err := LoadSession(first.dir, "backend-a")
			if err != nil {
				t.Fatal(err)
			}
			if saved.RefreshSecret != newest.RefreshSecret || saved.PendingNextSecret != "" || calls.Load() != 3 {
				t.Fatal("late reply changed a newer generation")
			}
		})
	}
}

func TestProfileTransactionsPreserveUnknownFieldsAndReplacement(t *testing.T) {
	client, _ := recoveryClient(t)
	path, err := sessionPath(client.dir, "backend-a")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["futureRoute"] = json.RawMessage(`{"addresses":["https://future.example"]}`)
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := client.renew(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := client.SetNickname("GPU"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	var route struct {
		Addresses []string `json:"addresses"`
	}
	if err := json.Unmarshal(fields["futureRoute"], &route); err != nil {
		t.Fatal(err)
	}
	if len(route.Addresses) != 1 || route.Addresses[0] != "https://future.example" {
		t.Fatal("unknown field was discarded")
	}
	newer := client.Session()
	newer.SessionID = "replacement-session"
	if err := SaveSession(client.dir, newer); err != nil {
		t.Fatal(err)
	}
	if err := client.Forget(); err != nil {
		t.Fatal(err)
	}
	kept, err := LoadSession(client.dir, "backend-a")
	if err != nil || kept.SessionID != newer.SessionID {
		t.Fatal("old owner removed a newer pairing")
	}
}

func TestProfileLockValidatesAndHonoursContention(t *testing.T) {
	for _, bad := range []struct{ dir, name string }{{"", "device-key.pem"}, {t.TempDir(), "../escape"}} {
		if unlock, err := lockProfile(t.Context(), bad.dir, bad.name); err == nil {
			unlock()
			t.Fatal("accepted invalid path")
		}
	}
	dir := t.TempDir()
	release, err := lockProfile(t.Context(), dir, "session")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	if unlock, err := lockProfile(ctx, dir, "session"); !errors.Is(err, context.DeadlineExceeded) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("contended lock: %v", err)
	}
	release()
	unlock, err := lockProfile(t.Context(), dir, "session")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if _, err := os.Stat(filepath.Join(dir, "locks", "session.lock")); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryCannotDowngradeOrFollowCredentialRedirect(t *testing.T) {
	t.Run("older host", func(t *testing.T) {
		be := newBackend(t)
		client, _ := openAgainst(t, be, func(s *Session) { supported := true; s.RefreshRecovery = &supported })
		for i := 0; i < 2; i++ {
			if err := client.renew(t.Context()); err == nil {
				t.Fatal("unsupported endpoint succeeded")
			}
		}
		if be.rotations.Load() != 0 {
			t.Fatal("older host consumed a recoverable renewal")
		}
		held, err := LoadSession(client.dir, "backend-a")
		if err != nil || held.PendingNextSecret == "" {
			t.Fatal("pending recovery was discarded")
		}
	})
	t.Run("redirect", func(t *testing.T) {
		var leaked atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1); w.WriteHeader(200) }))
		defer target.Close()
		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
		defer redirect.Close()
		client, _ := openAgainst(t, &backend{Server: redirect}, nil)
		if err := client.renew(t.Context()); err == nil {
			t.Fatal("redirected renewal succeeded")
		}
		if leaked.Load() != 0 {
			t.Fatal("credential followed a redirect")
		}
		if _, err := LoadSession(client.dir, "backend-a"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLegacyRenewalIsSerializedAcrossClientsSharingAProfile(t *testing.T) {
	be := newBackend(t)
	first, dir := openAgainst(t, be, nil)
	second, err := Open(dir, first.Session())
	if err != nil {
		t.Fatal(err)
	}
	held := heldRotationTransport{base: first.http.Transport, arrived: make(chan struct{}), resume: make(chan struct{})}
	first.http.Transport = held
	one, two := make(chan error, 1), make(chan error, 1)
	go func() { one <- first.renew(t.Context()) }()
	<-held.arrived
	go func() { two <- second.renew(t.Context()) }()
	close(held.resume)
	if err := <-one; err != nil {
		t.Fatal(err)
	}
	if err := <-two; err != nil {
		t.Fatal(err)
	}
	if be.rotations.Load() != 1 {
		t.Fatal("two callers spent the same legacy secret")
	}
}

func TestRenewalKeepsUnknownAndRetryableRefusals(t *testing.T) {
	for _, reason := range []string{"future_host_reason", "proof_replayed", "outside_time_window"} {
		t.Run(reason, func(t *testing.T) {
			be := newBackend(t)
			be.refusal = reason
			client, dir := openAgainst(t, be, nil)
			if err := client.renew(t.Context()); err == nil || errors.Is(err, ErrSessionEnded) {
				t.Fatalf("unexpected renewal result: %v", err)
			}
			if _, err := LoadSession(dir, "backend-a"); err != nil {
				t.Fatal("nonterminal refusal removed the pairing")
			}
		})
	}
}
