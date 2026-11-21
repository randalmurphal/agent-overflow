package trackerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// errorReadLimit bounds the body read from an error answer, which the
// dialect reads for its message.
const errorReadLimit = 64 << 10

// Do sends r and returns the answer read whole. A non-nil error comes
// with a nil Response, except ErrCredentialChanged, which comes with the
// answer's status, header and CredentialMark and no body; the error
// types follow the table in docs/architecture/tracker-clients.md#errors.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	k, err := c.prepareCall(r)
	if err != nil {
		return nil, err
	}
	k.acceptJSON = true
	a, err := c.execute(ctx, k, (*Client).readBody)
	if err != nil {
		return nil, err
	}
	return c.response(k, a)
}

// JSON is Do decoding the answer into out, unless out is nil or the
// answer is a 204. A body that does not decode fails the call with the
// Response it came in; for a write the outcome is then unknown.
func (c *Client) JSON(ctx context.Context, r Request, out any) (*Response, error) {
	k, err := c.prepareCall(r)
	if err != nil {
		return nil, err
	}
	k.acceptJSON = true
	a, err := c.execute(ctx, k, (*Client).readBody)
	if err != nil {
		return nil, err
	}
	resp, err := c.response(k, a)
	if err != nil {
		return resp, err
	}
	if out == nil || a.status == http.StatusNoContent {
		return resp, nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
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

// response is the Response a caller gets for a, its body scrubbed of the
// credential the request carried. Body is nil for Stream, which reads
// none into a. An answer whose credential was dropped before its body
// could be checked fails with ErrCredentialChanged and the Response
// without its body; for a write the outcome is then unknown.
func (c *Client) response(k *call, a *answer) (*Response, error) {
	var s scrubber
	c.loadScrubber(&s, a.sent)
	defer s.clear()
	body, err := s.body(a.body)
	if err != nil || len(body) > 0 && &body[0] != &a.body[0] {
		// The answer as sent may repeat the credential: nothing keeps it.
		clear(a.body)
	}
	resp := &Response{Status: a.status, Header: a.header, Body: body, scrub: c.refOf(a.sent)}
	if err != nil {
		return resp, fmt.Errorf("%s: %w", c.where(k.method, a.url), err)
	}
	return resp, nil
}

// receiver reads a successful answer (below 400 and not a refused login)
// into a, and sets a.limited from the dialect. Its error fails the
// attempt.
type receiver func(c *Client, ctx context.Context, k *call, resp *http.Response, st *stall, a *answer) error

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
// Close ends it through the client's lifetime, which ended then reports.
// Inside a walk, a write is refused unsent and the answer that settled
// the call, an error answer included, is checked against the walk
// (walk.go).
func (c *Client) execute(ctx context.Context, k *call, recv receiver) (*answer, error) {
	if err := c.refuseInWalk(ctx, k); err != nil {
		return nil, err
	}
	var cancel context.CancelFunc
	if k.transfer {
		ctx, cancel = context.WithCancel(ctx)
	} else {
		ctx, cancel = context.WithTimeout(ctx, k.timeout)
	}
	stop := context.AfterFunc(c.life, cancel)
	defer func() {
		stop()
		cancel()
	}()
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
			if a != nil {
				if moved := c.walked(ctx, a); moved != nil {
					return nil, moved
				}
			}
			if err != nil {
				return nil, err
			}
			return a, nil
		}
	}
}

// readBody is the receiver of Do and JSON: the answer whole, refused
// unread when its length is over the bound.
func (c *Client) readBody(ctx context.Context, k *call, resp *http.Response, st *stall, a *answer) error {
	if resp.ContentLength > k.limit {
		return c.tooLarge(k, a)
	}
	body, err := readUpTo(st.reader(resp.Body), declaredSize(resp), k.limit)
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

// readErrorBody reads an error answer's body, up to the call's bound or
// errorReadLimit, whichever is less, and keeps what fits: the status is
// the answer. A body cut short fails a read as transient and leaves a
// write's status standing with what arrived. The call ending during the
// read is reported as its end.
func (c *Client) readErrorBody(ctx context.Context, k *call, st *stall, resp *http.Response, a *answer) error {
	limit := min(k.limit, errorReadLimit)
	body, err := readUpTo(st.reader(resp.Body), declaredSize(resp), limit)
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

const (
	// answerBufferStart is the first size of a pooled answer buffer.
	answerBufferStart = 32 << 10
	// answerBufferKeep bounds a pooled answer buffer: one grown past it
	// is dropped after its read, so one large answer is not retained. It
	// holds the largest page a client asks for as a matter of course (500
	// board rows of Jira, measured at 1.27 MB), so a walk of such pages
	// reuses one buffer.
	answerBufferKeep = 2 << 20
)

// answerBuffers holds the buffers readUpTo grows an answer of unknown
// length in, each cleared before it goes back. The garbage collector
// empties the pool of what two cycles did not use, so an idle client
// retains none.
var answerBuffers sync.Pool // of *[]byte

// readUpTo reads r to its end or limit bytes, whichever comes first, and
// returns a slice of exactly what it read, with the error that stopped
// it short of the end. size is the length the answer declared, or below
// zero when it declared none, as for a compressed answer, which net/http
// decodes as it is read. A declared length is read into one buffer of
// that length and a byte past it, which sees the end. Otherwise the read
// grows a pooled buffer and the caller gets a copy of what it read; the
// buffer is cleared before it is pooled again, since it stays reachable
// and held answer text the scrub has not seen.
func readUpTo(r io.Reader, size, limit int64) ([]byte, error) {
	if size < 0 {
		return readGrowing(r, nil, limit)
	}
	buf := make([]byte, min(size, limit-1)+1)
	n, err := readInto(r, buf)
	if err != nil || n < len(buf) || int64(n) == limit {
		return buf[:n:n], err
	}
	// Longer than it declared: read on.
	return readGrowing(r, buf, limit)
}

// declaredSize is the length resp's body declares, zero for a body that
// has none (a HEAD's, a 204's), and below zero when unknown.
func declaredSize(resp *http.Response) int64 {
	if resp.Body == http.NoBody {
		return 0
	}
	return resp.ContentLength
}

// readGrowing is readUpTo of an answer of unknown length whose first
// bytes, prefix, were read already.
func readGrowing(r io.Reader, prefix []byte, limit int64) ([]byte, error) {
	held, _ := answerBuffers.Get().(*[]byte)
	if held == nil {
		buf := make([]byte, 0, answerBufferStart)
		held = &buf
	}
	buf := append((*held)[:0], prefix...)
	var err error
	var next [1]byte
	for int64(len(buf)) < limit {
		// A full buffer may hold the whole answer: its next byte is read
		// apart, so an answer of exactly the buffer's size is not doubled
		// to learn that it ended.
		full := len(buf) == cap(buf)
		dst := next[:]
		if !full {
			dst = buf[len(buf):min(cap(buf), len(buf)+int(min(limit-int64(len(buf)), math.MaxInt32)))]
		}
		n, readErr := r.Read(dst)
		switch {
		case n == 0:
		case !full:
			buf = buf[:len(buf)+n]
		default:
			// Exactly double, so a buffer that began at answerBufferStart
			// reaches answerBufferKeep and not past it, and a growing
			// read discards less than twice its answer.
			grown := make([]byte, len(buf)+1, 2*cap(buf))
			copy(grown, buf)
			grown[len(buf)] = next[0]
			buf = grown
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				err = readErr
			}
			break
		}
	}
	out := make([]byte, len(buf))
	copy(out, buf)
	// A buffer that goes back to the pool stays reachable, so it is
	// cleared of the answer. One the read outgrew or drops here is garbage,
	// as net/http's own read buffers are.
	if cap(buf) <= answerBufferKeep {
		clear(buf)
		*held = buf[:0]
		answerBuffers.Put(held)
	}
	return out, err
}

// readInto reads r into buf until buf is full or r ends: what it read,
// and r's error, nil at its end.
func readInto(r io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
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
// out of time (transient), or cancelled by its caller. A cancellation
// once the client's lifetime ended is Close's.
func (c *Client) ended(ctx context.Context, where string) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &TransientError{Err: fmt.Errorf("%s: %w", where, context.DeadlineExceeded)}
	case c.life.Err() != nil:
		return fmt.Errorf("%s: %w", where, errClosed)
	}
	return fmt.Errorf("%s: %w", where, ctx.Err())
}

func (c *Client) where(method string, u *url.URL) string {
	return c.name + " " + method + " " + redact(u)
}
