// tier0_proxy.go — Tier-0 Go raw-inbound proxy (VALIDATION ONLY).
//
// The claude-facing (inbound) leg of the Go-inbound + Bun-outbound topology.
// claude points ANTHROPIC_BASE_URL at this proxy; the proxy:
//
//  1. accepts the connection RAW (not net/http) and parses the request by hand,
//     because net/http canonicalizes header names on read (anthropic-version ->
//     Anthropic-Version) and that destroys claude's fingerprint casing. The raw
//     read is the ONLY way to recover the exact names/casing claude sent (§12
//     rule 2).
//  2. strips the framing headers fetch will regenerate (Host, Connection,
//     Content-Length, Accept-Encoding, Transfer-Encoding) and forwards claude's
//     remaining app headers, casing intact, as a JSON envelope to the Bun sidecar
//     (§12 rule 3). The sidecar's version-pinned fetch reconstructs claude's full
//     fingerprint on the real outbound TLS leg.
//  3. streams the sidecar's SSE response back to claude chunk-by-chunk with an
//     immediate flush per chunk (hop c — the proven flush-relay), recording chunk
//     arrival times so a probe can confirm end-to-end incremental streaming.
//
// One request per connection, Connection: close — no keep-alive juggling; this is
// a gate-closer, not a production proxy.
//
// SECURITY: capture redacts credential header VALUES (authorization, cookie,
// x-api-key, *-token). Bodies (claude's /v1/messages JSON) are captured plain for
// marker matching — they are not credentials.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	upstream   = flag.String("upstream", "https://api.anthropic.com", "origin base URL (real or hermetic sink)")
	sidecarURL = flag.String("sidecar", "", "Bun sidecar relay URL, e.g. http://127.0.0.1:NNN/")
	listen     = flag.String("listen", "127.0.0.1:0", "claude-facing listen address")
	capPath    = flag.String("cap", "/tmp/tier0-cap.jsonl", "capture JSONL path")
)

// framing headers fetch regenerates; never forward them (§12 rule 3).
var stripForward = map[string]bool{
	"host":              true,
	"connection":        true,
	"content-length":    true,
	"accept-encoding":   true,
	"transfer-encoding": true,
	"proxy-connection":  true,
}

var t0 = time.Now()
var reqSeq int64

func nowMS() float64 {
	return float64(time.Since(t0).Microseconds()) / 1000.0
}

func redact(name, value string) string {
	low := strings.ToLower(name)
	if low == "authorization" || low == "cookie" || low == "x-api-key" || strings.HasSuffix(low, "-token") {
		return fmt.Sprintf("<redacted len=%d>", len(value))
	}
	return value
}

type capWriter struct {
	mu sync.Mutex
	f  *os.File
}

func (c *capWriter) write(rec map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := json.Marshal(rec)
	c.f.Write(append(b, '\n'))
}

type request struct {
	method, path string
	headers      [][2]string
	body         []byte
}

func parseRequest(r *bufio.Reader) (*request, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("bad request line %q", line)
	}
	req := &request{method: parts[0], path: parts[1]}
	clen := 0
	for {
		l, e := r.ReadString('\n')
		if e != nil {
			return nil, e
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			break
		}
		i := strings.IndexByte(l, ':')
		if i < 0 {
			continue
		}
		name := l[:i]
		val := strings.TrimLeft(l[i+1:], " \t")
		req.headers = append(req.headers, [2]string{name, val})
		if strings.EqualFold(name, "content-length") {
			clen, _ = strconv.Atoi(val)
		}
	}
	if clen > 0 {
		req.body = make([]byte, clen)
		if _, err := io.ReadFull(r, req.body); err != nil {
			return nil, err
		}
	}
	return req, nil
}

// envelope is the JSON contract with the Bun sidecar.
type envelope struct {
	URL     string      `json:"url"`
	Method  string      `json:"method"`
	Headers [][2]string `json:"headers"`
	Body    string      `json:"body"`
}

func handleConn(conn net.Conn, client *http.Client, cap *capWriter) {
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetNoDelay(true)
	}
	reqID := fmt.Sprintf("r%d", atomic.AddInt64(&reqSeq, 1))

	req, err := parseRequest(bufio.NewReader(conn))
	if err != nil {
		return
	}

	// build the forwarded header set (claude's app headers, casing intact)
	fwd := make([][2]string, 0, len(req.headers))
	capHdrs := make([][2]string, 0, len(req.headers))
	for _, h := range req.headers {
		capHdrs = append(capHdrs, [2]string{h[0], redact(h[0], h[1])})
		if stripForward[strings.ToLower(h[0])] {
			continue
		}
		fwd = append(fwd, h)
	}
	cap.write(map[string]any{
		"kind": "request", "req_id": reqID, "t_ms": nowMS(),
		"method": req.method, "path": req.path,
		"headers": capHdrs, "body": string(req.body),
	})

	env := envelope{
		URL:     strings.TrimRight(*upstream, "/") + req.path,
		Method:  req.method,
		Headers: fwd,
		Body:    string(req.body),
	}
	envBytes, _ := json.Marshal(env)

	resp, err := client.Post(*sidecarURL, "application/json", strings.NewReader(string(envBytes)))
	if err != nil {
		cap.write(map[string]any{"kind": "error", "req_id": reqID, "t_ms": nowMS(), "stage": "sidecar_post", "err": err.Error()})
		writeError(conn)
		return
	}
	defer resp.Body.Close()

	// response head -> claude (chunked, Connection: close)
	var head strings.Builder
	fmt.Fprintf(&head, "HTTP/1.1 %d %s\r\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	respHdrNames := make([]string, 0, len(resp.Header))
	for k, vs := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "content-length" || lk == "transfer-encoding" || lk == "connection" {
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&head, "%s: %s\r\n", k, v)
		}
		respHdrNames = append(respHdrNames, k)
	}
	head.WriteString("Transfer-Encoding: chunked\r\n")
	head.WriteString("Connection: close\r\n\r\n")
	conn.Write([]byte(head.String()))
	cap.write(map[string]any{
		"kind": "response_head", "req_id": reqID, "t_ms": nowMS(),
		"status": resp.StatusCode, "resp_headers": respHdrNames,
	})

	// flush-per-chunk relay (hop c): each upstream read becomes one HTTP chunk,
	// written and implicitly flushed (TCP_NODELAY) before the next read.
	buf := make([]byte, 16*1024)
	chunks := 0
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			fmt.Fprintf(conn, "%x\r\n", n)
			conn.Write(buf[:n])
			conn.Write([]byte("\r\n"))
			chunks++
			cap.write(map[string]any{"kind": "response_chunk", "req_id": reqID, "t_ms": nowMS(), "n": n})
		}
		if rerr != nil {
			break
		}
	}
	conn.Write([]byte("0\r\n\r\n"))
	cap.write(map[string]any{"kind": "response_end", "req_id": reqID, "t_ms": nowMS(), "chunks": chunks})
}

func writeError(conn net.Conn) {
	body := `{"type":"error","error":{"type":"proxy_error","message":"tier0 sidecar relay failed"}}`
	fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
}

func main() {
	flag.Parse()
	if *sidecarURL == "" {
		fmt.Fprintln(os.Stderr, "tier0_proxy: --sidecar is required")
		os.Exit(2)
	}
	f, err := os.Create(*capPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cap open:", err)
		os.Exit(1)
	}
	defer f.Close()
	cap := &capWriter{f: f}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	// no client-side timeout: a streaming turn holds the connection open for the
	// whole turn; rely on the sidecar/origin to end the stream.
	client := &http.Client{Timeout: 0}

	fmt.Printf("{\"listen\":%q,\"upstream\":%q,\"sidecar\":%q,\"cap\":%q}\n",
		ln.Addr().String(), *upstream, *sidecarURL, *capPath)

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleConn(conn, client, cap)
	}
}
