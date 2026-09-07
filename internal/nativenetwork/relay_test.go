package nativenetwork

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/servercert"
)

func TestRelayTargetCannotTurnNetworkClientsIntoLocalPeers(t *testing.T) {
	for _, target := range []string{"https://127.0.0.1:1234", "https://localhost:1234", "https://[::1]:1234", "https://[::ffff:127.0.0.1]:1234", "http://192.168.1.3:1234", "https://public.example:1234", "https://192.168.1.3", "https://192.168.1.3:1234/path", "https://user@192.168.1.3:1234", "https://8.8.8.8:443"} {
		if _, _, err := relayTarget(target); err == nil {
			t.Errorf("accepted unsafe target %s", target)
		}
	}
	if target, port, err := relayTarget("https://172.20.3.4:4242"); err != nil || target != "172.20.3.4:4242" || port != "4242" {
		t.Fatalf("target=%s port=%s error=%v", target, port, err)
	}
}

// Bypass production private-IP admission only inside the byte-pipe fixture;
// all sockets stay on loopback and no developer LAN listener is exposed.
func testRelay(t *testing.T, target string) (string, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	relay := &Relay{target: target, slots: make(chan struct{}, maxConnections)}
	done := make(chan struct{})
	go func() { relay.accept(ctx, listener); relay.wg.Wait(); close(done) }()
	stop := func() { cancel(); listener.Close() }
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("relay did not close")
		}
	})
	return listener.Addr().String(), stop, done
}

func TestRelayPreservesHostTLSAndDoesNotInjectCredentials(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Ao-Session") != "" || r.Header.Get("X-Forwarded-For") != "" {
			t.Error("relay injected HTTP authority")
		}
		if r.Host != "chosen-host.invalid" {
			t.Errorf("changed authority to %s", r.Host)
		}
		io.Copy(w, r.Body)
	}))
	defer server.Close()
	address, _, _ := testRelay(t, server.Listener.Addr().String())
	transport := deviceclient.NewPinnedTransport(servercert.Fingerprint(server.Certificate().Raw))
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	request, err := http.NewRequest(http.MethodPost, "https://"+address, strings.NewReader("original payload"))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "chosen-host.invalid"
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "original payload" {
		t.Fatalf("body=%q error=%v", body, err)
	}
}

func TestRelayHalfCloseLetsHostFinishAndCancellationClosesActiveStreams(t *testing.T) {
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := upstream.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	address, stop, done := testRelay(t, upstream.Addr().String())
	client, err := net.Dial("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(client, "request")
	client.(*net.TCPConn).CloseWrite()
	var host net.Conn
	select {
	case host = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream was not reached")
	}
	defer host.Close()
	host.SetDeadline(time.Now().Add(3 * time.Second))
	request, err := io.ReadAll(host)
	if err != nil || string(request) != "request" {
		t.Fatalf("request=%q error=%v", request, err)
	}
	if _, err := io.WriteString(host, "response after request EOF"); err != nil {
		t.Fatal(err)
	}
	host.(*net.TCPConn).CloseWrite()
	response, err := io.ReadAll(client)
	if err != nil || string(response) != "response after request EOF" {
		t.Fatalf("response=%q error=%v", response, err)
	}
	// A second idle connection must not hold shutdown indefinitely.
	idle, err := net.Dial("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("active stream survived shutdown")
	}
}

// A bound Windows listener does not prove the backend is reachable. Keep the
// latest upstream failure visible in host settings, and clear it on recovery.
func TestRelayReportsUpstreamFailureAndClearsItAfterRecovery(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := listener.Addr().String()
	listener.Close()
	relay := &Relay{target: target}
	carry := func() {
		client, server := net.Pipe()
		done := make(chan struct{})
		go func() { relay.carry(context.Background(), server); close(done) }()
		client.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("relay did not finish")
		}
	}
	carry()
	if err := relay.Err(); err == nil || !strings.Contains(err.Error(), "cannot reach the WSL backend") {
		t.Fatalf("missing host-side upstream error: %v", err)
	}
	listener, err = net.Listen("tcp4", target)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			conn.Close()
		}
	}()
	carry()
	if err := relay.Err(); err != nil {
		t.Fatalf("recovery retained stale error: %v", err)
	}
}
