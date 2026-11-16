package trackerapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync/atomic"
)

// redirectMode is what a call does with a 3xx.
type redirectMode uint8

const (
	// refuseRedirect fails the call with a *StatusError: every method but
	// GET and HEAD. A write is never sent on to another URL, so its
	// outcome is never taken from another request's answer.
	refuseRedirect redirectMode = iota
	// followRedirect follows 301, 302, 303, 307 and 308 that name a
	// target: a GET or HEAD through Do, JSON or Stream.
	followRedirect
	// answerRedirect returns the 3xx: Location.
	answerRedirect
)

// sentWith is what a hop put on the wire: whether it went to the base
// origin, and then the credential and its generation, and the scheme and
// Authorization value it was sent as. The value is the request's own
// header string, held until the answer is settled, from which errors are
// scrubbed.
type sentWith struct {
	onBase bool
	cred   *Credential
	gen    uint64
	scheme authScheme
	auth   string
}

// attempt sends k once, following a GET's redirects hop by hop. The gate
// is checked before any credential work; every hop then passes the gate
// and the pacing, goes to an allowed URL, and carries the credential only
// on the base origin. It returns the last hop's answer; its error is a
// failure to send or to read that answer.
func (c *Client) attempt(ctx context.Context, k *call, state *retries, recv receiver) (*answer, error) {
	if err := c.waitGate(ctx, k, state); err != nil {
		return nil, err
	}
	if err := c.loadCredential(ctx, false, 0); err != nil {
		where := c.where(k.method, k.target)
		if ctx.Err() != nil {
			return nil, &NotSentError{Err: c.ended(ctx, where)}
		}
		return nil, &NotSentError{Err: fmt.Errorf("%s: %w", where, err)}
	}
	var st *stall
	if k.transfer {
		st = newStall(ctx, len(k.body) > 0)
		defer st.stop()
	}
	target := k.target
	for hops := 0; ; hops++ {
		resp, with, err := c.hop(ctx, k, state, st, target)
		if err != nil {
			if notSent, ok := errors.AsType[*NotSentError](err); ok && hops > 0 {
				// The first request reached the tracker.
				return nil, notSent.Err
			}
			return nil, err
		}
		next, err := c.redirectTarget(k, resp, hops)
		if err != nil || next == nil {
			var a *answer
			if err == nil {
				a, err = c.receive(ctx, k, state, st, resp, with, recv)
			}
			// Closed without reading on: a remainder left unread, over a
			// bound or past an error's read, can be slow to come and would
			// hold the call. A body read to its end leaves the connection
			// reusable.
			_ = resp.Body.Close()
			return a, err
		}
		_ = resp.Body.Close()
		st.sent()
		target = next
	}
}

// hop admits and sends one request to target. Its error is a
// *NotSentError until the transport holds a connection for the request:
// nothing can have reached the tracker before. After that the outcome of
// a write is unknown.
func (c *Client) hop(ctx context.Context, k *call, state *retries, st *stall, target *url.URL) (*http.Response, sentWith, error) {
	where := c.where(k.method, target)
	if err := c.admit(ctx, k, state); err != nil {
		return nil, sentWith{}, err
	}
	var connected atomic.Bool
	traced := httptrace.WithClientTrace(st.context(ctx), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	req, err := k.request(traced, target, st, c.userAgent)
	if err != nil {
		return nil, sentWith{}, &NotSentError{Err: fmt.Errorf("%s: %w", where, err)}
	}
	var with sentWith
	if originOf(target) == c.origin {
		c.credMu.RLock()
		c.cred.authorize(req.Header)
		with = sentWith{onBase: true, cred: c.cred, gen: c.credGen}
		if c.cred != nil {
			with.scheme = c.cred.scheme
		}
		c.credMu.RUnlock()
		with.auth = req.Header.Get("Authorization")
	}
	// A context already ended fails here too, before any connection.
	resp, err := c.transport.RoundTrip(req)
	if err != nil {
		failure := c.sendError(ctx, where, st, newScrubber(with.scheme, with.auth).error(err))
		if !connected.Load() {
			failure = &NotSentError{Err: failure}
		}
		return nil, sentWith{}, failure
	}
	st.receiving()
	return resp, with, nil
}

// redirectTarget is the URL a followed redirect names, or nil when resp is
// the answer. A redirect's target is parsed here, never by net/http, so
// no error quotes it: errors carry its redacted form at most.
func (c *Client) redirectTarget(k *call, resp *http.Response, hops int) (*url.URL, error) {
	location := resp.Header.Get("Location")
	if k.redirects != followRedirect || !followedStatus(resp.StatusCode) || location == "" {
		return nil, nil
	}
	where := c.where(k.method, resp.Request.URL)
	if hops+1 >= maxRedirects {
		return nil, fmt.Errorf("%s: %w: stopped after %d redirects", where, errRedirect, maxRedirects)
	}
	target, err := resp.Request.URL.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: the target is not a URL", where, errRedirect)
	}
	target.Fragment, target.RawFragment = "", ""
	if target.User != nil {
		return nil, fmt.Errorf("%s: %w: the target carries user info", where, errRedirect)
	}
	if err := allowedURL(target); err != nil {
		return nil, fmt.Errorf("%s: %w: %w", where, errRedirect, err)
	}
	return target, nil
}

func followedStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// receive turns the last hop's answer into an answer for settle. A rate
// limit closes the gate from the status and headers before the body is
// read; the body can only extend that closure.
func (c *Client) receive(ctx context.Context, k *call, state *retries, st *stall, resp *http.Response, with sentWith, recv receiver) (*answer, error) {
	a := &answer{status: resp.StatusCode, header: resp.Header, url: resp.Request.URL, sent: with}
	if until, limited := c.limitOf(a.status, a.header, nil); limited {
		if until.IsZero() {
			until = c.now().Add(backoff(state.count))
		}
		c.gate.close(until)
	}
	a.refused = with.onBase && (a.status == http.StatusUnauthorized || c.dialect.RejectedLogin(a.header))
	if a.refused || a.status >= 400 {
		return a, c.readErrorBody(ctx, k, st, resp, a)
	}
	if a.status >= 300 && k.redirects == refuseRedirect {
		return a, nil
	}
	if err := recv(ctx, resp, st, a); err != nil {
		return nil, err
	}
	return a, nil
}
