package mcpprobe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/mcp"
	"agent-overflow/internal/store"
)

func TestProbeHTTP_Ready(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"fake","version":"0.1"}}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := Probe(ctx, store.MCPServer{ID: "x", Transport: mcp.TransportHTTP, URL: srv.URL, Enabled: true})

	if got.Status != mcp.StatusReady {
		t.Fatalf("status = %v, want ready (err=%q)", got.Status, got.Error)
	}
	if got.ProtocolVer != "2025-06-18" {
		t.Errorf("protocolVersion = %q", got.ProtocolVer)
	}
	if got.ServerName != "fake" {
		t.Errorf("serverName = %q", got.ServerName)
	}
}

func TestProbeHTTP_NeedsAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="example"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	got := Probe(context.Background(), store.MCPServer{ID: "x", Transport: mcp.TransportHTTP, URL: srv.URL, Enabled: true})
	if got.Status != mcp.StatusNeedsAuth {
		t.Fatalf("status = %v, want needs-auth", got.Status)
	}
	if !strings.Contains(got.Error, "Bearer") {
		t.Errorf("error should carry WWW-Authenticate: %q", got.Error)
	}
}

func TestProbeHTTP_NeedsAuthWithoutWWWAuthenticate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	got := Probe(context.Background(), store.MCPServer{ID: "x", Transport: mcp.TransportHTTP, URL: srv.URL, Enabled: true})
	if got.Status != mcp.StatusNeedsAuth {
		t.Fatalf("status = %v, want needs-auth", got.Status)
	}
	if got.Error != "HTTP 401" {
		t.Errorf("error = %q, want HTTP 401 fallback", got.Error)
	}
}

func TestProbeHTTP_FailedOn500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`internal boom`))
	}))
	defer srv.Close()

	got := Probe(context.Background(), store.MCPServer{ID: "x", Transport: mcp.TransportHTTP, URL: srv.URL, Enabled: true})
	if got.Status != mcp.StatusFailed {
		t.Fatalf("status = %v, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "HTTP 500") {
		t.Errorf("error should mention HTTP 500: %q", got.Error)
	}
}

func TestProbeHTTP_FailedOnInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	got := Probe(context.Background(), store.MCPServer{ID: "x", Transport: mcp.TransportHTTP, URL: srv.URL, Enabled: true})
	if got.Status != mcp.StatusFailed {
		t.Fatalf("status = %v, want failed", got.Status)
	}
}

func TestProbeHTTP_SSEContentTypeIsReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: hello\ndata: {}\n\n"))
	}))
	defer srv.Close()

	got := Probe(context.Background(), store.MCPServer{ID: "x", Transport: mcp.TransportHTTP, URL: srv.URL, Enabled: true})
	if got.Status != mcp.StatusReady {
		t.Fatalf("status = %v, want ready for SSE content-type", got.Status)
	}
}

func TestProbeHTTP_CustomHeaderAndBearerEnv(t *testing.T) {
	t.Setenv("AO_TEST_TOKEN", "secret-value")
	var gotAuth, gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCustom = r.Header.Get("X-Custom")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"x","capabilities":{},"serverInfo":{"name":"f","version":"0"}}}`))
	}))
	defer srv.Close()

	_ = Probe(context.Background(), store.MCPServer{
		ID:        "x",
		Transport: mcp.TransportHTTP,
		URL:       srv.URL,
		Headers:   map[string]string{"X-Custom": "ok"},
		BearerEnv: "AO_TEST_TOKEN",
		Enabled:   true,
	})
	if gotAuth != "Bearer secret-value" {
		t.Errorf("Authorization header = %q", gotAuth)
	}
	if gotCustom != "ok" {
		t.Errorf("X-Custom header = %q", gotCustom)
	}
}

func TestProbeHTTP_FailedOnUnreachableURL(t *testing.T) {
	// Bind a listener, immediately close it. The port is now unused; any
	// connection attempt fails.
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	got := Probe(ctx, store.MCPServer{ID: "x", Transport: mcp.TransportHTTP, URL: addr, Enabled: true})
	if got.Status != mcp.StatusFailed {
		t.Fatalf("status = %v, want failed", got.Status)
	}
}
