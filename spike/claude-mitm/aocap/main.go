// Command aocap is a capturing loopback reverse proxy that mirrors the
// production claude-tui gateway (internal/provider/claudetui/gateway.go): plain
// Go net/http forward to api.anthropic.com, stripping Accept-Encoding (so Go
// transparently decompresses) and Host. It tees every request body and the
// decoded SSE response into a JSONL capture so a spike can inspect, on the real
// binary, whether --thinking-display lands thinking.display on the wire and what
// the TUI's internal title-generation request looks like.
//
// Stdlib-only so it runs with `go run /tmp/aocap/main.go`. Forwards credentials
// untouched (it must, or auth breaks) and never logs credential headers.
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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	capMu   sync.Mutex
	capFile *os.File
	reqSeq  atomic.Int64
	start   = time.Now()
)

func tms() float64 { return float64(time.Since(start).Microseconds()) / 1000.0 }

func emit(rec map[string]any) {
	capMu.Lock()
	defer capMu.Unlock()
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	capFile.Write(b)
	capFile.Write([]byte("\n"))
	capFile.Sync()
}

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "loopback listen addr")
	upstream := flag.String("upstream", "https://api.anthropic.com", "upstream base")
	cap := flag.String("cap", "/tmp/aocap.jsonl", "capture jsonl path")
	flag.Parse()

	f, err := os.Create(*cap)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create cap:", err)
		os.Exit(1)
	}
	capFile = f
	defer capFile.Close()

	up := strings.TrimRight(*upstream, "/")
	client := &http.Client{
		// No Client.Timeout: SSE streams are long-lived (mirrors gateway.go).
		Transport: &http.Transport{
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        10,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	handler := func(w http.ResponseWriter, r *http.Request) {
		rid := reqSeq.Add(1)
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		emit(map[string]any{
			"kind": "request", "req_id": rid, "ts": tms(),
			"method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery,
			"body": string(body),
		})

		target := up + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		upReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, strings.NewReader(string(body)))
		if err != nil {
			http.Error(w, "build upstream", http.StatusBadGateway)
			emit(map[string]any{"kind": "error", "req_id": rid, "stage": "build", "err": err.Error()})
			return
		}
		for name, vals := range r.Header {
			if strings.EqualFold(name, "Accept-Encoding") || strings.EqualFold(name, "Host") {
				continue
			}
			for _, v := range vals {
				upReq.Header.Add(name, v)
			}
		}

		resp, err := client.Do(upReq)
		if err != nil {
			http.Error(w, "upstream", http.StatusBadGateway)
			emit(map[string]any{"kind": "error", "req_id": rid, "stage": "upstream", "err": err.Error()})
			return
		}
		defer resp.Body.Close()

		for name, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(name, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		emit(map[string]any{"kind": "response_head", "req_id": rid, "ts": tms(), "status": resp.StatusCode})

		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 16*1024)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				w.Write(chunk)
				if flusher != nil {
					flusher.Flush()
				}
				emit(map[string]any{"kind": "response_chunk", "req_id": rid, "ts": tms(), "text": string(chunk)})
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				emit(map[string]any{"kind": "error", "req_id": rid, "stage": "read", "err": readErr.Error()})
				break
			}
		}
		emit(map[string]any{"kind": "response_end", "req_id": rid, "ts": tms()})
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	// Report the bound address so the Python driver can read it (line-buffered).
	out := bufio.NewWriter(os.Stdout)
	json.NewEncoder(out).Encode(map[string]string{"listen": ln.Addr().String()})
	out.Flush()

	srv := &http.Server{Handler: http.HandlerFunc(handler)}
	emit(map[string]any{"kind": "proxy_start", "ts": tms(), "upstream": up, "listen": ln.Addr().String()})
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
	}
}
