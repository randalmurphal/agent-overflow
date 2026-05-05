package design

import (
	"bytes"
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

// TestInjectDiagnosticScript_DisablesFontEmbedding pins the
// load-bearing fix for the screenshot capture failure on the
// sandbox=allow-scripts iframe. modern-screenshot's default font
// embedding fetches @import / @font-face URLs via fetch() — which
// is rejected as cross-origin from the iframe's opaque origin and
// surfaces as "Unsafe attempt to load URL ... Domains, protocols
// and ports must match" + a 404, hard-failing both the agent's
// read_screenshot tool and the user's "Send to thread" button.
//
// Don't relax the assertion. If a future refactor drops the
// `font: false` option (or replaces it with a no-op), this test
// fails before the regression reaches the user. The failure mode
// is silent in unit tests because the capture path runs inside
// a real browser; only this script-content check catches it
// without a browser harness.
func TestInjectDiagnosticScript_DisablesFontEmbedding(t *testing.T) {
	body := []byte(`<!doctype html><html><body></body></html>`)
	out, ok := injectDiagnosticScript(body)
	if !ok {
		t.Fatal("inject returned ok=false on valid HTML")
	}
	got := string(out)
	if !strings.Contains(got, "font: false") {
		t.Fatalf("injected capture script missing `font: false` option; the modern-screenshot embedWebFont path will fetch @font-face / @import URLs from the iframe's opaque origin and the cross-origin fetch will reject the capture. Got:\n%s", got)
	}
}

// TestInjectDiagnosticScript_LoadsModernScreenshotFromSelfHostedURL
// pins the second half of the capture fix. WebKitGTK refuses dynamic
// ESM imports of cross-origin modules from a sandbox=allow-scripts
// (opaque-origin) iframe — the user saw "Failed to fetch dynamically
// imported module: https://esm.sh/modern-screenshot@4.13.0" before
// this fix. The replacement is a classic <script src> tag pointing at
// our own loopback file server, which serves the embedded UMD bundle
// with CORS headers (see TestFileHandler_ServesModernScreenshotAsset).
//
// If a future refactor reintroduces the esm.sh import, this test
// fails — same reason as the font-embedding pin: real-browser-only
// regression that's silent in unit tests.
func TestInjectDiagnosticScript_LoadsModernScreenshotFromSelfHostedURL(t *testing.T) {
	body := []byte(`<!doctype html><html><body></body></html>`)
	out, ok := injectDiagnosticScript(body)
	if !ok {
		t.Fatal("inject returned ok=false on valid HTML")
	}
	got := string(out)
	if !strings.Contains(got, "/design/_aoassets/modern-screenshot.js") {
		t.Fatalf("injected capture script does not reference the self-hosted modern-screenshot asset URL. Got:\n%s", got)
	}
	if strings.Contains(got, "esm.sh") || strings.Contains(got, "unpkg.com") || strings.Contains(got, "jsdelivr") {
		t.Fatalf("injected capture script still references an external CDN — dynamic imports from sandbox=allow-scripts iframes are blocked. Got:\n%s", got)
	}
}

func TestFileHandler_ServesModernScreenshotAsset(t *testing.T) {
	base := t.TempDir()
	srv := httptest.NewServer(FileHandler(base))
	defer srv.Close()

	// The asset path is the same the iframe-injected script imports.
	resp, err := http.Get(srv.URL + "/_aoassets/modern-screenshot.js")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/javascript") {
		t.Fatalf("Content-Type = %q, want application/javascript prefix", got)
	}
	// Loopback peers fetch the asset from the iframe's opaque origin;
	// without CORS the browser refuses to expose the response.
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want *", got)
	}
	body, err := readAllAndCloseServer(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) < 1024 {
		t.Fatalf("asset is only %d bytes — embedded bundle is missing or truncated", len(body))
	}
	// Sanity check we shipped the UMD bundle (which exposes
	// modernScreenshot on the global). If a future refactor swaps the
	// vendored file for the ESM build, the iframe's <script src>
	// loader can't pick up a global and capture fails.
	if !bytes.Contains(body, []byte("modernScreenshot")) {
		t.Fatal("vendored bundle missing the modernScreenshot global symbol; wrong build format?")
	}
}

// TestModernScreenshotBundle_PatchesOutSandboxIframe pins the
// runtime patch we apply to modern-screenshot's UMD bundle. The
// library's `vt(e)` helper creates an internal hidden iframe to read
// default browser styles, and inside our sandbox=allow-scripts parent
// iframe (opaque origin) that read fails with:
//
//	Blocked a frame with origin 'null' from accessing a cross-origin
//	frame.
//
// We short-circuit the iframe-creating expression so it never runs.
// If a future upstream bundle update changes the minified pattern,
// this test fails — the engineer re-derives the pattern and updates
// the constants in server.go. Without this guard, the screenshot
// path silently degrades back to the broken behavior the user saw
// before the patch.
func TestModernScreenshotBundle_PatchesOutSandboxIframe(t *testing.T) {
	if !bytes.Contains(modernScreenshotBundleRaw, []byte(modernScreenshotSandboxOriginal)) {
		t.Fatalf("vendored modern-screenshot bundle no longer contains the original sandbox-iframe pattern %q. A version bump may have shifted the minified output — re-derive the pattern from the new bundle and update modernScreenshotSandboxOriginal in server.go.", modernScreenshotSandboxOriginal)
	}
	if !bytes.Contains(modernScreenshotBundle, []byte(modernScreenshotSandboxPatched)) {
		t.Fatalf("runtime patch failed to apply: served bundle does not contain %q. Either the bytes.Replace at modernScreenshotBundle didn't run, or the original pattern was missing (see other test).", modernScreenshotSandboxPatched)
	}
	if bytes.Contains(modernScreenshotBundle, []byte(modernScreenshotSandboxOriginal)) {
		t.Fatalf("served bundle still contains the un-patched sandbox-iframe pattern %q. The runtime patch should replace exactly one occurrence; verify there is only one match in the source.", modernScreenshotSandboxOriginal)
	}
}

func TestFileHandler_ModernScreenshotAssetIsNotInjectionWrapped(t *testing.T) {
	// The injection middleware only touches text/html responses; this
	// test pins that contract for the JS asset. If the middleware ever
	// starts rewriting JS, the iframe's <script> tag will execute a
	// rewritten body and fail to expose modernScreenshot — same hard
	// failure as the dynamic-import case. Pin so the regression fires
	// before users hit it.
	base := t.TempDir()
	srv := httptest.NewServer(FileHandler(base))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/_aoassets/modern-screenshot.js")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	body, err := readAllAndCloseServer(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if bytes.Contains(body, []byte("__aoDesignBootstrap")) {
		t.Fatal("JS asset got wrapped with the diagnostic script — InjectionMiddleware mistakenly treated it as HTML")
	}
}

// readAllAndCloseServer is a small helper for the new asset-route
// tests; the existing readAll in this file uses a fixed-size loop.
func readAllAndCloseServer(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
