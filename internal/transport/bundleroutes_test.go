package transport

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"testing/fstest"

	"agent-overflow/internal/bundle"
)

// bundleFixture is a running server serving one small SPA, with a session
// resolver a test drives from a request header.
//
// The resolver mirrors what the App's real one does with the shape that
// matters here: a request naming no session resolves to the empty id,
// which is what `sessionAdmitsRequest` reads as "unauthenticated". The
// device proof itself is the identity core's business and is exercised
// there; what these tests pin is that these two routes ask the same
// question `/bootstrap.json` asks and refuse the same callers.
type bundleFixture struct {
	*serverFixture
	spa *bundle.Bundle
}

const bundleSessionHeader = "X-Test-Session"

func newBundleFixture(t *testing.T, withBundle bool) *bundleFixture {
	t.Helper()
	tree := fstest.MapFS{
		"index.html":        &fstest.MapFile{Data: []byte("<!doctype html><title>spa</title>")},
		"assets/app.js":     &fstest.MapFile{Data: []byte("export const a = 1;\n")},
		"assets/app.js.map": &fstest.MapFile{Data: []byte(`{"version":3}`)},
		"assets/app.css":    &fstest.MapFile{Data: []byte("body{margin:0}")},
	}
	spa := bundle.New(tree, "9.9.9")
	fixture := newServerFixtureWith(t, func(cfg *Config) {
		cfg.SessionForRequest = func(r *http.Request) (string, bool) {
			return r.Header.Get(bundleSessionHeader), true
		}
		if withBundle {
			cfg.Bundle = spa
		}
	})
	return &bundleFixture{serverFixture: fixture, spa: spa}
}

// get issues one request, optionally naming a session and an origin.
func (f *bundleFixture) get(t *testing.T, path, session, origin string) *http.Response {
	t.Helper()
	return f.do(t, http.MethodGet, path, session, origin, nil)
}

func (f *bundleFixture) do(
	t *testing.T,
	method, path, session, origin string,
	preflightFor http.Header,
) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+f.srv.Addr()+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if session != "" {
		req.Header.Set(bundleSessionHeader, session)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for name, values := range preflightFor {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// A caller naming no session gets the unfingerprintable 404 — including a
// loopback page, which is not an exception carved out for it but the
// plain consequence of the rule this surface states.
func TestBundleRoutesRefuseACallerWithNoSession(t *testing.T) {
	f := newBundleFixture(t, true)
	for _, path := range []string{"/bundle/manifest.json", "/bundle/archive.zip"} {
		if got := f.get(t, path, "", "").StatusCode; got != http.StatusNotFound {
			t.Errorf("GET %s with no session = %d, want 404", path, got)
		}
	}
}

// A backend with no bundle answers the same 404, so a shell cannot tell
// "no such route" from "this backend does not supply bundles" — and both
// leave it running what it has.
func TestBundleRoutesAreQuietWithoutABundle(t *testing.T) {
	f := newBundleFixture(t, false)
	for _, path := range []string{"/bundle/manifest.json", "/bundle/archive.zip"} {
		if got := f.get(t, path, "sess-1", "").StatusCode; got != http.StatusNotFound {
			t.Errorf("GET %s with no configured bundle = %d, want 404", path, got)
		}
	}
}

func TestBundleManifestAnswersASession(t *testing.T) {
	f := newBundleFixture(t, true)
	resp := f.get(t, "/bundle/manifest.json", "sess-1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	var served bundle.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&served); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	want, err := f.spa.Manifest()
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	if served.ID != want.ID || served.Version != want.Version || served.MinShellBuild != want.MinShellBuild {
		t.Fatalf("served %+v, want id/version/minShellBuild of %+v", served, want)
	}
	if len(served.Files) != len(want.Files) {
		t.Fatalf("served %d files, want %d", len(served.Files), len(want.Files))
	}
	for _, file := range served.Files {
		if file.Path == "assets/app.js.map" {
			t.Fatal("a source map reached the manifest")
		}
	}
}

func TestBundleArchiveAnswersTheManifestsFiles(t *testing.T) {
	f := newBundleFixture(t, true)
	resp := f.get(t, "/bundle/archive.zip", "sess-1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	// A shell sizes its download off this before it has a byte, so an
	// absent or wrong length is a real failure rather than a nicety.
	if resp.ContentLength != int64(len(body)) {
		t.Fatalf("Content-Length = %d, body is %d bytes", resp.ContentLength, len(body))
	}
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("the served body is not a zip: %v", err)
	}
	manifest, err := f.spa.Manifest()
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	if len(reader.File) != len(manifest.Files) {
		t.Fatalf("archive holds %d entries, manifest names %d", len(reader.File), len(manifest.Files))
	}
	for i, entry := range reader.File {
		if entry.Name != manifest.Files[i].Path {
			t.Fatalf("entry %d is %q, manifest says %q", i, entry.Name, manifest.Files[i].Path)
		}
	}
}

// Range is what lets a phone that lost a 5 MB transfer at 90% resume it
// rather than pay for the whole body again.
func TestBundleArchiveServesARange(t *testing.T) {
	f := newBundleFixture(t, true)
	req, err := http.NewRequest(http.MethodGet, "http://"+f.srv.Addr()+"/bundle/archive.zip", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(bundleSessionHeader, "sess-1")
	req.Header.Set("Range", "bytes=0-9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read range: %v", err)
	}
	if len(body) != 10 {
		t.Fatalf("range served %d bytes, want 10", len(body))
	}
}

// The shell's page origin is not this backend's, so both routes have to
// answer its CORS question — and answer it for exactly one origin.
func TestBundleRoutesAnswerTheShellOrigin(t *testing.T) {
	f := newBundleFixture(t, true)
	for _, path := range []string{"/bundle/manifest.json", "/bundle/archive.zip"} {
		resp := f.get(t, path, "sess-1", ShellOrigin)
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != ShellOrigin {
			t.Errorf("GET %s allow-origin = %q, want %q", path, got, ShellOrigin)
		}
		if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("GET %s wrote allow-credentials %q; it must never be sent", path, got)
		}
		if vary := resp.Header.Values("Vary"); len(vary) == 0 {
			t.Errorf("GET %s wrote no Vary; the answer varies by origin", path)
		}

		// A page origin this backend does not serve gets nothing added,
		// so the middleware cannot be asked which origins it knows.
		foreign := f.get(t, path, "sess-1", "https://elsewhere.example")
		if got := foreign.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("GET %s answered a foreign origin with %q", path, got)
		}
	}
}

// Both patterns are method-qualified, so the mux would answer 405 to the
// preflight and the browser would never send the real request. Each
// therefore registers its own OPTIONS pattern.
func TestBundlePreflightsAreAnswered(t *testing.T) {
	f := newBundleFixture(t, true)
	preflight := http.Header{
		"Access-Control-Request-Method":  []string{http.MethodGet},
		"Access-Control-Request-Headers": []string{"x-ao-session, x-ao-device-key"},
	}
	for _, path := range []string{"/bundle/manifest.json", "/bundle/archive.zip"} {
		resp := f.do(t, http.MethodOptions, path, "", ShellOrigin, preflight)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("OPTIONS %s = %d, want 204", path, resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != ShellOrigin {
			t.Errorf("OPTIONS %s allow-origin = %q, want %q", path, got, ShellOrigin)
		}
		if got := resp.Header.Get("Access-Control-Allow-Methods"); !bytes.Contains([]byte(got), []byte(http.MethodGet)) {
			t.Errorf("OPTIONS %s allow-methods = %q, want it to name GET", path, got)
		}
		headers := resp.Header.Get("Access-Control-Allow-Headers")
		for _, needed := range []string{SessionCredentialHeader, DeviceKeyHeader} {
			if !bytes.Contains(bytes.ToLower([]byte(headers)), bytes.ToLower([]byte(needed))) {
				t.Errorf("OPTIONS %s allow-headers = %q, want it to name %s", path, headers, needed)
			}
		}
		// A preflight for an origin this backend does not serve gets the
		// listener's ordinary 404, never a 405 that would confirm the
		// route exists.
		foreign := f.do(t, http.MethodOptions, path, "", "https://elsewhere.example", preflight)
		if foreign.StatusCode != http.StatusNotFound {
			t.Errorf("OPTIONS %s from a foreign origin = %d, want 404", path, foreign.StatusCode)
		}
	}
}
