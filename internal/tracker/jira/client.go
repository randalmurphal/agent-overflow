// Package jira is the typed client for the Jira Cloud platform REST API
// (rest/api/3) and the Jira Software APIs (rest/agile/1.0,
// rest/software/1.0) that the Issues pane drives: issues and search,
// fields and their metadata, values built from field names and Markdown,
// create and edit planned then applied, transitions, comments, links,
// attachments, watchers and people; boards, sprints, ranking and issue
// lists (agile.go); changelogs (history.go), Jira's own worklogs
// (worklogs.go) and the planning poker add-on's calls on the site
// (poker.go). The contract is docs/architecture/tracker-clients.md.
//
// A Client is built by its caller, one per site, and is safe for
// concurrent use. It sends every request through its own
// trackerapi.Client and imports nothing of the app but trackerapi and
// adf. It caches users by account ID, the signed-in user, the field
// catalog, create screens, board configurations and sprint lists, all in memory, bounded
// and droppable (cache.go);
// nothing is written to disk and nothing prints. Every exported call passes one admission guard
// first (admit), so a closed client or an ended context fails it even
// when a cache holds the answer.
//
// Errors wrap the transport's with %w, so errors.As finds the trackerapi
// types and trackerapi.WriteNotApplied tells a refused write from one
// whose outcome is unknown. A request the client refuses before sending
// is a *trackerapi.NotSentError. Jira answers some requests with a 404
// when the credential is bad; the client tells that apart from a missing
// resource by asking who it is signed in as (identity.go). Every answer
// body arrives scrubbed of the credential by the transport
// (trackerapi.Response.Body); a read's is decoded by decodeAnswer, a
// write's by write (writes.go).
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
	"sync"
	"sync/atomic"

	"agent-overflow/internal/tracker/trackerapi"
)

// apiPath is the platform REST API root every path starts from.
const apiPath = "rest/api/3/"

// defaultMaxImages bounds the images one write embeds when
// Options.MaxImageBytes is zero.
const defaultMaxImages = 100 << 20

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
	// OnAnswer, when set, is told the status of every answer and those of
	// its headers that are the rate limit headers Atlassian documents for
	// Jira Cloud (rateLimitHeaders): X-RateLimit-Limit,
	// X-RateLimit-Remaining, X-RateLimit-Reset, X-RateLimit-NearLimit,
	// RateLimit-Reason, Retry-After, RateLimit, RateLimit-Policy,
	// Beta-RateLimit, Beta-RateLimit-Policy, Beta-Retry-After,
	// X-Beta-RateLimit-Limit, X-Beta-RateLimit-Remaining,
	// X-Beta-RateLimit-Reset, X-Beta-RateLimit-NearLimit and
	// X-Beta-RateLimit-Reason, each scrubbed of the credential. No other
	// header reaches it. The timing rules are trackerapi.Options.OnAnswer's.
	OnAnswer func(status int, header http.Header)
	// MaxImageBytes bounds the local images one write embeds, together,
	// which the write holds in memory from its plan's check to its
	// uploads; zero means 100 MiB. A file attached from a stream (Attach)
	// is not held, and this does not bound it.
	MaxImageBytes int64
}

// Client calls one Jira Cloud site. It is safe for concurrent use; Close
// releases it.
type Client struct {
	api       *trackerapi.Client
	maxImages int64

	// closedErr is the transport's refusal of a call on a closed client;
	// it is set before closed.
	closeOnce sync.Once
	closed    atomic.Bool
	closedErr error

	id      identity
	users   *cache[string, User]
	catalog *cache[struct{}, *Catalog]
	configs *cache[int64, BoardConfig]
	// createScreens are the create screens by project and issue type ID,
	// kept for createScreenAge.
	createScreens *cache[[2]string, Screen]
	// sprintLists are the boards' sprint listings, kept and dropped
	// with their configurations.
	sprintLists *cache[int64, SprintList]
	// me is who the credential belongs to (Me).
	me *cache[struct{}, User]
	// voterMisses are the poker voter lookups Jira withheld (PokerVoter),
	// kept as long as a user is.
	voterMisses *cache[string, error]
	// cloudID, poker and pokerTokens are planning poker's (poker.go):
	// the site's cloud ID, its add-on as the extension listing names it,
	// and the context tokens by issue ID.
	cloudID     *cache[struct{}, string]
	poker       *cache[struct{}, pokerApp]
	pokerTokens *cache[string, pokerToken]
}

// rateLimitHeaders are the rate limit headers Jira Cloud documents
// (developer.atlassian.com, Jira Cloud platform, Rate limiting): the
// current ones, the structured RateLimit and RateLimit-Policy with their
// Beta- forms, and the legacy beta ones. None holds a URL.
func rateLimitHeaders() []string {
	return []string{
		"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "X-RateLimit-NearLimit",
		"RateLimit-Reason", "Retry-After",
		"RateLimit", "RateLimit-Policy", "Beta-RateLimit", "Beta-RateLimit-Policy", "Beta-Retry-After",
		"X-Beta-RateLimit-Limit", "X-Beta-RateLimit-Remaining", "X-Beta-RateLimit-Reset",
		"X-Beta-RateLimit-NearLimit", "X-Beta-RateLimit-Reason",
	}
}

// New builds a Client and the trackerapi.Client under it. Nothing is
// sent until the first call. It refuses what trackerapi.New refuses and
// a negative MaxImageBytes.
func New(opts Options) (*Client, error) {
	if opts.MaxImageBytes < 0 {
		return nil, errors.New("jira: Options.MaxImageBytes is negative")
	}
	api, err := trackerapi.New(trackerapi.Options{
		Name:          "jira",
		BaseURL:       opts.BaseURL,
		Version:       opts.Version,
		Credentials:   opts.Credentials,
		Dialect:       dialect{},
		Proxy:         opts.Proxy,
		OnAnswer:      opts.OnAnswer,
		ReportHeaders: rateLimitHeaders(),
		ErrorHeaders:  []string{accountHeader},
	})
	if err != nil {
		return nil, fmt.Errorf("jira: %w", err)
	}
	c := &Client{
		api: api, maxImages: opts.MaxImageBytes,
		users:         newCache[string, User](userCacheSize, siteAge, nil, api.CredentialMark),
		voterMisses:   newCache[string, error](userCacheSize, siteAge, nil, api.CredentialMark),
		catalog:       newCache[struct{}, *Catalog](1, siteAge, nil, api.CredentialMark),
		configs:       newCache[int64, BoardConfig](boardConfigCacheSize, 0, cloneBoardConfig, api.CredentialMark),
		createScreens: newCache[[2]string, Screen](createScreenCacheSize, createScreenAge, cloneScreen, api.CredentialMark),
		sprintLists:   newCache[int64, SprintList](boardConfigCacheSize, 0, cloneSprintList, api.CredentialMark),
		me:            newCache[struct{}, User](1, 0, nil, api.CredentialMark),
		cloudID:       newCache[struct{}, string](1, 0, nil, api.CredentialMark),
		poker:         newCache[struct{}, pokerApp](1, siteAge, nil, api.CredentialMark),
		pokerTokens:   newCache[string, pokerToken](pokerTokenCacheSize, 0, nil, api.CredentialMark),
	}
	if c.maxImages == 0 {
		c.maxImages = defaultMaxImages
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
// hit included: a closed client and an ended context fail it, each with
// a *trackerapi.NotSentError, so a write it stops is known not sent and
// the context's error still matches errors.Is.
func (c *Client) admit(ctx context.Context) error {
	if c.closed.Load() {
		return c.closedErr
	}
	if err := ctx.Err(); err != nil {
		return &trackerapi.NotSentError{Err: err}
	}
	return nil
}

// ForgetCredential drops the credential, so the next call reads the
// source again, and detaches the sign-in checks made with it.
func (c *Client) ForgetCredential() {
	c.api.ForgetCredential()
	c.id.forget()
	c.me.clear()
}

// call sends r and decodes the answer into out (nil skips decoding)
// through decodeAnswer. A 404 is checked against the sign-in
// (identity.go), and an AuthError detaches the sign-in checks. A read
// composed of several requests runs them under trackerapi.Walk, which
// the transport holds every answer to.
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

// sendWrite is write for a write of body to path under apiPath whose
// answer is not decoded.
func (c *Client) sendWrite(ctx context.Context, method, path string, q url.Values, body any, want writeAnswer) error {
	_, err := c.write(ctx, trackerapi.Request{Method: method, Path: apiPath + path, Query: q, Body: body}, want, nil)
	return err
}

// explain turns a transport error into the client's: a 404 the
// credential caused becomes an AuthError. The error is judged by its own
// outcome, the outermost (see outcomeOf).
func (c *Client) explain(ctx context.Context, err error) error {
	switch e := outcomeOf(err).(type) {
	case *trackerapi.AuthError:
		c.id.forget()
		c.me.clear()
	case *trackerapi.StatusError:
		if e.Status == http.StatusNotFound {
			return c.checkNotFound(ctx, err)
		}
	}
	return err
}

// notSent is the client's refusal of its input before any request was
// sent; it matches trackerapi.ErrInvalidInput.
func notSent(format string, args ...any) error {
	return trackerapi.InvalidInput(fmt.Errorf("jira: "+format, args...))
}

// notSentErr marks err, a failure before a write was sent, as a
// *trackerapi.NotSentError, so the write reads as not applied while
// errors.As still finds the cause. A call that failed (failedCall: a read
// the write depends on, an ended context) is marked only that. Any other
// failure is the client refusing the write as given (its own check, a
// typed refusal such as *StaleError, *MatchError, *MissingFieldsError or
// *adf.LossError, a local file it cannot use) and also matches
// trackerapi.ErrInvalidInput. An error already marked is returned as it
// is.
func notSentErr(err error) error {
	switch _, marked := err.(*trackerapi.NotSentError); {
	case err == nil || marked:
		return err
	case failedCall(err):
		return &trackerapi.NotSentError{Err: err}
	}
	return trackerapi.InvalidInput(err)
}

// failedCall reports an error from a call that failed rather than from a
// refusal of the input: an ended context, the tracker's answer (an error
// status, a refused credential, a rate limit, an answer that did not read
// or was too large), a transient failure, or a call the transport did
// not make. An error that already holds a refusal of the input is not
// one.
func failedCall(err error) bool {
	if errors.Is(err, trackerapi.ErrInvalidInput) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return hasType[*trackerapi.NotSentError](err) || hasType[*trackerapi.StatusError](err) ||
		hasType[*trackerapi.AuthError](err) || hasType[*trackerapi.RateLimitedError](err) ||
		hasType[*trackerapi.TransientError](err) || hasType[*trackerapi.BodyTooLargeError](err) ||
		hasType[*AnswerError](err) || hasType[*UnconfirmedError](err) || hasType[*UnprovenError](err)
}

func hasType[T error](err error) bool {
	_, ok := errors.AsType[T](err)
	return ok
}

// checkIssue refuses ref unless it is an issue key or a numeric issue
// ID, so no caller text reaches a path as another resource.
func checkIssue(ref string) error {
	if IsIssueKey(ref) || isNumeric(ref) {
		return nil
	}
	return notSent("%q is not an issue key or ID", ref)
}

// checkProject refuses ref unless it is a project key or numeric ID.
func checkProject(ref string) error {
	if isKeyName(ref) || isNumeric(ref) {
		return nil
	}
	return notSent("%q is not a project key or ID", ref)
}

// checkID refuses id unless it is a numeric ID; what names it.
func checkID(what, id string) error {
	if isNumeric(id) {
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
	if isNumeric(ref) {
		q.Set("issueId", ref)
	} else {
		q.Set("issueKey", ref)
	}
	return nil
}
