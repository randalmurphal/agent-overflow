package forgeapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Defaults from docs/architecture/forge-transport.md#requests.
const (
	defaultReadTimeout  = 45 * time.Second
	defaultMaxBodyBytes = 1 << 20
	hostConcurrency     = 4
	tokenRereadAfter    = 5 * time.Minute
	tokenFailureTTL     = 5 * time.Second
	githubAPIVersion    = "2022-11-28"
	publicGitHubHost    = "github.com"
)

// Options configures a Service.
type Options struct {
	// Version is the build version sent as User-Agent
	// "agent-overflow/<version>"; empty sends "dev".
	Version string
	// TokenSource reads each host's token: CLITokenSource in production.
	// Must be nil when Isolated is set.
	TokenSource TokenSource
	// Isolated points every host at a fake forge with a fixed token, so an
	// isolated boot cannot reach a real forge by construction.
	Isolated *Isolated
	// ReadTimeout bounds a call whose Request.Timeout is zero; zero means
	// 45s.
	ReadTimeout time.Duration
	// MaxBodyBytes bounds a body Do, JSON and GraphQL read; zero means
	// 1 MiB.
	MaxBodyBytes int64
}

// Isolated is the fake forge an isolated boot talks to. Every host's REST
// base becomes BaseURL/github/rest/ or BaseURL/gitlab/api/v4/ and the
// GitHub GraphQL URL BaseURL/github/graphql; a request carries the forge
// host it is for as its Host header. An attachment's absolute URL becomes
// BaseURL/{forge}/absolute/<host>/<path>. Token is sent on every request.
// An empty BaseURL refuses every request.
type Isolated struct {
	BaseURL string
	Token   string
}

// errNoFakeForge is a request in an isolated boot that has no fake forge.
var errNoFakeForge = errors.New("forge API is disabled in this isolated boot: no fake forge is configured")

// Service is the forge API transport of one process: one http.Transport,
// a Client per (forge, host) with its credential and concurrency slots,
// the rate gates and the ETag store. Safe for concurrent use.
type Service struct {
	opts      Options
	source    TokenSource
	isolated  *url.URL
	userAgent string

	transport *http.Transport
	follow    *http.Client
	noFollow  *http.Client
	gates     *gates
	etags     *etagStore

	// now and resolveBase are replaced only by tests, before first use.
	now         func() time.Time
	resolveBase func(forge, host string, info SourceInfo) (rest, graphQL *url.URL, err error)

	mu      sync.Mutex
	closed  bool
	clients map[clientKey]*Client
}

type clientKey struct{ forge, host string }

// New builds a Service. Nothing is read or dialed until the first request.
// It refuses a configuration that could send a real login to a fake or
// reach a real forge from an isolated boot: Isolated together with a
// TokenSource, an Isolated without a BaseURL, no TokenSource without
// Isolated, and an empty Version.
func New(opts Options) (*Service, error) {
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = defaultReadTimeout
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = defaultMaxBodyBytes
	}
	if opts.Version == "" {
		return nil, errors.New("forgeapi: Options.Version is required for the User-Agent")
	}
	s := &Service{
		opts:      opts,
		source:    opts.TokenSource,
		userAgent: "agent-overflow/" + opts.Version,
		gates:     newGates(),
		etags:     newETagStore(),
		now:       time.Now,
		clients:   make(map[clientKey]*Client),
	}
	s.resolveBase = s.defaultBase
	switch {
	case opts.Isolated != nil && opts.TokenSource != nil:
		return nil, errors.New("forgeapi: an isolated Service cannot take a real token source")
	case opts.Isolated != nil && opts.Isolated.BaseURL == "":
		return nil, errNoFakeForge
	case opts.Isolated != nil:
		base, err := url.Parse(strings.TrimRight(opts.Isolated.BaseURL, "/"))
		if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
			return nil, fmt.Errorf("forgeapi: isolated base URL %q is not an http(s) URL", opts.Isolated.BaseURL)
		}
		s.isolated = base
		s.source = fixedSource{token: opts.Isolated.Token}
	case opts.TokenSource == nil:
		return nil, errors.New("forgeapi: no token source")
	}
	s.transport = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxConnsPerHost:       hostConcurrency,
		MaxIdleConnsPerHost:   hostConcurrency,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	authorizing := authTransport{base: s.transport}
	s.follow = &http.Client{Transport: authorizing, CheckRedirect: dropCrossHostAuth}
	s.noFollow = &http.Client{Transport: authorizing, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return s, nil
}

// Isolated reports whether the Service talks to a fake forge
// (Options.Isolated) rather than real forges.
func (s *Service) Isolated() bool { return s.isolated != nil }

// dropCrossHostAuth is the followed-redirect policy: at most 10 hops, and
// no forge credential header on a hop to another host than the request's
// (a GitHub job log redirects to signed blob storage). authTransport never
// puts the token on the request http.Client copies headers from, so this
// is defense in depth.
func dropCrossHostAuth(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		req.Header.Del("Authorization")
		req.Header.Del("Private-Token")
	}
	return nil
}

// GitHub returns the client for a GitHub host, spelled as
// PRReference.Host.
func (s *Service) GitHub(host string) *Client { return s.client(ForgeGitHub, host) }

// GitLab returns the client for a GitLab host, spelled as
// PRReference.Host.
func (s *Service) GitLab(host string) *Client { return s.client(ForgeGitLab, host) }

func (s *Service) client(forge, host string) *Client {
	host = strings.ToLower(host)
	key := clientKey{forge: forge, host: host}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.clients[key]; c != nil {
		return c
	}
	c := &Client{svc: s, forge: forge, host: host, slots: make(chan struct{}, hostConcurrency), readLock: make(chan struct{}, 1)}
	if err := validHost(host); err != nil {
		c.err = err
	}
	if !s.closed {
		s.clients[key] = c
	}
	return c
}

// validHost accepts a URL host: a name or address with an optional port,
// nothing else.
func validHost(host string) error {
	if host == "" {
		return errors.New("forgeapi: empty forge host")
	}
	u, err := url.Parse("https://" + host + "/")
	if err != nil || u.Host != host || u.User != nil || u.Hostname() == "" {
		return fmt.Errorf("forgeapi: %q is not a forge host", host)
	}
	return nil
}

// DropNegative forgets a failed token read for host, so the next request
// reads the login again at once. Every interactive request runs it before
// it reads the credential (Client.credential), so a person's Retry after
// `gh auth login` needs no caller to remember it.
func (s *Service) DropNegative(host string) {
	host = strings.ToLower(host)
	s.mu.Lock()
	clients := make([]*Client, 0, 2)
	for key, c := range s.clients {
		if key.host == host {
			clients = append(clients, c)
		}
	}
	s.mu.Unlock()
	for _, c := range clients {
		c.credMu.Lock()
		c.failErr = nil
		c.credMu.Unlock()
	}
}

// Close zeroes every token, drops the gates, quota and ETag entries and
// closes idle connections. A request after Close fails.
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	clients := s.clients
	s.clients = make(map[clientKey]*Client)
	s.mu.Unlock()
	for _, c := range clients {
		c.credMu.Lock()
		c.token.Zero()
		c.token = nil
		c.fp = ""
		c.credMu.Unlock()
	}
	s.gates.clear()
	s.etags.clear()
	s.transport.CloseIdleConnections()
}

func (s *Service) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// defaultBase resolves a host's REST base and GraphQL URL (nil for
// GitLab) per the table in docs/architecture/forge-transport.md.
func (s *Service) defaultBase(forge, host string, info SourceInfo) (*url.URL, *url.URL, error) {
	if s.isolated != nil {
		switch forge {
		case ForgeGitHub:
			return s.isolated.JoinPath("github", "rest/"), s.isolated.JoinPath("github", "graphql"), nil
		default:
			return s.isolated.JoinPath("gitlab", "api", "v4/"), nil, nil
		}
	}
	switch forge {
	case ForgeGitHub:
		if host == publicGitHubHost {
			return &url.URL{Scheme: "https", Host: "api.github.com", Path: "/"}, &url.URL{Scheme: "https", Host: "api.github.com", Path: "/graphql"}, nil
		}
		return &url.URL{Scheme: "https", Host: host, Path: "/api/v3/"}, &url.URL{Scheme: "https", Host: host, Path: "/api/graphql"}, nil
	default:
		scheme, apiHost := info.APIProtocol, info.APIHost
		if scheme == "" {
			scheme = "https"
		}
		if apiHost == "" {
			apiHost = host
		}
		if scheme != "https" && scheme != "http" {
			return nil, nil, fmt.Errorf("forgeapi: glab api_protocol %q for %s is not http or https", scheme, host)
		}
		if err := validHost(strings.ToLower(apiHost)); err != nil {
			return nil, nil, fmt.Errorf("forgeapi: glab api_host for %s: %w", host, err)
		}
		return &url.URL{Scheme: scheme, Host: strings.ToLower(apiHost), Path: "/api/v4/"}, nil, nil
	}
}

// fixedSource is the isolated boot's token source: the fake's fixed token,
// presented the way a personal token is on each forge.
type fixedSource struct{ token string }

func (f fixedSource) Token(_ context.Context, forge, _ string) ([]byte, SourceInfo, error) {
	if f.token == "" {
		return nil, SourceInfo{}, errors.New("forgeapi: isolated boot has no fake forge token")
	}
	info := SourceInfo{Header: AuthBearer}
	if forge == ForgeGitLab {
		info.Header = AuthPrivateToken
	}
	return []byte(f.token), info, nil
}
