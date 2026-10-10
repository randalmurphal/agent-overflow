package forgeapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Request is one forge API call.
type Request struct {
	Method string // empty means GET
	// Path is relative to the host's REST base ("repos/o/r/pulls/1",
	// "projects/g%2Fr/merge_requests"), with an optional query. It is an
	// absolute https URL only when Attachment is set.
	Path  string
	Query url.Values
	// Header carries Range, If-None-Match and Accept. Authorization,
	// Private-Token and Host are refused.
	Header http.Header
	// Body is JSON-encoded; an io.Reader passes through.
	Body any
	// Timeout bounds the whole call, headers and body; zero takes
	// Options.ReadTimeout. A shorter context deadline wins.
	Timeout time.Duration
	// Attachment allows an absolute Path. Only the attachment fetchers set
	// it.
	Attachment bool
}

// Response is a forge answer. Whether the request was interactive comes
// from its context (WithInteractive), not the Request.
type Response struct {
	Status int
	Header http.Header
	// Body is the response body for Do, JSON and GraphQL, bounded by
	// Options.MaxBodyBytes; nil for Stream.
	Body []byte
	// NotModified is a 304. Do and JSON answer it from the ETag store with
	// the stored body and the headers it came with.
	NotModified bool
	// Rate is the quota the response's headers reported, nil when none.
	Rate *RateLimit
}

// Client talks to one forge host. Obtain it from Service.GitHub or
// Service.GitLab. Safe for concurrent use; at most four requests per host
// are in flight.
type Client struct {
	svc   *Service
	forge string
	host  string
	err   error
	slots chan struct{}

	// readLock serializes token reads, so concurrent requests fork one
	// handoff.
	readLock chan struct{}

	// credMu guards the credential and the resolved bases. A request's
	// header is written under the read lock and the token is zeroed under
	// the write lock, so no request sends a zeroed token.
	credMu    sync.RWMutex
	token     *Token
	fp        string
	readAt    time.Time
	failErr   error
	failUntil time.Time
	rest      *url.URL
	graphQL   *url.URL
}

// Do sends r and returns the answer read whole. A GET whose URL the ETag
// store holds is sent conditionally; a 304 returns the stored body with
// NotModified. Errors follow the table in
// docs/architecture/forge-transport.md#errors.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	return c.call(ctx, r, false)
}

// JSON is Do decoding a successful body into out.
func (c *Client) JSON(ctx context.Context, r Request, out any) (*Response, error) {
	resp, err := c.call(ctx, r, false)
	if err != nil {
		return resp, err
	}
	if out != nil {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return resp, fmt.Errorf("forge %s %s: decode response: %w", methodOf(r), c.host, err)
		}
	}
	return resp, nil
}

// GraphQL posts query with vars as the JSON variables (never interpolated
// into the query) and decodes the envelope's data into out. operation is
// the name of the operation query defines, sent as operationName so the
// forge (and the isolated boot's fake, which dispatches on it) runs that
// one. Errors in the envelope are a *GraphQLError, returned with the
// Response and with any data beside them decoded into out, so the caller
// decides what partial data it accepts. GitLab hosts serve no GraphQL
// here.
func (c *Client) GraphQL(ctx context.Context, operation, query string, vars map[string]any, out any) (*Response, error) {
	if c.forge != ForgeGitHub {
		return nil, fmt.Errorf("forgeapi: %s serves no GraphQL through this transport", c.forge)
	}
	if operation == "" {
		return nil, errors.New("forgeapi: a GraphQL request names its operation")
	}
	payload := map[string]any{"query": query, "operationName": operation}
	if len(vars) > 0 {
		payload["variables"] = vars
	}
	resp, err := c.call(ctx, Request{Method: http.MethodPost, Body: payload}, true)
	var gqlErr *GraphQLError
	if err != nil && !errors.As(err, &gqlErr) {
		return resp, err
	}
	data := json.RawMessage(nil)
	if gqlErr != nil {
		data = gqlErr.Data
	} else if env, ok := parseGraphQLEnvelope(resp.Body); ok {
		data = env.Data
	} else {
		return resp, fmt.Errorf("forge GraphQL %s: response is not a GraphQL envelope", c.host)
	}
	if out != nil && len(data) > 0 && string(data) != "null" {
		if decodeErr := json.Unmarshal(data, out); decodeErr != nil {
			return resp, errors.Join(err, fmt.Errorf("forge GraphQL %s: decode data: %w", c.host, decodeErr))
		}
	}
	return resp, err
}

// Pages sends r and follows the next page, GitHub's Link rel="next" first,
// then GitLab's X-Next-Page, for as long as visit answers more. The page
// cap is visit's: it returns false once the caller has the pages it wants.
// A next link off the host's REST base is refused.
func (c *Client) Pages(ctx context.Context, r Request, visit func(*Response) (more bool, err error)) error {
	next := r
	for {
		resp, err := c.Do(ctx, next)
		if err != nil {
			return err
		}
		more, err := visit(resp)
		if err != nil || !more {
			return err
		}
		following, ok, err := c.nextPage(resp, next)
		if err != nil || !ok {
			return err
		}
		next = following
	}
}

func (c *Client) nextPage(resp *Response, r Request) (Request, bool, error) {
	if link := nextLink(resp.Header.Values("Link")); link != "" {
		target, err := url.Parse(link)
		if err != nil {
			return Request{}, false, fmt.Errorf("forge %s: next page link: %w", c.host, err)
		}
		c.credMu.RLock()
		base := c.rest
		c.credMu.RUnlock()
		prefix := base.String()
		if !strings.HasPrefix(target.String(), prefix) {
			return Request{}, false, fmt.Errorf("forge %s: next page link %s is off the API base", c.host, RedactURL(target))
		}
		r.Path = strings.TrimPrefix(target.String(), prefix)
		r.Query = nil
		return r, true, nil
	}
	if page := strings.TrimSpace(resp.Header.Get("X-Next-Page")); page != "" {
		query := url.Values{}
		for k, v := range r.Query {
			query[k] = v
		}
		query.Set("page", page)
		r.Query = query
		return r, true, nil
	}
	return Request{}, false, nil
}

// nextLink finds rel="next" in RFC 8288 Link header values.
func nextLink(values []string) string {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
			if !ok {
				continue
			}
			for _, param := range strings.Split(params, ";") {
				name, val, _ := strings.Cut(strings.TrimSpace(param), "=")
				if strings.EqualFold(name, "rel") && strings.Trim(val, `"`) == "next" {
					return strings.Trim(strings.TrimSpace(target), "<>")
				}
			}
		}
	}
	return ""
}

// exchange is one prepared request: the http.Request, the gate lease and
// the ETag key it may revalidate.
type exchange struct {
	req     *http.Request
	key     gateKey
	lease   lease
	graphQL bool
	etag    *etagEntry
	etagKey etagKey
	body    bool // carries a body that cannot be resent
}

func (c *Client) call(ctx context.Context, r Request, graphQL bool) (*Response, error) {
	ctx, cancel := c.withTimeout(ctx, r.Timeout)
	defer cancel()
	refreshed := false
	for {
		resp, retry, err := c.attempt(ctx, r, graphQL, refreshed)
		if !retry {
			return resp, err
		}
		refreshed = true
	}
}

func (c *Client) attempt(ctx context.Context, r Request, graphQL, refreshed bool) (*Response, bool, error) {
	ex, err := c.prepare(ctx, r, graphQL)
	if err != nil {
		return nil, false, err
	}
	if ex.req.Method == http.MethodGet && ex.req.Header.Get("If-None-Match") == "" {
		ex.etagKey = etagKey{host: c.host, fingerprint: ex.key.fingerprint, method: http.MethodGet, url: ex.req.URL.String()}
		if entry, ok := c.svc.etags.lookup(ex.etagKey); ok {
			ex.etag = entry
			ex.req.Header.Set("If-None-Match", entry.etag)
		}
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
	defer drain(resp)
	body, readErr := readBounded(resp.Body, c.svc.opts.MaxBodyBytes)
	if readErr != nil {
		if !errors.Is(readErr, ErrBodyTooLarge) {
			return nil, false, c.bodyError(ctx, ex.req, readErr)
		}
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			return nil, false, fmt.Errorf("forge %s %s: %w (limit %d)", ex.req.Method, c.redact(ex.req.URL), ErrBodyTooLarge, c.svc.opts.MaxBodyBytes)
		}
	}
	return c.settle(ctx, ex, ex.req, resp, body, refreshed)
}

// settle classifies a read answer, updates the gate and the ETag store and
// decides whether the call retries after a token refresh.
func (c *Client) settle(ctx context.Context, ex *exchange, answered *http.Request, resp *http.Response, body []byte, refreshed bool) (*Response, bool, error) {
	now := c.svc.now()
	rate := c.observe(ex, resp.Header)
	result := classify(answer{
		host: c.host, pool: ex.key.pool, method: ex.req.Method, url: c.redact(answered.URL),
		status: resp.StatusCode, header: resp.Header, body: body, graphQL: ex.graphQL, now: now,
	})
	out := &Response{Status: resp.StatusCode, Header: resp.Header, Body: body, Rate: rate}
	switch result.outcome {
	case outcomeRateLimited:
		limited := result.err.(*RateLimitedError)
		limited.Until = c.svc.gates.limited(ex.lease, limited.Until, now)
		return nil, false, limited
	case outcomeUnauthorized:
		used := ex.key.fingerprint
		if refreshed {
			return nil, false, UnauthenticatedError(c.forge, result.err)
		}
		if err := c.refresh(ctx, used); err != nil {
			return nil, false, err
		}
		if ex.body {
			// The body is spent; the next call uses the new token.
			return nil, false, &TransientError{Err: result.err}
		}
		return nil, true, nil
	case outcomeNotModified:
		c.svc.gates.succeeded(ex.lease, now)
		out.NotModified = true
		out.Body = nil
		if ex.etag != nil {
			out.Body = append([]byte(nil), ex.etag.body...)
			out.Header = ex.etag.header.Clone()
		}
		return out, false, nil
	case outcomeOK, outcomeGraphQL:
		c.svc.gates.succeeded(ex.lease, now)
		if ex.etagKey.url != "" {
			if tag := resp.Header.Get("ETag"); tag != "" && resp.StatusCode == http.StatusOK {
				c.svc.etags.store(ex.etagKey, tag, body, resp.Header)
			} else if ex.etag != nil {
				c.svc.etags.remove(ex.etagKey)
			}
		}
		return out, false, result.err
	default:
		return nil, false, result.err
	}
}

// observe records the response's quota and returns it.
func (c *Client) observe(ex *exchange, header http.Header) *RateLimit {
	rate := parseRateLimit(header, ex.key.pool)
	if rate != nil {
		key := ex.key
		key.pool = rate.Pool
		c.svc.gates.observe(key, rate)
	}
	return rate
}

func (c *Client) usable() error {
	switch {
	case c.err != nil:
		return c.err
	case c.svc.isClosed():
		return errClosed
	}
	return nil
}

// pool is the quota pool a request draws on, known before the forge names
// it: GitHub keeps GraphQL apart from REST, GitLab has one pool for
// authenticated API calls.
func (c *Client) pool(graphQL bool) string {
	switch {
	case c.forge == ForgeGitLab:
		return "throttle_authenticated_api"
	case graphQL:
		return "graphql"
	}
	return "core"
}

func (c *Client) withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = c.svc.opts.ReadTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (c *Client) acquire(ctx context.Context) (func(), error) {
	select {
	case c.slots <- struct{}{}:
		return func() { <-c.slots }, nil
	case <-ctx.Done():
		return nil, c.contextError(ctx, nil)
	}
}

// send runs one request and maps a failure to reach the forge to a
// *TransientError whose message carries the redacted URL, or the caller's
// cancellation.
func (c *Client) send(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err == nil {
		return resp, nil
	}
	cause := err
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		cause = urlErr.Err
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, fmt.Errorf("forge %s %s: %w", req.Method, c.redact(req.URL), context.Canceled)
	}
	return nil, &TransientError{Err: fmt.Errorf("%s %s: %w", req.Method, c.redact(req.URL), cause)}
}

func (c *Client) bodyError(ctx context.Context, req *http.Request, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("forge %s %s: %w", req.Method, c.redact(req.URL), context.Canceled)
	}
	return &TransientError{Err: fmt.Errorf("%s %s: read body: %w", req.Method, c.redact(req.URL), err)}
}

// contextError reports a call that ended waiting: cancelled by its caller,
// or out of time, which is transient.
func (c *Client) contextError(ctx context.Context, cause error) error {
	if cause == nil {
		cause = ctx.Err()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &TransientError{Err: fmt.Errorf("forge %s: %w", c.host, cause)}
	}
	return fmt.Errorf("forge %s: %w", c.host, cause)
}

// readBounded reads at most limit bytes; a longer body returns what was
// read with ErrBodyTooLarge.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return body[:limit], ErrBodyTooLarge
	}
	return body, nil
}

// drain closes a response after discarding a bounded remainder, so the
// connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
