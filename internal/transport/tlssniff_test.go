package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"agent-overflow/internal/servercert"
)

// The certificate under test is the real one the boot mints. A hand-made
// one in this file would prove that this listener serves SOME
// certificate; the point is that it serves THAT one, and that the
// fingerprint a pinning client compares is the fingerprint the pairing
// payload carries.
func tlsFixture(t *testing.T, mutate func(*Config)) (*Server, servercert.Material) {
	t.Helper()
	material, err := servercert.Load(t.TempDir())
	if err != nil {
		t.Fatalf("mint a certificate: %v", err)
	}
	srv := newServerFixtureWith(t, func(cfg *Config) {
		cfg.TLSCertificate = &material.Certificate
		if mutate != nil {
			mutate(cfg)
		}
	}).srv
	return srv, material
}

// pinnedTLSConfig is what a Go-native client does with the fingerprint
// the pairing payload handed it: no CA to verify against, and the
// certificate's exact bytes as the whole check.
func pinnedTLSConfig(material servercert.Material) *tls.Config {
	return &tls.Config{
		// No chain to build: the check below is stricter than a CA
		// signature, not weaker than one.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("the peer presented no certificate")
			}
			if got := servercert.Fingerprint(rawCerts[0]); got != material.Fingerprint {
				return fmt.Errorf("presented %s, pinned %s", got, material.Fingerprint)
			}
			return nil
		},
		MinVersion: tls.VersionTLS12,
	}
}

func pinnedClient(material servercert.Material) *http.Client {
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: pinnedTLSConfig(material)},
	}
}

// One port answers both, which is the whole shape: the pinned endpoint
// and the browser's cleartext one are the same authority, so the share
// URL, the cookie names and the origin allow-list all keep describing
// one listener.
func TestTLSAndCleartextShareTheOnePort(t *testing.T) {
	srv, material := tlsFixture(t, nil)

	secure, err := pinnedClient(material).Get("https://" + srv.Addr() + HealthPath)
	if err != nil {
		t.Fatalf("TLS request: %v", err)
	}
	defer secure.Body.Close()
	if secure.StatusCode != http.StatusOK {
		t.Fatalf("TLS %s = %d, want 200", HealthPath, secure.StatusCode)
	}
	if secure.TLS == nil {
		t.Fatal("the response the client read was not over TLS")
	}

	plain, err := http.Get("http://" + srv.Addr() + HealthPath)
	if err != nil {
		t.Fatalf("cleartext request: %v", err)
	}
	defer plain.Body.Close()
	if plain.StatusCode != http.StatusOK {
		t.Fatalf("cleartext %s = %d, want 200 — a browser still has to reach this listener", HealthPath, plain.StatusCode)
	}
}

// A certificate other than the one the pairing payload named must not
// satisfy a pinning client. This is the check the whole wave exists for,
// so it is asserted rather than assumed of crypto/tls.
func TestAPinningClientRefusesAnotherCertificate(t *testing.T) {
	srv, _ := tlsFixture(t, nil)
	other, err := servercert.Load(t.TempDir())
	if err != nil {
		t.Fatalf("mint a second certificate: %v", err)
	}
	if _, err := pinnedClient(other).Get("https://" + srv.Addr() + HealthPath); err == nil {
		t.Fatal("a client pinning a different certificate completed the handshake")
	}
}

// The upgrade has to survive TLS: the WebSocket library takes the raw
// connection over from net/http and reads and writes through it, and here
// that connection is a *tls.Conn.
func TestWebSocketUpgradesOverTLS(t *testing.T) {
	srv, material := tlsFixture(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "wss://"+srv.Addr()+"/ws?token=test-token", &websocket.DialOptions{
		HTTPClient: pinnedClient(material),
	})
	if err != nil {
		t.Fatalf("dial wss: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// The hello frame is written synchronously before anything else, so
	// reading it proves the socket carries real traffic in both
	// directions rather than merely completing a handshake.
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read the hello frame over TLS: %v", err)
	}
	var hello ServerFrame
	if err := json.Unmarshal(data, &hello); err != nil {
		t.Fatalf("decode the hello frame: %v", err)
	}
	if hello.Type != "hello" {
		t.Fatalf("first frame over TLS = %q, want hello", hello.Type)
	}
}

// Over TLS the manifest names a wss:// socket and the cookie that rides
// the same exchange is Secure. Both are downstream of r.TLS being set,
// which is why the sniff hands net/http a real *tls.Conn.
func TestBootstrapOverTLSNamesWSSAndSetsASecureCookie(t *testing.T) {
	srv, material := tlsFixture(t, nil)

	url := fmt.Sprintf("https://%s/bootstrap.json?%s=%s", srv.Addr(), PageTicketParam, pageURLTicket(t, srv))
	resp, err := pinnedClient(material).Get(url)
	if err != nil {
		t.Fatalf("bootstrap over TLS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap over TLS = %d, want 200", resp.StatusCode)
	}
	cookie := pageCookieFrom(t, resp)
	if !cookie.Secure {
		t.Error("the page cookie planted over TLS is not Secure")
	}
	var manifest Bootstrap
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	if !strings.HasPrefix(manifest.WSURL, "wss://") {
		t.Fatalf("wsUrl over TLS = %q, want a wss:// URL", manifest.WSURL)
	}
}

// The cleartext half of the same server keeps its old answers, so a
// browser that cannot pin is unaffected by the certificate existing.
func TestBootstrapOverCleartextIsUnchangedByTheCertificate(t *testing.T) {
	srv, _ := tlsFixture(t, nil)

	resp := bootstrapWithTicket(t, srv.Addr(), pageURLTicket(t, srv))
	defer resp.Body.Close()
	if cookie := pageCookieFrom(t, resp); cookie.Secure {
		t.Error("a cleartext exchange planted a Secure cookie, which the browser would drop")
	}
	var manifest Bootstrap
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	if !strings.HasPrefix(manifest.WSURL, "ws://") {
		t.Fatalf("wsUrl over cleartext = %q, want a ws:// URL", manifest.WSURL)
	}
}

// A LAN toggle rebinds the one listener, and TLS has to come with it: a
// paired client that pinned this certificate would read a bind without
// it as the backend having vanished.
func TestRebindKeepsTerminatingTLS(t *testing.T) {
	srv, material := tlsFixture(t, nil)

	if err := srv.Rebind("127.0.0.1:0", nil); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	resp, err := pinnedClient(material).Get("https://" + srv.Addr() + HealthPath)
	if err != nil {
		t.Fatalf("TLS request after rebind: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("TLS %s after rebind = %d, want 200", HealthPath, resp.StatusCode)
	}
}

// A peer that connects and says nothing is closed on its own deadline,
// and reaches nothing else: the classification runs off the accept loop,
// so another client is served while the silent connection is still
// parked.
func TestASilentConnectionIsClosedWithoutHoldingUpTheAcceptLoop(t *testing.T) {
	const window = 300 * time.Millisecond
	srv, material := tlsFixture(t, func(cfg *Config) {
		cfg.HTTPReadHeaderTimeout = window
	})

	silent, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer silent.Close()

	// Still inside the silent connection's window: a client that speaks
	// must be answered anyway.
	resp, err := pinnedClient(material).Get("https://" + srv.Addr() + HealthPath)
	if err != nil {
		t.Fatalf("a second client was not served while one connection stalled: %v", err)
	}
	_ = resp.Body.Close()

	if err := silent.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := silent.Read(make([]byte, 1)); err == nil {
		t.Fatal("a connection that sent nothing was left open")
	}
}

// The three scheme arms of the manifest's socket URL. The forwarded
// header is the proxy case the spec calls out; a value that is not the
// declared spelling lands on the cleartext answer rather than being
// guessed at.
func TestDeriveWSURLTakesItsSchemeFromTheRequest(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*http.Request)
		want    string
	}{
		{
			name:    "plain request",
			prepare: func(*http.Request) {},
			want:    "ws://backend.example:7423/ws",
		},
		{
			name:    "request that arrived over TLS",
			prepare: func(r *http.Request) { r.TLS = &tls.ConnectionState{} },
			want:    "wss://backend.example:7423/ws",
		},
		{
			name: "TLS-terminating proxy in front",
			prepare: func(r *http.Request) {
				r.Header.Set(ForwardedProtoHeader, "https")
			},
			want: "wss://backend.example:7423/ws",
		},
		{
			name: "proxy that says the client spoke cleartext",
			prepare: func(r *http.Request) {
				r.Header.Set(ForwardedProtoHeader, "http")
			},
			want: "ws://backend.example:7423/ws",
		},
		{
			name: "a value nobody declared",
			prepare: func(r *http.Request) {
				r.Header.Set(ForwardedProtoHeader, "https, http")
			},
			want: "ws://backend.example:7423/ws",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://backend.example:7423/bootstrap.json", nil)
			r.Host = "backend.example:7423"
			test.prepare(r)
			if got := deriveWSURL(r); got != test.want {
				t.Fatalf("deriveWSURL = %q, want %q", got, test.want)
			}
		})
	}
}

// The wrapper replays the byte the sniff consumed and then gets out of
// the way. Read is the one method it overrides, so the one thing that
// can go wrong is the boundary between the replayed byte and the socket.
func TestPeekedConnReplaysTheSniffedByteExactlyOnce(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	go func() {
		_, _ = client.Write([]byte("GET / HTTP/1.1\r\n"))
	}()

	peeked := &peekedConn{Conn: server}
	if _, err := io.ReadFull(server, peeked.first[:]); err != nil {
		t.Fatalf("sniff: %v", err)
	}
	rest, err := io.ReadAll(io.LimitReader(peeked, int64(len("GET / HTTP/1.1\r\n"))))
	if err != nil {
		t.Fatalf("read through the wrapper: %v", err)
	}
	if string(rest) != "GET / HTTP/1.1\r\n" {
		t.Fatalf("wrapper delivered %q, want the whole request line back", rest)
	}
}
