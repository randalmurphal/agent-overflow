package forgeapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedirectDropsCredentialsAcrossHosts(t *testing.T) {
	t.Parallel()
	var blob seen
	blobSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blob.record(r)
		_, _ = fmt.Fprint(w, "log text")
	}))
	defer blobSrv.Close()
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && r.Header.Get("Private-Token") == "" {
			http.Error(w, "no credential", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, blobSrv.URL+"/logs/1.txt?sig=signed", http.StatusFound)
	}))
	defer forge.Close()
	for _, tc := range []struct {
		forge string
		info  SourceInfo
	}{{ForgeGitHub, SourceInfo{Header: AuthBearer}}, {ForgeGitLab, SourceInfo{Header: AuthPrivateToken}}} {
		s, _ := liveService(t, &testSource{tokens: []string{testSecret}, info: tc.info}, forge)
		resp, err := s.client(tc.forge, "forge.example").Do(t.Context(), Request{Path: "jobs/1/logs"})
		if err != nil || string(resp.Body) != "log text" {
			t.Fatalf("%s: Do = %v", tc.forge, err)
		}
		req, _ := blob.last()
		if req.Header.Get("Authorization") != "" || req.Header.Get("Private-Token") != "" {
			t.Fatalf("%s: the blob host received the forge credential: %v", tc.forge, req.Header)
		}
	}
}

func TestTokenLifetime(t *testing.T) {
	t.Parallel()
	var accepted atomic.Value
	accepted.Store(testSecret)
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		if r.Header.Get("Authorization") != "Bearer "+accepted.Load().(string) {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = fmt.Fprint(w, `{"n":1}`)
	}))
	defer srv.Close()
	src := &testSource{tokens: []string{testSecret}}
	s, clock := liveService(t, src, srv)
	gh := s.GitHub("github.com")

	if _, err := gh.Do(t.Context(), Request{Path: "user"}); err != nil {
		t.Fatal(err)
	}
	if _, err := gh.Do(t.Context(), Request{Path: "user"}); err != nil || src.readCount() != 1 {
		t.Fatalf("second request read the token again (reads %d, err %v)", src.readCount(), err)
	}

	t.Run("re-read after five minutes", func(t *testing.T) {
		clock.advance(tokenRereadAfter)
		if _, err := gh.Do(t.Context(), Request{Path: "user"}); err != nil || src.readCount() != 2 {
			t.Fatalf("reads = %d, err %v", src.readCount(), err)
		}
	})

	t.Run("one refresh after a 401, and the ETag entries go with the old token", func(t *testing.T) {
		accepted.Store("fake-token-2")
		src.set(nil, "fake-token-2")
		before := src.readCount()
		resp, err := gh.Do(t.Context(), Request{Path: "user"})
		if err != nil || resp.NotModified || src.readCount() != before+1 {
			t.Fatalf("Do = %+v, %v (reads %d -> %d)", resp, err, before, src.readCount())
		}
		req, _ := got.last()
		if req.Header.Get("If-None-Match") != "" {
			t.Fatal("the ETag of the old token's answer was sent with the new token")
		}
		oldFP := fingerprint([]byte(testSecret))
		s.etags.mu.Lock()
		defer s.etags.mu.Unlock()
		for key := range s.etags.byKey {
			if key.fingerprint == oldFP {
				t.Fatal("the old token's ETag entries outlived the token change")
			}
		}
	})

	t.Run("a second 401 is a setup error", func(t *testing.T) {
		accepted.Store("fake-token-3")
		_, err := gh.Do(t.Context(), Request{Path: "user"})
		var setup *SetupError
		if !errors.As(err, &setup) || setup.Kind != SetupUnauthenticated || setup.Forge != ForgeGitHub {
			t.Fatalf("err = %v, want unauthenticated", err)
		}
	})

	t.Run("a failed read is cached for five seconds until DropNegative", func(t *testing.T) {
		clock.advance(tokenRereadAfter)
		src.set(UnauthenticatedError(ForgeGitHub, nil))
		before := src.readCount()
		for range 3 {
			if _, err := gh.Do(t.Context(), Request{Path: "user"}); err == nil {
				t.Fatal("request succeeded without a login")
			}
		}
		if src.readCount() != before+1 {
			t.Fatalf("reads = %d, want one inside the negative cache", src.readCount()-before)
		}
		clock.advance(tokenFailureTTL)
		_, _ = gh.Do(t.Context(), Request{Path: "user"})
		if src.readCount() != before+2 {
			t.Fatalf("reads = %d after the negative TTL, want 2", src.readCount()-before)
		}
		// A person's own request reads the login again inside the negative
		// TTL; a background one keeps the cached failure.
		_, _ = gh.Do(t.Context(), Request{Path: "user"})
		if src.readCount() != before+2 {
			t.Fatalf("reads = %d, want the background request answered from the negative cache", src.readCount()-before)
		}
		_, _ = gh.Do(WithInteractive(t.Context()), Request{Path: "user"})
		if src.readCount() != before+3 {
			t.Fatalf("reads = %d, want an interactive request to drop the negative cache", src.readCount()-before)
		}
		accepted.Store("fake-token-4")
		src.set(nil, "fake-token-4")
		s.DropNegative("GitHub.com")
		if _, err := gh.Do(t.Context(), Request{Path: "user"}); err != nil {
			t.Fatalf("after DropNegative: %v", err)
		}
	})

	t.Run("Close zeroes the token and refuses requests", func(t *testing.T) {
		gh.credMu.RLock()
		backing := gh.token.b
		gh.credMu.RUnlock()
		s.Close()
		for _, c := range backing {
			if c != 0 {
				t.Fatal("Close left token bytes behind")
			}
		}
		if _, err := gh.Do(t.Context(), Request{Path: "user"}); !errors.Is(err, errClosed) {
			t.Fatalf("after Close: %v", err)
		}
	})
}

// The request a Client builds carries no credential: the token is added
// only on authTransport's per-hop clone. The forge still receives it.
func TestBuiltRequestCarriesNoCredential(t *testing.T) {
	t.Parallel()
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got.record(r) }))
	defer srv.Close()
	for _, tc := range []struct {
		forge string
		info  SourceInfo
	}{{ForgeGitHub, SourceInfo{Header: AuthBearer}}, {ForgeGitLab, SourceInfo{Header: AuthPrivateToken}}} {
		s, _ := liveService(t, &testSource{tokens: []string{testSecret}, info: tc.info}, srv)
		c := s.client(tc.forge, "forge.example")
		ex, err := c.prepare(t.Context(), Request{Path: "user"}, false)
		if err != nil {
			t.Fatal(err)
		}
		dump, err := httputil.DumpRequestOut(ex.req, true)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(dump, []byte(testSecret)) || bytes.Contains(bytes.ToLower(dump), []byte("authorization")) || bytes.Contains(bytes.ToLower(dump), []byte("private-token")) {
			t.Fatalf("%s: the built request carries a credential:\n%s", tc.forge, dump)
		}
		if _, err := c.Do(t.Context(), Request{Path: "user"}); err != nil {
			t.Fatal(err)
		}
		req, _ := got.last()
		if req.Header.Get("Authorization")+req.Header.Get("Private-Token") == "" {
			t.Fatalf("%s: the forge received no credential", tc.forge)
		}
	}
}

// GitLab answers a merge the login may not perform with 401, the status
// an expired token gets. After the refresh, a live login (its own user
// still reads) makes the 401 the forge's refusal of the action; a dead
// one is still unauthenticated. A read's second 401 probes nothing.
func TestMutationUnauthorizedIsARefusalWhileTheLoginReads(t *testing.T) {
	t.Parallel()
	var userStatus atomic.Int32
	userStatus.Store(http.StatusOK)
	var userProbes, merges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/user"):
			userProbes.Add(1)
			w.WriteHeader(int(userStatus.Load()))
			_, _ = fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodPut:
			merges.Add(1)
			http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
		default:
			http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	src := &testSource{tokens: []string{testSecret}, info: SourceInfo{Header: AuthPrivateToken}}
	s, _ := liveService(t, src, srv)
	gl := s.client(ForgeGitLab, "gitlab.example")

	_, err := gl.Do(t.Context(), Request{Method: http.MethodPut, Path: "projects/1/merge_requests/2/merge", Body: map[string]any{"sha": "abc"}})
	var status *StatusError
	if !errors.As(err, &status) || status.Status != http.StatusUnauthorized {
		t.Fatalf("merge refusal = %v, want the 401 StatusError", err)
	}
	var setup *SetupError
	if errors.As(err, &setup) {
		t.Fatalf("a refused merge read as a login failure: %v", err)
	}
	if merges.Load() != 2 || userProbes.Load() != 1 {
		t.Fatalf("merge attempts %d (want 2: one refresh), user probes %d (want 1)", merges.Load(), userProbes.Load())
	}

	userStatus.Store(http.StatusUnauthorized)
	_, err = gl.Do(t.Context(), Request{Method: http.MethodPut, Path: "projects/1/merge_requests/2/merge", Body: map[string]any{"sha": "abc"}})
	if !errors.As(err, &setup) || setup.Kind != SetupUnauthenticated {
		t.Fatalf("merge with a dead login = %v, want unauthenticated", err)
	}

	userStatus.Store(http.StatusOK)
	probesBefore := userProbes.Load()
	_, err = gl.Do(t.Context(), Request{Path: "projects/1/merge_requests/2"})
	if !errors.As(err, &setup) || userProbes.Load() != probesBefore {
		t.Fatalf("a read's second 401 = %v with %d probes, want unauthenticated and no probe", err, userProbes.Load()-probesBefore)
	}
}

// Mutations that reach their second 401 together probe the login after
// giving their host slots back: a probe that needed a slot while every
// slot was held by a mutation waiting on its probe could never run.
func TestConcurrentRefusedMutationsProbeOutsideTheirSlots(t *testing.T) {
	t.Parallel()
	var puts atomic.Int32
	firstRound, secondRound := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/user") {
			_, _ = fmt.Fprint(w, `{"id":1}`)
			return
		}
		// Each round answers once every mutation is in flight, so all of
		// them hold a slot when their 401 arrives.
		n := int(puts.Add(1))
		switch n {
		case hostConcurrency:
			close(firstRound)
		case 2 * hostConcurrency:
			close(secondRound)
		}
		round := firstRound
		if n > hostConcurrency {
			round = secondRound
		}
		select {
		case <-round:
		case <-r.Context().Done():
			return
		}
		http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	src := &testSource{tokens: []string{testSecret}, info: SourceInfo{Header: AuthPrivateToken}}
	s, _ := liveService(t, src, srv)
	gl := s.client(ForgeGitLab, "gitlab.example")

	errs := make([]error, hostConcurrency)
	start := time.Now()
	var wg sync.WaitGroup
	for i := range hostConcurrency {
		wg.Go(func() {
			_, errs[i] = gl.Do(t.Context(), Request{
				Method:  http.MethodPut,
				Path:    fmt.Sprintf("projects/1/merge_requests/%d/merge", i+1),
				Body:    map[string]any{"sha": "abc"},
				Timeout: 3 * time.Second,
			})
		})
	}
	wg.Wait()
	for i, err := range errs {
		var status *StatusError
		if !errors.As(err, &status) || status.Status != http.StatusUnauthorized {
			t.Fatalf("mutation %d = %v, want the 401 StatusError", i, err)
		}
		if _, setup := errors.AsType[*SetupError](err); setup {
			t.Fatalf("mutation %d read as a login failure: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the mutations settled after %v, waiting on slots", elapsed)
	}
}

// The probe tells a refusal from a dead login only when it is answered:
// a probe that fails any other way returns its own error, which the
// caller retries, never "log in again".
func TestMutationProbeFailureIsNotADeadLogin(t *testing.T) {
	t.Parallel()
	notSetup := func(t *testing.T, err error) {
		t.Helper()
		if _, setup := errors.AsType[*SetupError](err); setup {
			t.Fatalf("a failed probe read as a dead login: %v", err)
		}
	}
	cases := []struct {
		name    string
		user    func(w http.ResponseWriter, r *http.Request, arrived chan<- struct{})
		timeout time.Duration
		cancel  bool
		check   func(t *testing.T, err error)
	}{
		{name: "probe 200 is the forge's refusal",
			user: func(w http.ResponseWriter, _ *http.Request, _ chan<- struct{}) { _, _ = fmt.Fprint(w, `{"id":1}`) },
			check: func(t *testing.T, err error) {
				notSetup(t, err)
				if status, ok := errors.AsType[*StatusError](err); !ok || status.Status != http.StatusUnauthorized {
					t.Fatalf("err = %v, want the mutation's 401 StatusError", err)
				}
			}},
		{name: "probe 401 is a dead login",
			user: func(w http.ResponseWriter, _ *http.Request, _ chan<- struct{}) {
				http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
			},
			check: func(t *testing.T, err error) {
				if setup, ok := errors.AsType[*SetupError](err); !ok || setup.Kind != SetupUnauthenticated || setup.Host != "gitlab.example" {
					t.Fatalf("err = %v, want unauthenticated", err)
				}
				// The setup error carries the mutation's 401, not the probe's.
				if status, ok := errors.AsType[*StatusError](err); !ok || !strings.Contains(status.URL, "/merge") {
					t.Fatalf("err = %v, want the mutation's 401 underneath", err)
				}
			}},
		{name: "probe 429 is rate limited",
			user: func(w http.ResponseWriter, _ *http.Request, _ chan<- struct{}) {
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			check: func(t *testing.T, err error) {
				notSetup(t, err)
				if _, ok := errors.AsType[*RateLimitedError](err); !ok {
					t.Fatalf("err = %T %v, want *RateLimitedError", err, err)
				}
			}},
		{name: "probe 503 is the forge failing",
			user: func(w http.ResponseWriter, _ *http.Request, _ chan<- struct{}) {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			},
			check: func(t *testing.T, err error) {
				notSetup(t, err)
				if status, ok := errors.AsType[*StatusError](err); !ok || status.Status != http.StatusServiceUnavailable || !strings.HasSuffix(status.URL, "/user") {
					t.Fatalf("err = %v, want the probe's 503", err)
				}
			}},
		{name: "probe timeout is transient", timeout: 300 * time.Millisecond,
			user: func(_ http.ResponseWriter, r *http.Request, _ chan<- struct{}) { <-r.Context().Done() },
			check: func(t *testing.T, err error) {
				notSetup(t, err)
				if _, ok := errors.AsType[*TransientError](err); !ok {
					t.Fatalf("err = %T %v, want *TransientError", err, err)
				}
			}},
		{name: "probe canceled is the cancellation", cancel: true,
			user: func(_ http.ResponseWriter, r *http.Request, arrived chan<- struct{}) {
				arrived <- struct{}{}
				<-r.Context().Done()
			},
			check: func(t *testing.T, err error) {
				notSetup(t, err)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want the cancellation", err)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			arrived := make(chan struct{}, 1)
			var merges, probes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/user") {
					probes.Add(1)
					tc.user(w, r, arrived)
					return
				}
				merges.Add(1)
				http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
			}))
			defer srv.Close()
			s, _ := liveService(t, &testSource{tokens: []string{testSecret}, info: SourceInfo{Header: AuthPrivateToken}}, srv)
			gl := s.client(ForgeGitLab, "gitlab.example")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				go func() {
					select {
					case <-arrived:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			_, err := gl.Do(ctx, Request{Method: http.MethodPut, Path: "projects/1/merge_requests/2/merge", Body: map[string]any{"sha": "abc"}, Timeout: tc.timeout})
			tc.check(t, err)
			if merges.Load() != 2 || probes.Load() != 1 {
				t.Fatalf("merge attempts %d (want 2), probes %d (want 1)", merges.Load(), probes.Load())
			}
		})
	}
}

// CredentialFingerprint names the token the next request sends: the same
// secret the same name, another secret another, and a login replaced
// behind the client is read again after five minutes like a request reads
// it, with no request in between. Without a token it fails.
func TestCredentialFingerprint(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("CredentialFingerprint sent %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer srv.Close()
	src := &testSource{tokens: []string{testSecret}}
	s, clock := liveService(t, src, srv)
	gl := s.GitLab("gitlab.example.com")

	first, err := gl.CredentialFingerprint(t.Context())
	if err != nil || first == "" {
		t.Fatalf("CredentialFingerprint = %q, %v", first, err)
	}
	if again, err := gl.CredentialFingerprint(t.Context()); err != nil || again != first || src.readCount() != 1 {
		t.Fatalf("same token: %q vs %q, %v, reads %d", again, first, err, src.readCount())
	}
	if other, err := s.GitLab("other.example.com").CredentialFingerprint(t.Context()); err != nil || other != first {
		t.Fatalf("the same secret on another host = %q, %v, want %q", other, err, first)
	}

	src.set(nil, "fake-token-2")
	if within, err := gl.CredentialFingerprint(t.Context()); err != nil || within != first {
		t.Fatalf("within the re-read interval = %q, %v, want the current token's %q", within, err, first)
	}
	clock.advance(tokenRereadAfter)
	second, err := gl.CredentialFingerprint(t.Context())
	if err != nil || second == "" || second == first {
		t.Fatalf("after the login changed = %q, %v (first %q)", second, err, first)
	}
	if second != fingerprint([]byte("fake-token-2")) {
		t.Fatalf("fingerprint %q is not the new token's", second)
	}

	clock.advance(tokenRereadAfter)
	src.set(UnauthenticatedError(ForgeGitLab, nil))
	if fp, err := gl.CredentialFingerprint(t.Context()); err == nil || fp != "" {
		t.Fatalf("without a login = %q, %v, want an error", fp, err)
	}
}
