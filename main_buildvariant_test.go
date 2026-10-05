package main

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-overflow/internal/buildvariant"
)

// The SPA learns this build has no remote access from its entry shell,
// before anything renders. A build with remote access serves the shell
// byte for byte as built.
func TestEntryShellCarriesTheBuildVariant(t *testing.T) {
	handler, _, _, err := buildAssetHandler(testAssets, false)
	if err != nil {
		t.Fatalf("buildAssetHandler: %v", err)
	}
	built, err := fs.ReadFile(testAssets, "frontend/dist/index.html")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?host=webview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if buildvariant.RemoteAccess {
		if string(body) != string(built) {
			t.Fatal("a build with remote access changed the entry shell")
		}
	} else {
		if !strings.Contains(string(body), remoteAccessOffMeta) {
			t.Fatalf("the entry shell does not carry %s", remoteAccessOffMeta)
		}
		if strings.Replace(string(body), remoteAccessOffMeta, "", 1) != string(built) {
			t.Fatal("the entry shell changed beyond the build-variant tag")
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("Content-Type = %q, want text/html", ct)
		}
	}

	// /index.html still redirects to the one path that serves the shell.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "./" {
		t.Fatalf("GET /index.html = %d %q, want the file server's redirect to ./", rec.Code, rec.Header().Get("Location"))
	}
}
