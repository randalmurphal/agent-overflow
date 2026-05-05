package design

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileHandler_ServesFromBaseDir(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "t1", "main"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	body := `<!doctype html><html><head><title>x</title></head><body><p>hi</p></body></html>`
	if err := os.WriteFile(filepath.Join(base, "t1", "main", "index.html"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := httptest.NewServer(FileHandler(base))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/t1/main/index.html")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got, err := readAll(resp)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	if !strings.Contains(got, "<p>hi</p>") {
		t.Fatalf("body missing original content: %s", got)
	}
	// The injected script must land inside <head>.
	if !strings.Contains(got, "__aoDesignBootstrap") {
		t.Fatalf("body missing injected diagnostic script: %s", got)
	}
	headIdx := strings.Index(got, "<head>")
	scriptIdx := strings.Index(got, "<script>")
	if headIdx < 0 || scriptIdx < 0 || scriptIdx < headIdx {
		t.Fatalf("script not inside <head>: head=%d script=%d", headIdx, scriptIdx)
	}
	// ETag / Last-Modified must be stripped — otherwise a conditional GET
	// would replay an un-injected response.
	if got := resp.Header.Get("ETag"); got != "" {
		t.Fatalf("ETag = %q, want empty after injection", got)
	}
	if got := resp.Header.Get("Last-Modified"); got != "" {
		t.Fatalf("Last-Modified = %q, want empty after injection", got)
	}
}

func TestFileHandler_StreamsNonHTMLUnmodified(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "t1", "main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cssBody := "body { background: red; }"
	if err := os.WriteFile(filepath.Join(dir, "style.css"), []byte(cssBody), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := httptest.NewServer(FileHandler(base))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/t1/main/style.css")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	got, err := readAll(resp)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	if got != cssBody {
		t.Fatalf("CSS body modified: got %q want %q", got, cssBody)
	}
	if strings.Contains(got, "__aoDesignBootstrap") {
		t.Fatalf("CSS got the diagnostic script injected")
	}
}

func TestInjectDiagnosticScript_AddsHeadWhenAbsent(t *testing.T) {
	body := []byte(`<!doctype html><html><body><p>plain</p></body></html>`)
	out, ok := injectDiagnosticScript(body)
	if !ok {
		t.Fatal("inject returned ok=false on valid HTML")
	}
	got := string(out)
	if !strings.Contains(got, "<head>") {
		t.Fatalf("output missing <head>: %s", got)
	}
	if !strings.Contains(got, "__aoDesignBootstrap") {
		t.Fatalf("output missing script: %s", got)
	}
	if !strings.Contains(got, "<p>plain</p>") {
		t.Fatalf("output dropped original content: %s", got)
	}
}

func TestInjectDiagnosticScript_PreservesExistingHeadContent(t *testing.T) {
	body := []byte(`<!doctype html><html><head><title>existing</title><meta charset="utf-8"></head><body></body></html>`)
	out, ok := injectDiagnosticScript(body)
	if !ok {
		t.Fatal("inject returned ok=false")
	}
	got := string(out)
	if !strings.Contains(got, "<title>existing</title>") {
		t.Fatalf("dropped existing <title>: %s", got)
	}
	if !strings.Contains(got, `<meta charset="utf-8"`) {
		t.Fatalf("dropped existing <meta>: %s", got)
	}
	if !strings.Contains(got, "__aoDesignBootstrap") {
		t.Fatalf("missing script: %s", got)
	}
	// Script should appear before the existing <title>.
	scriptIdx := strings.Index(got, "__aoDesignBootstrap")
	titleIdx := strings.Index(got, "<title>existing</title>")
	if scriptIdx < 0 || titleIdx < 0 || scriptIdx > titleIdx {
		t.Fatalf("script not prepended into head: script=%d title=%d", scriptIdx, titleIdx)
	}
}

func TestFileHandler_404ForMissingFile(t *testing.T) {
	base := t.TempDir()
	srv := httptest.NewServer(FileHandler(base))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/no-such-thread/main/index.html")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func readAll(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	buf := make([]byte, 0, 1024)
	chunk := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return string(buf), nil
			}
			return string(buf), err
		}
	}
}
