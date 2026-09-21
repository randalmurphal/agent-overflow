package logging

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"
)

type httpLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
	warning chan struct{}
	once    sync.Once
}

func (b *httpLogBuffer) WriteString(s string) (int, error) { return b.Write([]byte(s)) }

func (b *httpLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.Buffer.Write(p)
	if bytes.Contains(p, []byte("http diagnostics:")) {
		b.once.Do(func() { close(b.warning) })
	}
	return n, err
}
func (b *httpLogBuffer) text() string { b.mu.Lock(); defer b.mu.Unlock(); return b.Buffer.String() }

// Exercise net/http's actual idle-channel warning, including its asynchronous
// arrival after a successful response. No connection behavior is replaced.
func TestUnsolicitedHTTPWarningIncludesConnectionCandidates(t *testing.T) {
	ring := &httpDiagnosticRing{}
	output := &httpLogBuffer{warning: make(chan struct{})}
	prior := log.Writer()
	log.SetOutput(&httpDiagnosticWriter{output: output, ring: ring})
	defer log.SetOutput(prior)
	release := make(chan struct{})
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		if _, err := rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"); err != nil {
			serverErr <- err
			return
		}
		if err := rw.Flush(); err != nil {
			serverErr <- err
			return
		}
		<-release
		_, err = conn.Write([]byte{1, 0, 0, 0})
		serverErr <- err
	}))
	defer server.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	req, err := http.NewRequest(http.MethodGet, server.URL+"/private-path?token=private-query", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("private-user", "private-password")
	response, err := client.Do(ring.trace(req, "test-http"))
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-output.warning:
	case <-time.After(3 * time.Second):
		t.Fatal("idle warning did not arrive")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	text := output.text()
	for _, want := range []string{"Unsolicited response received", `owner="test-http"`, `host="` + req.URL.Host + `"`, `protocol="http/1.1"`, `idle=true`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "private-") {
		t.Fatalf("request secrets in diagnostic: %s", text)
	}
}

type diagnosticConn struct {
	net.Conn
	address string
}

func (c diagnosticConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
}
func (c diagnosticConn) RemoteAddr() net.Addr { return diagnosticAddr(c.address) }

type diagnosticAddr string

func (a diagnosticAddr) String() string  { return string(a) }
func (a diagnosticAddr) Network() string { return "tcp" }

func TestHTTPDiagnosticRingIsBoundedAndExpires(t *testing.T) {
	ring := &httpDiagnosticRing{}
	for i := 0; i < 100; i++ {
		req, err := http.NewRequest(http.MethodGet, "https://example.test", nil)
		if err != nil {
			t.Fatal(err)
		}
		traced := ring.trace(req, "owner")
		trace := httptrace.ContextClientTrace(traced.Context())
		trace.GotConn(httptrace.GotConnInfo{Conn: diagnosticConn{address: fmt.Sprint(i)}, Reused: i > 0})
		trace.PutIdleConn(nil)
	}
	if got := strings.Count(ring.snapshot(time.Now()), "owner="); got != httpDiagnosticLimit {
		t.Fatalf("retained=%d", got)
	}
	if got := strings.Count(ring.snapshot(time.Now().Add(3*time.Minute)), "owner="); got != 0 {
		t.Fatalf("expired=%d", got)
	}
}

type failedDiagnosticWriter struct{}

func (failedDiagnosticWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestHTTPDiagnosticWriterPreservesOrdinaryLogsAndErrors(t *testing.T) {
	var output bytes.Buffer
	writer := WithHTTPDiagnostics(&output)
	if WithHTTPDiagnostics(writer) != writer {
		t.Fatal("nested diagnostic writer")
	}
	if _, err := writer.Write([]byte("ordinary log\n")); err != nil {
		t.Fatal(err)
	}
	if output.String() != "ordinary log\n" {
		t.Fatal("changed ordinary log")
	}
	if _, err := WithHTTPDiagnostics(failedDiagnosticWriter{}).Write([]byte("ordinary")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("error=%v", err)
	}
}

// Diagnostics compose with an existing trace and describe the negotiated
// protocol without changing TLS or HTTP/2 support.
func TestHTTPDiagnosticsPreserveTraceAndTLS(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	ring := &httpDiagnosticRing{}
	var priorCalls int
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			priorCalls++
			if info.Conn.(*tls.Conn).ConnectionState().NegotiatedProtocol != "h2" {
				t.Error("HTTP/2 was not negotiated")
			}
		},
	}))
	for range 2 {
		response, err := server.Client().Do(ring.trace(req, "tls-test"))
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if priorCalls != 2 {
		t.Fatalf("existing trace calls=%d", priorCalls)
	}
	snapshot := ring.snapshot(time.Now())
	if strings.Count(snapshot, "owner=") != 1 || !strings.Contains(snapshot, `protocol="h2" reused=true`) {
		t.Fatalf("reused TLS connection: %s", snapshot)
	}
}
