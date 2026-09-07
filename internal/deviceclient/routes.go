package deviceclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/computerroute"
)

// Match native route selection: allow DNS and a cold VPN path to establish.
// The first verified alternative wins immediately; this only bounds failures.
const routeProbeTimeout = 20 * time.Second

// A route and its verifier are immutable together. The carrier keeps its
// original target URL; this transport selects before sending, never retries a
// request that may already have crossed the wire, and rewrites only authority.
type dialRoute struct {
	computerroute.Route
	target    *url.URL
	transport http.RoundTripper
}

type routeSelection struct {
	done  chan struct{}
	route *dialRoute
	err   error
}

type routeTransport struct {
	owner        *Client
	original     *url.URL
	initial      *dialRoute
	mu           sync.Mutex
	current      *dialRoute
	alternatives []*dialRoute
	failed       bool
	selecting    *routeSelection
	lastError    error
	retryAt      time.Time
	revision     uint64
}

func (c *Client) installRoutes() {
	original, _ := url.Parse(c.base) // dialBase already validated it.
	initial := computerroute.Route{Endpoint: c.base, CertFingerprint: c.session.CertFingerprint}
	if normalized, err := computerroute.Normalize(initial); err == nil {
		initial = normalized
	}
	target, _ := url.Parse(initial.Endpoint)
	first := &dialRoute{Route: initial, target: target, transport: c.http.Transport}
	routes := &routeTransport{owner: c, original: original, initial: first, current: first}
	routes.update(computerroute.Merge(nil, c.session.Routes))
	for _, candidate := range routes.alternatives {
		if candidate.Endpoint == c.session.LastEndpoint && candidate != first {
			routes.current, routes.failed = candidate, true // verify a saved alternative on this boot.
			break
		}
	}
	c.routes = routes
	c.http.Transport = routes
}

func (t *routeTransport) update(routes []computerroute.Route) {
	t.mu.Lock()
	defer t.mu.Unlock()
	previous := t.alternatives
	reusable := append([]*dialRoute{t.initial, t.current}, previous...)
	var next []*dialRoute
	for _, route := range routes {
		var reused *dialRoute
		for _, old := range reusable {
			if old.Route == route {
				reused = old
				break
			}
		}
		if reused == nil {
			target, _ := url.Parse(route.Endpoint)
			reused = &dialRoute{Route: route, target: target, transport: NewPinnedTransport(route.CertFingerprint)}
		}
		next = append(next, reused)
	}
	if slices.Equal(previous, next) {
		return
	}
	t.alternatives = next
	t.revision++
	for _, route := range next {
		if route.Endpoint == t.current.Endpoint && route.CertFingerprint != t.current.CertFingerprint {
			t.failed = true
		}
	}
	for _, old := range previous {
		if old != t.initial && old != t.current && !slices.Contains(next, old) {
			closeIdleRoute(old)
		}
	}
	// Newly learned addresses can repair a previously exhausted route set.
	t.retryAt = time.Time{}
}

func closeIdleRoute(route *dialRoute) {
	if closer, ok := route.transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *routeTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	closeIdleRoute(t.initial)
	closeIdleRoute(t.current)
	for _, route := range t.alternatives {
		closeIdleRoute(route)
	}
}

func (t *routeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.User != nil || req.URL.Scheme != t.original.Scheme || !strings.EqualFold(req.URL.Host, t.original.Host) {
		return nil, errors.New("deviceclient: request does not belong to this computer")
	}
	route, err := t.choose(req.Context())
	if err != nil {
		return nil, err
	}
	t.owner.mu.Lock()
	retired := t.owner.retired
	t.owner.mu.Unlock()
	if retired {
		return nil, ErrNoSession
	}
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host, out.Host = route.target.Scheme, route.target.Host, ""
	response, err := route.transport.RoundTrip(out)
	// A proxy can serve HTTP while dropping the socket upgrade. Authentication
	// refusals still belong to renewal; other failed upgrades invalidate this
	// route so a reconnect can use a healthy listener for the same computer.
	badUpgrade := response != nil && strings.EqualFold(req.Header.Get("Upgrade"), "websocket") &&
		response.StatusCode != http.StatusSwitchingProtocols && response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden
	if (err != nil && !errors.Is(req.Context().Err(), context.Canceled)) || (response != nil && response.StatusCode >= 500) || badUpgrade {
		t.mu.Lock()
		if t.current == route {
			t.failed = true
		}
		t.mu.Unlock()
	}
	return response, err
}

func (t *routeTransport) choose(ctx context.Context) (*dialRoute, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	// Legacy single-endpoint clients keep their existing request behavior.
	if !t.failed || len(t.alternatives) == 0 {
		route := t.current
		t.mu.Unlock()
		return route, nil
	}
	flight := t.selecting
	if flight == nil {
		if time.Now().Before(t.retryAt) {
			err := t.lastError
			t.mu.Unlock()
			return nil, err
		}
		flight = &routeSelection{done: make(chan struct{})}
		t.selecting = flight
		current := t.current
		candidates := append([]*dialRoute(nil), t.alternatives...)
		// An advertised trust update for the original origin takes precedence.
		if !slices.ContainsFunc(candidates, func(route *dialRoute) bool { return route.Endpoint == t.initial.Endpoint }) {
			candidates = append(candidates, t.initial)
		}
		go t.selectRoute(flight, candidates, current, t.revision)
	}
	t.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		return flight.route, flight.err
	}
}

func (t *routeTransport) selectRoute(flight *routeSelection, candidates []*dialRoute, failed *dialRoute, revision uint64) {
	// A cancelled waiter cannot cancel another request's selection. Work
	// remains bounded to five credential-free probes and one connection deadline.
	ctx, cancel := context.WithTimeout(context.Background(), routeProbeTimeout)
	defer cancel()
	t.owner.mu.Lock()
	backendID := t.owner.session.BackendID
	t.owner.mu.Unlock()
	results := make(chan *dialRoute, len(candidates))
	for _, candidate := range candidates {
		go func() {
			if verifyComputerRoute(ctx, candidate, backendID) != nil {
				results <- nil
			} else {
				results <- candidate
			}
		}()
	}
	var fallback *dialRoute
	for remaining := len(candidates); remaining > 0; remaining-- {
		select {
		case route := <-results:
			if route == failed {
				fallback = route
			} else if route != nil {
				flight.route = route
			}
		case <-ctx.Done():
			remaining = 1
		}
		if flight.route != nil {
			break
		}
	}
	if flight.route == nil {
		flight.route = fallback
	}
	if flight.route == nil {
		flight.err = errors.New("deviceclient: no verified route to this computer is reachable")
	}
	t.mu.Lock()
	if t.revision != revision {
		flight.route, flight.err = nil, errors.New("deviceclient: computer routes changed while verifying; retry the connection")
	}
	if flight.route != nil {
		old := t.current
		t.current, t.failed = flight.route, false
		if old != t.initial && !slices.Contains(t.alternatives, old) {
			closeIdleRoute(old)
		}
	} else {
		t.lastError, t.retryAt = flight.err, time.Now().Add(250*time.Millisecond)
	}
	t.mu.Unlock()
	if flight.route != nil {
		// Remember only this installation's route choice. Persistence failure
		// cannot turn a verified connection into a pairing refusal; the trusted
		// candidates were already saved when their manifest was accepted.
		_ = t.owner.sessionTransaction(ctx, func(path string, latest *Session) error {
			if latest.LastEndpoint == flight.route.Endpoint {
				return nil
			}
			latest.LastEndpoint = flight.route.Endpoint
			return writeSession(path, *latest)
		})
	}
	t.mu.Lock()
	t.selecting = nil
	close(flight.done)
	t.mu.Unlock()
}

func verifyComputerRoute(ctx context.Context, route *dialRoute, backendID string) error {
	if backendID == "" {
		return errors.New("computer identity is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, route.Endpoint+"/healthz", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: route.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("computer health answered HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil {
		return err
	}
	if len(body) > 64*1024 {
		return errors.New("computer health response is too large")
	}
	var health struct {
		BackendID string `json:"backendId"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		return err
	}
	if health.BackendID != backendID {
		return errors.New("route belongs to a different computer")
	}
	return nil
}

// ObserveBootstrap learns alternatives only from the authenticated, pinned
// manifest fetch. It preserves routes omitted by an older host.
func (c *Client) ObserveBootstrap(ctx context.Context, body []byte) error {
	var manifest struct {
		BackendID string                `json:"backendId"`
		Routes    []computerroute.Route `json:"routes"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return err
	}
	c.mu.Lock()
	backendID := c.session.BackendID
	c.mu.Unlock()
	if manifest.BackendID != backendID {
		return errors.New("deviceclient: manifest belongs to a different computer")
	}
	if len(manifest.Routes) == 0 {
		return nil
	}
	var routes []computerroute.Route
	if err := c.sessionTransaction(ctx, func(path string, latest *Session) error {
		routes = computerroute.Merge(latest.Routes, manifest.Routes)
		if slices.Equal(routes, latest.Routes) {
			return nil
		}
		latest.Routes = routes
		return writeSession(path, *latest)
	}); err != nil {
		return err
	}
	c.routes.update(routes)
	return nil
}
