package logging

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

const httpDiagnosticLimit = 32

type httpConnectionDiagnostic struct {
	id                                   uint64
	at                                   time.Time
	owner, host, local, remote, protocol string
	reused, idle                         bool
}

// Retain only connection metadata, never requests, bodies, headers or URLs.
// net/http's idle-channel warning has no connection identity. These are
// candidates for attribution, not a claim about which peer sent the bytes.
type httpDiagnosticRing struct {
	mu      sync.Mutex
	next    uint64
	entries [httpDiagnosticLimit]httpConnectionDiagnostic
}

var recentHTTPConnections httpDiagnosticRing

// TraceHTTPRequest records bounded connection metadata without changing the
// request payload, authentication, redirect policy, retries or transport.
func TraceHTTPRequest(req *http.Request, owner string) *http.Request {
	return recentHTTPConnections.trace(req, owner)
}

func (d *httpDiagnosticRing) trace(req *http.Request, owner string) *http.Request {
	// Do not retain the request (including credentials and body) in callbacks.
	owner = strings.Clone(owner[:min(len(owner), 128)])
	host := strings.Clone(req.URL.Host[:min(len(req.URL.Host), 256)])
	var id uint64
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			protocol := "http/1.1"
			if conn, ok := info.Conn.(interface{ ConnectionState() tls.ConnectionState }); ok {
				if negotiated := conn.ConnectionState().NegotiatedProtocol; negotiated != "" {
					protocol = negotiated
				}
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			// A reused socket replaces its earlier request's metadata.
			local, remote := info.Conn.LocalAddr().String(), info.Conn.RemoteAddr().String()
			for i, entry := range d.entries {
				if entry.local == local && entry.remote == remote {
					d.entries[i] = httpConnectionDiagnostic{}
				}
			}
			d.next++
			id = d.next
			d.entries[id%httpDiagnosticLimit] = httpConnectionDiagnostic{
				id: id, at: time.Now(), owner: owner, host: host,
				local: local, remote: remote,
				protocol: protocol, reused: info.Reused,
			}
		},
		PutIdleConn: func(err error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			entry := &d.entries[id%httpDiagnosticLimit]
			if id != 0 && entry.id == id {
				entry.idle = err == nil
				entry.at = time.Now()
			}
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
}

func (d *httpDiagnosticRing) snapshot(now time.Time) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var buf bytes.Buffer
	buf.WriteString("http diagnostics: recent connection candidates (the warning does not identify its peer):\n")
	for _, entry := range d.entries {
		if entry.id == 0 || now.Sub(entry.at) > 2*time.Minute {
			continue
		}
		fmt.Fprintf(&buf, "  owner=%q host=%q local=%q remote=%q protocol=%q reused=%t idle=%t age=%s\n",
			entry.owner, entry.host, entry.local, entry.remote, entry.protocol, entry.reused, entry.idle, now.Sub(entry.at).Round(time.Millisecond))
	}
	return buf.String()
}

type httpDiagnosticWriter struct {
	output io.Writer
	ring   *httpDiagnosticRing
}

// WithHTTPDiagnostics annotates the standard library's unattributed idle HTTP
// warning. Ordinary logs pass through unchanged. Install on every shell's log
// output; this neither changes HTTP behavior nor suppresses the original error.
func WithHTTPDiagnostics(output io.Writer) io.Writer {
	if _, ok := output.(*httpDiagnosticWriter); ok {
		return output
	}
	return &httpDiagnosticWriter{output: output, ring: &recentHTTPConnections}
}

func (w *httpDiagnosticWriter) Write(p []byte) (int, error) {
	n, err := w.output.Write(p)
	if err != nil {
		return n, err
	}
	if bytes.Contains(p, []byte("Unsolicited response received on idle HTTP channel")) {
		_, err = io.WriteString(w.output, w.ring.snapshot(time.Now()))
	}
	return n, err
}
