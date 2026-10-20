package forgeapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// expiryForge is a forge whose own-token endpoint answers self and whose
// other paths answer 200, or 401 once denied is set. It counts the
// requests to each path.
type expiryForge struct {
	self   atomic.Value // string body; "" answers 500
	denied atomic.Bool
	header http.Header
	mu     sync.Mutex
	paths  []string
}

func (f *expiryForge) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		if f.denied.Load() {
			http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v4/"+gitlabTokenSelfPath {
			body, _ := f.self.Load().(string)
			switch {
			case body == "":
				http.Error(w, `{"message":"500 Internal Server Error"}`, http.StatusInternalServerError)
			case strings.HasPrefix(body, "status:"):
				var status int
				_, _ = fmt.Sscanf(body, "status:%d", &status)
				http.Error(w, `{"message":"refused"}`, status)
			default:
				_, _ = fmt.Fprint(w, body)
			}
			return
		}
		for name, values := range f.header {
			w.Header()[name] = values
		}
		_, _ = fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *expiryForge) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.paths {
		if p == path {
			n++
		}
	}
	return n
}

var gitlabPAT = SourceInfo{Header: AuthPrivateToken, ExpiryLookup: true}

// A GitLab personal access token's expiry is asked once, on the first
// request after the token is read, and not again for the same token; a
// token the source does not mark (an OAuth2 login, an isolated boot) is
// never asked about.
func TestGitLabTokenExpiryLookupRunsOnce(t *testing.T) {
	t.Parallel()
	forge := &expiryForge{}
	forge.self.Store(`{"expires_at":"2030-01-01","revoked":false,"active":true}`)
	srv := forge.server(t)
	s, clock := liveService(t, &testSource{tokens: []string{testSecret}, info: gitlabPAT}, srv)
	gl := s.GitLab("gitlab.example")
	for range 3 {
		if _, err := gl.Do(t.Context(), Request{Path: "projects/1"}); err != nil {
			t.Fatal(err)
		}
	}
	// A token re-read after five minutes is the same token.
	clock.advance(6 * time.Minute)
	if _, err := gl.Do(t.Context(), Request{Path: "projects/1"}); err != nil {
		t.Fatal(err)
	}
	if n := forge.count("/api/v4/" + gitlabTokenSelfPath); n != 1 {
		t.Fatalf("self lookups = %d, want 1", n)
	}
	if forge.paths[0] != "/api/v4/"+gitlabTokenSelfPath {
		t.Fatalf("first request = %s, want the lookup before the read", forge.paths[0])
	}

	for name, svc := range map[string]*Service{
		"oauth2": func() *Service {
			s, _ := liveService(t, &testSource{tokens: []string{testSecret}, info: SourceInfo{Header: AuthBearer}}, srv)
			return s
		}(),
		"isolated": isolatedService(t, srv),
	} {
		before := forge.count("/api/v4/" + gitlabTokenSelfPath)
		_, _ = svc.GitLab("gitlab.example").Do(t.Context(), Request{Path: "projects/1"})
		if n := forge.count("/api/v4/"+gitlabTokenSelfPath) - before; n != 0 {
			t.Fatalf("%s: self lookups = %d, want none", name, n)
		}
	}
}

// A token that later answers 401 past the refresh is the unauthenticated
// SetupError naming the host and, when the remembered expiry has passed,
// when it expired; a revoked token says so.
func TestExpiredTokenNamesItsExpiry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, self string
		advance    time.Duration
		expiredAt  time.Time
		message    string
	}{
		{"expired", `{"expires_at":"2023-11-20","revoked":false,"active":true}`, 10 * 24 * time.Hour, time.Date(2023, 11, 20, 0, 0, 0, 0, time.UTC), "expired on 2023-11-20"},
		{"not yet expired", `{"expires_at":"2030-01-01","revoked":false,"active":true}`, time.Hour, time.Time{}, "not authenticated"},
		{"no expiry", `{"expires_at":null,"revoked":false,"active":true}`, time.Hour, time.Time{}, "not authenticated"},
		{"revoked", `{"expires_at":"2030-01-01","revoked":true,"active":false}`, time.Hour, time.Time{}, "was revoked"},
	} {
		forge := &expiryForge{}
		forge.self.Store(tc.self)
		s, clock := liveService(t, &testSource{tokens: []string{testSecret}, info: gitlabPAT}, forge.server(t))
		gl := s.GitLab("gitlab.example")
		if _, err := gl.Do(t.Context(), Request{Path: "projects/1"}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		clock.advance(tc.advance)
		forge.denied.Store(true)
		_, err := gl.Do(t.Context(), Request{Path: "projects/1"})
		setup, ok := errors.AsType[*SetupError](err)
		if !ok || setup.Kind != SetupUnauthenticated || setup.Host != "gitlab.example" || !setup.ExpiredAt.Equal(tc.expiredAt) || !strings.Contains(setup.Message, tc.message) {
			t.Fatalf("%s: 401 = %#v, want Host, ExpiredAt %v and %q", tc.name, err, tc.expiredAt, tc.message)
		}
	}
}

// A failed lookup never fails the request it runs in, is not retried for
// the token, and a lookup answered 401 does not refresh the token.
func TestTokenExpiryLookupFailureIsIgnored(t *testing.T) {
	t.Parallel()
	for _, self := range []string{"", "status:400", "status:401", `{"expires_at":"not a date"}`} {
		forge := &expiryForge{}
		forge.self.Store(self)
		src := &testSource{tokens: []string{testSecret}, info: gitlabPAT}
		s, _ := liveService(t, src, forge.server(t))
		gl := s.GitLab("gitlab.example")
		for range 2 {
			if _, err := gl.Do(t.Context(), Request{Path: "projects/1"}); err != nil {
				t.Fatalf("%q: read = %v", self, err)
			}
		}
		if n := forge.count("/api/v4/" + gitlabTokenSelfPath); n != 1 {
			t.Fatalf("%q: self lookups = %d, want 1", self, n)
		}
		if n := src.readCount(); n != 1 {
			t.Fatalf("%q: token reads = %d, want the lookup's failure to refresh nothing", self, n)
		}
		forge.denied.Store(true)
		_, err := gl.Do(t.Context(), Request{Path: "projects/1"})
		if setup, ok := errors.AsType[*SetupError](err); !ok || setup.Host != "gitlab.example" || !setup.ExpiredAt.IsZero() {
			t.Fatalf("%q: 401 = %#v", self, err)
		}
	}
}

// GitHub's expiry comes from the response header, with no extra request,
// and names an expired token's 401.
func TestGitHubTokenExpirationHeader(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"2023-11-20 00:00:00 UTC", "2023-11-19 17:00:00 -0700"} {
		forge := &expiryForge{header: http.Header{githubTokenExpirationHeader: {value}}}
		s, clock := liveService(t, &testSource{tokens: []string{testSecret}}, forge.server(t))
		gh := s.GitHub("github.com")
		if _, err := gh.Do(t.Context(), Request{Path: "user"}); err != nil {
			t.Fatal(err)
		}
		if n := len(forge.paths); n != 1 {
			t.Fatalf("requests = %q, want the read alone", forge.paths)
		}
		clock.advance(10 * 24 * time.Hour)
		forge.denied.Store(true)
		_, err := gh.Do(t.Context(), Request{Path: "user"})
		setup, ok := errors.AsType[*SetupError](err)
		if !ok || setup.Binary != "gh" || setup.Host != "github.com" || !setup.ExpiredAt.Equal(time.Date(2023, 11, 20, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("%q: 401 = %#v", value, err)
		}
	}
}

// A token source's SetupError carries the host it read for, as a copy.
func TestSourceSetupErrorNamesTheHost(t *testing.T) {
	t.Parallel()
	original := UnauthenticatedError(ForgeGitHub, errors.New("no token"))
	src := &testSource{tokens: []string{testSecret}, err: original}
	s, _ := liveService(t, src, (&expiryForge{}).server(t))
	_, err := s.GitHub("ghe.example:8443").Do(t.Context(), Request{Path: "user"})
	setup, ok := errors.AsType[*SetupError](err)
	if !ok || setup.Host != "ghe.example:8443" || setup == original || original.Host != "" {
		t.Fatalf("source failure = %#v (original Host %q)", err, original.Host)
	}
}
