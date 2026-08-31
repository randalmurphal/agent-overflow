// Package relaysession holds the session credential a same-host relay
// forwards on behalf of the backend's local page channel.
//
// Two processes sit between a person and a backend on the same machine:
// the Windows launcher, which reaches a WSL backend over the localhost
// relay, and the `agent-overflow --connect` stub, which carries a page's
// WebSocket to an upstream backend. Both look like a loopback peer at the
// socket — indistinguishable from any same-host relay carrying somebody
// else's traffic — so neither may be trusted for its apparent topology
// (docs/specs/remote-access.md §4, "Local clients"). Presenting the
// credential the backend minted for its own local page channel is what
// makes each connection attributable and revocable instead.
//
// The credential is not on any bootstrap line. It cannot be: a relay
// learns its endpoint as soon as the transport binds, and the session
// core boots later during the backend's startup. So a relay ASKS, over
// the channel it is already authenticated on — /bootstrap.json, with its
// launch token — and reads the credential out of the session cookie that
// exchange plants. One route, one exchange, no second delivery mechanism.
//
// Best-effort by construction. Every failure — the backend is still
// booting, the session core did not start, an older backend that has no
// such cookie — leaves the credential empty, and an empty credential
// means the connection carries the launch token alone, exactly as it did
// before this existed. What that then buys depends on where the relay
// sits: the backend admits a sessionless upgrade from a peer on its own
// machine and refuses one from anywhere else, so forwarding is attribution
// for a same-host relay (both of today's callers) and the only way in for
// a relay reaching a backend across a network.
//
// No transport import, deliberately: this package is compiled into the
// Windows launcher binary, which does not link the transport server. The
// two spellings it restates are pinned to the transport's own by the
// drift-guard test in this package.
package relaysession

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// CookiePrefix is the name prefix the transport gives the page's session
// cookie (internal/transport/authroutes.go). Only the prefix is knowable:
// the full name is port-qualified.
const CookiePrefix = "ao_session_"

// Header is the header the transport reads a forwarded session credential
// from, in canonical MIME form (internal/transport.SessionCredentialHeader).
const Header = "X-Ao-Session"

// BootstrapURL maps a WebSocket endpoint onto the backend's manifest
// endpoint: ws→http / wss→https, and a trailing `/ws` segment — the
// transport's upgrade route — replaced by `/bootstrap.json` so a reverse
// proxy's path prefix survives. Query and fragment are dropped: whatever
// authenticates the upgrade is not what authenticates this fetch.
//
// One derivation for both relays, so the endpoint a relay dials and the
// endpoint it asks for a credential can never name different backends.
func BootstrapURL(wsURL string) (string, error) {
	parsed, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("relaysession: parse ws url: %w", err)
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	default:
		return "", fmt.Errorf("relaysession: unsupported scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("relaysession: ws url names no host")
	}
	prefix := strings.TrimSuffix(strings.TrimSuffix(parsed.Path, "/"), "/ws")
	parsed.Path = prefix + "/bootstrap.json"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// Source fetches and caches one backend's local page channel credential.
// Safe for concurrent use; a relay reads it from every connection attempt.
type Source struct {
	// bootstrapURL is the backend's /bootstrap.json. Empty means this
	// source can never fetch, which is the honest state for a relay whose
	// endpoint would not parse: the only alternative is guessing at an
	// authority.
	bootstrapURL string
	// token is the launch credential, the thing that authenticates the
	// fetch.
	token string
	// client performs the fetch. The CALLER's context is what bounds it,
	// so a relay that must not stall passes a deadline rather than
	// configuring one here.
	client *http.Client

	mu sync.Mutex
	// credential is the last value fetched. Kept across a failed refetch:
	// a backend mid-restart answers 503, and discarding a working
	// credential over that would drop attribution for every reconnect
	// until it came back.
	credential string
	// stale marks the held value as suspect, so the next read refetches.
	// Set when a connection this credential was attached to was refused.
	stale bool
}

// New builds a source for one backend. An empty bootstrapURL or token
// yields a source that never fetches and always answers "".
//
// A nil client uses http.DefaultClient: the fetch is bounded by the
// context the caller passes, which is the bound that matters — a relay's
// own deadline, not one this package invented.
func New(bootstrapURL, token string, client *http.Client) *Source {
	return &Source{bootstrapURL: bootstrapURL, token: token, client: client}
}

// Stale marks the held credential as suspect without discarding it. The
// next Credential call refetches, and keeps what it holds if that fails.
//
// This is what a REFUSED connection means and the only thing it can mean:
// the backend restarted, or the session was revoked. Marking rather than
// refetching on the spot keeps the refusal path free of a round trip, and
// puts the fetch at the next attempt — after the caller's backoff, when
// the backend is likelier to answer.
func (s *Source) Stale() {
	s.mu.Lock()
	s.stale = true
	s.mu.Unlock()
}

// Credential returns the credential to forward, fetching it at most once
// per staleness period and reusing it afterwards.
//
// Cached rather than re-fetched per connection: the backend re-issues the
// local credential well before its window closes and hands the same string
// out until then, so a fetch per reconnect would be a round trip for an
// answer that has not changed.
//
// Never returns an error. There is nothing a caller could do with one that
// differs from what it does with an empty string, and a relay that treated
// this as a failure would refuse to connect over an improvement.
func (s *Source) Credential(ctx context.Context) string {
	s.mu.Lock()
	held, stale := s.credential, s.stale
	s.mu.Unlock()
	if held != "" && !stale {
		return held
	}
	fetched, err := s.fetch(ctx)
	if err != nil || fetched == "" {
		return held
	}
	s.mu.Lock()
	s.credential, s.stale = fetched, false
	s.mu.Unlock()
	return fetched
}

// fetch performs the authenticated bootstrap exchange and reads the
// session cookie out of it.
func (s *Source) fetch(ctx context.Context) (string, error) {
	if s.bootstrapURL == "" || s.token == "" {
		return "", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.bootstrapURL, nil)
	if err != nil {
		return "", err
	}
	// The bearer carrier, not `?token=`: the query slot on this route
	// belongs to the one-time page ticket, and a relay that put its token
	// there would be presenting a ticket nobody minted.
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
		// not worth an error string in a relay's log every retry.
		return "", fmt.Errorf("relaysession: bootstrap status %d", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if strings.HasPrefix(cookie.Name, CookiePrefix) && cookie.Value != "" {
			return cookie.Value, nil
		}
	}
	// An older backend, or one whose session core did not start. Not an
	// error: the launch token still authorizes everything the relay does.
	return "", nil
}
