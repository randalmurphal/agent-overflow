package cdpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/loopback"
)

// Target is one row of the debugger's target listing. Only the fields a
// selector reads are declared; the endpoint carries several more that
// nothing here decides anything from.
type Target struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Endpoint is a resolved debugger address: either an HTTP base to
// discover targets through, or a page WebSocket a caller already holds.
type Endpoint struct {
	// HTTPBase is the debugger's HTTP root ("http://127.0.0.1:9224"),
	// empty when WSURL was given directly.
	HTTPBase string
	// WSURL names a requested page, but Attach still rediscovers and verifies
	// it against the authenticated target listing.
	WSURL string
}

// ForRediscovery converts a page-specific websocket selector into its
// debugger HTTP base. A page reload mints a new websocket target, so callers
// that retain the old WSURL must rediscover by authenticated page identity.
func (e Endpoint) ForRediscovery() (Endpoint, error) {
	if e.HTTPBase != "" {
		return e, nil
	}
	if e.WSURL == "" {
		return Endpoint{}, fmt.Errorf("devtools endpoint has neither an HTTP base nor a page websocket")
	}
	base, err := debuggerHTTPBase(e.WSURL)
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{HTTPBase: base}, nil
}

// ParseEndpoint reads the four spellings an operator has: a bare port, a
// host:port, an http:// base, or a ws:// debugger URL.
//
// A bare port is the common one (the WebView2 shells publish 9224/9225),
// and it resolves to loopback rather than to a hostname: a debugger port
// is not something to reach across a network, and a typo that silently
// dialled one would be worse than a refusal.
func ParseEndpoint(spec string) (Endpoint, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Endpoint{}, fmt.Errorf("no devtools endpoint given")
	}
	switch {
	case strings.HasPrefix(spec, "ws://"), strings.HasPrefix(spec, "wss://"):
		return Endpoint{WSURL: spec}, nil
	case strings.HasPrefix(spec, "http://"), strings.HasPrefix(spec, "https://"):
		parsed, err := url.Parse(spec)
		if err != nil {
			return Endpoint{}, fmt.Errorf("parse devtools endpoint %q: %w", spec, err)
		}
		if parsed.Host == "" {
			return Endpoint{}, fmt.Errorf("devtools endpoint %q names no host", spec)
		}
		return Endpoint{HTTPBase: parsed.Scheme + "://" + parsed.Host}, nil
	}
	if port, err := parsePort(spec); err == nil {
		return Endpoint{HTTPBase: "http://127.0.0.1:" + strconv.Itoa(port)}, nil
	}
	host, port, err := net.SplitHostPort(spec)
	if err != nil || host == "" || port == "" {
		return Endpoint{}, fmt.Errorf(
			"cannot read %q as a devtools endpoint (want a port, host:port, http://host:port, or a ws:// debugger url)", spec)
	}
	if _, err := parsePort(port); err != nil {
		return Endpoint{}, fmt.Errorf("devtools endpoint %q names no valid port", spec)
	}
	return Endpoint{HTTPBase: "http://" + net.JoinHostPort(host, port)}, nil
}

func parsePort(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty port")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a port: %q", s)
		}
		n = n*10 + int(r-'0')
		if n > 65535 {
			return 0, fmt.Errorf("port out of range: %q", s)
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("port out of range: %q", s)
	}
	return n, nil
}

// Attach resolves an endpoint to one authenticated page and opens its
// debugger socket. Even an explicit ws:// URL is checked against the
// debugger's target listing. The URL is a selector, not proof that it names
// this harness instance.
func Attach(ctx context.Context, endpoint Endpoint, wantURL string) (*Conn, Target, error) {
	return AttachForPage(ctx, endpoint, wantURL, "")
}

// AttachForPage is Attach with an optional harness page identity. The page
// identity is carried in the target URL by the frontend bridge, so a shared
// browser endpoint can be selected without falling back to whichever tab the
// debugger lists first.
func AttachForPage(ctx context.Context, endpoint Endpoint, wantURL, pageID string) (*Conn, Target, error) {
	if endpoint.WSURL != "" {
		base, err := debuggerHTTPBase(endpoint.WSURL)
		if err != nil {
			return nil, Target{}, err
		}
		targets, err := ListTargets(ctx, base)
		if err != nil {
			return nil, Target{}, err
		}
		target, err := selectPageTarget(targets, wantURL, pageID)
		if err != nil {
			return nil, Target{}, err
		}
		if target.WebSocketDebuggerURL != endpoint.WSURL {
			return nil, Target{}, fmt.Errorf("explicit devtools page does not match the authenticated instance page: got %s, want %s", endpoint.WSURL, target.WebSocketDebuggerURL)
		}
		conn, err := Dial(ctx, target.WebSocketDebuggerURL)
		return conn, target, err
	}
	targets, err := ListTargets(ctx, endpoint.HTTPBase)
	if err != nil {
		return nil, Target{}, err
	}
	target, err := selectPageTarget(targets, wantURL, pageID)
	if err != nil {
		return nil, Target{}, err
	}
	conn, err := Dial(ctx, target.WebSocketDebuggerURL)
	if err != nil {
		return nil, target, err
	}
	return conn, target, nil
}

func debuggerHTTPBase(wsURL string) (string, error) {
	u, err := url.Parse(wsURL)
	if err != nil || u.Host == "" || (u.Scheme != "ws" && u.Scheme != "wss") {
		return "", fmt.Errorf("invalid explicit devtools page url %q", wsURL)
	}
	scheme := "http"
	if u.Scheme == "wss" {
		scheme = "https"
	}
	return scheme + "://" + u.Host, nil
}

// targetListTimeout bounds the discovery GET. Loopback, so anything
// slower is a wedged browser rather than a slow network.
const targetListTimeout = 10 * time.Second

// ListTargets reads the debugger's target listing.
func ListTargets(ctx context.Context, httpBase string) ([]Target, error) {
	listURL := strings.TrimSuffix(httpBase, "/") + "/json/list"
	ctx, cancel := context.WithTimeout(ctx, targetListTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", listURL, err)
	}
	defer resp.Body.Close()
	// Bounded: the listing is a handful of small objects, and an endpoint
	// that answers with something else entirely should not be read into
	// memory unbounded on the strength of its port number.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", listURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", listURL, strings.TrimSpace(string(body)))
	}
	var targets []Target
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, fmt.Errorf("decode %s: %w", listURL, err)
	}
	return targets, nil
}

// SelectPageTarget picks the page to attach to without a page identity.
//
// The rule is strict: an attachable page must be on the same origin as
// wantURL and carry the same authenticated page marker. There is no sole-page
// fallback. Anything else is an error listing the candidates — a browser
// with three tabs open must not be profiled at whichever one the listing put
// first.
func SelectPageTarget(targets []Target, wantURL string) (Target, error) {
	return selectPageTarget(targets, wantURL, "")
}

// SelectPageTargetForPage picks the page using the authenticated page marker
// and an optional exact frontend page identity.
func SelectPageTargetForPage(targets []Target, wantURL, pageID string) (Target, error) {
	return selectPageTarget(targets, wantURL, pageID)
}

func selectPageTarget(targets []Target, wantURL, pageID string) (Target, error) {
	var pages, detached []Target
	for _, t := range targets {
		if t.Type != "page" {
			continue
		}
		if t.WebSocketDebuggerURL == "" {
			// Already claimed by another debugger client, so there is no
			// socket to open. Kept for the error message: "no page target" is
			// the wrong diagnosis when the page is there and taken.
			detached = append(detached, t)
			continue
		}
		pages = append(pages, t)
	}
	if len(pages) == 0 {
		if len(detached) > 0 {
			return Target{}, fmt.Errorf(
				"every page target on this endpoint is already claimed by another debugger client (close the open DevTools window or the other tool)%s",
				renderCandidates(detached))
		}
		return Target{}, fmt.Errorf("no page target on this devtools endpoint (%d target(s) of other kinds)", len(targets))
	}

	if wantURL == "" {
		return Target{}, fmt.Errorf("cannot select a page without the authenticated instance url%s", renderCandidates(pages))
	}
	var matched []Target
	for _, t := range pages {
		if sameOrigin(t.URL, wantURL) && samePageMarker(t.URL, wantURL) && samePageID(t.URL, pageID) {
			matched = append(matched, t)
		}
	}
	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		return Target{}, fmt.Errorf("no page target on the authenticated instance origin and page marker %s%s", pageMarkerForURL(wantURL), renderCandidates(pages))
	default:
		return Target{}, fmt.Errorf("%d page targets match the authenticated instance origin and page marker; name one with a ws:// debugger url%s", len(matched), renderCandidates(matched))
	}
}

func renderCandidates(targets []Target) string {
	sorted := make([]Target, len(targets))
	copy(sorted, targets)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].URL < sorted[j].URL })
	var b strings.Builder
	b.WriteString(":")
	for _, t := range sorted {
		fmt.Fprintf(&b, "\n  %s  %s", t.URL, t.Title)
		if t.WebSocketDebuggerURL != "" {
			fmt.Fprintf(&b, "\n    %s", t.WebSocketDebuggerURL)
		}
	}
	return b.String()
}

// sameOrigin decides whether a target is showing the instance. Host
// equality is the primary answer; loopback spellings are reconciled
// because the page may have been opened as localhost against an instance
// that publishes 127.0.0.1 (and the token query string never matches).
func sameOrigin(targetURL, wantURL string) bool {
	a, errA := url.Parse(targetURL)
	b, errB := url.Parse(wantURL)
	if errA != nil || errB != nil || a.Host == "" || b.Host == "" {
		return false
	}
	if !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	if strings.EqualFold(a.Host, b.Host) {
		return true
	}
	if a.Port() == "" || a.Port() != b.Port() {
		return false
	}
	return loopback.EndpointHostname(a.Hostname()) && loopback.EndpointHostname(b.Hostname())
}

const pageMarkerQuery = "page"
const pageIDQuery = "pageId"

func samePageID(targetURL, wantPageID string) bool {
	if wantPageID == "" {
		return true
	}
	u, err := url.Parse(targetURL)
	return err == nil && u.Query().Get(pageIDQuery) == wantPageID
}

func pageMarkerForURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if marker := u.Query().Get(pageMarkerQuery); marker != "" {
		return marker
	}
	// Older harness URLs used the authenticated transport token as the page
	// marker. Keep that strict check for callers still holding such a URL;
	// new harness pages carry the dedicated `page` nonce.
	if marker := u.Query().Get("t"); marker != "" {
		return marker
	}
	return u.Query().Get("token")
}

func samePageMarker(targetURL, wantURL string) bool {
	a, errA := url.Parse(targetURL)
	if errA != nil {
		return false
	}
	marker := pageMarkerForURL(wantURL)
	if marker == "" {
		return false
	}
	actual := a.Query().Get(pageMarkerQuery)
	if actual == "" {
		actual = a.Query().Get("t")
	}
	if actual == "" {
		actual = a.Query().Get("token")
	}
	return actual == marker
}
