package claudetui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
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

	ln     net.Listener
	server *http.Server
}

// agentTurnDriver is the slice of the session the gateway needs: open and close
// one agent turn's reconstruction. Begin/end mutate shared per-session
// reconstruction state, so the session guards them; onSSE on the returned
// agentRequest is lock-free (local assembler + the envelope feed channel).
type agentTurnDriver interface {
	beginAgentTurn(req *messagesRequest) *agentRequest
	endAgentTurn(ar *agentRequest)
}

// defaultUpstream is the Anthropic API base the gateway forwards to when no
// override is configured.
const defaultUpstream = "https://api.anthropic.com"

// newGateway binds a loopback listener on an ephemeral port and prepares the
// proxy. Call start to serve. baseURL is what gets injected as
// ANTHROPIC_BASE_URL for the spawned claude.
func newGateway(upstream string, drive agentTurnDriver, onError func(error)) (*gateway, error) {
	if upstream == "" {
		upstream = defaultUpstream
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("claudetui gateway: bind loopback: %w", err)
	}
	g := &gateway{
		upstream: strings.TrimRight(upstream, "/"),
		drive:    drive,
		onError:  onError,
		ln:       ln,
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
	if !isLoopback(r.RemoteAddr) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
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
	// forwarded transparently without surfacing as a turn.
	var ar *agentRequest
	var scanner *sseScanner
	if r.Method == http.MethodPost && r.URL.Path == "/v1/messages" && resp.StatusCode == http.StatusOK {
		if class, req := classifyRequest(body); class == classAgent {
			ar = g.drive.beginAgentTurn(req)
			scanner = newSSEScanner(ar.onSSE)
		}
	}

	g.stream(w, resp.Body, scanner)

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
// live SSE, and tees into the scanner when one is set.
func (g *gateway) stream(w http.ResponseWriter, body io.Reader, scanner *sseScanner) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				g.onError(fmt.Errorf("claudetui gateway: client write: %w", werr))
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
			g.onError(fmt.Errorf("claudetui gateway: upstream read: %w", readErr))
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
