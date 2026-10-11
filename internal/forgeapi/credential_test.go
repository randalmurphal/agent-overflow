package forgeapi

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"sync/atomic"
	"testing"
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
