package transport

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// A temporary accept failure belongs to net/http's retry loop. Caching it
// forever leaves established sockets alive while every new request hangs.
func TestTLSSniffRecoversAfterTemporaryAcceptError(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	trigger := make(chan struct{})
	release := sync.OnceFunc(func() { close(trigger) })
	fault := &temporaryAcceptListener{Listener: inner, trigger: trigger}
	listener := sniffTLS(fault, &tls.Config{}, time.Second)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ok")
		}),
		ErrorLog: log.New(io.Discard, "", 0),
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	t.Cleanup(func() {
		release()
		_ = srv.Close()
		<-done
	})
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Timeout: time.Second, Transport: transport}
	url := "http://" + listener.Addr().String()
	get := func(client *http.Client) {
		t.Helper()
		response, err := client.Get(url)
		if err != nil {
			t.Fatalf("request: %v (underlying accepts: %d)", err, fault.calls.Load())
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || string(body) != "ok" {
			t.Fatalf("body = %q, error = %v", body, err)
		}
	}
	get(client) // Establish a connection before the failure.
	release()
	get(client) // Already accepted connections remain usable, even before the fix.
	freshTransport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(freshTransport.CloseIdleConnections)
	response, err := (&http.Client{Timeout: time.Second, Transport: freshTransport}).Get(url)
	if err != nil {
		t.Fatalf("request after a temporary accept error: %v (underlying accepts: %d)", err, fault.calls.Load())
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

type temporaryAcceptListener struct {
	net.Listener
	calls   atomic.Int32
	trigger <-chan struct{}
}

func (l *temporaryAcceptListener) Accept() (net.Conn, error) {
	if l.calls.Add(1) == 2 {
		<-l.trigger
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	return l.Listener.Accept()
}

func TestTLSSniffCloseReleasesPendingAcceptError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		listener := sniffTLS(failedAcceptListener{}, &tls.Config{}, time.Second)
		// No caller drains Accept: the worker is waiting to deliver its error.
		synctest.Wait()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // The bubble must end without a parked delivery goroutine.
	})
}

type failedAcceptListener struct{}

func (failedAcceptListener) Accept() (net.Conn, error) {
	return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
}
func (failedAcceptListener) Close() error   { return nil }
func (failedAcceptListener) Addr() net.Addr { return &net.TCPAddr{} }
