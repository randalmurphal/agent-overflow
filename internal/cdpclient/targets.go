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
	// WSURL skips discovery entirely.
	WSURL string
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

// Attach resolves an endpoint to one page and opens its debugger socket.
// A ws:// endpoint skips discovery: the caller already named the target,
// and asking the browser to confirm it would only add a way to fail.
func Attach(ctx context.Context, endpoint Endpoint, wantURL string) (*Conn, Target, error) {
	if endpoint.WSURL != "" {
		conn, err := Dial(ctx, endpoint.WSURL)
		return conn, Target{WebSocketDebuggerURL: endpoint.WSURL}, err
	}
	targets, err := ListTargets(ctx, endpoint.HTTPBase)
	if err != nil {
		return nil, Target{}, err
	}
	target, err := SelectPageTarget(targets, wantURL)
	if err != nil {
		return nil, Target{}, err
	}
	conn, err := Dial(ctx, target.WebSocketDebuggerURL)
	if err != nil {
		return nil, target, err
	}
	return conn, target, nil
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

// SelectPageTarget picks the page to attach to.
//
// The rule, in order: page targets that are actually attachable; then the
// one whose URL sits on the same origin as wantURL; then, if wantURL
// matched nothing (or was not given), the ONLY page. Anything else is an
// error listing the candidates — a browser with three tabs open must not
// be profiled at whichever one the listing happened to put first.
func SelectPageTarget(targets []Target, wantURL string) (Target, error) {
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

	if wantURL != "" {
		var matched []Target
		for _, t := range pages {
			if sameOrigin(t.URL, wantURL) {
				matched = append(matched, t)
			}
		}
		switch len(matched) {
		case 1:
			return matched[0], nil
		case 0:
			// Fall through to the single-page rule: a page can be showing
			// something else entirely (a blank tab mid-navigation) and still
			// be the only thing here.
		default:
			return Target{}, fmt.Errorf(
				"%d page targets are on %s; name one with a ws:// debugger url%s",
				len(matched), wantURL, renderCandidates(matched))
		}
	}

	if len(pages) == 1 {
		return pages[0], nil
	}
	return Target{}, fmt.Errorf(
		"%d page targets on this devtools endpoint and none is on %s; name one with a ws:// debugger url%s",
		len(pages), orAny(wantURL), renderCandidates(pages))
}

func orAny(wantURL string) string {
	if wantURL == "" {
		return "a known url"
	}
	return wantURL
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
	if strings.EqualFold(a.Host, b.Host) {
		return true
	}
	if a.Port() == "" || a.Port() != b.Port() {
		return false
	}
	return isLoopbackHost(a.Hostname()) && isLoopbackHost(b.Hostname())
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
