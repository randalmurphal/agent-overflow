package trackerapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync/atomic"
	"time"
)

// limitBodyWait bounds the read of a rate limit's body, which can only
// refine a limit its status and headers already settled.
const limitBodyWait = 2 * time.Second

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
// origin and, when it did, the credential, the client's generation, the
// generation of the read that stored the credential, and whether the
// request carried it (a zeroed credential sets no header).
type sentWith struct {
	onBase  bool
	cred    *Credential
	gen     uint64
	read    uint64
	carried bool
}

// exchange is one hop whose answer's headers arrived.
type exchange struct {
	resp *http.Response
	sent sentWith
	// st times the hop's transfer, from its admission; nil unless a
	// transfer.
	st *stall
	// cancel ends the hop's request and its body read.
	cancel context.CancelFunc
	// trace tells connected once the transport holds a connection for
	// the request.
	trace     httptrace.ClientTrace
	connected atomic.Bool
}

func (x *exchange) gotConn(httptrace.GotConnInfo) { x.connected.Store(true) }

// close ends the hop. A remainder left unread, over a bound or past an
// error's read, can be slow to come and would hold the call, so the body
// is closed without reading on; a body read to its end leaves the
// connection reusable.
func (x *exchange) close() {
	_ = x.resp.Body.Close()
	x.cancel()
	x.st.stop()
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
	target := k.target
	for hops := 0; ; hops++ {
		x, err := c.hop(ctx, k, state, target)
		if err != nil {
			if notSent, ok := errors.AsType[*NotSentError](err); ok && hops > 0 {
				// The first request reached the tracker.
				return nil, notSent.Err
			}
			return nil, err
		}
		next, err := c.redirectTarget(k, x.resp, hops)
		if err != nil || next == nil {
			var a *answer
			if err == nil {
				a, err = c.receive(ctx, k, state, x, recv)
			}
			x.close()
			return a, err
		}
		x.close()
		target = next
	}
}

// hop admits and sends one request to target. A transfer's stall and
// header timers start once it is admitted. Its error is a *NotSentError
// until the transport holds a connection for the request: nothing can
// have reached the tracker before, and a context already ended fails
// there too. After that the outcome of a write is unknown.
func (c *Client) hop(ctx context.Context, k *call, state *retries, target *url.URL) (*exchange, error) {
	if err := c.admit(ctx, k, state); err != nil {
		return nil, err
	}
	x := &exchange{}
	if k.transfer {
		x.st = newStall(ctx, k.sending())
	}
	hopCtx, cancel := context.WithCancel(x.st.context(ctx))
	x.cancel = cancel
	fail := func(err error) (*exchange, error) {
		cancel()
		x.st.stop()
		return nil, err
	}
	x.trace.GotConn = x.gotConn
	req, sent, err := k.request(httptrace.WithClientTrace(hopCtx, &x.trace), target, x.st, c.userAgent)
	if err != nil {
		return fail(&NotSentError{Err: fmt.Errorf("%s: %w", c.where(k.method, target), c.cause(sentWith{}, err))})
	}
	if originOf(target) == c.origin {
		c.credMu.RLock()
		x.sent = sentWith{onBase: true, cred: c.cred, gen: c.credGen, read: c.credRead, carried: c.cred.authorize(req.Header)}
		c.credMu.RUnlock()
	}
	resp, err := c.conns.transport.RoundTrip(req)
	if err != nil {
		failure := c.sendError(ctx, c.where(k.method, target), x.st, x.sent, err)
		switch source := sent.sourceFailure(); {
		case !x.connected.Load():
			failure = &NotSentError{Err: failure}
		case source != nil:
			// The body stopped short of the length it declared, so the
			// tracker never had the whole request to act on.
			failure = &SourceError{Err: fmt.Errorf("%s: %w", c.where(k.method, target), source)}
		}
		return fail(failure)
	}
	x.resp = resp
	x.st.receiving()
	c.report(resp, x.sent)
	return x, nil
}

// report hands an answer's status and the headers ReportHeaders names to
// OnAnswer, unless Close has begun; Close waits for a report in progress.
// What it hands out is answer text with no Scrub of its own, so it is
// made safe here: the names are the caller's, never the tracker's, and
// every value is scrubbed of the credential the request carried.
func (c *Client) report(resp *http.Response, sent sentWith) {
	if c.onAnswer == nil {
		return
	}
	c.reportMu.RLock()
	defer c.reportMu.RUnlock()
	if c.reportsOff {
		return
	}
	var s scrubber
	c.loadScrubber(&s, sent)
	defer s.clear()
	c.onAnswer(resp.StatusCode, s.headers(resp.Header, c.reported))
}

// headers is a new map of the headers of h that names holds (canonical
// names), every value scrubbed.
func (s *scrubber) headers(h http.Header, names []string) http.Header {
	out := make(http.Header, len(names))
	for _, name := range names {
		values := h[name]
		if len(values) == 0 {
			continue
		}
		scrubbed := make([]string, len(values))
		for i, v := range values {
			scrubbed[i] = s.text(v)
		}
		out[name] = scrubbed
	}
	return out
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
// limit its status and headers show closes the gate and settles the call
// before the body is read; the body can only name a later time.
func (c *Client) receive(ctx context.Context, k *call, state *retries, x *exchange, recv receiver) (*answer, error) {
	resp := x.resp
	a := &answer{status: resp.StatusCode, header: resp.Header, url: resp.Request.URL, sent: x.sent}
	until, limited := c.limitOf(a.status, a.header, nil)
	if limited {
		if until.IsZero() {
			until = c.now().Add(backoff(state.count))
		}
		c.gate.close(until)
	}
	a.refused = x.sent.onBase && (a.status == http.StatusUnauthorized || c.dialect.RejectedLogin(a.header))
	switch {
	case a.refused:
		return a, c.readErrorBody(ctx, k, x.st, resp, a)
	case limited:
		c.readLimitBody(k, x, a, until)
		return a, nil
	case a.status >= 400:
		return a, c.readErrorBody(ctx, k, x.st, resp, a)
	case a.status >= 300 && k.redirects == refuseRedirect:
		return a, nil
	}
	if err := recv(c, ctx, k, resp, x.st, a); err != nil {
		return nil, err
	}
	return a, nil
}

// readLimitBody settles a as the rate limit its headers showed, until
// until or the time the dialect reads from the body; the gate keeps the
// later of the two. Retry-After is not read again: it counts from the
// answer's arrival. The body is read for at most limitBodyWait, whatever
// the call's own time; a failure to read it, the call ending included,
// changes nothing.
func (c *Client) readLimitBody(k *call, x *exchange, a *answer, until time.Time) {
	timer := time.AfterFunc(limitBodyWait, x.cancel)
	body, _ := readUpTo(x.st.reader(x.resp.Body), declaredSize(x.resp), min(k.limit, errorReadLimit))
	timer.Stop()
	a.limited, a.until = true, until
	if named, limited := c.dialect.RateLimited(a.status, a.header, body); limited {
		a.until = named
	}
}
