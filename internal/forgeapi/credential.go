package forgeapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// authTransport is where a token meets a request. The request a Client
// builds carries no credential; its context names the Client, and each
// hop, redirects included, is cloned and given the Client's token only
// when the hop's host, port included, is one of that forge's own hosts
// (Client.authorizes). No request object a caller, an error or a redirect
// copy can see ever holds the token.
type authTransport struct{ base http.RoundTripper }

type clientKeyCtx struct{}

func (t authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c, _ := req.Context().Value(clientKeyCtx{}).(*Client)
	if c == nil || !c.authorizes(req.URL) {
		return t.base.RoundTrip(req)
	}
	hop := req.Clone(req.Context())
	c.credMu.RLock()
	c.token.authorize(hop)
	c.credMu.RUnlock()
	return t.base.RoundTrip(hop)
}

// authorizes reports whether u is one of the forge's own hosts, the only
// ones that receive the token: the API bases and the forge host itself
// (an attachment URL).
func (c *Client) authorizes(u *url.URL) bool {
	c.credMu.RLock()
	defer c.credMu.RUnlock()
	if c.svc.isolated != nil {
		return strings.EqualFold(u.Host, c.svc.isolated.Host)
	}
	return (c.rest != nil && strings.EqualFold(u.Host, c.rest.Host)) ||
		(c.graphQL != nil && strings.EqualFold(u.Host, c.graphQL.Host)) ||
		strings.EqualFold(u.Host, c.host)
}

// credential makes sure the client holds a current token: read at first
// use and again on the first request after five minutes. A failed read is
// answered from cache for five seconds, except to a person's own action
// (WithInteractive): that drops the host's cached failure first, so a
// Retry after `gh auth login` reads the new login at once.
func (c *Client) credential(ctx context.Context) error {
	if IsInteractive(ctx) {
		c.svc.DropNegative(c.host)
	}
	return c.read(ctx, "")
}

// refresh re-reads the token after a 401 answered a request that used the
// token with fingerprint used. When another request already replaced that
// token, the current one is used without reading again.
func (c *Client) refresh(ctx context.Context, used string) error {
	return c.read(ctx, used)
}

func (c *Client) read(ctx context.Context, rejected string) error {
	select {
	case c.readLock <- struct{}{}:
	case <-ctx.Done():
		return c.contextError(ctx, nil)
	}
	defer func() { <-c.readLock }()

	now := c.svc.now()
	c.credMu.RLock()
	current := c.token != nil && now.Sub(c.readAt) < tokenRereadAfter && (rejected == "" || c.fp != rejected)
	failed, failUntil := c.failErr, c.failUntil
	c.credMu.RUnlock()
	if current {
		return nil
	}
	if failed != nil && now.Before(failUntil) {
		return failed
	}
	secret, info, err := c.svc.source.Token(ctx, c.forge, c.host)
	defer clear(secret)
	if err != nil {
		if ctx.Err() != nil {
			return c.contextError(ctx, err)
		}
		c.credMu.Lock()
		c.failErr, c.failUntil = err, now.Add(tokenFailureTTL)
		c.token.Zero()
		c.token, c.fp = nil, ""
		c.credMu.Unlock()
		return err
	}
	fp := fingerprint(secret)
	c.credMu.Lock()
	defer c.credMu.Unlock()
	if c.rest == nil {
		rest, gql, err := c.svc.resolveBase(c.forge, c.host, info)
		if err != nil {
			c.failErr, c.failUntil = err, now.Add(tokenFailureTTL)
			return err
		}
		c.rest, c.graphQL = rest, gql
	}
	c.failErr = nil
	c.readAt = now
	if fp == c.fp && c.token != nil {
		return nil
	}
	c.token.Zero()
	c.token, c.fp = newToken(secret, info.Header), fp
	c.svc.etags.dropHost(c.host, "")
	c.svc.gates.dropHost(c.host, fp)
	return nil
}
