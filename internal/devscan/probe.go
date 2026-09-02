package devscan

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/loopback"
)

// The candidate probe: does this port answer like a page?
//
// An open port proves nothing. A language server's RPC socket, a debug
// listener, a database — all of them are LISTEN on loopback, and offering
// a "preview" of one would be worse than offering nothing. So one bounded
// GET decides, and the verdict is the same shape t3code shipped: HTML, or
// a redirect that names where the HTML is.
//
// The dial goes through loopback.Dialer, which resolves `localhost`
// statically to 127.0.0.1 and ::1 and never asks a resolver. The reasons
// live there; the one that matters here is that a verdict must not be
// steerable by a resolver answer.
const (
	// probeTimeout bounds one candidate. Loopback answers in
	// milliseconds, so a second is generous; what it really bounds is a
	// port that accepts and then says nothing, which would otherwise hold
	// the scan.
	probeTimeout = time.Second

	// probeVerdictTTL is how long one verdict is reused. Above the 3s
	// scan cadence so a steady state costs no dials at all, and short
	// enough that a dev server that just started is noticed within a few
	// ticks.
	probeVerdictTTL = 15 * time.Second

	// probeBodyLimit is how much of a response body is read before the
	// connection is dropped. Nothing here parses a body — the verdict is
	// the status and the content type — but a body left unread means the
	// connection cannot be reused, so a small drain is cheaper than the
	// alternative.
	probeBodyLimit = 4 << 10

	// maxVerdicts bounds the memo's memory. Keys are ports and pids, so
	// the space is bounded by the machine anyway; the cap is what keeps a
	// host that churns pids from accumulating entries for ports nothing
	// is on.
	maxVerdicts = 128
)

// prober answers "does this port serve a page" and remembers the answer.
type prober struct {
	client *http.Client
	now    func() time.Time

	mu       sync.Mutex
	verdicts map[string]verdict
}

type verdict struct {
	// scheme is what answered — "http" or "https" — and "" when nothing
	// did. It is carried rather than a bare bool because the gateway
	// proxies to this port later and has to speak the same thing to it:
	// a dev server on https answered here and then 502'd there when the
	// proxy always dialled http.
	scheme  string
	expires time.Time
}

func newProber(now func() time.Time) *prober {
	// One transport, and its dialer is the whole static-resolution rule:
	// the address net/http hands it is discarded and the two loopback
	// literals are tried in order.
	transport := &http.Transport{
		DialContext:         loopback.Dialer(probeTimeout),
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     probeVerdictTTL,
		// A dev server serving TLS with a certificate nothing can verify
		// is the norm, and this dial never leaves the machine: the
		// connection is to a loopback literal this code chose, not to a
		// name anything else resolved. Verifying here would refuse every
		// https dev server and prove nothing about the one hop involved.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback-only, see above
	}
	return &prober{
		client: &http.Client{
			Transport: transport,
			// Redirects are the VERDICT, not something to follow: a dev
			// server answering 302 to its own base path has proved it is
			// a web server, and following the hop would be a second
			// request for an answer already in hand.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Timeout:       probeTimeout,
		},
		now:      now,
		verdicts: make(map[string]verdict),
	}
}

// pageScheme reports which scheme this port serves something a browser
// would render on, and whether anything did. http is tried first because
// nearly every dev server is cleartext on loopback; https is the second
// attempt rather than a separate configuration.
//
// The scheme is the answer, not a detail of it. Whatever proxies to this
// port has to dial the same way, and a bool would have made every https
// dev server a listed preview that 502s.
//
// The verdict is keyed by port AND pid, so a port that changed hands is
// re-probed rather than inheriting the previous occupant's answer.
func (p *prober) pageScheme(ctx context.Context, port, pid int) (string, bool) {
	key := strconv.Itoa(port) + "/" + strconv.Itoa(pid)
	if scheme, ok := p.cached(key); ok {
		return scheme, scheme != ""
	}
	answered := ""
	for _, scheme := range []string{"http", "https"} {
		if p.request(ctx, scheme, port) {
			answered = scheme
			break
		}
	}
	if answered == "" && ctx.Err() != nil {
		// A deadline is not an answer. The scan is bounded, so a slow
		// machine ends its pass mid-probe routinely; storing "not a page"
		// there would write false into the memo for the whole TTL for a
		// port that was never actually asked, and every later scan would
		// read the memo instead of asking. A verdict is only ever stored
		// for a request that got to run.
		return "", false
	}
	p.store(key, answered)
	return answered, answered != ""
}

// request performs one GET and judges the response.
func (p *prober) request(ctx context.Context, scheme string, port int) bool {
	url := scheme + "://localhost:" + strconv.Itoa(port) + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeBodyLimit))
		_ = resp.Body.Close()
	}()
	return respondsLikeAPage(resp)
}

// respondsLikeAPage is the verdict rule, apart from the transport so it
// is readable and testable on its own.
//
//   - 2xx counts only with a document content type. A JSON API on
//     loopback is not a preview, and its 200 would otherwise claim one.
//   - 3xx counts when it names where to go. That is what a dev server
//     serving its app under a base path answers at `/`, and refusing it
//     would drop exactly the servers that are configured rather than
//     default.
func respondsLikeAPage(resp *http.Response) bool {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
		switch strings.ToLower(strings.TrimSpace(mediaType)) {
		case "text/html", "application/xhtml+xml":
			return true
		}
		return false
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return resp.Header.Get("Location") != ""
	default:
		return false
	}
}

// cached returns the remembered scheme and whether one was remembered at
// all. The two are different answers: "" with ok is a port that was asked
// and did not serve a page.
func (p *prober) cached(key string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.verdicts[key]
	if !ok || !p.now().Before(entry.expires) {
		return "", false
	}
	return entry.scheme, true
}

func (p *prober) store(key string, scheme string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if _, exists := p.verdicts[key]; !exists && len(p.verdicts) >= maxVerdicts {
		p.evictLocked(now)
	}
	p.verdicts[key] = verdict{scheme: scheme, expires: now.Add(probeVerdictTTL)}
}

// evictLocked drops lapsed entries, then the entry closest to expiry if
// that was not enough, so a store always has room.
func (p *prober) evictLocked(now time.Time) {
	for key, entry := range p.verdicts {
		if !now.Before(entry.expires) {
			delete(p.verdicts, key)
		}
	}
	for len(p.verdicts) >= maxVerdicts {
		oldestKey := ""
		var oldest time.Time
		for key, entry := range p.verdicts {
			if oldestKey == "" || entry.expires.Before(oldest) {
				oldestKey, oldest = key, entry.expires
			}
		}
		delete(p.verdicts, oldestKey)
	}
}
