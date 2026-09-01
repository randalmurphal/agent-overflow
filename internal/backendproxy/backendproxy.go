// Package backendproxy carries a page's bytes to ONE backend that is not
// this process, swapping the page's credential for the upstream's on the
// way out.
//
// Two callers make the same hop. `agent-overflow --connect` is a whole
// window pointed at one carried backend (internal/clientmode). The
// desktop's attached backends run one of these per machine the owner has
// attached, on the local backend's own listener, so the page reaches
// every machine same-origin (docs/specs/remote-access.md §10). Those were
// the same code twice before this package existed, and the half a
// duplicate gets wrong is always the half that attaches a credential.
//
// What this package deliberately does NOT do is admission. Every caller
// decides for itself who may reach the hop — the page cookie, the origin
// rule, the loopback host guard — and this package begins after that
// verdict. It holds no read model, parses no frame and knows no method
// table: bytes in, bytes out, one credential swapped.
package backendproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"agent-overflow/internal/loopback"
	"agent-overflow/internal/relaysession"
	"agent-overflow/internal/transport"
)

// PairedUpstream is the paired-device credential a carrier presents when
// the backend is on ANOTHER machine (docs/specs/remote-access.md §7).
//
// Declared here and satisfied by `*deviceclient.Client`, the same
// direction as every other seam this package has: it owns the carry and
// knows nothing about device keys, refresh rotation or certificate
// pinning.
//
// Three methods, because a carried upgrade needs three different things
// and each has to be per-request:
//
//   - Authorize attaches the session credential and a proof minted for
//     THAT request. A proof binds the method and the path and is spent on
//     first use, so it cannot be prepared once and reused.
//   - Ticket mints the single-use `?ticket=` the upgrade names its
//     session with. The header carrier does NOT stand in for the launch
//     credential on `/ws` (`internal/transport/AGENTS.md`), and a
//     cross-host carrier holds no launch credential at all — so the
//     ticket is the whole of how a paired upgrade is admitted.
//   - RoundTripper is the pinned transport both the manifest fetch and
//     the proxy dial through, so the certificate this process verifies is
//     the one the device pinned when it paired rather than whatever the
//     host trust store would accept.
type PairedUpstream interface {
	Authorize(req *http.Request) error
	Ticket(ctx context.Context) (string, error)
	RoundTripper() http.RoundTripper
}

// Config describes one upstream backend and how to reach it.
type Config struct {
	// WSURL is the upstream WebSocket endpoint. Must be ws:// or wss://.
	// It carries no credential: the caller splits one out before building
	// this struct, and the carrier is the only thing that re-attaches it.
	WSURL string

	// Token is the upstream backend's launch credential, for a SAME-HOST
	// attach: the WSL launcher's relay, an SSH tunnel, a developer
	// pointing one process at another on their own machine.
	//
	// Exactly one of Token and Paired is required. A launch credential
	// alone cannot admit an off-host upgrade (spec §4, "Local clients"),
	// and a paired device has no launch credential to present.
	Token string

	// Paired is the device session for a backend on another machine.
	// When set, the upstream is reached with that session's credential,
	// a fresh proof per request, and a fresh socket ticket per carried
	// upgrade — all over the pinned transport it supplies.
	Paired PairedUpstream

	// TransferPrefix is the local path prefix CarryTransfer strips before
	// forwarding, for a caller that mounts the upstream's byte routes
	// under a per-backend subtree of its own. Empty means the path
	// crosses untouched, which is what a caller whose whole listener
	// belongs to one backend wants.
	TransferPrefix string

	// Name identifies this carrier in the handful of lines it logs. A
	// process running several needs to know which one failed.
	Name string
}

// Carrier is one built hop. Safe for concurrent use: everything mutable
// lives behind the relay session's own lock, and each request carries its
// own credential.
type Carrier struct {
	cfg Config

	// ws carries an upgrade to the upstream's /ws, adding the upstream
	// credential on the way out. Built once.
	ws *httputil.ReverseProxy

	// bytes carries attachment bodies to the upstream's transfer routes,
	// query untouched and no credential added. Built once.
	bytes *httputil.ReverseProxy

	// session is the upstream backend's local page-channel credential,
	// forwarded on every carried upgrade so a same-host hop names a
	// session instead of being trusted for arriving over loopback.
	// Best-effort, and nil for a paired upstream, which holds a better
	// session of its own. See internal/relaysession.
	session *relaysession.Source

	// client bounds the upstream manifest fetch. One client, because the
	// fetch and the credential refresh behind it speak to the same
	// endpoint and the fetch runs inline on a page load.
	client *http.Client

	bootstrapURL string
	remote       bool
}

// bootstrapFetchTimeout bounds one upstream manifest fetch. A caller
// treats a failure as transient and stays on its reconnect ladder, so a
// slow answer only delays a verdict — it never wedges anything.
const bootstrapFetchTimeout = 5 * time.Second

// maxBootstrapBytes bounds what FetchBootstrap reads back. The manifest
// is a few hundred bytes; anything approaching this is not one.
const maxBootstrapBytes = 64 << 10

// New builds the carrier for one upstream.
func New(cfg Config) (*Carrier, error) {
	if cfg.WSURL == "" {
		return nil, errors.New("backendproxy: Config.WSURL is required")
	}
	// Exactly one credential, checked in both directions. Neither is the
	// obvious default: a carrier with no credential reaches nothing, and
	// one holding both would have two answers to "whose request is this"
	// and no rule for which wins.
	switch {
	case cfg.Token == "" && cfg.Paired == nil:
		return nil, errors.New("backendproxy: Config.Token or Config.Paired is required")
	case cfg.Token != "" && cfg.Paired != nil:
		return nil, errors.New("backendproxy: Config.Token and Config.Paired are alternatives, not a pair")
	}
	parsed, err := url.Parse(cfg.WSURL)
	if err != nil {
		return nil, fmt.Errorf("backendproxy: parse websocket URL: %w", err)
	}
	bootstrapURL, err := relaysession.BootstrapURL(cfg.WSURL)
	if err != nil {
		return nil, fmt.Errorf("backendproxy: derive upstream bootstrap URL: %w", err)
	}
	client := &http.Client{Timeout: bootstrapFetchTimeout}
	// A paired upstream is reached through the transport the device
	// paired over, so the certificate verified here is the one it pinned
	// rather than whatever the host trust store would accept — and
	// through the same one the proxies dial on, so a certificate that
	// changed under this process fails every half at once instead of
	// leaving a manifest that says the backend is fine and a socket that
	// cannot reach it.
	var session *relaysession.Source
	if cfg.Paired != nil {
		client.Transport = cfg.Paired.RoundTripper()
	} else {
		// relaysession is the SAME-HOST relay's mechanism: it fetches the
		// backend's own local page-channel credential so a hop that would
		// otherwise be trusted for its topology alone names a session. A
		// paired device already holds a session of its own, which is
		// better in every respect — it is revocable, it is scoped, and it
		// is this device's rather than the backend's.
		session = relaysession.New(bootstrapURL, cfg.Token, client)
	}
	c := &Carrier{
		cfg:          cfg,
		session:      session,
		client:       client,
		bootstrapURL: bootstrapURL,
		remote:       !loopback.EndpointAuthority(parsed.Host),
	}
	if c.ws, err = c.newWSProxy(); err != nil {
		return nil, fmt.Errorf("backendproxy: build websocket proxy: %w", err)
	}
	if c.bytes, err = c.newByteProxy(); err != nil {
		return nil, fmt.Errorf("backendproxy: build attachment proxy: %w", err)
	}
	return c, nil
}

// Remote reports whether the upstream endpoint is off this machine, the
// same locality bit a manifest carries.
func (c *Carrier) Remote() bool { return c.remote }

// BootstrapURL is the upstream's own /bootstrap.json.
func (c *Carrier) BootstrapURL() string { return c.bootstrapURL }

// FetchBootstrap asks the upstream for its manifest with this carrier's
// credential, and returns what it said.
//
// The status is the verdict on whether that credential is still honoured,
// which is the one question a page cannot ask for itself: a cross-origin
// fetch dies on the read rules and a refused upgrade is a bare 1006. The
// body is bounded and fully read, so the connection is reusable and a
// caller that wants the manifest's own fields has them.
func (c *Carrier) FetchBootstrap(ctx context.Context) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.bootstrapURL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("backendproxy: build the manifest request: %w", err)
	}
	// Header, not query: the upstream's `?t=` slot takes a one-time page
	// ticket, and this is a client that is not a browser presenting the
	// credential it was configured with. Which credential that is, is the
	// whole of the difference between the two modes — a launch token for
	// a same-host attach, this device's session plus a proof minted for
	// THIS request for a paired one.
	if c.cfg.Paired != nil {
		if err := c.cfg.Paired.Authorize(req); err != nil {
			return 0, nil, fmt.Errorf("backendproxy: authorize the manifest request: %w", err)
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBootstrapBytes))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// CarryUpgrade carries one WebSocket upgrade to the upstream.
//
// Admission is the caller's and has already happened. What is left is the
// credential, and for a PAIRED upstream one more thing that has to be
// minted here rather than baked into the proxy: the socket ticket naming
// this device's session. It is single-use and lives seconds, so one
// ticket serves one handshake — a ticket configured once into the proxy's
// target would be spent by the first upgrade and refused by every one
// after it.
func (c *Carrier) CarryUpgrade(w http.ResponseWriter, r *http.Request) {
	if c.cfg.Paired != nil {
		ticket, err := c.cfg.Paired.Ticket(r.Context())
		if err != nil {
			// One shape for every mint failure, matching what the proxy's
			// own ErrorHandler answers an unreachable upstream with. A
			// refused upgrade is not where a page learns its session is
			// finished: the manifest fetch is the one place that maps an
			// upstream verdict onto a terminal state, and it runs the
			// same credential against the same backend moments later.
			c.logf("mint upstream socket ticket: %v", err)
			http.Error(w, "backend unreachable", http.StatusServiceUnavailable)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), upgradeTicketKey{}, ticket))
	}
	c.ws.ServeHTTP(w, r)
}

// CarryTransfer carries one attachment body between the page and the
// upstream (internal/transport/attachmentroutes.go).
//
// This hop attaches no credential of its own, unlike CarryUpgrade,
// because the byte routes deliberately accept none: the upstream's
// admission is the single-use TICKET already on the query, which the page
// obtained through a carried RPC. The caller's own admission stops here.
//
// The query passes through untouched — it was minted by the upstream, for
// the upstream, and rewriting it would only be a way to get it wrong. The
// path crosses untouched too, except for the per-backend prefix a caller
// mounted these routes under, which is this process's own addressing and
// means nothing upstream.
func (c *Carrier) CarryTransfer(w http.ResponseWriter, r *http.Request) {
	// The same window the backend gives itself. Without this a caller's
	// own read and write timeouts would cut a transfer the upstream was
	// willing to finish, and the page would see the hop fail rather than
	// the backend refuse.
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(transport.AttachmentTransferWindow)
	if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		c.logf("extend attachment read deadline: %v", err)
	}
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		c.logf("extend attachment write deadline: %v", err)
	}
	c.bytes.ServeHTTP(w, r)
}

// upgradeTicketKey carries the socket ticket CarryUpgrade minted from the
// request into the proxy's Rewrite, which is the only other place that
// sees this particular upgrade. A context value rather than a field,
// because the proxy is built once and every handshake needs its own.
type upgradeTicketKey struct{}

func upgradeTicket(ctx context.Context) string {
	ticket, _ := ctx.Value(upgradeTicketKey{}).(string)
	return ticket
}

// newWSProxy builds the carrier for a page's WebSocket. The rewrite is
// the whole of it:
//
//   - Address the upstream's own /ws, keeping any path prefix the
//     configured URL carried (a reverse proxy in front of the backend).
//     Host is cleared so the request goes out naming the upstream, which
//     is what the upstream's own loopback host guard expects when the
//     endpoint is reached through a tunnel.
//   - Attach the upstream credential. This is the only place it appears
//     on a wire, and it replaces the page's own credential rather than
//     travelling beside it. Which credential depends on the mode: a
//     bearer launch token for a same-host attach, and for a paired
//     device the single-use `?ticket=` CarryUpgrade minted from this
//     request. The ticket is the whole of a paired upgrade's admission —
//     a spent one both names the session and stands in for the launch
//     credential the device does not have, which the session HEADER
//     deliberately does not do (`internal/transport/AGENTS.md`), so
//     sending the header here as well would put a credential on the wire
//     that nothing reads.
//   - Drop the browser's Cookie and Origin. This process's cookie means
//     nothing upstream, and this process's origin is not one the upstream
//     serves — an Origin it does not recognise is refused, correctly,
//     because a proxy hop is not a browser. Removing it presents the hop
//     as what it is: a client that is not a browser.
//   - Carry the page's declared client identity across the hop. The
//     query is REPLACED (the configured URL owns it, and the page's own
//     `?t=` ticket means nothing upstream), so the two identity
//     parameters have to be re-emitted explicitly or the upstream sees
//     an anonymous connection — which since the ui_state scope became
//     connection-derived would leave this hop with no bucket at all.
//     Parsed and re-rendered rather than copied, so only the two
//     declared, length-bounded parameters cross.
//
// Everything else is byte-transparent: the handshake key, the requested
// subprotocols and the compression extension all pass through, so the
// browser and the upstream negotiate with each other and this process
// only carries frames.
//
// httputil.ReverseProxy handles the 101 itself — it takes the connection
// over from net/http and splices both directions, which also clears the
// HTTP server's write deadline, so a caller's request timeouts cannot cut
// a healthy long-lived socket.
func (c *Carrier) newWSProxy() (*httputil.ReverseProxy, error) {
	target, err := upstreamHTTPTarget(c.cfg.WSURL)
	if err != nil {
		return nil, err
	}
	if target.Path == "" || target.Path == "/" {
		target.Path = transport.WSPath
	}
	session := c.session
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = target.Path
			pr.Out.URL.RawQuery = upstreamQuery(target.RawQuery, pr.In.URL.Query(), upgradeTicket(pr.In.Context()))
			pr.Out.Host = ""
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Origin")
			// Both credential headers are DELETED before either is
			// written, in both modes. A browser cannot put a header on an
			// upgrade, but a local client that is not a browser holding
			// this process's cookie could, and a forwarded one would let
			// it name a credential it did not obtain.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del(relaysession.Header)
			if session == nil {
				// Paired: the ticket on the query is the credential, and
				// it was minted for this handshake alone.
				return
			}
			pr.Out.Header.Set("Authorization", "Bearer "+c.cfg.Token)
			if credential := session.Credential(pr.In.Context()); credential != "" {
				pr.Out.Header.Set(relaysession.Header, credential)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// Anything but the switch is a refused upgrade, and a refusal
			// is the one signal that a forwarded credential has gone stale
			// — the upstream restarted, or the session was revoked. Mark
			// it rather than refetching here: a page's reconnect ladder
			// owns the retry, and the next carried upgrade fetches a live
			// one instead of replaying the dead one.
			//
			// The response is passed through untouched. The verdict on
			// whether the CREDENTIAL is still honoured belongs to the
			// manifest fetch, which is the one place that maps upstream
			// status onto a page's terminal state.
			//
			// Nothing to mark in paired mode: the ticket was single-use
			// and is already spent, and the session behind it renews on
			// its own schedule rather than on a refusal.
			if session != nil && resp.StatusCode != http.StatusSwitchingProtocols {
				session.Stale()
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// An unreachable upstream is a page's ordinary outage: its
			// reconnect ladder retries and its manifest refetch owns the
			// "credential is dead" verdict. Log once, answer the shape a
			// refused upgrade already has.
			c.logf("websocket proxy: %v", err)
			http.Error(w, "backend unreachable", http.StatusServiceUnavailable)
		},
	}
	if c.cfg.Paired != nil {
		// The pinned transport carries the upgrade too. ReverseProxy
		// dials through it and takes the 101 over itself, so the socket
		// the page's frames ride is the one whose certificate this device
		// verified — not merely a fetch that agreed beforehand.
		proxy.Transport = c.cfg.Paired.RoundTripper()
	}
	return proxy, nil
}

// newByteProxy builds the attachment relay.
//
// It is a second ReverseProxy rather than a branch inside the WebSocket
// one because the two hops agree on almost nothing: this one preserves
// the path, forwards no credential, carries a body in both directions and
// never expects a 101. Sharing a Rewrite between them would mean a
// conditional in the one function where a mistake attaches a credential to
// the wrong request.
func (c *Carrier) newByteProxy() (*httputil.ReverseProxy, error) {
	target, err := upstreamHTTPTarget(c.cfg.WSURL)
	if err != nil {
		return nil, err
	}
	prefix := c.cfg.TransferPrefix
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			if prefix != "" {
				// The mount point is this process's addressing. What
				// crosses is the route the upstream published, so a
				// per-backend subtree is stripped down to it exactly.
				pr.Out.URL.Path = "/" + strings.TrimPrefix(strings.TrimPrefix(pr.In.URL.Path, prefix), "/")
			}
			// Host cleared so the request names the upstream, which is
			// what the upstream's own loopback host guard expects when it
			// is reached through a tunnel.
			pr.Out.Host = ""
			// Nothing this process or this browser holds means anything
			// upstream, and a forwarded credential would let a local
			// client that is not a browser name one it did not obtain.
			// The ticket on the query is the whole admission.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del(relaysession.Header)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// An unreachable upstream is a page's ordinary outage; the
			// composer's own failure path reports it. Same shape the
			// upgrade relay answers with.
			c.logf("attachment proxy: %v", err)
			http.Error(w, "backend unreachable", http.StatusServiceUnavailable)
		},
	}
	if c.cfg.Paired != nil {
		proxy.Transport = c.cfg.Paired.RoundTripper()
	}
	return proxy, nil
}

// logf names the carrier in every line, because a process running several
// of them needs to know which upstream failed.
func (c *Carrier) logf(format string, args ...any) {
	if c.cfg.Name == "" {
		log.Printf("backendproxy: "+format, args...)
		return
	}
	log.Printf("backendproxy["+c.cfg.Name+"]: "+format, args...)
}

// upstreamHTTPTarget maps a ws:// or wss:// endpoint onto the http:// or
// https:// origin the same backend answers on. Shared by both proxies so
// a scheme accepted in one hop cannot be one rejected in the other.
func upstreamHTTPTarget(wsURL string) (url.URL, error) {
	parsed, err := url.Parse(wsURL)
	if err != nil {
		return url.URL{}, fmt.Errorf("parse ws url: %w", err)
	}
	target := *parsed
	switch parsed.Scheme {
	case "ws":
		target.Scheme = "http"
	case "wss":
		target.Scheme = "https"
	default:
		return url.URL{}, fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
	return target, nil
}

// upstreamQuery assembles the query a proxied upgrade carries: the
// configured URL's own parameters, the page's declared client identity,
// and — for a paired device — the single-use socket ticket minted for this
// handshake.
//
// The configured values win a collision. They are the endpoint's
// configuration and the page cannot see them; a page parameter that
// overwrote one would be the page reconfiguring the hop.
//
// The ticket is the one value that wins outright, because it is not
// configuration: it is this handshake's credential, and a configured URL
// that carried a stale `ticket=` would otherwise spend a dead one and
// leave the fresh one unpresented.
func upstreamQuery(configured string, page url.Values, ticket string) string {
	declared := transport.ParseClientIdentity(page).Query()
	if len(declared) == 0 && ticket == "" {
		return configured
	}
	values, err := url.ParseQuery(configured)
	if err != nil {
		// A configured URL we cannot parse is one we must not rewrite
		// from a guess: forward it byte-for-byte, and lose only
		// attribution.
		return configured
	}
	for key, list := range declared {
		if values.Has(key) {
			continue
		}
		values[key] = list
	}
	if ticket != "" {
		values.Set(transport.WSTicketParam, ticket)
	}
	return values.Encode()
}
