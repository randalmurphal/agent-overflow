package filepreview

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/transport"
)

func writePage(t *testing.T, dir, name, content string) string {
	t.Helper()
	file := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestFilesContainmentAndHTTP(t *testing.T) {
	dir := t.TempDir()
	writePage(t, dir, "pages/index.html", "<h1>preview</h1>")
	writePage(t, dir, "assets/main.js", "document.title='ready'")
	writePage(t, dir, ".env", "SECRET")
	writePage(t, dir, ".git/config", "SECRET")
	outside := writePage(t, t.TempDir(), "secret.html", "OUTSIDE")
	if err := os.Symlink(outside, filepath.Join(dir, "escape.html")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	handler := files{root}
	for _, tt := range []struct {
		path, method string
		code         int
		body         string
	}{
		{"/pages/index.html", "GET", 200, "<h1>preview</h1>"},
		{"/pages/", "GET", 200, "<h1>preview</h1>"},
		{"/assets/main.js", "GET", 200, "document.title='ready'"},
		{"/pages/index.html", "HEAD", 200, ""},
		{"/pages/index.html", "POST", 405, ""},
		{"/.env", "GET", 404, ""},
		{"/.git/config", "GET", 404, ""},
		{"/escape.html", "GET", 404, ""},
		{"/../secret.html", "GET", 404, ""},
		{"/assets/../pages/index.html", "GET", 404, ""},
		{"/assets/", "GET", 404, ""},
		{"/assets", "GET", 404, ""},
		{"/missing.html", "GET", 404, ""},
		{"/pages%5cindex.html", "GET", 404, ""},
	} {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(tt.method, tt.path, nil))
			if rr.Code != tt.code {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			if tt.code == 200 && rr.Body.String() != tt.body {
				t.Fatalf("body %q", rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), "SECRET") || strings.Contains(rr.Body.String(), "OUTSIDE") {
				t.Fatal("leaked file")
			}
			if rr.Header().Get("Cache-Control") != "no-store" || rr.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("missing response policy")
			}
		})
	}
	request := httptest.NewRequest("GET", "/pages/index.html", nil)
	request.Header.Set("Range", "bytes=4-10")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)
	if rr.Code != http.StatusPartialContent || rr.Body.String() != "preview" {
		t.Fatalf("range: %d %q", rr.Code, rr.Body.String())
	}
	request.Header.Set("Service-Worker", "script")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, request)
	if rr.Code != 403 {
		t.Fatal("service worker admitted")
	}
}

func TestResolveUsesWorkspaceOrContainingFolder(t *testing.T) {
	dir := t.TempDir()
	file := writePage(t, dir, "pages/a #?.HTML", "page")
	root, target, err := resolve(file, dir)
	if err != nil || root != dir || target != "pages/a #?.HTML" {
		t.Fatalf("%q %q %v", root, target, err)
	}
	if previewPath(target) != "/pages/a%20%23%3F.HTML" {
		t.Fatal(previewPath(target))
	}
	root, target, err = resolve(file, t.TempDir())
	if err != nil || root != filepath.Dir(file) || target != "a #?.HTML" {
		t.Fatalf("%q %q %v", root, target, err)
	}
	for _, name := range []string{"x.txt", ".hidden.html", ".private/x.html"} {
		writePage(t, dir, name, "bad")
		if _, _, err := resolve(filepath.Join(dir, name), dir); err == nil {
			t.Fatalf("admitted %s", name)
		}
	}
}

func TestLocalPreviewTicketsAndManagerLifetime(t *testing.T) {
	dir := t.TempDir()
	file := writePage(t, dir, "a #?.html", "<h1>ready</h1>")
	m := New(transport.PreviewGatewayConfig{})
	t.Cleanup(m.Close)
	link, err := m.Open(file, dir, "", true)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		t.Fatalf("not literal loopback: %s", link)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	response, err := client.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || string(body) != "<h1>ready</h1>" {
		t.Fatalf("%d %q %v", response.StatusCode, body, err)
	}
	if response.Request.URL.RawQuery != "" {
		t.Fatal("ticket remains in address")
	}
	unauthorized, err := (&http.Client{Timeout: 3 * time.Second}).Get(response.Request.URL.String())
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != 401 {
		t.Fatalf("unauthorized status %d", unauthorized.StatusCode)
	}
	if _, err := m.Open(file, dir, "remote-session", false); err == nil {
		t.Fatal("remote session fell back to cleartext loopback")
	}
	if len(m.entries) != 1 {
		t.Fatal("failed remote open leaked an entry")
	}
	m.Close()
	if _, err := m.Open(file, dir, "", true); err == nil {
		t.Fatal("closed manager reopened")
	}
}

func TestDirectoryBudgetRetiresOldest(t *testing.T) {
	m := New(transport.PreviewGatewayConfig{})
	t.Cleanup(m.Close)
	var oldest *entry
	for i := 0; i <= maxDirectories; i++ {
		dir := t.TempDir()
		file := writePage(t, dir, "index.html", "page")
		if _, err := m.Open(file, dir, "", true); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			oldest = m.entries[key{dir, true}]
		}
	}
	if len(m.entries) != maxDirectories {
		t.Fatal("directory budget exceeded")
	}
	if _, err := oldest.gateway.MintURL("", oldest.port, "/index.html"); err == nil {
		t.Fatal("retired directory retained authorization")
	}
	if err := oldest.check("index.html"); err == nil {
		t.Fatal("retired root remained open")
	}
}
