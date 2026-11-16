package trackerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// errorReadLimit bounds the body read from an error answer, which the
// dialect reads for its message.
const errorReadLimit = 64 << 10

// Do sends r and returns the answer read whole. A non-nil error comes
// with a nil Response; the error types follow the table in
// docs/architecture/tracker-clients.md#errors.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	k, err := c.prepareCall(r)
	if err != nil {
		return nil, err
	}
	k.accept = "application/json"
	a, err := c.execute(ctx, k, c.readBody(k))
	if err != nil {
		return nil, err
	}
	return &Response{Status: a.status, Header: a.header, Body: a.body}, nil
}

// JSON is Do decoding the answer into out, unless out is nil or the
// answer is a 204. A body that does not decode fails the call with the
// Response it came in; for a write the outcome is then unknown.
func (c *Client) JSON(ctx context.Context, r Request, out any) (*Response, error) {
	k, err := c.prepareCall(r)
	if err != nil {
		return nil, err
	}
	k.accept = "application/json"
	a, err := c.execute(ctx, k, c.readBody(k))
	if err != nil {
		return nil, err
	}
	resp := &Response{Status: a.status, Header: a.header, Body: a.body}
	if out == nil || a.status == http.StatusNoContent {
		return resp, nil
	}
	if err := json.Unmarshal(a.body, out); err != nil {
		return resp, fmt.Errorf("%s: decode the answer: %w", c.where(k.method, a.url), err)
	}
	return resp, nil
}

// prepareCall is prepare with its error as a *NotSentError.
func (c *Client) prepareCall(r Request) (*call, error) {
	k, err := c.prepare(r)
	if err != nil {
		return nil, &NotSentError{Err: fmt.Errorf("trackerapi: %s: %w", c.name, err)}
	}
	return k, nil
}

// answer is what one attempt got back.
type answer struct {
	status int
	header http.Header
	body   []byte
	url    *url.URL // the URL that answered, after redirects
	// refused is a 401 or a dialect's refused login, on a hop that was
	// sent to the base origin.
	refused bool
	limited bool
	until   time.Time // when a limit lifts; zero when the answer named no time
	sentGen uint64
	sentTo  *Credential
}

// receiver reads a successful answer (below 400 and not a refused login)
// into a, and sets a.limited from the dialect. Its error fails the
// attempt.
type receiver func(ctx context.Context, resp *http.Response, st *stall, a *answer) error

// retries is a call's retry state.
type retries struct {
	count  int  // retries after a connection failure, a limit or a 503
	reread bool // the credential was read again after a refusal
}

// execute runs k until an answer settles it: retries reads after a
// connection failure, a 429 and a 503, any call after a 429 when it is
// interactive, and any call once after a refused credential is replaced.
func (c *Client) execute(ctx context.Context, k *call, recv receiver) (*answer, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(c.life, func() { cancel(errClosed) })
	defer func() {
		stop()
		cancel(nil)
	}()
	if !k.transfer {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, k.timeout)
		defer cancelTimeout()
	}
	state := &retries{}
	for {
		a, err := c.attempt(ctx, k, recv)
		var again bool
		if err != nil {
			again, err = c.afterFailure(ctx, k, state, err)
		} else {
			again, err = c.settle(ctx, k, state, a)
		}
		if !again {
			if err != nil {
				return nil, err
			}
			return a, nil
		}
	}
}

// attempt sends k once. Its error is a failure to send or to read the
// answer; an answer of any status is returned for settle.
func (c *Client) attempt(ctx context.Context, k *call, recv receiver) (*answer, error) {
	where := c.where(k.method, k.target)
	if err := c.loadCredential(ctx, false, 0); err != nil {
		if ctx.Err() != nil {
			return nil, &NotSentError{Err: c.ended(ctx, where)}
		}
		return nil, &NotSentError{Err: fmt.Errorf("%s: %w", where, err)}
	}
	if err := c.admit(ctx, k); err != nil {
		return nil, err
	}
	var st *stall
	if k.transfer {
		st = newStall(ctx, len(k.body) > 0)
		defer st.stop()
	}
	slot := &sent{}
	req, err := k.request(context.WithValue(st.context(ctx), sentKey{}, slot), st, c.userAgent)
	if err != nil {
		return nil, &NotSentError{Err: fmt.Errorf("%s: %w", where, err)}
	}
	client := c.follow
	if !k.follow {
		client = c.noFollow
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, c.sendError(ctx, where, st, err)
	}
	// Closed without reading on: a remainder left unread, over a bound or
	// past an error's first 64 KiB, can be slow to come and would hold
	// the call. A body read to its end leaves the connection reusable.
	defer func() { _ = resp.Body.Close() }()
	st.receiving()
	a := &answer{status: resp.StatusCode, header: resp.Header, url: resp.Request.URL}
	onBase, cred, gen := slot.get()
	a.sentTo, a.sentGen = cred, gen
	a.refused = onBase && (resp.StatusCode == http.StatusUnauthorized || c.dialect.RejectedLogin(resp.Header))
	if a.refused || resp.StatusCode >= 400 {
		// The status settles an error answer; a body cut short keeps what
		// arrived, for the dialect's message.
		a.body, _ = io.ReadAll(io.LimitReader(st.reader(resp.Body), errorReadLimit))
		a.until, a.limited = c.limitOf(a.status, a.header, a.body)
		return a, nil
	}
	if err := recv(ctx, resp, st, a); err != nil {
		return nil, err
	}
	return a, nil
}

// readBody is the receiver of Do and JSON: the answer whole, refused
// unread when its length is over the bound.
func (c *Client) readBody(k *call) receiver {
	return func(ctx context.Context, resp *http.Response, st *stall, a *answer) error {
		if resp.ContentLength > k.limit {
			return c.tooLarge(k, a)
		}
		body, err := io.ReadAll(io.LimitReader(st.reader(resp.Body), k.limit+1))
		if err == nil {
			err = cutOff(ctx, st)
		}
		if err != nil {
			return c.readError(ctx, k, st, a, err)
		}
		if int64(len(body)) > k.limit {
			return c.tooLarge(k, a)
		}
		a.body = body
		a.until, a.limited = c.limitOf(a.status, a.header, a.body)
		return nil
	}
}

func (c *Client) tooLarge(k *call, a *answer) error {
	return &BodyTooLargeError{Tracker: c.name, Method: k.method, URL: redact(a.url), Limit: k.limit}
}

// cutOff reports an attempt whose context ended, which a transport can
// hand the body's reader as a clean end of the body.
func cutOff(ctx context.Context, st *stall) error {
	return st.context(ctx).Err()
}

// readError is a failure to read an answer body: the call's end, or a
// transient failure.
func (c *Client) readError(ctx context.Context, k *call, st *stall, a *answer, err error) error {
	where := c.where(k.method, a.url)
	if ctx.Err() != nil {
		return c.ended(ctx, where)
	}
	return &TransientError{Err: fmt.Errorf("%s: read the answer: %w", where, st.explain(err))}
}

// limitOf reports whether an answer is a rate limit, and until when when
// the answer says: the dialect's time, else Retry-After.
func (c *Client) limitOf(status int, h http.Header, body []byte) (time.Time, bool) {
	until, limited := c.dialect.RateLimited(status, h, body)
	if status == http.StatusTooManyRequests {
		limited = true
	}
	if limited && until.IsZero() {
		now := c.now()
		if wait, ok := retryAfter(h, now); ok {
			until = now.Add(wait)
		}
	}
	return until, limited
}

// sendError maps a failure of http.Client.Do. The *url.Error wrapper is
// dropped: its message carries the whole URL, query included.
func (c *Client) sendError(ctx context.Context, where string, st *stall, err error) error {
	cause := st.explain(parseCause(err))
	var out error
	switch {
	case ctx.Err() != nil:
		out = c.ended(ctx, where)
	case errors.Is(cause, errRedirect):
		out = fmt.Errorf("%s: %w", where, cause)
	default:
		out = &TransientError{Err: fmt.Errorf("%s: %w", where, cause)}
	}
	if neverConnected(cause) {
		out = &NotSentError{Err: out}
	}
	return out
}

// ended is the error of a call whose context ended: closed by Close,
// out of time (transient), or cancelled by its caller.
func (c *Client) ended(ctx context.Context, where string) error {
	switch {
	case errors.Is(context.Cause(ctx), errClosed):
		return fmt.Errorf("%s: %w", where, errClosed)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &TransientError{Err: fmt.Errorf("%s: %w", where, context.DeadlineExceeded)}
	}
	return fmt.Errorf("%s: %w", where, ctx.Err())
}

func (c *Client) where(method string, u *url.URL) string {
	return c.name + " " + method + " " + redact(u)
}
