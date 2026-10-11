package forgeapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Stream writes the response body to dst and fails with ErrBodyTooLarge
// once limit bytes pass. It never uses the ETag store: a caller's
// If-None-Match passes through, the response's ETag is in Header, and a
// 304 writes nothing and reports NotModified.
//
// A *TailBuffer dst declares a tail: redirects are followed by hand, and a
// final answer whose Content-Length exceeds the buffer's Cap is closed
// unread and re-requested as Range: bytes=(total-cap)- against the URL
// that answered (for a GitHub job log, the blob of the first hop, not the
// logs endpoint). A 206 is the tail; a 200 (the server ignores Range) is
// read through the buffer, which keeps the tail. A 416 is a *StatusError.
func (c *Client) Stream(ctx context.Context, r Request, dst io.Writer, limit int64) (*Response, error) {
	if limit <= 0 {
		return nil, errors.New("forgeapi: Stream needs a positive limit")
	}
	tail, _ := dst.(*TailBuffer)
	ctx, cancel := c.withTimeout(ctx, r.Timeout)
	defer cancel()
	refreshed := false
	for {
		resp, retry, err := c.streamAttempt(ctx, r, dst, tail, limit, refreshed)
		if !retry {
			return resp, err
		}
		refreshed = true
	}
}

func (c *Client) streamAttempt(ctx context.Context, r Request, dst io.Writer, tail *TailBuffer, limit int64, refreshed bool) (*Response, bool, error) {
	ex, err := c.prepare(ctx, r, false)
	if err != nil {
		return nil, false, err
	}
	release, err := c.acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()

	client := c.svc.follow
	if tail != nil {
		client = c.svc.noFollow
	}
	resp, err := c.send(ctx, client, ex.req)
	if err != nil {
		return nil, false, err
	}
	answered := ex.req
	for hops := 0; tail != nil && isRedirect(resp.StatusCode); hops++ {
		location, locErr := resp.Location()
		drain(resp)
		if locErr != nil || hops >= 10 {
			return nil, false, fmt.Errorf("forge %s %s: redirect without a usable Location", ex.req.Method, c.redact(answered.URL))
		}
		if answered, err = c.hop(answered, location, nil); err != nil {
			return nil, false, err
		}
		if resp, err = c.send(ctx, client, answered); err != nil {
			return nil, false, err
		}
	}
	if tail != nil && resp.StatusCode == http.StatusOK && resp.ContentLength > int64(tail.Cap()) {
		total := resp.ContentLength
		drain(resp)
		rangeHeader := http.Header{"Range": {"bytes=" + strconv.FormatInt(total-int64(tail.Cap()), 10) + "-"}}
		if answered, err = c.hop(answered, answered.URL, rangeHeader); err != nil {
			return nil, false, err
		}
		if resp, err = c.send(ctx, client, answered); err != nil {
			return nil, false, err
		}
	}
	defer drain(resp)

	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		c.observe(ex, resp.Header)
		c.svc.gates.succeeded(ex.lease, c.svc.now())
		if resp.ContentLength > limit {
			return nil, false, fmt.Errorf("forge %s %s: %w (%d bytes, limit %d)", ex.req.Method, c.redact(answered.URL), ErrBodyTooLarge, resp.ContentLength, limit)
		}
		written, copyErr := io.Copy(dst, io.LimitReader(resp.Body, limit+1))
		if copyErr != nil {
			return nil, false, c.bodyError(ctx, answered, copyErr)
		}
		if written > limit {
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

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// hop builds the next GET of a hand-followed Stream: the caller's headers
// plus extra, on the call's context, so authTransport gives it a
// credential only when its host is one of the forge's own.
func (c *Client) hop(prev *http.Request, target *url.URL, extra http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(prev.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("forge %s: build request: %w", c.host, err)
	}
	for k, v := range prev.Header {
		if k == "Authorization" || k == "Private-Token" || k == "Range" {
			continue
		}
		req.Header[k] = append([]string(nil), v...)
	}
	for k, v := range extra {
		req.Header[k] = append([]string(nil), v...)
	}
	if strings.EqualFold(target.Host, prev.URL.Host) {
		req.Host = prev.Host
	}
	return req, nil
}
