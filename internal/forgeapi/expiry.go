package forgeapi

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A forge that answers an expired token with a plain 401 says nothing of
// why, so the client learns a token's expiry while the token still works
// and names it on the SetupError the 401 later becomes. GitLab reports a
// personal access token's expiry at personal_access_tokens/self, asked
// once per token on the first request after it is read; GitHub sends a
// fine-grained token's expiry on every response
// (GitHub-Authentication-Token-Expiration). See
// docs/architecture/forge-transport.md#credentials.

// gitlabTokenSelfPath is GitLab's own-token endpoint. It answers 400 for a
// token that is not a personal access token (an OAuth2 login).
const gitlabTokenSelfPath = "personal_access_tokens/self"

// expiryLookupTimeout bounds the lookup inside the request that runs it.
const expiryLookupTimeout = 10 * time.Second

// githubTokenExpirationHeader carries a fine-grained or expiring GitHub
// token's expiry, as "2006-01-02 15:04:05 UTC".
const githubTokenExpirationHeader = "GitHub-Authentication-Token-Expiration"

// tokenExpiry is what the client learned about the token with
// fingerprint fp. A fingerprint set with known false is a lookup in flight
// or one that failed: either way not asked again for that token.
type tokenExpiry struct {
	fp        string
	known     bool
	expiresAt time.Time // zero when the token does not expire
	revoked   bool
}

// expiryState is a Client's tokenExpiry with its lock and the once-only
// log of a failed lookup.
type expiryState struct {
	mu     sync.Mutex
	token  tokenExpiry
	logged bool
}

// lookupExpiry asks GitLab for the current token's expiry, once per token,
// when the source marked the token as one GitLab reports
// (SourceInfo.ExpiryLookup). It runs inside a request's prepare, under
// that request's context, and never fails it: a failed lookup is logged
// once per host and the token is not asked about again.
func (c *Client) lookupExpiry(ctx context.Context) {
	c.credMu.RLock()
	fp, want := c.fp, c.expiryLookup
	c.credMu.RUnlock()
	if !want || fp == "" {
		return
	}
	c.expiry.mu.Lock()
	if c.expiry.token.fp == fp {
		c.expiry.mu.Unlock()
		return
	}
	c.expiry.token = tokenExpiry{fp: fp}
	c.expiry.mu.Unlock()

	var self struct {
		ExpiresAt *string `json:"expires_at"`
		Revoked   bool    `json:"revoked"`
	}
	learned, err := func() (tokenExpiry, error) {
		if _, err := c.JSON(ctx, Request{Path: gitlabTokenSelfPath, Timeout: expiryLookupTimeout, expiryLookup: true}, &self); err != nil {
			return tokenExpiry{}, err
		}
		out := tokenExpiry{fp: fp, known: true, revoked: self.Revoked}
		if self.ExpiresAt != nil && *self.ExpiresAt != "" {
			// GitLab expires a token at the start of its expires_at day,
			// UTC.
			at, err := time.Parse(time.DateOnly, *self.ExpiresAt)
			if err != nil {
				return tokenExpiry{}, fmt.Errorf("expires_at %q: %w", *self.ExpiresAt, err)
			}
			out.expiresAt = at.UTC()
		}
		return out, nil
	}()
	c.expiry.mu.Lock()
	defer c.expiry.mu.Unlock()
	if err != nil {
		if !c.expiry.logged {
			c.expiry.logged = true
			log.Printf("forgeapi: %s token expiry lookup failed; an expired token will read as not authenticated: %v", c.host, err)
		}
		return
	}
	if c.expiry.token.fp == fp {
		c.expiry.token = learned
	}
}

// observeExpiry records the expiry a GitHub response names for the token
// with fingerprint fp.
func (c *Client) observeExpiry(fp string, header http.Header) {
	if c.forge != ForgeGitHub || fp == "" {
		return
	}
	value := strings.TrimSpace(header.Get(githubTokenExpirationHeader))
	if value == "" {
		return
	}
	at, ok := parseGitHubExpiry(value)
	if !ok {
		return
	}
	c.expiry.mu.Lock()
	defer c.expiry.mu.Unlock()
	c.expiry.token = tokenExpiry{fp: fp, known: true, expiresAt: at}
}

func parseGitHubExpiry(value string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", time.RFC3339} {
		if at, err := time.Parse(layout, value); err == nil {
			return at.UTC(), true
		}
	}
	return time.Time{}, false
}

// unauthenticated is the SetupError of a 401 the refresh did not cure, for
// the token with fingerprint fp: the host named, and the expiry or the
// revocation when the client learned it for that token.
func (c *Client) unauthenticated(cause error, fp string) *SetupError {
	setup := UnauthenticatedError(c.forge, cause)
	setup.Host = c.host
	c.expiry.mu.Lock()
	known := c.expiry.token
	c.expiry.mu.Unlock()
	if known.fp != fp || !known.known {
		return setup
	}
	name, login := "GitHub", "gh"
	if c.forge == ForgeGitLab {
		name, login = "GitLab", "glab"
	}
	switch {
	case known.revoked:
		setup.Message = fmt.Sprintf("Your %s token for %s was revoked. Run `%s auth login --hostname %s` to continue.", name, c.host, login, c.host)
	case !known.expiresAt.IsZero() && !c.svc.now().Before(known.expiresAt):
		setup.ExpiredAt = known.expiresAt
		setup.Message = fmt.Sprintf("Your %s token for %s expired on %s. Run `%s auth login --hostname %s` to continue.", name, c.host, known.expiresAt.Format(time.DateOnly), login, c.host)
	}
	return setup
}

// withHost is a token source's SetupError with the host it was reading
// for, as a copy: the source's value may be shared.
func withHost(err error, host string) error {
	setup, ok := err.(*SetupError)
	if !ok || setup.Host != "" {
		return err
	}
	out := *setup
	out.Host = host
	return &out
}
