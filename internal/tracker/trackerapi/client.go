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
	// block or call Close. The header is a copy it may keep; it carries no
	// URL and no body.
	OnAnswer func(status int, header http.Header)
}

// Client calls one tracker endpoint. It is safe for concurrent use. Close
// releases it.
type Client struct {
	name        string
	userAgent   string
	base        *url.URL // ends in "/"
	origin      origin
	source      CredentialSource
	dialect     Dialect
	readTimeout time.Duration
	maxBody     int64

	// transport sends each hop as built: it follows no redirect and adds
	// no credential.
	transport http.RoundTripper
	closeIdle func()

	onAnswer func(status int, header http.Header)
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
	// epoch changes with ForgetCredential and Close; a read started under
	// an older epoch stores nothing.
	epoch uint64
}

// New builds a Client. Nothing is read or dialed until the first call.
// It refuses an empty Name or Version, a nil Credentials or Dialect, a
// BaseURL that is not https unless its host is a loopback IP address, and
// a negative or non-finite limit.
func New(opts Options) (*Client, error) {
	transport := &http.Transport{
		Proxy:               opts.Proxy,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
		MaxConnsPerHost:     maxConnsPerHost,
		MaxIdleConnsPerHost: maxConnsPerHost,
		IdleConnTimeout:     90 * time.Second,
	}
	c, err := newClient(opts, transport)
	if err != nil {
		return nil, err
	}
	c.closeIdle = transport.CloseIdleConnections
	return c, nil
}

// newClient builds a Client on transport, which every hop goes through.
func newClient(opts Options, transport http.RoundTripper) (*Client, error) {
	baseURL, err := checkOptions(opts)
	if err != nil {
		return nil, err
	}
	c := &Client{
		name:        opts.Name,
		userAgent:   "agent-overflow/" + opts.Version,
		base:        baseURL,
		origin:      originOf(baseURL),
		source:      opts.Credentials,
		dialect:     opts.Dialect,
		readTimeout: opts.ReadTimeout,
		maxBody:     opts.MaxBodyBytes,
		transport:   transport,
		closeIdle:   func() {},
		onAnswer:    opts.OnAnswer,
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
// flight, zeroes the credential and closes idle connections. Every later
// call fails with a *NotSentError. Close may be called more than once.
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
		c.closeIdle()
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
