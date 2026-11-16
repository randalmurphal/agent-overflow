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
	return c.response(a), nil
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
	resp := c.response(a)
	if out == nil || a.status == http.StatusNoContent {
		return resp, nil
	}
	if err := json.Unmarshal(a.body, out); err != nil {
		return resp, fmt.Errorf("%s: decode the answer: %w", c.where(k.method, a.url), c.cause(a.sent, err))
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
	// cut is an error body that stops short of the answer's end: at its
	// bound, or cut short.
	cut bool
	url *url.URL // the URL that answered, after redirects
	// refused is a 401 or a dialect's refused login, on a hop that was
	// sent to the base origin.
	refused bool
	limited bool
	until   time.Time // when a limit lifts; zero when the answer named no time
	sent    sentWith
}

// response is the Response a caller gets for a. Body is nil for Stream,
// which reads none into a.
func (c *Client) response(a *answer) *Response {
	return &Response{Status: a.status, Header: a.header, Body: a.body, scrub: c.scrubRef(a.sent)}
}

// receiver reads a successful answer (below 400 and not a refused login)
// into a, and sets a.limited from the dialect. Its error fails the
// attempt.
type receiver func(ctx context.Context, resp *http.Response, st *stall, a *answer) error

// retries is a call's retry state.
type retries struct {
	count  int  // retries after a connection failure, a limit or a 503
	reread bool // the credential was read again after a refusal
	// gateWaited is the time an interactive call waited for the gate, at
	// most interactiveGateWait over all of its attempts.
	gateWaited time.Duration
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
		a, err := c.attempt(ctx, k, state, recv)
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

// readBody is the receiver of Do and JSON: the answer whole, refused
// unread when its length is over the bound.
func (c *Client) readBody(k *call) receiver {
	return func(ctx context.Context, resp *http.Response, st *stall, a *answer) error {
		if resp.ContentLength > k.limit {
			return c.tooLarge(k, a)
		}
		body, err := readUpTo(st.reader(resp.Body), k.limit)
		over := false
		if err == nil && int64(len(body)) == k.limit {
			over, err = hasMore(st.reader(resp.Body))
		}
		if err == nil {
			err = cutOff(ctx, st)
		}
		switch {
		case err != nil:
			return c.readError(ctx, k, st, a, err)
		case over:
			return c.tooLarge(k, a)
		}
		a.body = body
		a.until, a.limited = c.limitOf(a.status, a.header, a.body)
		return nil
	}
}

// readErrorBody reads an error answer's body, up to the call's bound or
// errorReadLimit, whichever is less, and keeps what fits: the status is
// the answer. A body cut short fails a read as transient and leaves a
// write's status standing with what arrived. The call ending during the
// read is reported as its end.
func (c *Client) readErrorBody(ctx context.Context, k *call, st *stall, resp *http.Response, a *answer) error {
	limit := min(k.limit, errorReadLimit)
	body, err := readUpTo(st.reader(resp.Body), limit)
	if err == nil {
		err = cutOff(ctx, st)
	}
	if err != nil && (k.read || ctx.Err() != nil) {
		return c.readError(ctx, k, st, a, err)
	}
	a.body, a.cut = body, err != nil || int64(len(body)) == limit
	a.until, a.limited = c.limitOf(a.status, a.header, a.body)
	return nil
}

// readUpTo reads r to its end or limit bytes, whichever comes first.
func readUpTo(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}

// hasMore reports whether r holds another byte.
func hasMore(r io.Reader) (bool, error) {
	var probe [1]byte
	n, err := io.ReadFull(r, probe[:])
	if n > 0 {
		return true, nil
	}
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	return false, err
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
	return &TransientError{Err: fmt.Errorf("%s: read the answer: %w", where, c.cause(a.sent, st.explain(err)))}
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

// sendError maps a failure of the transport's RoundTrip: the call's end,
// or a transient failure. A *url.Error, which quotes a whole URL, is
// dropped for its cause.
func (c *Client) sendError(ctx context.Context, where string, st *stall, sent sentWith, err error) error {
	if ctx.Err() != nil {
		return c.ended(ctx, where)
	}
	return &TransientError{Err: fmt.Errorf("%s: %w", where, c.cause(sent, st.explain(parseCause(err))))}
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
