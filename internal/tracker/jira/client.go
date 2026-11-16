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
// memory, bounded and droppable; nothing is written to disk and nothing
// prints.
//
// Errors wrap the transport's with %w, so errors.As finds the trackerapi
// types and trackerapi.WriteNotApplied tells a refused write from one
// whose outcome is unknown. A request the client refuses before sending
// is a *trackerapi.NotSentError. Jira answers some requests with a 404
// when the credential is bad; the client tells that apart from a missing
// resource by asking who it is signed in as (identity.go).
//
// Rich text is Atlassian Document Format; package adf converts it to and
// from Markdown (adf.ToMarkdown renders an Issue's Description).
package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"

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
	// MaxUploadBytes bounds one upload request, multipart framing
	// included; zero means 100 MiB.
	MaxUploadBytes int64
}

// Client calls one Jira Cloud site. It is safe for concurrent use; Close
// releases it.
type Client struct {
	api       *trackerapi.Client
	maxUpload int64
	// now is replaced only by tests, before first use.
	now func() time.Time

	id      identity
	users   *userCache
	catalog catalogCache
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
	})
	if err != nil {
		return nil, fmt.Errorf("jira: %w", err)
	}
	c := &Client{api: api, maxUpload: opts.MaxUploadBytes, now: time.Now, users: newUserCache(userCacheSize)}
	if c.maxUpload == 0 {
		c.maxUpload = defaultMaxUpload
	}
	return c, nil
}

// Close cancels the calls in flight, waits for a sign-in check in flight
// and zeroes the credential. Every later call fails with a
// *trackerapi.NotSentError. Close may be called more than once.
func (c *Client) Close() {
	c.api.Close()
	c.id.close()
}

// ForgetCredential drops the credential, so the next call reads the
// source again, and drops the last sign-in check with it. For a caller
// that knows the credential changed.
func (c *Client) ForgetCredential() {
	c.api.ForgetCredential()
	c.id.forget()
}

// call sends r and decodes the answer into out (nil skips decoding). A
// 404 is checked against the sign-in (identity.go) and an AuthError
// drops the last check.
func (c *Client) call(ctx context.Context, r trackerapi.Request, out any) (*trackerapi.Response, error) {
	resp, err := c.api.JSON(ctx, r, out)
	if err != nil {
		return resp, c.explain(ctx, err)
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
// happened to a request: a transport error, or a call that ended.
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
		// An answer that did not decode.
		func(e error) bool { _, ok := errors.AsType[*json.SyntaxError](e); return ok },
		func(e error) bool { _, ok := errors.AsType[*json.UnmarshalTypeError](e); return ok },
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

// flight is one load shared by its callers: done closes once val and err
// are set.
type flight[T any] struct {
	done chan struct{}
	val  T
	err  error
}

// await waits for f or ctx, whichever ends first.
func (f *flight[T]) await(ctx context.Context) (T, error) {
	select {
	case <-f.done:
		return f.val, f.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}
