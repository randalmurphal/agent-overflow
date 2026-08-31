package wsllauncher

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// The launcher forwards the backend's session credential rather than
// relying on the fact that its packets arrive over loopback
// (docs/specs/remote-access.md §4, "Local clients").
//
// The launcher runs on Windows and reaches the WSL backend through the
// localhost relay, so every one of its connections looks like a loopback
// peer at the socket — indistinguishable from a same-host relay carrying
// somebody else's traffic. Presenting the credential the backend minted
// for its own local page channel is what makes the launcher's notification
// socket an attributable, revocable connection instead of one trusted for
// its apparent topology.
//
// The credential is not on the bootstrap line. It cannot be: the launcher
// receives that line as soon as the transport binds, and the session core
// boots later during the backend's startup. So the launcher ASKS, over the
// channel it is already authenticated on — /bootstrap.json, with its
// launch token — and reads the credential out of the session cookie that
// exchange plants. One route, one exchange, no second delivery mechanism.

// sessionCookiePrefix is the name prefix the transport gives the page's
// session cookie (internal/transport/authroutes.go). Restated here rather
// than imported for the reason the page-URL path is
// (internal/wsllauncher/pageurl_test.go): this package is compiled into
// the Windows launcher binary, which does not link the transport, and a
// drift-guard test pins the two spellings together.
const sessionCookiePrefix = "ao_session_"

// transportSessionHeader is the header the transport reads a forwarded
// session credential from (internal/transport.SessionCredentialHeader).
// Restated for the same reason and pinned by the same drift-guard test.
const transportSessionHeader = "X-Ao-Session"

// sessionCredentialSource fetches and caches the backend's local page
// channel credential.
//
// Best-effort by construction. Every failure — the backend is still
// booting, the session core did not start, an older backend that has no
// such cookie — leaves the credential empty, and an empty credential means
// the connection carries the launch token alone, exactly as it did before
// this existed. Forwarding is an improvement in attribution, never a new
// requirement for the launcher to connect.
type sessionCredentialSource struct {
	// bootstrapURL is the backend's /bootstrap.json, derived once from the
	// WebSocket URL so the two can never name different backends.
	bootstrapURL string
	// token is the launch credential, the thing that authenticates the
	// fetch.
	token string
	// client is the HTTP client used for the fetch. Injectable for tests;
	// nil means http.DefaultClient.
	client *http.Client

	mu         sync.Mutex
	credential string
}

// newSessionCredentialSource derives the bootstrap URL from a WebSocket
// URL. An unparseable URL yields a source that never fetches, because the
// only honest alternative is guessing at an authority.
func newSessionCredentialSource(wsURL, token string) *sessionCredentialSource {
	source := &sessionCredentialSource{token: token}
	parsed, err := url.Parse(wsURL)
	if err != nil || parsed.Host == "" {
		return source
	}
	scheme := "http"
	if parsed.Scheme == "wss" {
		scheme = "https"
	}
	source.bootstrapURL = (&url.URL{Scheme: scheme, Host: parsed.Host, Path: "/bootstrap.json"}).String()
	return source
}

// credentialFor returns the credential to forward, fetching it once and
// reusing it afterwards.
//
// Cached rather than re-fetched per connection: the backend re-issues the
// local credential well before its window closes and hands the same string
// out until then, so a fetch per reconnect would be a round trip for an
// answer that has not changed. `refresh` forces one, which is what a
// connection refused with a credential in hand should do.
func (s *sessionCredentialSource) credentialFor(ctx context.Context, refresh bool) string {
	s.mu.Lock()
	held := s.credential
	s.mu.Unlock()
	if held != "" && !refresh {
		return held
	}
	fetched, err := s.fetch(ctx)
	if err != nil || fetched == "" {
		// Keep whatever we had. A backend mid-restart answers 503, and
		// discarding a working credential over that would drop
		// attribution for every reconnect until it came back.
		return held
	}
	s.mu.Lock()
	s.credential = fetched
	s.mu.Unlock()
	return fetched
}

// fetch performs the authenticated bootstrap exchange and reads the
// session cookie out of it.
func (s *sessionCredentialSource) fetch(ctx context.Context) (string, error) {
	if s.bootstrapURL == "" || s.token == "" {
		return "", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.bootstrapURL, nil)
	if err != nil {
		return "", err
	}
	// The bearer carrier, not `?token=`: the query slot on this route
	// belongs to the one-time page ticket, and a launcher that put its
	// token there would be presenting a ticket nobody minted.
	req.Header.Set("Authorization", "Bearer "+s.token)
	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// 503 while the backend finishes booting is the common case and is
		// not worth an error string in the launcher's log every retry.
		return "", fmt.Errorf("bootstrap: status %d", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if strings.HasPrefix(cookie.Name, sessionCookiePrefix) && cookie.Value != "" {
			return cookie.Value, nil
		}
	}
	// An older backend, or one whose session core did not start. Not an
	// error: the launch token still authorizes everything the launcher
	// does.
	return "", nil
}
