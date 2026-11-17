// Package jira is the typed client for the Jira Cloud platform REST API
// (rest/api/3) that the Issues pane drives: issues and search, fields and
// their metadata, values built from field names and Markdown, create and
// edit planned then applied, transitions, comments, links, attachments,
// watchers and people. The contract is docs/architecture/tracker-clients.md.
//
// A Client is built by its caller, one per site, and is safe for
// concurrent use. It sends every request through its own
// trackerapi.Client and imports nothing of the app but trackerapi and
// adf. It caches users by account ID and the field catalog, both in
// memory, bounded and droppable (cache.go); nothing is written to disk
// and nothing prints. Every exported call passes one admission guard
// first (admit), so a closed client or an ended context fails it even
// when a cache holds the answer.
//
// Errors wrap the transport's with %w, so errors.As finds the trackerapi
// types and trackerapi.WriteNotApplied tells a refused write from one
// whose outcome is unknown. A request the client refuses before sending
// is a *trackerapi.NotSentError. Jira answers some requests with a 404
// when the credential is bad; the client tells that apart from a missing
// resource by asking who it is signed in as (identity.go). Every
// successful answer is decoded in one place (decodeAnswer), which removes
// the credential from all of its text.
//
// Rich text is Atlassian Document Format; package adf converts it to and
// from Markdown (adf.ToMarkdown renders an Issue's Description).
package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"sync/atomic"

	"agent-overflow/internal/tracker/trackerapi"
)

// apiPath is the platform REST API root every path starts from.
const apiPath = "rest/api/3/"

// defaultMaxUpload bounds one upload when Options.MaxUploadBytes is zero.
// An upload is held in memory while it is sent.
const defaultMaxUpload = 100 << 20

// Options configures a Client.
type Options struct {
	// BaseURL is the site, such as https://example.atlassian.net. It is
	// https, or http only on a loopback IP address.
	BaseURL string
	// Version is the app version sent in the User-Agent. Required.
	Version string
	// Credentials is read at first use and again after a 401. Required.
	Credentials trackerapi.CredentialSource
	// Proxy picks the proxy for a request; nil connects directly.
	Proxy func(*http.Request) (*url.URL, error)
	// OnAnswer, when set, is told the status and headers of every answer,
	// as trackerapi.Options.OnAnswer describes.
	OnAnswer func(status int, header http.Header)
	// MaxUploadBytes bounds one upload request, multipart framing
	// included, and the local images one write embeds, together; zero
	// means 100 MiB.
	MaxUploadBytes int64
}

// Client calls one Jira Cloud site. It is safe for concurrent use; Close
// releases it.
type Client struct {
	api       *trackerapi.Client
	maxUpload int64

	// closedErr is the transport's refusal of a call on a closed client;
	// it is set before closed.
	closeOnce sync.Once
	closed    atomic.Bool
	closedErr error

	id      identity
	users   *cache[string, User]
	catalog *cache[struct{}, []Field]
}

// New builds a Client and the trackerapi.Client under it. Nothing is
// sent until the first call. It refuses what trackerapi.New refuses and
// a negative MaxUploadBytes.
func New(opts Options) (*Client, error) {
	if opts.MaxUploadBytes < 0 {
		return nil, errors.New("jira: Options.MaxUploadBytes is negative")
	}
	api, err := trackerapi.New(trackerapi.Options{
		Name:        "jira",
		BaseURL:     opts.BaseURL,
		Version:     opts.Version,
		Credentials: opts.Credentials,
		Dialect:     dialect{},
		Proxy:       opts.Proxy,
		OnAnswer:    opts.OnAnswer,
	})
	if err != nil {
		return nil, fmt.Errorf("jira: %w", err)
	}
	c := &Client{
		api: api, maxUpload: opts.MaxUploadBytes,
		users:   newCache[string, User](userCacheSize, nil),
		catalog: newCache[struct{}, []Field](1, cloneFields),
	}
	if c.maxUpload == 0 {
		c.maxUpload = defaultMaxUpload
	}
	return c, nil
}

// Close cancels the calls in flight, waits for a sign-in check in flight
// and zeroes the credential. Every later call, one a cache could answer
// included, fails with the transport's *trackerapi.NotSentError for a
// closed client. Close may be called more than once.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.api.Close()
		// A closed transport refuses a call before it sends or reads
		// anything; its refusal is what every later call returns.
		_, err := c.api.Do(context.Background(), trackerapi.Request{Path: myselfPath})
		if _, ok := errors.AsType[*trackerapi.NotSentError](err); !ok {
			err = &trackerapi.NotSentError{Err: fmt.Errorf("jira: the client is closed: %w", err)}
		}
		c.closedErr = err
		c.closed.Store(true)
	})
	c.id.close()
}

// admit is the guard every exported call passes before any work, a cache
// hit included: a closed client and an ended context fail it.
func (c *Client) admit(ctx context.Context) error {
	if c.closed.Load() {
		return c.closedErr
	}
	return ctx.Err()
}

// ForgetCredential drops the credential, so the next call reads the
// source again, and detaches the sign-in checks made with it.
func (c *Client) ForgetCredential() {
	c.api.ForgetCredential()
	c.id.forget()
}

// call sends r and decodes the answer into out (nil skips decoding)
// through decodeAnswer. A 404 is checked against the sign-in
// (identity.go), and an AuthError detaches the sign-in checks.
func (c *Client) call(ctx context.Context, r trackerapi.Request, out any) (*trackerapi.Response, error) {
	resp, err := c.api.Do(ctx, r)
	if err != nil {
		return resp, c.explain(ctx, err)
	}
	if out != nil {
		if err := decodeAnswer(resp, out); err != nil {
			return resp, err
		}
	}
	return resp, nil
}

// get is call for a GET of path under apiPath.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	_, err := c.call(ctx, trackerapi.Request{Path: apiPath + path, Query: q}, out)
	return err
}

// send is call for a write of body to path under apiPath.
func (c *Client) send(ctx context.Context, method, path string, q url.Values, body, out any) error {
	_, err := c.call(ctx, trackerapi.Request{Method: method, Path: apiPath + path, Query: q, Body: body}, out)
	return err
}

// explain turns a transport error into the client's: a 404 the
// credential caused becomes an AuthError.
func (c *Client) explain(ctx context.Context, err error) error {
	if _, ok := errors.AsType[*trackerapi.AuthError](err); ok {
		c.id.forget()
		return err
	}
	if errors.Is(err, trackerapi.ErrNotFound) {
		return c.checkNotFound(ctx, err)
	}
	return err
}

// notSent is a refusal made before any request was sent.
func notSent(format string, args ...any) error {
	return &trackerapi.NotSentError{Err: fmt.Errorf("jira: "+format, args...)}
}

// notSentErr wraps err as a *trackerapi.NotSentError when it is the
// client's own refusal, and returns it as it is when it says what
// happened to a request: a transport error, an answer, or a call that
// ended.
func notSentErr(err error) error {
	if err == nil || fromRequest(err) {
		return err
	}
	return &trackerapi.NotSentError{Err: err}
}

// fromRequest reports an error that came from sending a request, or
// already says it was not sent.
func fromRequest(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	for _, is := range []func(error) bool{
		func(e error) bool { _, ok := errors.AsType[*trackerapi.NotSentError](e); return ok },
		func(e error) bool { _, ok := errors.AsType[*trackerapi.StatusError](e); return ok },
		func(e error) bool { _, ok := errors.AsType[*trackerapi.AuthError](e); return ok },
		func(e error) bool { _, ok := errors.AsType[*trackerapi.TransientError](e); return ok },
		func(e error) bool { _, ok := errors.AsType[*trackerapi.RateLimitedError](e); return ok },
		func(e error) bool { _, ok := errors.AsType[*trackerapi.BodyTooLargeError](e); return ok },
		func(e error) bool { _, ok := errors.AsType[*UnconfirmedError](e); return ok },
		func(e error) bool { _, ok := errors.AsType[*AnswerError](e); return ok },
	} {
		if is(err) {
			return true
		}
	}
	return false
}

var (
	issueKeyRE   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-[0-9]+$`)
	projectKeyRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	numericRE    = regexp.MustCompile(`^[0-9]+$`)
)

// checkIssue refuses ref unless it is an issue key or a numeric issue
// ID, so no caller text reaches a path as another resource.
func checkIssue(ref string) error {
	if issueKeyRE.MatchString(ref) || numericRE.MatchString(ref) {
		return nil
	}
	return notSent("%q is not an issue key or ID", ref)
}

// checkProject refuses ref unless it is a project key or numeric ID.
func checkProject(ref string) error {
	if projectKeyRE.MatchString(ref) || numericRE.MatchString(ref) {
		return nil
	}
	return notSent("%q is not a project key or ID", ref)
}

// checkID refuses id unless it is a numeric ID; what names it.
func checkID(what, id string) error {
	if numericRE.MatchString(id) {
		return nil
	}
	return notSent("%q is not a %s ID", id, what)
}

// issuePath is the path of issue ref, checked, under apiPath.
func issuePath(ref string) (string, error) {
	if err := checkIssue(ref); err != nil {
		return "", err
	}
	return "issue/" + url.PathEscape(ref), nil
}

// issueParam sets ref, checked, as the issueId or issueKey query
// parameter, for the endpoints that tell the two apart.
func issueParam(q url.Values, ref string) error {
	if err := checkIssue(ref); err != nil {
		return err
	}
	if numericRE.MatchString(ref) {
		q.Set("issueId", ref)
	} else {
		q.Set("issueKey", ref)
	}
	return nil
}
