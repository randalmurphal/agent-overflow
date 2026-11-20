// Package linear is the typed client for Linear's GraphQL API: teams,
// workflow states, cycles, users, labels, projects, issues, comments,
// relations, link attachments, history and manual ordering. The shared
// contract is docs/architecture/tracker-clients.md.
//
// Every GraphQL operation is a package-level document, sent with its
// values as GraphQL variables and never formatted into the text. The
// tests check each document against Linear's public schema
// (linearschema/schema.graphql, refreshed by `make linear-schema`).
//
// The package owns no state beyond a Client, reads no environment and
// logs nothing. It imports internal/tracker/trackerapi, which owns the
// HTTP transport: credentials, retries, the rate gate and the typed
// transport errors. A caller marks a person's own action with
// trackerapi.WithInteractive so it waits out a short rate limit.
package linear

import (
	"errors"
	"net/http"
	"net/url"
	"sync"

	"agent-overflow/internal/tracker/trackerapi"
)

// DefaultBaseURL is Linear's API origin. Requests go to BaseURL + "/graphql".
const DefaultBaseURL = "https://api.linear.app"

// Options configures a Client.
type Options struct {
	// BaseURL is the API origin; empty means DefaultBaseURL. Overridden
	// by tests with a loopback server, the only place http is accepted.
	BaseURL string
	// Version is the app version sent in the User-Agent. Required.
	Version string
	// Credentials supplies the key. A personal API key is presented bare
	// (trackerapi.Key); an OAuth access token with trackerapi.Bearer.
	// Required.
	Credentials trackerapi.CredentialSource
	// Proxy picks the proxy for a request; nil connects directly.
	Proxy func(*http.Request) (*url.URL, error)
	// Workspace is the URL key of the workspace the client is for
	// (linear.app/<key>). When set, no call is sent with a key until
	// Linear has said, asked with that key, that it belongs to this
	// workspace (workspace.go); empty binds the client to none.
	Workspace string
}

// Client calls one Linear workspace. It is safe for concurrent use.
// Close releases it.
type Client struct {
	http *trackerapi.Client
	// endpoint is the redacted GraphQL URL, for errors built here.
	endpoint string
	// bound checks each key against Options.Workspace; nil when unbound.
	bound *boundSource

	mu    sync.Mutex
	usage Usage // the most recent answer's budget headers
}

// New builds a Client. Nothing is sent until the first call. It refuses
// what trackerapi.New refuses: a missing Version or Credentials, and a
// BaseURL that is not https unless its host is a loopback IP address.
func New(opts Options) (*Client, error) {
	if opts.Workspace == "" {
		return newClient(opts, nil, nil)
	}
	if opts.Credentials == nil {
		return nil, errors.New("linear: Options.Credentials is required")
	}
	c, err := newClient(opts, nil, nil)
	if err != nil {
		return nil, err
	}
	// The client that checks a key answers to the same origin over c's
	// connections, so the check and the call it admits share one TLS
	// handshake; it records its answers' budget in c's Usage and is bound
	// to nothing.
	check := opts
	check.Workspace, check.Credentials, check.Proxy = "", &candidate{}, nil
	checker, err := newClient(check, c.observeAnswer, c.http)
	if err != nil {
		c.Close()
		return nil, err
	}
	c.bound.checker, c.bound.candidate = checker, check.Credentials.(*candidate)
	return c, nil
}

// newClient builds a Client; observe, when set, records its answers'
// budget headers in place of its own Usage, and peer, when set, is the
// transport whose connections it sends over (opts.Proxy then nil). With
// opts.Workspace set, its transport reads the key through a boundSource
// the caller completes.
func newClient(opts Options, observe func(int, http.Header), peer *trackerapi.Client) (*Client, error) {
	base := opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	c := &Client{endpoint: base}
	if observe == nil {
		observe = c.observeAnswer
	}
	source := opts.Credentials
	if opts.Workspace != "" {
		c.bound = &boundSource{want: opts.Workspace, inner: opts.Credentials}
		source = c.bound
	}
	topts := trackerapi.Options{
		Name:          "linear",
		BaseURL:       base,
		Version:       opts.Version,
		Credentials:   source,
		Dialect:       dialect{},
		Proxy:         opts.Proxy,
		OnAnswer:      observe,
		ReportHeaders: usageHeaders,
	}
	var hc *trackerapi.Client
	var err error
	if peer != nil {
		hc, err = trackerapi.NewSharing(peer, topts)
	} else {
		hc, err = trackerapi.New(topts)
	}
	if err != nil {
		return nil, err
	}
	c.http = hc
	if u, err := url.Parse(base); err == nil {
		u.Path = u.JoinPath(graphqlPath).Path
		c.endpoint = u.String()
	}
	return c, nil
}

// Close cancels the calls in flight and zeroes the held credential. Every
// later call fails with a *trackerapi.NotSentError. Close may be called
// more than once.
func (c *Client) Close() {
	c.http.Close()
	if c.bound != nil && c.bound.checker != nil {
		c.bound.checker.Close()
	}
}

// ForgetCredential drops the held key so the next call reads the
// credential source again, for a caller that knows the key changed. On a
// bound client the key read next is checked again, even one refused as
// another workspace's.
func (c *Client) ForgetCredential() {
	if c.bound != nil {
		c.bound.forget()
	}
	c.http.ForgetCredential()
}

// Usage is the rate limit budget as of the most recent answer, whichever
// call it answered and whether or not it succeeded: a 429 or another
// failed answer counts. Complexity is that answer's; each Budget is from
// the most recent answer that carried its headers, so an answer without
// them (an error page from a proxy) does not make a spent budget look
// unknown. A page carries its own answer's Usage. Zero before the first
// answer.
func (c *Client) Usage() Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usage
}

// observeAnswer is the transport's OnAnswer, told of every answer on the
// calling goroutine before its body is read. It is the only place the
// client's Usage is recorded. A call's own Usage is read from its own
// answer's headers, never from here, so concurrent calls cannot swap
// their pages' budgets.
func (c *Client) observeAnswer(_ int, h http.Header) {
	u := usageOf(h)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.usage.Complexity = u.Complexity
	if u.Requests.Known {
		c.usage.Requests = u.Requests
	}
	if u.Points.Known {
		c.usage.Points = u.Points
	}
}
