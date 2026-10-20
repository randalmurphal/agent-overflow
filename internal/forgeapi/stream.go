package forgeapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// NoLimit as Stream's limit streams a body of any size. A CI job log is
// written to disk as it arrives, so its size is not a memory bound.
const NoLimit int64 = -1

// StreamStarter is a Stream destination told the answer before its body:
// the status (a 2xx) and the headers. An error it returns ends the call
// unread with that error.
type StreamStarter interface {
	StartStream(status int, header http.Header) error
}

// Stream writes the response body to dst and fails with ErrBodyTooLarge
// once limit bytes pass; NoLimit streams any size. It never uses the ETag
// store: a caller's If-None-Match passes through, the response's ETag is
// in Header, and a 304 writes nothing and reports NotModified. Redirects
// are followed with the caller's headers (Range and If-None-Match reach a
// GitHub job log's blob) and without the forge credential on another
// host. A dst that is a StreamStarter sees the answer first.
func (c *Client) Stream(ctx context.Context, r Request, dst io.Writer, limit int64) (*Response, error) {
	if limit <= 0 && limit != NoLimit {
		return nil, errors.New("forgeapi: Stream needs a positive limit or NoLimit")
	}
	ctx, cancel := c.withTimeout(ctx, r.Timeout)
	defer cancel()
	refreshed := false
	for {
		resp, retry, err := c.streamAttempt(ctx, r, dst, limit, refreshed)
		if !retry {
			return resp, c.settleProbe(ctx, err)
		}
		refreshed = true
	}
}

func (c *Client) streamAttempt(ctx context.Context, r Request, dst io.Writer, limit int64, refreshed bool) (*Response, bool, error) {
	ex, err := c.prepare(ctx, r, false)
	if err != nil {
		return nil, false, err
	}
	release, err := c.acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()

	resp, err := c.send(ctx, c.svc.follow, ex.req)
	if err != nil {
		return nil, false, err
	}
	answered := ex.req
	if resp.Request != nil {
		answered = resp.Request
	}
	defer drain(resp)

	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		c.observe(ex, resp.Header)
		c.svc.gates.succeeded(ex.lease, c.svc.now())
		if limit != NoLimit && resp.ContentLength > limit {
			return nil, false, fmt.Errorf("forge %s %s: %w (%d bytes, limit %d)", ex.req.Method, c.redact(answered.URL), ErrBodyTooLarge, resp.ContentLength, limit)
		}
		if starter, ok := dst.(StreamStarter); ok {
			if err := starter.StartStream(resp.StatusCode, resp.Header); err != nil {
				return nil, false, err
			}
		}
		body := io.Reader(resp.Body)
		if limit != NoLimit {
			body = io.LimitReader(resp.Body, limit+1)
		}
		written, copyErr := io.Copy(dst, body)
		if copyErr != nil {
			return nil, false, c.bodyError(ctx, answered, copyErr)
		}
		if limit != NoLimit && written > limit {
			return nil, false, fmt.Errorf("forge %s %s: %w (limit %d)", ex.req.Method, c.redact(answered.URL), ErrBodyTooLarge, limit)
		}
		return &Response{Status: resp.StatusCode, Header: resp.Header, Rate: parseRateLimit(resp.Header, ex.key.pool)}, false, nil
	}
	body, readErr := readBounded(resp.Body, c.svc.opts.MaxBodyBytes)
	if readErr != nil && !errors.Is(readErr, ErrBodyTooLarge) {
		return nil, false, c.bodyError(ctx, answered, readErr)
	}
	return c.settle(ctx, ex, answered, resp, body, refreshed)
}
