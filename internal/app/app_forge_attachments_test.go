package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/transport"
)

const testForgeUploadSecret = "0123456789abcdef0123456789abcdef"

var testGitLabPR = gitops.PRReference{Forge: "gitlab", Namespace: "group", Repo: "widget", Number: 12}

func testUploadHref() string { return "/uploads/" + testForgeUploadSecret + "/hero.png" }

// stubGlab writes a fake glab on PATH that emits body and counts its own
// invocations, and returns the counter path.
func stubGlab(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock forge CLI is unix-only")
	}
	binDir := t.TempDir()
	counter := filepath.Join(binDir, "runs")
	script := fmt.Sprintf("#!/bin/sh\necho run >> %q\n%s", counter, body)
	if err := os.WriteFile(filepath.Join(binDir, "glab"), []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return counter
}

func runCount(t *testing.T, counter string) int {
	t.Helper()
	raw, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read run counter: %v", err)
	}
	return strings.Count(string(raw), "run\n")
}

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

// TestFetchForgeAttachmentRoundTripsOverHTTP is the whole path: the CLI
// fetch on the computer that owns the PR, the signature classification,
// the ticketed URL, and the bytes coming back unchanged at the page
// origin — which is what makes this work on a phone.
func TestFetchForgeAttachmentRoundTripsOverHTTP(t *testing.T) {
	payload := realPNGBytes(t)
	stubGlab(t, "cat "+shellQuote(writeTempFile(t, payload))+"\n")
	app, base := forgeAttachmentApp(t)

	got, err := app.FetchForgeAttachment(testGitLabPR, testUploadHref())
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
// same image three times must spawn one glab, not three.
func TestFetchForgeAttachmentReusesTheCachedBytes(t *testing.T) {
	counter := stubGlab(t, "cat "+shellQuote(writeTempFile(t, realPNGBytes(t)))+"\n")
	app, _ := forgeAttachmentApp(t)

	first, err := app.FetchForgeAttachment(testGitLabPR, testUploadHref())
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	second, err := app.FetchForgeAttachment(testGitLabPR, "  "+testUploadHref()+"\n")
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if runs := runCount(t, counter); runs != 1 {
		t.Fatalf("glab ran %d times for one reference, want 1", runs)
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
	if _, err := app.FetchForgeAttachment(other, testUploadHref()); err != nil {
		t.Fatalf("other-PR fetch: %v", err)
	}
	if runs := runCount(t, counter); runs != 2 {
		t.Fatalf("glab ran %d times, want 2 (one per PR)", runs)
	}
}

// TestFetchForgeAttachmentRefusesBeforeSpawning: a bad PR reference or a
// href that is not an upload costs no subprocess.
func TestFetchForgeAttachmentRefusesBeforeSpawning(t *testing.T) {
	counter := stubGlab(t, "printf 'x'\n")
	app, _ := forgeAttachmentApp(t)

	if _, err := app.FetchForgeAttachment(testGitLabPR, "https://example.com/logo.png"); err == nil {
		t.Fatal("FetchForgeAttachment accepted a href that is not a forge upload")
	}
	bad := testGitLabPR
	bad.Number = 0
	if _, err := app.FetchForgeAttachment(bad, testUploadHref()); err == nil {
		t.Fatal("FetchForgeAttachment accepted a PR number of zero")
	}
	if runs := runCount(t, counter); runs != 0 {
		t.Fatalf("glab ran %d times for references that never resolved", runs)
	}
}

func TestFetchForgeAttachmentNeedsATransport(t *testing.T) {
	stubGlab(t, "cat "+shellQuote(writeTempFile(t, realPNGBytes(t)))+"\n")
	app := &App{configDir: t.TempDir()}
	_, err := app.FetchForgeAttachment(testGitLabPR, testUploadHref())
	if err == nil || !strings.Contains(err.Error(), "transport is not serving") {
		t.Fatalf("error = %v, want a transport-not-serving refusal", err)
	}
}

func TestFetchForgeAttachmentStopsWhenShuttingDown(t *testing.T) {
	stubGlab(t, "printf 'x'\n")
	app, _ := forgeAttachmentApp(t)
	app.shuttingDown.Store(true)
	if _, err := app.FetchForgeAttachment(testGitLabPR, testUploadHref()); err != ErrShuttingDown {
		t.Fatalf("error = %v, want ErrShuttingDown", err)
	}
	if _, err := app.SaveForgeAttachment(testGitLabPR, testUploadHref()); err != ErrShuttingDown {
		t.Fatalf("error = %v, want ErrShuttingDown", err)
	}
}

// TestSaveForgeAttachmentNeverOverwrites: the save lands in a directory
// the person browses, so a second save of the same name is a new file
// rather than a lost one.
func TestSaveForgeAttachmentNeverOverwrites(t *testing.T) {
	payload := realPNGBytes(t)
	stubGlab(t, "cat "+shellQuote(writeTempFile(t, payload))+"\n")
	home := t.TempDir()
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("create Downloads: %v", err)
	}
	t.Setenv("HOME", home)
	app, _ := forgeAttachmentApp(t)

	first, err := app.SaveForgeAttachment(testGitLabPR, testUploadHref())
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

	second, err := app.SaveForgeAttachment(testGitLabPR, testUploadHref())
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
	stubGlab(t, "cat "+shellQuote(writeTempFile(t, realPNGBytes(t)))+"\n")
	t.Setenv("HOME", t.TempDir())
	app, _ := forgeAttachmentApp(t)

	path, err := app.SaveForgeAttachment(testGitLabPR, testUploadHref())
	if err != nil {
		t.Fatalf("SaveForgeAttachment: %v", err)
	}
	if want := filepath.Join(app.configDir, "downloads", "hero.png"); path != want {
		t.Fatalf("saved to %q, want %q", path, want)
	}
}

func TestDownloadFileName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hero.png", "hero.png"},
		{"Screen Shot (1).png", "Screen Shot (1).png"},
		{"Отчёт.png", "Отчёт.png"},
		{"../../etc/passwd", "passwd"},
		{`C:\Users\me\clip.mp4`, "clip.mp4"},
		{"a;rm -rf ~.png", "a-rm -rf.png"},
		{"", "attachment"},
		{"...", "attachment"},
		{".hidden", "attachment.hidden"},
		{strings.Repeat("x", 200) + ".png", strings.Repeat("x", 80) + ".png"},
	}
	for _, tc := range cases {
		if got := downloadFileName(tc.in); got != tc.want {
			t.Errorf("downloadFileName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestOpenForgeAttachmentMissesAfterEviction: the route's 404 for an id
// the cache no longer holds comes from here.
func TestOpenForgeAttachmentMissesAfterEviction(t *testing.T) {
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
