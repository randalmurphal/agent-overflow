package claudetui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"agent-overflow/internal/loopback"
)

// gateway is the per-session loopback reverse proxy bound to ANTHROPIC_BASE_URL.
// It forwards Claude Code's traffic to the upstream Anthropic API untouched
// (credentials included — it must, or auth breaks) and tees the /v1/messages
// SSE response into the reconstruction. It logs nothing by default: production
// stores AO-normalized events, not raw bodies (raw capture is a dev-only,
// local-only follow-on). See docs/architecture/claude-tui-provider.md.
type gateway struct {
	upstream string
	client   *http.Client
	drive    agentTurnDriver
	onError  func(error)
	// onClassify, when non-nil, is called for every POST /v1/messages with its
	// classification, the upstream status, and the request body — debug-only
	// tracing wired by the session when the event logger is live. Body-derived
	// only: no credential headers ever reach it. See debuglog.go.
	onClassify func(class requestClass, status int, body []byte)

	// maxBodyBytes caps how much of an inbound request body the gateway will
	// buffer. Defaults to maxRequestBodyBytes; the field exists so tests can
	// drive the over-limit path with a tiny body. Set once before start(), then
	// read-only on the serving goroutines.
	maxBodyBytes int64

	ln     net.Listener
	server *http.Server
}

// agentTurnDriver is the slice of the session the gateway needs: open and close
// one agent turn's reconstruction. Begin/end mutate shared per-session
// reconstruction state, so the session guards them; onSSE on the returned
// agentRequest is lock-free (local assembler + the envelope feed channel).
// beginSubagentTurn returns nil when the subagent can't be matched to its
// launching Agent — the gateway then forwards the request without reconstructing.
type agentTurnDriver interface {
	beginAgentTurn(req *messagesRequest) *agentRequest
	beginSubagentTurn(req *messagesRequest, agentID string) *agentRequest
	// beginCompactionCapture returns a capture agentRequest when a compaction is
	// armed (PreCompact seen, summarizer not yet captured), else nil so the gateway
	// falls through to normal subagent/main routing. The summarizer is the first
	// classAgent request after PreCompact.
	beginCompactionCapture(req *messagesRequest) *agentRequest
	endAgentTurn(ar *agentRequest)
}

// agentIDHeader is the per-subagent identifier Claude Code sets on a subagent's
// /v1/messages requests; it is absent on main-agent requests. It both partitions
// subagent turns from the main loop and keys the correlation that nests a
// subagent's several requests under one Agent card. Confirmed on 2.1.170
// (spike/claude-mitm). NEVER logged — see debuglog.go's body-only discipline.
const agentIDHeader = "X-Claude-Code-Agent-Id"

// defaultUpstream is the Anthropic API base the gateway forwards to when no
// override is configured.
const defaultUpstream = "https://api.anthropic.com"

// maxRequestBodyBytes bounds how much of an inbound request body the gateway
// buffers into memory. It is a memory-pressure backstop, not a correctness
// gate: the upstream Anthropic API is the authority on request size (Messages
// and Token Counting cap at 32 MB, enforced at Cloudflare's edge before the
// request reaches the API), so this only needs to sit comfortably ABOVE the
// largest body Claude Code legitimately sends through the proxy. 64 MiB is 2x
// the 32 MB Messages limit — any request the upstream would accept passes with
// headroom, and anything larger the upstream would reject anyway. We fail loud
// (413) rather than silently truncate: this body is proxied verbatim upstream,
// so a short read would corrupt the request. The interactive TUI inlines
// attachments as base64 into /v1/messages rather than using the 500 MB Files
// API, so that higher ceiling does not apply here; re-tune if a future Claude
// Code build streams large bodies through a higher-limit endpoint.
const maxRequestBodyBytes = 64 << 20 // 64 MiB

// newGateway binds a loopback listener on an ephemeral port and prepares the
// proxy. Call start to serve. baseURL is what gets injected as
// ANTHROPIC_BASE_URL for the spawned claude.
func newGateway(upstream string, drive agentTurnDriver, onError func(error), onClassify func(requestClass, int, []byte)) (*gateway, error) {
	if upstream == "" {
		upstream = defaultUpstream
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("claudetui gateway: bind loopback: %w", err)
	}
	g := &gateway{
		upstream:     strings.TrimRight(upstream, "/"),
		drive:        drive,
		onError:      onError,
		onClassify:   onClassify,
		maxBodyBytes: maxRequestBodyBytes,
		ln:           ln,
		client: &http.Client{
			// No Client.Timeout: /v1/messages SSE streams are long-lived and a
			// blanket deadline would truncate a turn. The transport bounds the
			// connection-establishment phases instead.
			Transport: &http.Transport{
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          10,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	g.server = &http.Server{Handler: http.HandlerFunc(g.handle)}
	return g, nil
}

// baseURL is the loopback address to inject as ANTHROPIC_BASE_URL.
func (g *gateway) baseURL() string {
	return "http://" + g.ln.Addr().String()
}

// start serves the proxy until close. A non-ErrServerClosed exit is surfaced.
func (g *gateway) start() {
	go func() {
		if err := g.server.Serve(g.ln); err != nil && err != http.ErrServerClosed {
			g.onError(fmt.Errorf("claudetui gateway serve: %w", err))
		}
	}()
}

// close shuts the proxy down. Safe to call once.
func (g *gateway) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return g.server.Shutdown(ctx)
}

func (g *gateway) handle(w http.ResponseWriter, r *http.Request) {
	// Loopback-peer check, mirroring the hook relay. The gateway is bound to
	// 127.0.0.1 and forwards to a FIXED upstream with caller-supplied credentials
	// (it injects none of its own), so it is not a credential-escalation vector —
	// but rejecting any non-loopback peer removes the asymmetry with the relay and
	// closes a colocated-process proxy to the user's Anthropic account. Go sets
	// RemoteAddr from the accepted socket, so this is not header-spoofable.
	if !loopback.PeerAddress(r.RemoteAddr) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.maxBodyBytes))
	if err != nil {
		// An over-limit body is the memory-pressure backstop firing, not an
		// upstream fault: fail loud with 413 rather than forward a truncated
		// (corrupt) body. Other read failures stay 502.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			g.onError(fmt.Errorf("claudetui gateway: request body exceeds %d bytes", g.maxBodyBytes))
			return
		}
		http.Error(w, "read request body", http.StatusBadGateway)
		g.onError(fmt.Errorf("claudetui gateway: read request: %w", err))
		return
	}
	_ = r.Body.Close()

	upReq, err := g.buildUpstreamRequest(r, body)
	if err != nil {
		http.Error(w, "build upstream request", http.StatusBadGateway)
		g.onError(fmt.Errorf("claudetui gateway: build upstream: %w", err))
		return
	}

	resp, err := g.client.Do(upReq)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		g.onError(fmt.Errorf("claudetui gateway: upstream: %w", err))
		return
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// Set up reconstruction only for a real main-loop agent turn; everything
	// else (preflight, title gen, nested sub-calls, non-message paths) is
	// forwarded transparently without surfacing as a turn. Classify once and
	// report every /v1/messages call (agent or dropped) to onClassify when the
	// debug logger is live, so a phantom turn can be traced to its request.
	var ar *agentRequest
	var scanner *sseScanner
	if r.Method == http.MethodPost && r.URL.Path == "/v1/messages" {
		class, req := classifyRequest(body)
		if g.onClassify != nil {
			g.onClassify(class, resp.StatusCode, body)
		}
		if resp.StatusCode == http.StatusOK && class == classAgent {
			// A compaction summarizer (the forked agent that runs between the
			// PreCompact and PostCompact hooks) is wire-identical to a normal turn,
			// so it can only be told apart by the armed hook state. Try the capture
			// path first: when armed, it claims this request (and disarms) so the
			// summarizer's thinking + summary group under the Compacted item instead
			// of rendering as a phantom turn. nil when not armed — fall through to
			// normal subagent/main routing.
			if ar = g.drive.beginCompactionCapture(req); ar == nil {
				// A subagent's request carries X-Claude-Code-Agent-Id; route it to the
				// nesting reconstruction so it surfaces under its Agent card instead of
				// as a phantom main turn. beginSubagentTurn may return nil (parent not
				// resolvable) — then the request is forwarded without reconstruction.
				//
				// The header alone is NOT sufficient: when the MAIN loop resumes to
				// observe a backgrounded subagent's completion, Claude attaches that
				// subagent's agent-id to the resume as well (plus cc_is_subagent=true).
				// requestReportsAgentCompletion catches that observation — the body
				// carries a <task-notification> whose task-id == agentID, the agent
				// reporting itself — and keeps it on the main path, where
				// emitBackgroundCompletions runs and the response surfaces as a
				// top-level turn. See turndriver.go for the full rationale.
				if agentID := r.Header.Get(agentIDHeader); agentID != "" && !requestReportsAgentCompletion(req.Messages, agentID) {
					ar = g.drive.beginSubagentTurn(req, agentID)
				} else {
					ar = g.drive.beginAgentTurn(req)
				}
			}
			if ar != nil {
				scanner = newSSEScanner(ar.onSSE)
			}
		}
	}

	g.stream(r.Context(), w, resp.Body, scanner)

	if ar != nil {
		g.drive.endAgentTurn(ar)
	}
}

// buildUpstreamRequest mirrors the inbound request to the upstream, stripping
// Accept-Encoding (so Go negotiates gzip and hands us plaintext SSE) and Host
// (so the transport sets it).
func (g *gateway) buildUpstreamRequest(r *http.Request, body []byte) (*http.Request, error) {
	target := g.upstream + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for name, vals := range r.Header {
		if strings.EqualFold(name, "Accept-Encoding") || strings.EqualFold(name, "Host") {
			continue
		}
		for _, v := range vals {
			upReq.Header.Add(name, v)
		}
	}
	return upReq, nil
}

// stream copies the upstream response to the client, flushing each chunk for
// live SSE, and tees into the scanner when one is set. ctx is the inbound
// request's context; when it is canceled the client (the interactive claude)
// aborted its own request — it is retrying after a transient API error, or the
// user pressed Esc — so the resulting read/write failures are NOT gateway
// faults and must not surface as an error banner. The retry surfaces as
// api_retry on the reconstructed stream; the Esc closes the turn via the
// interrupt path. A genuine upstream failure (ctx still live) keeps surfacing
// with its real message so the actual issue is visible.
func (g *gateway) stream(ctx context.Context, w http.ResponseWriter, body io.Reader, scanner *sseScanner) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				if ctx.Err() == nil {
					g.onError(fmt.Errorf("claudetui gateway: client write: %w", werr))
				}
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if scanner != nil {
				scanner.write(buf[:n])
			}
		}
		if readErr == io.EOF {
			return
		}
		if readErr != nil {
			// A canceled inbound context is the authoritative "client went away"
			// signal (the TUI aborting to retry, or user Esc) — the read failure
			// is its symptom, not a gateway fault, so suppress it. The upstream
			// request uses this same context, so a genuine upstream failure only
			// ever surfaces with the context still live.
			if ctx.Err() == nil {
				g.onError(fmt.Errorf("claudetui gateway: upstream read: %w", readErr))
			}
			return
		}
	}
}

func copyHeader(dst, src http.Header) {
	for name, vals := range src {
		for _, v := range vals {
			dst.Add(name, v)
		}
	}
}
