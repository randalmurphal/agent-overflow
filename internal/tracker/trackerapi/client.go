// Package trackerapi is the HTTP transport under the tracker clients:
// credentials, retries, the rate gate, typed errors, uploads and
// downloads. The contract is docs/architecture/tracker-clients.md.
//
// The package logs nothing: every failure is returned to the caller.
package trackerapi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultReadTimeout  = 45 * time.Second
	defaultMaxBodyBytes = 8 << 20
	// maxRetries bounds the retries of one call after a connection
	// failure, a 429 or a 503.
	maxRetries = 4
	// maxRetryWait caps one wait between attempts.
	maxRetryWait = 60 * time.Second
	// interactiveGateWait is the longest an interactive call waits for a
	// closed rate gate; a longer closure fails it as it fails a background
	// call.
	interactiveGateWait = 10 * time.Second
	// credentialReadTimeout bounds one shared credential read.
	credentialReadTimeout = time.Minute
	maxRedirects          = 10
	maxConnsPerHost       = 8
)

// Options configures a Client.
type Options struct {
	// Name names the client in errors: "jira", "tempo", "linear".
	// Required.
	Name string
	// BaseURL is the root every relative Request.Path resolves against.
	// It is https, or http only on a loopback IP address, and carries no
	// user info, query or fragment.
	BaseURL string
	// Version is sent as User-Agent "agent-overflow/<version>". Required.
	Version string
	// Credentials is read at first use and again after one 401. Required.
	Credentials CredentialSource
	// Dialect reads the tracker's own error, login and limit conventions.
	// Required; PlainDialect reads none.
	Dialect Dialect
	// MaxPerSecond spaces every request of this client, retries included,
	// at most this many a second. Zero does not pace. A rate whose spacing
	// is under a nanosecond or over a time.Duration is refused.
	MaxPerSecond float64
	// ReadTimeout bounds a call whose Request.Timeout is zero, retries and
	// waits included; zero means 45s. A Transfer call has no total bound.
	ReadTimeout time.Duration
	// MaxBodyBytes bounds the answer body Do and JSON read when the
	// request names no bound of its own; zero means 8 MiB.
	MaxBodyBytes int64
	// Proxy picks the proxy for a request; nil connects directly. The
	// package reads no environment, so a caller that honors the user's
	// proxy settings passes http.ProxyFromEnvironment.
	Proxy func(*http.Request) (*url.URL, error)
	// OnAnswer, when set, is told the status and headers of every answer
	// whose headers arrive: on every redirect hop and every attempt,
	// success or error, before its body is read and before the call
	// settles. It is not called when nothing answered, and never after
	// Close returns. It runs on the calling goroutine, so it must not
	// block or call Close. The header holds only the headers ReportHeaders
	// names, in a map it may keep, with the credential the request carried
	// replaced by secret.Redacted in every value. Nothing else of the
	// answer reaches it: no other header, no URL and no body.
	OnAnswer func(status int, header http.Header)
	// ReportHeaders names the answer headers OnAnswer receives, such as a
	// tracker's rate budget headers. A header that can hold a URL does not
	// belong here.
	ReportHeaders []string
	// ErrorHeaders names the answer headers a StatusError keeps in its
	// Header, such as one that names who the answer was made for. The
	// rule for ReportHeaders holds: none that can hold a URL.
	ErrorHeaders []string
}

// Client calls one tracker endpoint. It is safe for concurrent use. Close
// releases it.
type Client struct {
	name        string
	userAgent   []string // the User-Agent header value every request shares
	base        *url.URL // ends in "/"
	origin      origin
	source      CredentialSource
	dialect     Dialect
	readTimeout time.Duration
	maxBody     int64

	// conns sends each hop as built: it follows no redirect and adds no
	// credential. NewSharing hands it to another client.
	conns *connections

	onAnswer func(status int, header http.Header)
	reported []string // canonical names of the headers OnAnswer receives
	kept     []string // canonical names of the headers a StatusError keeps
	// reportMu is held for reading through each OnAnswer call; Close
	// takes it to set reportsOff, so no report starts or runs after it.
	reportMu   sync.RWMutex
	reportsOff bool

	gate gate
	pace *pacer
	// now and sleep are replaced only by tests, before first use.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	// life ends at Close: it cancels the calls in flight and the shared
	// credential read, which reads joins.
	life      context.Context
	stop      context.CancelFunc
	reads     sync.WaitGroup
	closeOnce sync.Once
	mu        sync.Mutex // guards closed against a read starting
	closed    bool

	// readMu guards reading, the one credential read in flight.
	readMu  sync.Mutex
	reading *credentialRead

	// credMu guards the credential. A request's header is written under
	// the read lock and the credential is zeroed under the write lock, so
	// no request sends zeroed bytes.
	credMu sync.RWMutex
	cred   *Credential
	// credGen counts completed reads. A request records the generation it
	// sent, so a 401 reads again only when no read completed since.
	credGen uint64
	// credRead is the generation of the read that stored cred. A re-read
	// that returns an equal credential keeps cred and its credRead.
	credRead uint64
	// epoch changes with ForgetCredential and Close; a read started under
	// an older epoch stores nothing.
	epoch uint64

	// tag tells this client's credential marks from another client's.
	tag *clientTag
}

// New builds a Client. Nothing is read or dialed until the first call.
// It refuses an empty Name or Version, a nil Credentials or Dialect, a
// BaseURL that is not https unless its host is a loopback IP address, and
// a negative or non-finite limit.
func New(opts Options) (*Client, error) {
	return newClientOn(opts, ownConnections(newTransport(opts.Proxy)))
}

// NewSharing builds a Client from opts, as New does, that sends its
// requests over peer's connections: one pool of connections and TLS
// sessions for two clients of one owner that call one origin, such as a
// client and the client that checks a key for it. Everything else is its
// own: its credentials and their marks, pacing, rate gate, OnAnswer,
// timeouts and body bound. Closing either client ends only its own calls
// and leaves the other working; the idle connections close when the last
// client sharing them closes. It refuses what New refuses, a BaseURL on
// an origin other than peer's, a Proxy (the connections take peer's), and
// a peer that is closed.
func NewSharing(peer *Client, opts Options) (*Client, error) {
	switch {
	case peer == nil:
		return nil, errors.New("trackerapi: NewSharing needs a peer")
	case opts.Proxy != nil:
		return nil, errors.New("trackerapi: a client that shares connections takes its peer's proxy; Options.Proxy must be nil")
	}
	base, err := checkOptions(opts)
	if err != nil {
		return nil, err
	}
	if originOf(base) != peer.origin {
		return nil, fmt.Errorf("trackerapi: %s shares connections only on its peer's origin, not %s", opts.Name, redact(base))
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	// An open peer holds its own share, so the connections are open too.
	if peer.closed {
		return nil, fmt.Errorf("trackerapi: %s: the peer whose connections it would share is closed", opts.Name)
	}
	peer.conns.join()
	c, err := newClientOn(opts, peer.conns)
	if err != nil {
		peer.conns.leave()
		return nil, err
	}
	return c, nil
}

// tlsSessions bounds a transport's TLS session cache, one session per
// host: the base origin and the hosts its redirects name.
const tlsSessions = 4

// newTransport is the transport New gives a client. Its session cache
// lets a connection made after the idle close resume its TLS session
// instead of a full handshake.
func newTransport(proxy func(*http.Request) (*url.URL, error)) *http.Transport {
	return &http.Transport{
		Proxy:       proxy,
		DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tls.NewLRUClientSessionCache(tlsSessions),
		},
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
		MaxConnsPerHost:     maxConnsPerHost,
		MaxIdleConnsPerHost: maxConnsPerHost,
		IdleConnTimeout:     90 * time.Second,
	}
}

// connections is a transport and the count of open clients that send
// through it. The last to leave closes its idle connections.
type connections struct {
	transport http.RoundTripper
	closeIdle func()
	mu        sync.Mutex
	users     int
}

// ownConnections is t for one client.
func ownConnections(t *http.Transport) *connections {
	return &connections{transport: t, closeIdle: t.CloseIdleConnections, users: 1}
}

// join adds a client. The caller holds an open client that shares them.
func (p *connections) join() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.users++
}

// leave removes a client and closes the idle connections after the last.
func (p *connections) leave() {
	p.mu.Lock()
	p.users--
	last := p.users == 0
	p.mu.Unlock()
	if last {
		p.closeIdle()
	}
}

// newClient builds a Client on transport, which every hop goes through,
// closing nothing at Close.
func newClient(opts Options, transport http.RoundTripper) (*Client, error) {
	return newClientOn(opts, &connections{transport: transport, closeIdle: func() {}, users: 1})
}

// newClientOn builds a Client on conns, which it leaves at Close.
func newClientOn(opts Options, conns *connections) (*Client, error) {
	baseURL, err := checkOptions(opts)
	if err != nil {
		return nil, err
	}
	c := &Client{
		name:        opts.Name,
		userAgent:   []string{"agent-overflow/" + opts.Version},
		base:        baseURL,
		origin:      originOf(baseURL),
		source:      opts.Credentials,
		dialect:     opts.Dialect,
		readTimeout: opts.ReadTimeout,
		maxBody:     opts.MaxBodyBytes,
		conns:       conns,
		onAnswer:    opts.OnAnswer,
		reported:    canonicalNames(opts.ReportHeaders),
		kept:        canonicalNames(opts.ErrorHeaders),
		tag:         &clientTag{},
		now:         time.Now,
		sleep:       sleep,
	}
	if c.readTimeout == 0 {
		c.readTimeout = defaultReadTimeout
	}
	if c.maxBody == 0 {
		c.maxBody = defaultMaxBodyBytes
	}
	if opts.MaxPerSecond > 0 {
		c.pace = newPacer(paceInterval(opts.MaxPerSecond))
	}
	c.life, c.stop = context.WithCancel(context.Background())
	return c, nil
}

// paceInterval is the spacing that rate requests a second take, or zero
// when it is under a nanosecond or does not fit a time.Duration.
func paceInterval(rate float64) time.Duration {
	interval := float64(time.Second) / rate
	if interval >= math.MaxInt64 {
		return 0
	}
	return time.Duration(interval)
}

func checkOptions(opts Options) (*url.URL, error) {
	switch {
	case opts.Name == "":
		return nil, errors.New("trackerapi: Options.Name is required")
	case opts.Version == "":
		return nil, errors.New("trackerapi: Options.Version is required for the User-Agent")
	case opts.Credentials == nil:
		return nil, errors.New("trackerapi: Options.Credentials is required")
	case opts.Dialect == nil:
		return nil, errors.New("trackerapi: Options.Dialect is required")
	case opts.MaxPerSecond < 0 || math.IsNaN(opts.MaxPerSecond) || math.IsInf(opts.MaxPerSecond, 0):
		return nil, fmt.Errorf("trackerapi: Options.MaxPerSecond %v is not a finite rate of zero or more", opts.MaxPerSecond)
	case opts.MaxPerSecond > 0 && paceInterval(opts.MaxPerSecond) == 0:
		return nil, fmt.Errorf("trackerapi: Options.MaxPerSecond %v spaces requests by less than a nanosecond or more than a time.Duration holds", opts.MaxPerSecond)
	case opts.ReadTimeout < 0:
		return nil, errors.New("trackerapi: Options.ReadTimeout is negative")
	case opts.MaxBodyBytes < 0:
		return nil, errors.New("trackerapi: Options.MaxBodyBytes is negative")
	}
	base, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("trackerapi: Options.BaseURL is not a URL: %w", parseCause(err))
	}
	switch {
	case !base.IsAbs() || base.Host == "":
		return nil, errors.New("trackerapi: Options.BaseURL is not an absolute URL")
	case base.User != nil:
		return nil, errors.New("trackerapi: Options.BaseURL carries user info; credentials come from Options.Credentials")
	case base.RawQuery != "" || base.ForceQuery || base.Fragment != "":
		return nil, errors.New("trackerapi: Options.BaseURL carries a query or fragment")
	}
	if err := allowedURL(base); err != nil {
		return nil, fmt.Errorf("trackerapi: Options.BaseURL: %w", err)
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
		if base.RawPath != "" {
			base.RawPath += "/"
		}
	}
	return base, nil
}

// ForgetCredential zeroes and drops the credential, so the next call reads
// the source again. A read in flight stores nothing. For a caller that
// knows the credential changed.
func (c *Client) ForgetCredential() {
	c.credMu.Lock()
	defer c.credMu.Unlock()
	c.cred.Zero()
	c.cred = nil
	c.epoch++
}

// Close cancels the calls in flight, ends and joins the credential read in
// flight, zeroes the credential and closes idle connections, unless a
// client made by NewSharing still sends over them. Every later call fails
// with a *NotSentError. Close may be called more than once.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.reportMu.Lock()
		c.reportsOff = true
		c.reportMu.Unlock()
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.stop()
		c.reads.Wait()
		c.ForgetCredential()
		c.conns.leave()
	})
}

// startRead runs read on the client's lifetime, or reports false after
// Close.
func (c *Client) startRead(read func()) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.reads.Go(read)
	return true
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func canonicalNames(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = http.CanonicalHeaderKey(name)
	}
	return out
}
