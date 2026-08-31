package harnessclient

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/transport"
)

// TestPageURLPathMatchesTransport is the drift guard for the route this
// package restates. harnessclient stays linkable without the transport
// server, so the path is spelled twice; a rename on the server side that
// missed this copy would make every `ao-harness open` hand out a URL from
// a route nobody serves.
func TestPageURLPathMatchesTransport(t *testing.T) {
	if pageURLPath != transport.PageURLPath {
		t.Fatalf("pageURLPath = %q, transport.PageURLPath = %q", pageURLPath, transport.PageURLPath)
	}
}

// bootstrapFor points a Bootstrap at a test responder. PageURL builds its
// endpoint from the port alone, so only the port travels.
func bootstrapFor(t *testing.T, srv *httptest.Server, token string) Bootstrap {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split %q: %v", srv.URL, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port %q: %v", port, err)
	}
	return Bootstrap{Port: n, Token: token}
}

// TestPageURL_PresentsTheTokenAsAHeader pins the carrier. The query slot
// on the transport's routes belongs to the one-time page ticket, and a
// header keeps the session credential out of process listings and logs.
func TestPageURL_PresentsTheTokenAsAHeader(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		gotPath = r.URL.Path
		// Trailing newline: the route writes one, and callers must not
		// paste it into a browser.
		_, _ = w.Write([]byte("http://127.0.0.1:5173/?t=fresh-ticket\n"))
	}))
	defer srv.Close()

	got, err := bootstrapFor(t, srv, "sess-token").PageURL(context.Background())
	if err != nil {
		t.Fatalf("PageURL: %v", err)
	}
	if want := "http://127.0.0.1:5173/?t=fresh-ticket"; got != want {
		t.Fatalf("PageURL = %q, want %q", got, want)
	}
	if gotAuth != "Bearer sess-token" {
		t.Fatalf("Authorization = %q, want the bearer form", gotAuth)
	}
	if gotQuery != "" {
		t.Fatalf("request carried a query string %q; the credential belongs in the header", gotQuery)
	}
	if gotPath != transport.PageURLPath {
		t.Fatalf("path = %q, want %q", gotPath, transport.PageURLPath)
	}
}

// TestPageURL_RefusesUnusableAnswers: every failure has to surface as an
// error, because the caller's fallback is the boot URL whose ticket is
// probably spent. A silent empty string would navigate a browser nowhere.
func TestPageURL_RefusesUnusableAnswers(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"credential refused": func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		},
		"not yet listening": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		},
		"empty body": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("  \n"))
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			if got, err := bootstrapFor(t, srv, "sess-token").PageURL(context.Background()); err == nil {
				t.Fatalf("PageURL returned %q, want an error", got)
			}
		})
	}

	// No credential to present: fail before opening a socket rather than
	// letting the instance answer 404 to an anonymous request.
	if _, err := (Bootstrap{Port: 1}).PageURL(context.Background()); err == nil {
		t.Fatal("PageURL accepted a bootstrap with no token")
	}
	if _, err := (Bootstrap{Token: "t"}).PageURL(context.Background()); err == nil {
		t.Fatal("PageURL accepted a bootstrap with no port")
	}
}
