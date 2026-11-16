// Package tempo is the typed client for the Tempo Cloud REST API v4:
// worklogs (read by author and date range, created, changed, deleted, one
// at a time or as a batch with a per-item outcome), the checks Tempo would
// refuse a worklog for that can be made before sending it, work
// attributes, accounts, Tempo's settings, the user schedule and timesheet
// approvals.
//
// The package owns Tempo's request and response shapes, the paging, the
// rule that a change replaces the whole record, and Tempo's error and
// rate limit conventions (its trackerapi.Dialect). It does not know Jira:
// Tempo stores a worklog's issue by numeric ID, and the caller's IssueIDs
// maps keys to IDs and back. The HTTP transport, credentials, retries and
// the rate gate are trackerapi's. Nothing is stored on disk and nothing is
// printed.
//
// Units: durations are time.Duration, sent to Tempo as whole seconds
// rounded to the nearest second. Dates are Date and times of day
// TimeOfDay; Start states the time zone rule.
package tempo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"agent-overflow/internal/tracker/trackerapi"
)

// Base URLs Tempo documents. A base URL for a region other than the one
// the site's Tempo data lives in answers with an authorization error.
const (
	// DefaultBaseURL serves every region.
	DefaultBaseURL = "https://api.tempo.io"
	// EUBaseURL is the direct path to data held in Europe.
	EUBaseURL = "https://api.eu.tempo.io"
	// USBaseURL is the direct path to data held in the Americas.
	USBaseURL = "https://api.us.tempo.io"
)

// maxPerSecond is Tempo's documented gateway limit: 5 requests a second
// per IP address, shared by every integration on that address.
const maxPerSecond = 5

// IssueIDs maps Jira issue keys to the numeric IDs Tempo stores, and
// back. The caller implements it, usually over its Jira client or local
// mirror. The client asks once per call with every distinct value that
// call needs (a batch, one page of a listing), never once per worklog.
type IssueIDs interface {
	// IDs returns the numeric ID of each key it knows, keyed by the key
	// as asked. A key it does not know is absent, not an error; an error
	// is a failure to look the keys up at all.
	IDs(ctx context.Context, keys []string) (map[string]int64, error)
	// Keys returns the current key of each issue ID it knows. An ID it
	// does not know (a deleted issue, one the user cannot see) is absent.
	Keys(ctx context.Context, ids []int64) (map[int64]string, error)
}

// Options configures a Client.
type Options struct {
	// BaseURL is Tempo's API host; empty means DefaultBaseURL. A loopback
	// http URL is accepted for tests.
	BaseURL string
	// Version is the app version sent in the User-Agent. Required.
	Version string
	// Credentials gives the Tempo API token as a trackerapi.Bearer
	// credential. Required.
	Credentials trackerapi.CredentialSource
	// Proxy picks a proxy for a request; nil connects directly.
	Proxy func(*http.Request) (*url.URL, error)
	// Issues maps issue keys and IDs. Required.
	Issues IssueIDs
}

// Client calls Tempo for one site and region. It is safe for concurrent
// use. Close releases it.
type Client struct {
	http   *trackerapi.Client
	issues IssueIDs
	site   siteCache
	// life ends at Close; calls into issues run under it too.
	life context.Context
	stop context.CancelFunc
}

// New builds a Client. Nothing is sent until the first call.
func New(opts Options) (*Client, error) {
	topts, err := transportOptions(opts)
	if err != nil {
		return nil, err
	}
	return newClient(topts, opts.Issues)
}

// transportOptions is the trackerapi configuration New uses.
func transportOptions(opts Options) (trackerapi.Options, error) {
	if opts.Issues == nil {
		return trackerapi.Options{}, errors.New("tempo: Options.Issues is required")
	}
	base := opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return trackerapi.Options{
		Name:         "tempo",
		BaseURL:      base,
		Version:      opts.Version,
		Credentials:  opts.Credentials,
		Dialect:      dialect{},
		MaxPerSecond: maxPerSecond,
		Proxy:        opts.Proxy,
	}, nil
}

func newClient(topts trackerapi.Options, issues IssueIDs) (*Client, error) {
	h, err := trackerapi.New(topts)
	if err != nil {
		return nil, fmt.Errorf("tempo: %w", err)
	}
	life, stop := context.WithCancel(context.Background())
	return &Client{http: h, issues: issues, life: life, stop: stop}, nil
}

// Close ends the calls in flight, including calls into IssueIDs, whose
// context it cancels, and releases the connection and the credential.
// Every later call fails with a *trackerapi.NotSentError. Close may be
// called more than once.
func (c *Client) Close() {
	c.stop()
	c.http.Close()
	c.site.shut()
}

// ForgetCredential drops the credential so the next call reads it again,
// for a caller that knows the token changed.
func (c *Client) ForgetCredential() { c.http.ForgetCredential() }

// DropCache drops the cached work attributes and settings; the next call
// that needs them reads them again. A write Tempo refuses drops them too.
func (c *Client) DropCache() { c.site.drop() }

// errClosed is a call on a closed Client that the transport did not see.
var errClosed = errors.New("tempo: client is closed")

// bound is ctx, ended also by Close: the context of a call into the
// caller's IssueIDs. done releases it.
func (c *Client) bound(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(c.life, func() { cancel(errClosed) })
	return ctx, func() {
		stop()
		cancel(nil)
	}
}

// unreadable is a value in an answer that does not read, its text
// scrubbed of the credential the request carried.
func unreadable(resp *trackerapi.Response, err error) error {
	return errors.New(resp.Scrub(err.Error()))
}

// path builds a request path from fixed segments and escaped values.
func path(fixed string, values ...string) string {
	out := fixed
	for _, v := range values {
		out += "/" + url.PathEscape(v)
	}
	return out
}
