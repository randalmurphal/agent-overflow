package appupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// testValidDigestHex is a syntactically valid 64-hex-char (32-byte) sha256
// digest used in mock SHASUMS256 bodies.
const testValidDigestHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const testRepo = "owner/repo"

// platformAssetName is the release artifact for whatever GOOS/GOARCH the test
// runs on, spelled the way matchReleaseAsset requires: the product prefix, the
// exact platform and arch tokens, and a shipped extension.
var platformAssetName = "agent-overflow-" + runtime.GOOS + "-" + runtime.GOARCH + ".zip"

// wslAssetName is the asset the headless WSL backend targets: a Windows binary
// the launcher swaps in, named for platform "wsl" rather than the linux/amd64
// process that downloads it.
const wslAssetName = "agent-overflow-wsl-amd64.exe"

// headlessAssetName is the windowless serve binary, and headlessPlatform is
// the token a serve host targets it with. It is in these fixtures because it
// is the artifact whose name CONTAINS another target's — the collision that
// made matchReleaseAsset exact — so a source built for either of the two must
// resolve only its own.
const (
	headlessPlatform  = "headless-linux"
	headlessArch      = "amd64"
	headlessAssetName = "agent-overflow-headless-linux-amd64"
	// plainLinuxAssetName is the desktop artifact headlessAssetName must never
	// be confused with, and vice versa.
	plainLinuxAssetName = "agent-overflow-linux-amd64"
)

// Asset payloads the mock release server hands out, and the real SHA-256 of the
// WSL one. Tests that download end to end need a checksum sidecar that actually
// matches the bytes; testValidDigestHex is deliberately bogus and only suits
// metadata-level assertions.
var (
	wslAssetBytes      = []byte("MZ agent-overflow wsl payload — deterministic test bytes\n")
	platformAssetBytes = []byte("agent-overflow desktop payload — deterministic test bytes\n")
	wslAssetDigest     = sha256.Sum256(wslAssetBytes)
	wslAssetDigestHex  = hex.EncodeToString(wslAssetDigest[:])

	// The serve host's pair: the windowless binary a supervised host installs,
	// and the desktop binary in the same release that it must not take.
	headlessAssetBytes     = []byte("agent-overflow headless serve payload — deterministic test bytes\n")
	headlessAssetDigest    = sha256.Sum256(headlessAssetBytes)
	headlessAssetDigestHex = hex.EncodeToString(headlessAssetDigest[:])
	plainLinuxAssetBytes   = []byte("agent-overflow linux desktop payload — deterministic test bytes\n")
)

// sumsForHeadless is a SHASUMS256 body whose headless entry is the TRUE digest
// of headlessAssetBytes, so a download through the real verifier succeeds. The
// desktop entry beside it is deliberately bogus: nothing here should be able to
// install that one, and a wrong digest is how that shows up as a refusal.
func sumsForHeadless(string) string {
	return headlessAssetDigestHex + "  " + headlessAssetName + "\n" +
		testValidDigestHex + "  " + plainLinuxAssetName + "\n"
}

// sumsForHeadlessCorrupt claims a digest the headless bytes do not have, which
// is what a mangled upload or a truncated transfer looks like from here.
func sumsForHeadlessCorrupt(string) string {
	return testValidDigestHex + "  " + headlessAssetName + "\n"
}

// sumsForWSL is a SHASUMS256 body whose wsl entry is the TRUE digest of
// wslAssetBytes, so a download through the real verifier succeeds.
func sumsForWSL(string) string {
	return wslAssetDigestHex + "  " + wslAssetName + "\n" +
		testValidDigestHex + "  " + platformAssetName + "\n"
}

// relSpec is a compact description of a mock GitHub release.
type relSpec struct {
	tag          string
	name         string
	prerelease   bool
	draft        bool
	withPlatform bool // include an asset matching the running platform
	withWSL      bool // include the wsl-target asset the WSL backend installs
	withHeadless bool // include the serve host's pair: headless AND plain linux
	withChecksum bool // include the SHASUMS256 sidecar
	withBogus    bool // include a non-matching asset (no platform/arch tokens)
	published    time.Time
}

func buildRelease(srvURL string, s relSpec) apiRelease {
	r := apiRelease{
		TagName:     s.tag,
		Name:        s.name,
		Prerelease:  s.prerelease,
		Draft:       s.draft,
		PublishedAt: s.published,
	}
	if s.withPlatform {
		r.Assets = append(r.Assets, apiAsset{
			Name:               platformAssetName,
			ContentType:        "application/zip",
			Size:               1024,
			BrowserDownloadURL: srvURL + "/dl/bin/" + s.tag,
		})
	}
	if s.withWSL {
		r.Assets = append(r.Assets, apiAsset{
			Name:               wslAssetName,
			ContentType:        "application/octet-stream",
			Size:               2048,
			BrowserDownloadURL: srvURL + "/dl/wsl/" + s.tag,
		})
	}
	if s.withHeadless {
		// Both, and the plain one FIRST: that is the ordering under which the
		// library's substring matcher would have handed a serve host the
		// desktop binary, so a fixture listing only one would not exercise
		// the rule that replaced it.
		r.Assets = append(r.Assets,
			apiAsset{
				Name:               plainLinuxAssetName,
				ContentType:        "application/octet-stream",
				Size:               4096,
				BrowserDownloadURL: srvURL + "/dl/linux/" + s.tag,
			},
			apiAsset{
				Name:               headlessAssetName,
				ContentType:        "application/octet-stream",
				Size:               3072,
				BrowserDownloadURL: srvURL + "/dl/headless/" + s.tag,
			})
	}
	if s.withBogus {
		r.Assets = append(r.Assets, apiAsset{
			Name:               "release-notes.txt",
			ContentType:        "text/plain",
			Size:               10,
			BrowserDownloadURL: srvURL + "/dl/notes/" + s.tag,
		})
	}
	if s.withChecksum {
		r.Assets = append(r.Assets, apiAsset{
			Name:               "SHASUMS256",
			ContentType:        "text/plain",
			Size:               128,
			BrowserDownloadURL: srvURL + "/dl/sums/" + s.tag,
		})
	}
	return r
}

// newMockGitHub stands up an httptest server speaking the subset of the GitHub
// releases API the provider uses: list, by-tag, latest, and the SHASUMS256
// download. checksumBody yields the sidecar text for a given tag.
func newMockGitHub(t *testing.T, specs []relSpec, checksumBody func(tag string) string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	listPath := "/repos/" + testRepo + "/releases"
	tagsPrefix := "/repos/" + testRepo + "/releases/tags/"
	latestPath := "/repos/" + testRepo + "/releases/latest"

	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("encode mock response: %v", err)
		}
	}

	mux.HandleFunc(latestPath, func(w http.ResponseWriter, r *http.Request) {
		for _, s := range specs {
			if s.draft || s.prerelease {
				continue
			}
			writeJSON(w, buildRelease(srv.URL, s))
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc(tagsPrefix, func(w http.ResponseWriter, r *http.Request) {
		tag := strings.TrimPrefix(r.URL.Path, tagsPrefix)
		for _, s := range specs {
			if s.tag == tag {
				writeJSON(w, buildRelease(srv.URL, s))
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc(listPath, func(w http.ResponseWriter, _ *http.Request) {
		out := make([]apiRelease, 0, len(specs))
		for _, s := range specs {
			out = append(out, buildRelease(srv.URL, s))
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/dl/sums/", func(w http.ResponseWriter, r *http.Request) {
		tag := strings.TrimPrefix(r.URL.Path, "/dl/sums/")
		if _, err := io.WriteString(w, checksumBody(tag)); err != nil {
			t.Errorf("write checksum body: %v", err)
		}
	})
	// Asset bytes. Only the download-through tests reach these; the resolve /
	// listing tests stop at metadata, which is why most of them can get away
	// with the fixed (bogus) digest in sumsForPlatform.
	serveAsset := func(body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			if _, err := w.Write(body); err != nil {
				t.Errorf("write asset body: %v", err)
			}
		}
	}
	mux.HandleFunc("/dl/wsl/", serveAsset(wslAssetBytes))
	mux.HandleFunc("/dl/bin/", serveAsset(platformAssetBytes))
	mux.HandleFunc("/dl/headless/", serveAsset(headlessAssetBytes))
	mux.HandleFunc("/dl/linux/", serveAsset(plainLinuxAssetBytes))

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestTargetable(srv *httptest.Server, current string) *targetableProvider {
	return newTestTargetableFor(srv, current, runtime.GOOS, runtime.GOARCH)
}

func newTestTargetableFor(srv *httptest.Server, current, platform, arch string) *targetableProvider {
	return &targetableProvider{
		repo:          testRepo,
		checksumAsset: "SHASUMS256",
		req:           updater.CheckRequest{CurrentVersion: current, Platform: platform, Arch: arch},
		baseURL:       srv.URL,
		httpClient:    srv.Client(),
		matcher:       matchReleaseAsset,
	}
}

// sumsForPlatform yields a SHASUMS256 body listing both the running platform's
// asset and the wsl-target asset.
func sumsForPlatform(string) string {
	return testValidDigestHex + "  " + platformAssetName + "\n" +
		testValidDigestHex + "  " + wslAssetName + "\n"
}

func platformRequest() updater.CheckRequest {
	return updater.CheckRequest{Platform: runtime.GOOS, Arch: runtime.GOARCH}
}

func TestValidReleaseTag(t *testing.T) {
	valid := []string{"v0.0.8", "0.0.8", "v1.2.3-rc1", "v0.0.2+build.5", "V2", "v1_0_0"}
	for _, tag := range valid {
		if !validReleaseTag(tag) {
			t.Errorf("validReleaseTag(%q) = false, want true", tag)
		}
	}
	invalid := []string{
		"",
		"../etc/passwd",
		"v0.0.8/extra",
		"v0.0.8?x=1",
		"release notes",
		"v0.0.8 ",
		".hidden",
		"-leading-dash",
		"v0.0.8#frag",
		"tag\nwith\nnewline",
		strings.Repeat("v1", 40), // 80 chars, over the 64-char body cap
	}
	for _, tag := range invalid {
		if validReleaseTag(tag) {
			t.Errorf("validReleaseTag(%q) = true, want false", tag)
		}
	}
}

func TestParseChecksumDigest(t *testing.T) {
	want, err := hex.DecodeString(testValidDigestHex)
	if err != nil {
		t.Fatalf("decode fixture digest: %v", err)
	}

	t.Run("plain two-space form", func(t *testing.T) {
		got, err := parseChecksumDigest(testValidDigestHex+"  app.zip\n", "app.zip")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.EqualFold(hex.EncodeToString(got), testValidDigestHex) {
			t.Fatalf("digest = %x, want %s", got, testValidDigestHex)
		}
		if len(got) != len(want) {
			t.Fatalf("digest len = %d, want %d", len(got), len(want))
		}
	})

	t.Run("binary-marker and dot-slash prefixes", func(t *testing.T) {
		for _, body := range []string{
			testValidDigestHex + " *app.zip\n",
			testValidDigestHex + "  ./app.zip\n",
		} {
			if _, err := parseChecksumDigest(body, "app.zip"); err != nil {
				t.Fatalf("body %q: unexpected error: %v", body, err)
			}
		}
	})

	t.Run("picks the matching entry among many", func(t *testing.T) {
		body := strings.Join([]string{
			"aaaa  other-a.zip",
			testValidDigestHex + "  app.zip",
			"bbbb  other-b.zip",
			"",
		}, "\n")
		got, err := parseChecksumDigest(body, "app.zip")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hex.EncodeToString(got) != testValidDigestHex {
			t.Fatalf("digest = %x, want %s", got, testValidDigestHex)
		}
	})

	t.Run("missing entry errors", func(t *testing.T) {
		if _, err := parseChecksumDigest(testValidDigestHex+"  other.zip\n", "app.zip"); err == nil {
			t.Fatal("expected error for missing entry, got nil")
		}
	})

	t.Run("non-hex digest errors", func(t *testing.T) {
		if _, err := parseChecksumDigest("zzzz  app.zip\n", "app.zip"); err == nil {
			t.Fatal("expected error for non-hex digest, got nil")
		}
	})

	t.Run("wrong digest length errors", func(t *testing.T) {
		// 8 hex chars = 4 bytes, not a sha256 digest.
		if _, err := parseChecksumDigest("0123abcd  app.zip\n", "app.zip"); err == nil {
			t.Fatal("expected error for short digest, got nil")
		}
	})
}

func TestTargetableProviderResolveTagAllowsDowngrade(t *testing.T) {
	// current 0.0.8, target the OLDER 0.0.5: the by-tag path must resolve it
	// (no newer-than-current gate) so rollback works.
	srv := newMockGitHub(t, []relSpec{
		{tag: "v0.0.5", name: "Old", withPlatform: true, withChecksum: true},
	}, sumsForPlatform)
	tp := newTestTargetable(srv, "0.0.8")
	tp.SetTarget("v0.0.5")

	rel, err := tp.Check(context.Background(), platformRequest())
	if err != nil {
		t.Fatalf("Check by older tag: unexpected error: %v", err)
	}
	if rel == nil {
		t.Fatal("expected a release for an explicit older tag, got nil (IsNewer gate leaked in?)")
	}
	if rel.Version != "0.0.5" {
		t.Fatalf("Version = %q, want 0.0.5", rel.Version)
	}
	if got := rel.Metadata["github.asset.url"]; got != srv.URL+"/dl/bin/v0.0.5" {
		t.Fatalf("asset url = %v, want %s/dl/bin/v0.0.5", got, srv.URL)
	}
	if rel.Verification == nil || hex.EncodeToString(rel.Verification.Digest) != testValidDigestHex {
		t.Fatalf("verification = %+v, want digest %s", rel.Verification, testValidDigestHex)
	}
}

func TestTargetableProviderResolveTagRejectsInvalidTag(t *testing.T) {
	tp := &targetableProvider{repo: testRepo, checksumAsset: "SHASUMS256", matcher: matchReleaseAsset}
	if _, err := tp.resolveTag(context.Background(), "../etc/passwd", platformRequest()); err == nil {
		t.Fatal("expected resolveTag to reject an unsafe tag, got nil error")
	}
}

func TestTargetableProviderResolveTagNoPlatformAsset(t *testing.T) {
	srv := newMockGitHub(t, []relSpec{
		{tag: "v0.0.5", withBogus: true, withChecksum: true}, // no matching asset
	}, sumsForPlatform)
	tp := newTestTargetable(srv, "0.0.8")
	tp.SetTarget("v0.0.5")
	if _, err := tp.Check(context.Background(), platformRequest()); err == nil {
		t.Fatal("expected error when no asset matches the platform, got nil")
	}
}

func TestTargetableProviderResolveTagMissingChecksumEntry(t *testing.T) {
	srv := newMockGitHub(t, []relSpec{
		{tag: "v0.0.5", withPlatform: true, withChecksum: true},
	}, func(string) string {
		// Sidecar exists but lists a different file — no entry for our asset.
		return testValidDigestHex + "  some-other-file.zip\n"
	})
	tp := newTestTargetable(srv, "0.0.8")
	tp.SetTarget("v0.0.5")
	if _, err := tp.Check(context.Background(), platformRequest()); err == nil {
		t.Fatal("expected error when the checksum sidecar has no entry for the asset, got nil")
	}
}

func TestTargetableProviderResolveTagNoChecksumSidecar(t *testing.T) {
	srv := newMockGitHub(t, []relSpec{
		{tag: "v0.0.5", withPlatform: true}, // no SHASUMS256 asset at all
	}, sumsForPlatform)
	tp := newTestTargetable(srv, "0.0.8")
	tp.SetTarget("v0.0.5")
	if _, err := tp.Check(context.Background(), platformRequest()); err == nil {
		t.Fatal("expected error when the release ships no checksum sidecar, got nil")
	}
}

func TestTargetableProviderListReleasesFiltersAndAnnotates(t *testing.T) {
	pub := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	srv := newMockGitHub(t, []relSpec{
		{tag: "v0.0.9", name: "Pre", prerelease: true, withPlatform: true, withChecksum: true},
		{tag: "v0.0.8", name: "Latest", withPlatform: true, withChecksum: true, published: pub},
		{tag: "v0.0.7", name: "Current", withPlatform: true, withChecksum: true},
		{tag: "v0.0.6", name: "Older", withPlatform: true, withChecksum: true},
		{tag: "v0.0.5", name: "Draft", draft: true, withPlatform: true, withChecksum: true}, // excluded: draft
		{tag: "v0.0.4", name: "NoAsset", withBogus: true, withChecksum: true},               // excluded: no platform asset
		{tag: "v0.0.3", name: "NoSum", withPlatform: true},                                  // excluded: no checksum
	}, sumsForPlatform)
	tp := newTestTargetable(srv, "0.0.7")

	got, err := tp.listReleases(context.Background())
	if err != nil {
		t.Fatalf("listReleases: %v", err)
	}

	wantTags := []string{"v0.0.9", "v0.0.8", "v0.0.7", "v0.0.6"}
	gotTags := make([]string, len(got))
	for i, r := range got {
		gotTags[i] = r.Tag
	}
	if strings.Join(gotTags, ",") != strings.Join(wantTags, ",") {
		t.Fatalf("tags = %v, want %v (drafts and non-installable releases must be filtered)", gotTags, wantTags)
	}

	by := make(map[string]ReleaseSummary, len(got))
	for _, r := range got {
		by[r.Tag] = r
	}

	// v0.0.9: newer than current but a prerelease — never "latest".
	if r := by["v0.0.9"]; !r.Prerelease || r.IsLatest || r.IsCurrent || r.IsOlder {
		t.Errorf("v0.0.9 = %+v, want prerelease, not latest/current/older", r)
	}
	// v0.0.8: newest stable → latest.
	if r := by["v0.0.8"]; !r.IsLatest || r.IsCurrent || r.IsOlder || r.Prerelease {
		t.Errorf("v0.0.8 = %+v, want isLatest only", r)
	}
	if r := by["v0.0.8"]; r.Version != "0.0.8" {
		t.Errorf("v0.0.8 Version = %q, want 0.0.8 (leading v stripped)", r.Version)
	}
	if r := by["v0.0.8"]; r.PublishedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("v0.0.8 PublishedAt = %q, want RFC3339 2026-01-02T03:04:05Z", r.PublishedAt)
	}
	// v0.0.7: equals the running build.
	if r := by["v0.0.7"]; !r.IsCurrent || r.IsLatest || r.IsOlder {
		t.Errorf("v0.0.7 = %+v, want isCurrent only", r)
	}
	// v0.0.6: a downgrade.
	if r := by["v0.0.6"]; !r.IsOlder || r.IsLatest || r.IsCurrent {
		t.Errorf("v0.0.6 = %+v, want isOlder only", r)
	}
}

func TestTargetableProviderTargetsConfiguredPlatform(t *testing.T) {
	// The headless WSL backend is a linux/amd64 process that installs the
	// `agent-overflow-wsl-amd64.exe` asset, so its provider is configured with
	// platform "wsl". Both the listing and the by-tag resolve must follow that
	// configuration, not runtime.GOOS.
	srv := newMockGitHub(t, []relSpec{
		{tag: "v0.0.8", name: "WSL + desktop", withPlatform: true, withWSL: true, withChecksum: true},
		{tag: "v0.0.7", name: "desktop only", withPlatform: true, withChecksum: true}, // no wsl asset
	}, sumsForPlatform)
	tp := newTestTargetableFor(srv, "0.0.7", "wsl", "amd64")

	got, err := tp.listReleases(context.Background())
	if err != nil {
		t.Fatalf("listReleases: %v", err)
	}
	if len(got) != 1 || got[0].Tag != "v0.0.8" {
		t.Fatalf("releases = %+v, want only v0.0.8 (the release shipping a wsl asset)", got)
	}

	tp.SetTarget("v0.0.8")
	// The incoming request describes the RUNNING process (linux/amd64 in the
	// backend's case); the provider must still resolve its configured wsl asset.
	rel, err := tp.Check(context.Background(), platformRequest())
	if err != nil {
		t.Fatalf("Check by tag: %v", err)
	}
	if rel.Artifact.Filename != wslAssetName {
		t.Fatalf("Artifact.Filename = %q, want %q", rel.Artifact.Filename, wslAssetName)
	}
	if got := rel.Metadata["github.asset.url"]; got != srv.URL+"/dl/wsl/v0.0.8" {
		t.Fatalf("asset url = %v, want %s/dl/wsl/v0.0.8", got, srv.URL)
	}
	if rel.Artifact.Platform != "wsl" || rel.Artifact.Arch != "amd64" {
		t.Fatalf("artifact platform/arch = %s/%s, want wsl/amd64", rel.Artifact.Platform, rel.Artifact.Arch)
	}
}

func TestTargetableProviderListReleasesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	tp := newTestTargetable(srv, "0.0.7")
	if _, err := tp.listReleases(context.Background()); err == nil {
		t.Fatal("expected an error when the releases endpoint 500s, got nil")
	}
}

// recordingProvider is a stub inner provider that records whether Check ran, so
// routing-by-target can be asserted without hitting the by-tag network path.
type recordingProvider struct {
	checked bool
	rel     *updater.Release
}

func (r *recordingProvider) Name() string { return "rec" }
func (r *recordingProvider) Check(context.Context, updater.CheckRequest) (*updater.Release, error) {
	r.checked = true
	return r.rel, nil
}
func (r *recordingProvider) Download(context.Context, *updater.Release, io.Writer, func(int64, int64)) error {
	return nil
}

func TestTargetableProviderCheckRoutesByTarget(t *testing.T) {
	srv := newMockGitHub(t, []relSpec{
		{tag: "v0.0.5", withPlatform: true, withChecksum: true},
	}, sumsForPlatform)
	tp := newTestTargetable(srv, "0.0.8")
	rec := &recordingProvider{rel: &updater.Release{Version: "9.9.9"}}
	tp.inner = rec

	// Empty target → delegate to inner (the stock latest path).
	rel, err := tp.Check(context.Background(), platformRequest())
	if err != nil {
		t.Fatalf("Check (latest): %v", err)
	}
	if !rec.checked {
		t.Fatal("empty target must delegate to inner.Check")
	}
	if rel.Version != "9.9.9" {
		t.Fatalf("Version = %q, want the inner release 9.9.9", rel.Version)
	}

	// Specific target → resolve by tag, bypassing inner entirely.
	rec.checked = false
	tp.SetTarget("v0.0.5")
	rel, err = tp.Check(context.Background(), platformRequest())
	if err != nil {
		t.Fatalf("Check (by tag): %v", err)
	}
	if rec.checked {
		t.Fatal("a specific target must NOT delegate to inner.Check")
	}
	if rel.Version != "0.0.5" {
		t.Fatalf("Version = %q, want 0.0.5", rel.Version)
	}
}

// noopUpdaterHost is a minimal updater.Host for constructing a real *Updater in
// tests that exercise the service's DownloadUpdate guards (which need a non-nil,
// configured updater but never reach the network).
type noopUpdaterHost struct{}

func (noopUpdaterHost) Emit(string, ...any) bool                              { return true }
func (noopUpdaterHost) OnEvent(string, func(any)) func()                      { return func() {} }
func (noopUpdaterHost) OpenWindow(updater.WindowOptions) updater.WindowHandle { return nil }
func (noopUpdaterHost) Quit()                                                 {}

func newConfiguredUpdater(t *testing.T) *updater.Updater {
	t.Helper()
	up := updater.New(noopUpdaterHost{})
	if err := up.Init(updater.Config{
		CurrentVersion: "0.0.1",
		Providers:      []updater.Provider{stubProvider{}},
		Window:         updater.WindowNone,
	}); err != nil {
		t.Fatalf("init updater: %v", err)
	}
	return up
}

func TestDownloadUpdateRejectsInvalidTag(t *testing.T) {
	a := &Service{updater: appUpdaterState{handle: newConfiguredUpdater(t)}}
	if err := a.DownloadUpdate("bad tag with spaces"); !errors.Is(err, ErrInvalidReleaseTag) {
		t.Fatalf("DownloadUpdate(invalid) = %v, want ErrInvalidReleaseTag", err)
	}
}

func TestDownloadUpdateEmptyTagRequiresPending(t *testing.T) {
	// No prior check has staged a pending release (updater sits at StateIdle),
	// so an empty-tag download must fail fast rather than launch a no-op flow.
	a := &Service{updater: appUpdaterState{handle: newConfiguredUpdater(t)}}
	if err := a.DownloadUpdate(""); !errors.Is(err, ErrNoUpdateToDownload) {
		t.Fatalf("DownloadUpdate(\"\") with no pending release = %v, want ErrNoUpdateToDownload", err)
	}
}

func TestDownloadUpdateRejectedWhileBusy(t *testing.T) {
	// A second download while one is already in flight must fail fast — the
	// updater installs one at a time.
	a := &Service{updater: appUpdaterState{handle: newConfiguredUpdater(t)}}
	a.updater.busy = true
	if err := a.DownloadUpdate("v0.0.6"); !errors.Is(err, ErrUpdateBusy) {
		t.Fatalf("DownloadUpdate while busy = %v, want ErrUpdateBusy", err)
	}
}

// newUpdaterApp wires the full production chain (real github.Provider →
// targetableProvider → verifiedProvider → real updater.Updater) against the
// mock GitHub server, so service-level updater methods can be driven end to end.
func newUpdaterApp(t *testing.T, srv *httptest.Server, current string) (*Service, *targetableProvider) {
	t.Helper()
	gh, err := newGitHubProvider(testRepo, "SHASUMS256", srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("newGitHubProvider: %v", err)
	}
	req := updater.CheckRequest{CurrentVersion: current, Platform: runtime.GOOS, Arch: runtime.GOARCH}
	tp := newTargetableProvider(gh, testRepo, "SHASUMS256", req, srv.Client())
	tp.baseURL = srv.URL // override the production api.github.com base for the mock
	up := updater.New(noopUpdaterHost{})
	if err := up.Init(updater.Config{
		CurrentVersion: req.CurrentVersion,
		Platform:       req.Platform,
		Arch:           req.Arch,
		Providers:      []updater.Provider{verifiedProvider{inner: tp}},
		Window:         updater.WindowNone,
	}); err != nil {
		t.Fatalf("init updater: %v", err)
	}
	return &Service{updater: appUpdaterState{handle: up, provider: tp}}, tp
}

func TestDownloadUpdateByTagResolveFailureEmitsError(t *testing.T) {
	// The requested tag has no release → resolveTag 404s → the updater's Check
	// returns an error (not an event). DownloadUpdate's goroutine must surface
	// updater:error itself, or the UI — already flipped to "downloading" —
	// would hang with no terminal event.
	srv := newMockGitHub(t, []relSpec{
		{tag: "v0.0.8", withPlatform: true, withChecksum: true}, // latest exists; v9.9.9 does not
	}, sumsForPlatform)
	a, _ := newUpdaterApp(t, srv, "0.0.1")

	got := make(chan updater.ErrorInfo, 1)
	a.deps.Emit = func(name eventchan.Channel, data any) {
		if name != "updater:error" {
			return
		}
		if info, ok := data.(updater.ErrorInfo); ok {
			select {
			case got <- info:
			default:
			}
		}
	}

	if err := a.DownloadUpdate("v9.9.9"); err != nil {
		t.Fatalf("DownloadUpdate returned a synchronous error: %v", err)
	}
	select {
	case info := <-got:
		if info.Stage != updater.StageCheck {
			t.Fatalf("emitted error stage = %q, want %q", info.Stage, updater.StageCheck)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for updater:error from a failed by-tag resolve")
	}
}

func TestCheckForUpdateSkipsProbeWhenBusy(t *testing.T) {
	// While a download holds the updater, a concurrent CheckForUpdate (only
	// reachable from a second --connect client) must NOT run its own Check:
	// doing so would retarget the provider and overwrite the pending release
	// being installed. It reports the current version with Available=false
	// instead. If the busy guard were broken it would probe the mock, see
	// v0.0.8 > 0.0.1, and report Available=true — so this asserts the skip.
	srv := newMockGitHub(t, []relSpec{
		{tag: "v0.0.8", withPlatform: true, withChecksum: true},
	}, sumsForPlatform)
	a, _ := newUpdaterApp(t, srv, "0.0.1")
	a.updater.busy = true

	avail, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if !avail.Supported {
		t.Fatal("busy check must still report Supported")
	}
	if avail.Available {
		t.Fatal("busy check must not probe and must not report Available")
	}
	if avail.CurrentVersion != "0.0.1" {
		t.Fatalf("CurrentVersion = %q, want 0.0.1", avail.CurrentVersion)
	}
}

func TestCheckForUpdateResetsRollbackTarget(t *testing.T) {
	// After a by-tag (rollback) download aimed the provider at an older tag, a
	// later passive check must report the LATEST release, not the rolled-back
	// one — CheckForUpdate clears the target before checking.
	srv := newMockGitHub(t, []relSpec{
		{tag: "v0.0.8", withPlatform: true, withChecksum: true},
	}, sumsForPlatform)
	a, tp := newUpdaterApp(t, srv, "0.0.1")
	tp.SetTarget("v0.0.6") // simulate a prior rollback aim

	avail, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if avail.LatestVersion != "0.0.8" {
		t.Fatalf("LatestVersion = %q, want 0.0.8 (target must reset to latest)", avail.LatestVersion)
	}
	if got := tp.target.Load(); got != nil {
		t.Fatalf("provider target = %v, want nil after reset", *got)
	}
}
