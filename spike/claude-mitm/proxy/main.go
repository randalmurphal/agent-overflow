// Command proxy is a throwaway logging reverse proxy for the Claude MITM spike.
//
// It accepts requests at a local address and forwards them to the real
// Anthropic API, recording every request body and the full (streamed)
// response body to a JSONL capture file. Point Claude Code at it with
// ANTHROPIC_BASE_URL=http://127.0.0.1:8080 and the proxy transparently
// captures the /v1/messages SSE stream without any TLS interception.
//
// Sensitive headers (auth/cookies) are redacted in the capture; request
// and response bodies are kept verbatim because that is the signal we are
// trying to characterize.
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	listenAddr = flag.String("listen", "127.0.0.1:8080", "local address to listen on")
	upstream   = flag.String("upstream", "https://api.anthropic.com", "upstream base URL to forward to")
	logPath    = flag.String("log", "/tmp/ao-mitm-proxy.jsonl", "JSONL capture file")
	tlsCert    = flag.String("tls-cert", "", "optional server cert (enables HTTPS listener)")
	tlsKey     = flag.String("tls-key", "", "optional server key")
)

var (
	logMu  sync.Mutex
	logOut *os.File
)

func emit(event map[string]any) {
	event["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(event)
	if err != nil {
		log.Printf("marshal log event: %v", err)
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	logOut.Write(line)
	logOut.Write([]byte("\n"))
}

func redactHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for name, vals := range h {
		lower := strings.ToLower(name)
		// Redact credential-bearing headers precisely. Do NOT match the bare
		// substring "token": it over-redacts rate-limit headers such as
		// `anthropic-ratelimit-*-input-tokens-*` that the UI needs. Match the
		// actual secret headers plus *-token credential variants.
		secret := lower == "authorization" ||
			lower == "proxy-authorization" ||
			lower == "x-api-key" ||
			lower == "cookie" ||
			lower == "set-cookie" ||
			strings.Contains(lower, "api-key") ||
			strings.Contains(lower, "access-token") ||
			strings.Contains(lower, "refresh-token") ||
			strings.Contains(lower, "session-token") ||
			strings.Contains(lower, "auth-token")
		if secret {
			out[name] = "<redacted len=" + itoa(len(strings.Join(vals, ","))) + ">"
			continue
		}
		out[name] = strings.Join(vals, ", ")
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// transport forwards to the upstream. Auto-gzip handling is left to Go so
// captured bodies are plaintext.
var transport = &http.Transport{
	ForceAttemptHTTP2:   true,
	MaxIdleConns:        10,
	IdleConnTimeout:     90 * time.Second,
	TLSHandshakeTimeout: 10 * time.Second,
}

func handler(w http.ResponseWriter, r *http.Request) {
	reqID := time.Now().UnixNano()

	// Read and record the request body, then restore it for forwarding.
	var bodyBytes []byte
	if r.Body != nil {
		bodyBytes, _ = io.ReadAll(r.Body)
		r.Body.Close()
	}

	emit(map[string]any{
		"kind":    "request",
		"req_id":  reqID,
		"method":  r.Method,
		"path":    r.URL.Path,
		"query":   r.URL.RawQuery,
		"headers": redactHeaders(r.Header),
		"body":    string(bodyBytes),
	})

	// Build the upstream request.
	target := *upstream + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, strings.NewReader(string(bodyBytes)))
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusBadGateway)
		emit(map[string]any{"kind": "error", "req_id": reqID, "stage": "build", "error": err.Error()})
		return
	}
	for name, vals := range r.Header {
		// Strip Accept-Encoding so Go negotiates gzip itself and hands us
		// plaintext; strip hop-by-hop Host so the transport sets it.
		if strings.EqualFold(name, "Accept-Encoding") || strings.EqualFold(name, "Host") {
			continue
		}
		for _, v := range vals {
			outReq.Header.Add(name, v)
		}
	}

	start := time.Now()
	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		emit(map[string]any{"kind": "error", "req_id": reqID, "stage": "roundtrip", "error": err.Error()})
		return
	}
	defer resp.Body.Close()

	emit(map[string]any{
		"kind":    "response_head",
		"req_id":  reqID,
		"status":  resp.StatusCode,
		"headers": redactHeaders(resp.Header),
		"ttfb_ms": time.Since(start).Milliseconds(),
	})

	// Copy response headers to the client.
	for name, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	var total int
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			total += n
			chunk := string(buf[:n])
			emit(map[string]any{
				"kind":   "response_chunk",
				"req_id": reqID,
				"bytes":  n,
				"text":   chunk,
				"t_ms":   time.Since(start).Milliseconds(),
			})
			if _, werr := w.Write(buf[:n]); werr != nil {
				emit(map[string]any{"kind": "error", "req_id": reqID, "stage": "client_write", "error": werr.Error()})
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			emit(map[string]any{"kind": "error", "req_id": reqID, "stage": "upstream_read", "error": readErr.Error()})
			break
		}
	}

	emit(map[string]any{
		"kind":        "response_end",
		"req_id":      reqID,
		"total_bytes": total,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func main() {
	flag.Parse()

	f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatalf("open log %s: %v", *logPath, err)
	}
	logOut = f
	defer f.Close()

	emit(map[string]any{"kind": "proxy_start", "listen": *listenAddr, "upstream": *upstream})
	log.Printf("MITM proxy listening on %s -> %s (log: %s)", *listenAddr, *upstream, *logPath)

	srv := &http.Server{
		Addr:    *listenAddr,
		Handler: http.HandlerFunc(handler),
		// No write timeout: SSE streams are long-lived.
		ReadHeaderTimeout: 30 * time.Second,
	}

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	if *tlsCert != "" && *tlsKey != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			log.Fatalf("load tls keypair: %v", err)
		}
		srv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			// Only advertise HTTP/1.1 so clients don't use h2 framing to us.
			NextProtos: []string{"http/1.1"},
		}
		log.Printf("serving HTTPS")
		log.Fatal(srv.ServeTLS(ln, "", ""))
	}

	// Plain HTTP listener (for ANTHROPIC_BASE_URL=http://...).
	log.Fatal(srv.Serve(ln))
}
