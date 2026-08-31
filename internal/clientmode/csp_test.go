package clientmode

import (
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/transport"
)

// TestStubShipsTheProductionPolicy pins the stub to the same policy the
// transport ships. The stub serves the SAME embedded bundle over its own
// origin, so a divergence here would mean the app is governed by one
// policy in the webview and another under --connect — and the mode that
// exists specifically to reach a remote backend would be the weaker one.
//
// It is CSPProduction unconditionally: this stub has no dev-server mode.
// It serves Config.Assets, which is always the embedded release bundle.
func TestStubShipsTheProductionPolicy(t *testing.T) {
	srv := serveStub(t, Config{WSURL: "ws://h/", Token: "t"})
	base := "http://" + srv.Addr()

	for _, tc := range []struct {
		name string
		path string
	}{
		{"spa shell", "/"},
		{"hashed asset", "/assets/index-abc.js"},
		{"root bundle file", "/boot-theme.js"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(base + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s: status %d, want 200", tc.path, resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Security-Policy"); got != string(transport.CSPProduction) {
				t.Errorf("GET %s: Content-Security-Policy = %q, want the production policy", tc.path, got)
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("GET %s: X-Content-Type-Options = %q, want nosniff", tc.path, got)
			}
		})
	}
}

// TestStubServesRootBundleFilesAsThemselves is the reason the mux uses
// "/{$}" for the shell. When the shell answered every unmatched path,
// /boot-theme.js came back as text/html — and under nosniff the browser
// refuses to run that as script, so the first-paint theme stamp silently
// stopped applying on this origin only. /favicon.svg was broken the same
// way, more visibly and less consequentially.
func TestStubServesRootBundleFilesAsThemselves(t *testing.T) {
	srv := serveStub(t, Config{WSURL: "ws://h/", Token: "t"})

	for _, tc := range []struct {
		path        string
		wantType    string
		wantContent string
	}{
		{"/boot-theme.js", "javascript", "/* fake boot theme */"},
		{"/favicon.svg", "image/svg+xml", "<svg"},
	} {
		resp, err := http.Get("http://" + srv.Addr() + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		body := make([]byte, 256)
		n, _ := resp.Body.Read(body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", tc.path, resp.StatusCode)
			continue
		}
		if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, tc.wantType) {
			t.Errorf("GET %s: Content-Type = %q, want it to name %s", tc.path, contentType, tc.wantType)
		}
		if !strings.Contains(string(body[:n]), tc.wantContent) {
			t.Errorf("GET %s: body %q does not contain %q", tc.path, body[:n], tc.wantContent)
		}
	}
}

// TestStubDoesNotAnswerUnknownPathsWithTheShell matches the transport,
// which 404s them. The SPA has no client-side router, so an unknown path
// is a missing bundle file, and answering it with HTML both hides that
// and puts a second copy of the document on every path a link can name.
func TestStubDoesNotAnswerUnknownPathsWithTheShell(t *testing.T) {
	srv := serveStub(t, Config{WSURL: "ws://h/", Token: "t"})

	resp, err := http.Get("http://" + srv.Addr() + "/not-a-bundle-file")
	if err != nil {
		t.Fatalf("GET /not-a-bundle-file: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
