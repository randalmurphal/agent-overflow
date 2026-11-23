package app

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/transport"
)

// listeningPreviewSource binds real loopback listeners and remembers each
// port's, so a test can reach the gateway as a browser does.
type listeningPreviewSource struct {
	mu        sync.Mutex
	listeners map[int]net.Listener
}

func (s *listeningPreviewSource) PreviewHost() string { return "backend.test" }

func (s *listeningPreviewSource) ListenPreview(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.listeners == nil {
		s.listeners = make(map[int]net.Listener)
	}
	s.listeners[port] = ln
	s.mu.Unlock()
	return ln, nil
}

func (s *listeningPreviewSource) addr(port int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listeners[port].Addr().String()
}

// An idle discovery tick, with nobody off this machine reading the list,
// keeps every listener while a browser holds a live grant: that browser
// was admitted on its own and outlives the app connection that handed it
// the preview. The tick goes through the app's own idle path
// (devServerScanTick), the one a browser's preview meets once the phone
// that opened it leaves.
func TestAnIdleTickKeepsTheListenersOfALiveBrowserGrant(t *testing.T) {
	t.Parallel()
	app := newPreviewTestApp(t, &fakeScanner{})
	source := &listeningPreviewSource{}
	gateway := transport.NewPreviewGateway(transport.PreviewGatewayConfig{
		Sources:     []transport.PreviewListenerSource{source},
		SessionLive: func(string) bool { return true },
	})
	t.Cleanup(gateway.Close)
	app.preview.mu.Lock()
	app.preview.gateway = gateway
	app.preview.mu.Unlock()
	gateway.SetPorts([]transport.PreviewTarget{{Port: 5173, Scheme: "http"}, {Port: 3000, Scheme: "http"}})
	served := gateway.Ports()

	minted, err := gateway.MintURL("phone", 3000, "/app/")
	if err != nil {
		t.Fatalf("MintURL: %v", err)
	}
	parsed, err := url.Parse(minted)
	if err != nil {
		t.Fatalf("parse %q: %v", minted, err)
	}
	browser := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := browser.Get("http://" + source.addr(3000) + parsed.RequestURI())
	if err != nil {
		t.Fatalf("spend the ticket: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || len(resp.Cookies()) != 1 {
		t.Fatalf("ticket exchange = %d with %d cookies, want a 302 granting one", resp.StatusCode, len(resp.Cookies()))
	}

	if app.devServerScanWanted() {
		t.Fatal("the fixture has a reader off this machine; the tick would scan instead of idling")
	}
	if !app.devServerScanTick(context.Background()) {
		t.Fatal("the idle tick stopped the loop")
	}
	if got := gateway.Ports(); len(got) != len(served) {
		t.Fatalf("ports = %v after an idle tick, want %v kept for the live browser grant", got, served)
	}
}
