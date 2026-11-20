package trackerapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Stream sends r, a GET, and copies the answer body to dst: at most limit
// bytes, after which the call fails with *BodyTooLargeError. An answer
// whose length is over limit is refused before a byte is written. A read
// is retried only until the first byte reaches dst. Redirects are
// followed, without the credential off the base origin. The Response has
// no Body.
func (c *Client) Stream(ctx context.Context, r Request, dst io.Writer, limit int64) (*Response, error) {
	switch {
	case limit <= 0:
		return nil, &NotSentError{Err: fmt.Errorf("trackerapi: %s: Stream needs a positive limit", c.name)}
	case r.MaxBodyBytes != 0:
		return nil, &NotSentError{Err: fmt.Errorf("trackerapi: %s: Stream takes its limit as an argument, not Request.MaxBodyBytes", c.name)}
	case dst == nil:
		return nil, &NotSentError{Err: fmt.Errorf("trackerapi: %s: Stream needs a destination", c.name)}
	}
	k, err := c.prepareGet(r, "Stream")
	if err != nil {
		return nil, err
	}
	k.limit, k.dst = limit, dst
	a, err := c.execute(ctx, k, (*Client).copyBody)
	if err != nil {
		return nil, err
	}
	// The answer went to dst as it arrived, a.body is empty and nothing is
	// scrubbed: a download is the file's bytes, which the caller asked
	// for, and holding it to search for the credential would undo the
	// stream's bounded memory.
	return c.response(k, a)
}

// prepareGet is prepareCall for a call that only takes a GET.
func (c *Client) prepareGet(r Request, name string) (*call, error) {
	k, err := c.prepareCall(r)
	if err != nil {
		return nil, err
	}
	if k.method != http.MethodGet {
		return nil, &NotSentError{Err: fmt.Errorf("trackerapi: %s: %s sends a GET, not a %s", c.name, name, k.method)}
	}
	return k, nil
}

// copyBody is the receiver of Stream: it copies the answer to k.dst. An
// answer its headers mark as a rate limit never reaches it, so no byte of
// one is copied.
func (c *Client) copyBody(ctx context.Context, k *call, resp *http.Response, st *stall, a *answer) error {
	if resp.ContentLength > k.limit {
		return c.tooLarge(k, a)
	}
	body := st.reader(resp.Body)
	written, err := io.Copy(&committingWriter{w: k.dst, k: k}, io.LimitReader(body, k.limit))
	if err == nil {
		err = cutOff(ctx, st)
	}
	if err != nil {
		if wErr, ok := errors.AsType[*destinationError](err); ok {
			return fmt.Errorf("%s: write the answer: %w", c.where(k.method, a.url), wErr.err)
		}
		return c.readError(ctx, k, st, a, err)
	}
	if written == k.limit {
		var probe [1]byte
		n, err := io.ReadFull(body, probe[:])
		if n > 0 {
			return c.tooLarge(k, a)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return c.readError(ctx, k, st, a, err)
		}
	}
	return nil
}

// committingWriter marks the call committed at its first write and tells
// a failure of the destination from one of the answer.
type committingWriter struct {
	w io.Writer
	k *call
}

func (w *committingWriter) Write(p []byte) (int, error) {
	w.k.committed = true
	n, err := w.w.Write(p)
	if err != nil {
		return n, &destinationError{err: err}
	}
	return n, nil
}

type destinationError struct{ err error }

func (e *destinationError) Error() string { return e.err.Error() }

// Location sends r, a GET, without following a redirect, and returns the
// redirect's target resolved against the URL that answered. The target is
// returned as the tracker gave it, signed query included, and is not
// requested. A caller parses it, and parsing undoes percent-encoding, so
// a target that repeats the credential the request carried, as written or
// percent-encoded, is refused instead of scrubbed; so is any target once
// the client no longer holds that credential. An answer that is not a
// redirect fails the call.
func (c *Client) Location(ctx context.Context, r Request) (string, error) {
	k, err := c.prepareGet(r, "Location")
	if err != nil {
		return "", err
	}
	k.redirects = answerRedirect
	discard := func(*Client, context.Context, *call, *http.Response, *stall, *answer) error { return nil }
	a, err := c.execute(ctx, k, discard)
	if err != nil {
		return "", err
	}
	where := c.where(k.method, a.url)
	location := a.header.Get("Location")
	if a.status < 300 || a.status > 399 || location == "" {
		return "", fmt.Errorf("%s: answered %d, not a redirect", where, a.status)
	}
	target, err := a.url.Parse(location)
	if err != nil {
		// Not quoted: the target can be signed.
		return "", fmt.Errorf("%s: the redirect target is not a URL", where)
	}
	var s scrubber
	c.loadScrubber(&s, a.sent)
	defer s.clear()
	text := target.String()
	if s.lost || s.holdsEncoded(text) {
		return "", fmt.Errorf("%s: the redirect target repeats the request's credential or can no longer be checked for it", where)
	}
	return text, nil
}
