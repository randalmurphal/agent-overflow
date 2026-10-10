package app

import (
	"bytes"
	"context"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/contentcache"
	"agent-overflow/internal/forgeapi"
	"agent-overflow/internal/forgeattach"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/transport"
)

const testForgeUploadSecret = "0123456789abcdef0123456789abcdef"

var testGitLabPR = gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "group", Repo: "widget", Number: 12}

func testUploadHref() string { return "/uploads/" + testForgeUploadSecret + "/hero.png" }

// gitlabUploads is a forge API transport whose GitLab serves the upload
// hero.png of testGitLabPR's project, answering payload on each request.
// It returns a Core reading through it and the count of requests made.
func gitlabUploads(t *testing.T, payload func() []byte) (*gitops.Core, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	want := "/gitlab/api/v4/projects/group%2Fwidget/uploads/" + testForgeUploadSecret + "/hero.png"
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.EscapedPath() != want || r.Header.Get("Private-Token") != "test-token" {
			t.Errorf("unexpected forge request %s %s", r.Method, r.URL.EscapedPath())
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload())
	}))
	t.Cleanup(fake.Close)
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", Isolated: &forgeapi.Isolated{BaseURL: fake.URL, Token: "test-token"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return gitops.NewCore(gitops.WithForgeAPI(svc)), &requests
}

func constantPayload(data []byte) func() []byte { return func() []byte { return data } }

// forgeAttachmentApp is a bare App with a live transport: no store and no
// thread, because a forge attachment belongs to a pull request rather
// than to anything this app persists.
func forgeAttachmentApp(t *testing.T) (*App, string) {
	t.Helper()
	app := &App{configDir: t.TempDir()}
	srv, err := transport.New(transport.Config{
		Dispatcher:         transport.NewDispatcher(),
		EventBus:           transport.NewEventBus(8),
		Token:              "forge-attachment-test-token",
		AttachmentTransfer: AttachmentTransfer(app),
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("transport.Server.Start: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})
	app.SetTransportServer(srv)
	return app, "http://" + srv.Addr()
}

// TestFetchForgeAttachmentRoundTripsOverHTTP is the whole path: the forge
// fetch on the computer that owns the PR, the signature classification,
// the ticketed URL, and the bytes coming back unchanged at the page
// origin — which is what makes this work on a phone.
func TestFetchForgeAttachmentRoundTripsOverHTTP(t *testing.T) {
	t.Parallel()
	payload := realPNGBytes(t)
	core, _ := gitlabUploads(t, constantPayload(payload))
	app, base := forgeAttachmentApp(t)
	app.git = core

	got, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, testUploadHref(), 0)
	if err != nil {
		t.Fatalf("FetchForgeAttachment: %v", err)
	}
	if got.Kind != "image" || got.MimeType != "image/png" {
		t.Fatalf("classified as (%q, %q), want (image/png, image)", got.MimeType, got.Kind)
	}
	if got.Filename != "hero.png" || got.SizeBytes != int64(len(payload)) {
		t.Fatalf("metadata = %+v, want hero.png at %d bytes", got, len(payload))
	}
	// Relative, for the reason every other minted URL is: the page can be
	// served from the webview, the --connect stub or a paired browser.
	if !strings.HasPrefix(got.URL, "/attachments/forge/") {
		t.Fatalf("minted URL %q is not the forge byte route", got.URL)
	}

	resp, err := http.Get(base + got.URL)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("served %d bytes, fetched %d", len(body), len(payload))
	}
}

// TestFetchForgeAttachmentReusesTheCachedBytes: a PR body referencing the
// same image three times must fetch once, not three times.
func TestFetchForgeAttachmentReusesTheCachedBytes(t *testing.T) {
	t.Parallel()
	core, counter := gitlabUploads(t, constantPayload(realPNGBytes(t)))
	app, _ := forgeAttachmentApp(t)
	app.git = core

	first, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, testUploadHref(), 0)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	second, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, "  "+testUploadHref()+"\n", 0)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if runs := int(counter.Load()); runs != 1 {
		t.Fatalf("the forge was asked %d times for one reference, want 1", runs)
	}
	// A fresh ticket each time, because a ticket is spent by its request.
	if first.URL == second.URL {
		t.Fatal("the second fetch reused a spent ticket")
	}
	if !strings.HasPrefix(second.URL, strings.SplitN(first.URL, "?", 2)[0]+"?") {
		t.Fatalf("second URL %q names different content than %q", second.URL, first.URL)
	}

	// A different PR is a different cache key even for the same href: the
	// upload secret is scoped to its project.
	other := testGitLabPR
	other.Number = 13
	if _, err := app.FetchForgeAttachment(t.Context(), other, testUploadHref(), 0); err != nil {
		t.Fatalf("other-PR fetch: %v", err)
	}
	if runs := int(counter.Load()); runs != 2 {
		t.Fatalf("the forge was asked %d times, want 2 (one per PR)", runs)
	}
}

// TestFetchForgeAttachmentRefusesBeforeRequesting: a bad PR reference or a
// href that is not an upload costs no forge request.
func TestFetchForgeAttachmentRefusesBeforeRequesting(t *testing.T) {
	t.Parallel()
	core, counter := gitlabUploads(t, constantPayload([]byte("x")))
	app, _ := forgeAttachmentApp(t)
	app.git = core

	if _, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, "https://example.com/logo.png", 0); err == nil {
		t.Fatal("FetchForgeAttachment accepted a href that is not a forge upload")
	}
	bad := testGitLabPR
	bad.Number = 0
	if _, err := app.FetchForgeAttachment(t.Context(), bad, testUploadHref(), 0); err == nil {
		t.Fatal("FetchForgeAttachment accepted a PR number of zero")
	}
	if runs := int(counter.Load()); runs != 0 {
		t.Fatalf("the forge was asked %d times for references that never resolved", runs)
	}
}

func TestFetchForgeAttachmentNeedsATransport(t *testing.T) {
	t.Parallel()
	core, _ := gitlabUploads(t, constantPayload(realPNGBytes(t)))
	app := &App{configDir: t.TempDir(), git: core}
	_, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, testUploadHref(), 0)
	if err == nil || !strings.Contains(err.Error(), "transport is not serving") {
		t.Fatalf("error = %v, want a transport-not-serving refusal", err)
	}
}

func TestFetchForgeAttachmentStopsWhenShuttingDown(t *testing.T) {
	t.Parallel()
	core, _ := gitlabUploads(t, constantPayload([]byte("x")))
	app, _ := forgeAttachmentApp(t)
	app.git = core
	app.shuttingDown.Store(true)
	if _, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, testUploadHref(), 0); err != ErrShuttingDown {
		t.Fatalf("error = %v, want ErrShuttingDown", err)
	}
	if _, err := app.SaveForgeAttachment(t.Context(), testGitLabPR, testUploadHref()); err != ErrShuttingDown {
		t.Fatalf("error = %v, want ErrShuttingDown", err)
	}
}

// TestSaveForgeAttachmentNeverOverwrites: the save lands in a directory
// the person browses, so a second save of the same name is a new file
// rather than a lost one.
func TestSaveForgeAttachmentNeverOverwrites(t *testing.T) {
	payload := realPNGBytes(t)
	core, _ := gitlabUploads(t, constantPayload(payload))
	home := t.TempDir()
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("create Downloads: %v", err)
	}
	t.Setenv("HOME", home)
	app, _ := forgeAttachmentApp(t)
	app.git = core

	first, err := app.SaveForgeAttachment(t.Context(), testGitLabPR, testUploadHref())
	if err != nil {
		t.Fatalf("SaveForgeAttachment: %v", err)
	}
	if first != filepath.Join(downloads, "hero.png") {
		t.Fatalf("saved to %q, want the user's Downloads directory", first)
	}
	saved, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if !bytes.Equal(saved, payload) {
		t.Fatalf("saved %d bytes, fetched %d", len(saved), len(payload))
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatalf("stat saved file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("saved mode = %v, want 0600", perm)
	}

	second, err := app.SaveForgeAttachment(t.Context(), testGitLabPR, testUploadHref())
	if err != nil {
		t.Fatalf("second SaveForgeAttachment: %v", err)
	}
	if second != filepath.Join(downloads, "hero (2).png") {
		t.Fatalf("second save went to %q, want the (2) suffix", second)
	}
}

// TestSaveForgeAttachmentFallsBackToTheAppDirectory covers a home with no
// Downloads folder: the save still has to land somewhere the app can
// report a path for.
func TestSaveForgeAttachmentFallsBackToTheAppDirectory(t *testing.T) {
	core, _ := gitlabUploads(t, constantPayload(realPNGBytes(t)))
	t.Setenv("HOME", t.TempDir())
	app, _ := forgeAttachmentApp(t)
	app.git = core

	path, err := app.SaveForgeAttachment(t.Context(), testGitLabPR, testUploadHref())
	if err != nil {
		t.Fatalf("SaveForgeAttachment: %v", err)
	}
	if want := filepath.Join(app.configDir, "downloads", "hero.png"); path != want {
		t.Fatalf("saved to %q, want %q", path, want)
	}
}

// A positive maxWidth serves an image derivative held beside the original:
// sized for the tier, described with the original's size and bytes, made
// once per tier, and never what a save writes.
func TestFetchForgeAttachmentServesADerivedTier(t *testing.T) {
	payload := sizedPNG(t, 641, 480)
	core, counter := gitlabUploads(t, constantPayload(payload))
	t.Setenv("HOME", t.TempDir())
	app, base := forgeAttachmentApp(t)
	app.git = core

	got, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, testUploadHref(), 300)
	if err != nil {
		t.Fatalf("FetchForgeAttachment: %v", err)
	}
	want := ForgeAttachment{
		URL: got.URL, MimeType: "image/png", Kind: "image", SizeBytes: int64(len(payload)), Filename: "hero.png",
		Width: 320, Height: 240, OriginalWidth: 641, OriginalHeight: 480, Derived: true,
	}
	if got != want {
		t.Fatalf("FetchForgeAttachment = %+v, want %+v", got, want)
	}
	resp, body := getBytes(t, base, got.URL)
	cfg, err := png.DecodeConfig(bytes.NewReader(body))
	if resp.StatusCode != http.StatusOK || err != nil || cfg.Width != 320 || cfg.Height != 240 {
		t.Fatalf("served %d %v %dx%d, want a 320x240 png", resp.StatusCode, err, cfg.Width, cfg.Height)
	}

	again, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, testUploadHref(), 320)
	if err != nil {
		t.Fatalf("second FetchForgeAttachment: %v", err)
	}
	original, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, testUploadHref(), 0)
	if err != nil {
		t.Fatalf("original FetchForgeAttachment: %v", err)
	}
	if !again.Derived || again.Width != 320 || original.Derived || original.Width != 641 || original.Height != 480 {
		t.Fatalf("second = %+v, original = %+v", again, original)
	}
	if runs := int(counter.Load()); runs != 1 {
		t.Fatalf("the forge was asked %d times for one attachment at two widths", runs)
	}
	resp, body = getBytes(t, base, original.URL)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, payload) {
		t.Fatalf("the original URL served %d with %d bytes, want the attachment", resp.StatusCode, len(body))
	}
	saved, err := app.SaveForgeAttachment(t.Context(), testGitLabPR, testUploadHref())
	if err != nil {
		t.Fatalf("SaveForgeAttachment: %v", err)
	}
	if data, err := os.ReadFile(saved); err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("saved %d bytes (%v), want the original %d", len(data), err, len(payload))
	}

	// A held derivative is answered without decoding the original again:
	// with the cached original's bytes no longer an image, the tier still
	// answers.
	held, ok := app.forgeAttachments().Lookup(forgeattach.CacheKey(testGitLabPR.Forge, testGitLabPR.Project(), testGitLabPR.Number, testUploadHref()))
	if !ok {
		t.Fatal("the original is not cached")
	}
	clear(held.Value.Data[:8])
	if again, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, testUploadHref(), 320); err != nil || !again.Derived || again.Width != 320 {
		t.Fatalf("held tier = %+v, %v; want the derivative answered from the cache", again, err)
	}
}

// A tier of an attachment Go cannot decode answers the attachment itself.
func TestFetchForgeAttachmentServesTheOriginalWhenItsPixelsDoNotDecode(t *testing.T) {
	t.Parallel()
	payload := undecodablePNG(t, 641, 480)
	core, _ := gitlabUploads(t, constantPayload(payload))
	app, base := forgeAttachmentApp(t)
	app.git = core

	got, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, testUploadHref(), 300)
	if err != nil {
		t.Fatalf("FetchForgeAttachment: %v", err)
	}
	if got.Derived || got.Kind != "image" || got.Width != 641 || got.Height != 480 || got.SizeBytes != int64(len(payload)) {
		t.Fatalf("FetchForgeAttachment = %+v, want the 641x480 attachment itself", got)
	}
	if resp, body := getBytes(t, base, got.URL); resp.StatusCode != http.StatusOK || !bytes.Equal(body, payload) {
		t.Fatalf("GET = %d with %d bytes, want the attachment", resp.StatusCode, len(body))
	}
}

// A derivative is keyed by its original's content id, so once the original
// expires and the forge serves different bytes, the tier is derived from the
// new bytes instead of answering a derivative of the old ones.
func TestForgeAttachmentDerivativeFollowsARefetchedOriginal(t *testing.T) {
	t.Parallel()
	source := writeTempFile(t, sizedPNG(t, 641, 480))
	core, _ := gitlabUploads(t, func() []byte {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Error(err)
		}
		return data
	})
	app, _ := forgeAttachmentApp(t)
	app.git = core
	now := time.Unix(1_700_000_000, 0)
	app.forgeAttachOnce.Do(func() {
		app.forgeAttachCache = contentcache.New(contentcache.Config[forgeattach.Attachment]{
			MaxBytes: forgeattach.DefaultCacheBytes,
			TTL:      time.Minute,
			Size:     func(a forgeattach.Attachment) int64 { return int64(len(a.Data)) },
			Now:      func() time.Time { return now },
		})
	})

	if _, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, testUploadHref(), 0); err != nil {
		t.Fatalf("original: %v", err)
	}
	now = now.Add(50 * time.Second)
	first, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, testUploadHref(), 320)
	if err != nil || first.Height != 240 {
		t.Fatalf("first tier = %+v, %v; want 320x240", first, err)
	}
	// The original expires; the derivative, stored later, is still live.
	now = now.Add(20 * time.Second)
	if err := os.WriteFile(source, sizedPNG(t, 641, 641), 0o600); err != nil {
		t.Fatalf("change the forge's bytes: %v", err)
	}
	second, err := app.FetchForgeAttachment(t.Context(), testGitLabPR, testUploadHref(), 320)
	if err != nil || !second.Derived || second.Width != 320 || second.Height != 320 || second.OriginalHeight != 641 {
		t.Fatalf("tier after a re-fetch = %+v, %v; want 320x320 from the new 641x641 bytes", second, err)
	}
}

// TestOpenForgeAttachmentMissesAfterEviction: the route's 404 for an id
// the cache no longer holds comes from here.
func TestOpenForgeAttachmentMissesAfterEviction(t *testing.T) {
	t.Parallel()
	app := &App{configDir: t.TempDir()}
	if _, err := (attachmentTransfer{app: app}).OpenForgeAttachment("never-stored"); err == nil {
		t.Fatal("OpenForgeAttachment answered for an id nothing stored")
	}
}

func writeTempFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	return path
}
