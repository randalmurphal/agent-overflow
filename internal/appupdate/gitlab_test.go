package appupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/forgeapi"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// The GitLab feed is tested over HTTP: an httptest GitLab REST API, read
// through an isolated forge API transport with a fixed token, as an
// isolated boot reads its fake forge. Nothing here can reach a real forge
// or the developer's glab login.

const (
	testGitLabHost     = "gitlab.example.com"
	testGitLabProject  = testGitLabHost + "/grp/app"
	testGitLabListPath = "projects/grp%2Fapp/releases?order_by=released_at&sort=desc&per_page=30"
	testGitLabToken    = "fake-gitlab-token"

	noremoteAssetName = "agent-overflow-wsl-noremote-amd64.exe"
	standardAssetName = "agent-overflow-wsl-amd64.exe"
)

// fakeRoute is the fake's answer for one API path. A zero status is 200.
// Chunked sends the body without a Content-Length.
type fakeRoute struct {
	status  int
	body    []byte
	chunked bool
}

// fakeCall is one request the fake served.
type fakeCall struct {
	Method string
	Path   string // relative to /api/v4/, with the raw query
	Host   string
	Token  string
	Accept string
}

// fakeGitLab is one project's releases, served over GitLab's REST API.
type fakeGitLab struct {
	t        *testing.T
	mu       sync.Mutex
	routes   map[string]fakeRoute
	served   []fakeCall
	releases []map[string]any
	svc      *forgeapi.Service
}

// fakeAsset is one release link. An empty link is the project's generic
// package registry on the configured host, which is what the release
// script publishes.
type fakeAsset struct {
	name string
	body []byte
	link string
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	t.Helper()
	f := &fakeGitLab{t: t, routes: map[string]fakeRoute{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", Isolated: &forgeapi.Isolated{BaseURL: srv.URL, Token: testGitLabToken}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	f.svc = svc
	return f
}

func (f *fakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/gitlab/api/v4/")
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	f.mu.Lock()
	f.served = append(f.served, fakeCall{Method: r.Method, Path: path, Host: r.Host, Token: r.Header.Get("Private-Token"), Accept: r.Header.Get("Accept")})
	route, ok := f.routes[path]
	f.mu.Unlock()
	if !ok || r.Method != http.MethodGet {
		route = fakeRoute{status: http.StatusNotFound, body: []byte(`{"message":"404 Not Found"}`)}
	}
	if !route.chunked {
		w.Header().Set("Content-Length", strconv.Itoa(len(route.body)))
	}
	if route.status != 0 {
		w.WriteHeader(route.status)
	}
	_, _ = w.Write(route.body)
	if route.chunked {
		w.(http.Flusher).Flush()
	}
}

// gitlab is the feed's client: the transport's GitLab client for host, as
// internal/app adapts it.
func (f *fakeGitLab) gitlab(host string) GitLabClient {
	return transportGitLab{client: f.svc.GitLab(host)}
}

type transportGitLab struct{ client *forgeapi.Client }

func (c transportGitLab) Stream(ctx context.Context, path string, dst io.Writer, limit int64) error {
	_, err := c.client.Stream(ctx, forgeapi.Request{Path: path, Header: http.Header{"Accept": {"*/*"}}}, dst, limit)
	return err
}

func packageLink(tag, name string) string {
	return "https://" + testGitLabHost + "/api/v4/projects/7/packages/generic/agent-overflow/" + trimVPrefix(tag) + "/" + name
}

func packagePath(tag, name string) string {
	return strings.TrimPrefix(packageLink(tag, name), "https://"+testGitLabHost+"/api/v4/")
}

// publish adds a release newer than every release published before it
// returns, so call it newest first, as GitLab lists them.
func (f *fakeGitLab) publish(tag string, assets ...fakeAsset) {
	f.t.Helper()
	links := make([]map[string]any, 0, len(assets))
	for _, asset := range assets {
		link := asset.link
		if link == "" {
			link = packageLink(tag, asset.name)
			f.set(packagePath(tag, asset.name), fakeRoute{body: asset.body})
		}
		links = append(links, map[string]any{"name": asset.name, "url": link, "link_type": "package"})
	}
	release := map[string]any{
		"tag_name":         tag,
		"name":             "Agent Overflow " + tag,
		"description":      "notes for " + tag,
		"released_at":      time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(len(f.releases)) * time.Hour).Format(time.RFC3339),
		"upcoming_release": false,
		"assets":           map[string]any{"count": len(links), "links": links},
	}
	f.releases = append(f.releases, release)
	byTag, err := json.Marshal(release)
	if err != nil {
		f.t.Fatal(err)
	}
	f.set("projects/grp%2Fapp/releases/"+tag, fakeRoute{body: byTag})
	listing, err := json.Marshal(f.releases)
	if err != nil {
		f.t.Fatal(err)
	}
	f.set(testGitLabListPath, fakeRoute{body: listing})
}

func (f *fakeGitLab) set(path string, route fakeRoute) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = route
}

func (f *fakeGitLab) calls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.served...)
}

func (f *fakeGitLab) source(t *testing.T, platform, current string) *ReleaseSource {
	t.Helper()
	src, err := newReleaseSource(Config{
		CurrentVersion: current,
		Platform:       platform,
		Arch:           "amd64",
		GitLab:         f.gitlab,
	}, testGitLabProject)
	if err != nil {
		t.Fatalf("newReleaseSource: %v", err)
	}
	return src
}

// setGitLabProject stands in for the link-time stamp.
func setGitLabProject(t *testing.T, project string) {
	t.Helper()
	previous := gitlabProject
	gitlabProject = project
	t.Cleanup(func() { gitlabProject = previous })
}

func payload(name, tag string) []byte {
	return []byte("MZ " + name + " " + tag + " deterministic test bytes\n")
}

// sumsOf is a SHASUMS256 in the release script's `<hex>  ./name` form.
func sumsOf(assets ...fakeAsset) fakeAsset {
	var b strings.Builder
	for _, asset := range assets {
		digest := sha256.Sum256(asset.body)
		b.WriteString(hex.EncodeToString(digest[:]) + "  ./" + asset.name + "\n")
	}
	return fakeAsset{name: "SHASUMS256", body: []byte(b.String())}
}

func noremoteAsset(tag string) fakeAsset {
	return fakeAsset{name: noremoteAssetName, body: payload(noremoteAssetName, tag)}
}

func standardAsset(tag string) fakeAsset {
	return fakeAsset{name: standardAssetName, body: payload(standardAssetName, tag)}
}

// publishTypical publishes a pre-release, the latest stable release with
// both WSL variants, a release with no noremote launcher, and the running
// release.
func publishTypical(f *fakeGitLab) {
	rc := noremoteAsset("v0.3.0-rc1")
	f.publish("v0.3.0-rc1", rc, sumsOf(rc))
	nr, std := noremoteAsset("v0.2.0"), standardAsset("v0.2.0")
	f.publish("v0.2.0", std, nr, sumsOf(std, nr))
	only := standardAsset("v0.1.5")
	f.publish("v0.1.5", only, sumsOf(only))
	cur := noremoteAsset("v0.1.0")
	f.publish("v0.1.0", cur, sumsOf(cur))
}

func TestGitLabFeedListsResolvesAndFetchesVerified(t *testing.T) {
	f := newFakeGitLab(t)
	publishTypical(f)
	src := f.source(t, "wsl-noremote", "0.1.0")
	ctx := context.Background()

	releases, err := src.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var tags []string
	for _, r := range releases {
		tags = append(tags, r.Tag)
	}
	if strings.Join(tags, ",") != "v0.3.0-rc1,v0.2.0,v0.1.0" {
		t.Fatalf("listing = %v, want the noremote releases newest first", tags)
	}
	if !releases[0].Prerelease || releases[0].IsLatest {
		t.Errorf("v0.3.0-rc1 = %+v, want a prerelease that is not latest", releases[0])
	}
	if !releases[1].IsLatest || releases[1].IsCurrent || releases[1].IsOlder {
		t.Errorf("v0.2.0 = %+v, want latest", releases[1])
	}
	if !releases[2].IsCurrent {
		t.Errorf("v0.1.0 = %+v, want current", releases[2])
	}

	latest, err := src.Latest(ctx)
	if err != nil || latest == nil || latest.Tag != "v0.2.0" {
		t.Fatalf("Latest = %+v, %v; want v0.2.0", latest, err)
	}

	var got bytes.Buffer
	resolved, err := src.Fetch(ctx, "v0.2.0", &got, nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := payload(noremoteAssetName, "v0.2.0")
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("fetched %q, want %q", got.Bytes(), want)
	}
	digest := sha256.Sum256(want)
	if resolved.AssetName != noremoteAssetName || resolved.Digest != hex.EncodeToString(digest[:]) || resolved.Tag != "v0.2.0" {
		t.Fatalf("resolved = %+v", resolved)
	}

	calls := f.calls()
	if len(calls) == 0 {
		t.Fatal("the fake GitLab was never asked")
	}
	for _, call := range calls {
		if call.Method != http.MethodGet || call.Host != testGitLabHost || call.Token != testGitLabToken || call.Accept != "*/*" {
			t.Errorf("request = %+v, want a GET on %s with the token, accepting any type", call, testGitLabHost)
		}
		if strings.Contains(call.Path, "://") {
			t.Errorf("an absolute URL reached the API: %q", call.Path)
		}
		if strings.Contains(call.Path, standardAssetName) {
			t.Errorf("the noremote feed requested the standard launcher: %q", call.Path)
		}
	}
}

func TestGitLabFeedOffersOnlyItsOwnVariant(t *testing.T) {
	f := newFakeGitLab(t)
	publishTypical(f)
	src := f.source(t, "wsl", "0.1.0")

	releases, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var tags []string
	for _, r := range releases {
		tags = append(tags, r.Tag)
	}
	if strings.Join(tags, ",") != "v0.2.0,v0.1.5" {
		t.Fatalf("standard listing = %v, want only releases shipping %s", tags, standardAssetName)
	}
	var got bytes.Buffer
	resolved, err := src.Fetch(context.Background(), "v0.2.0", &got, nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resolved.AssetName != standardAssetName {
		t.Fatalf("standard target fetched %s", resolved.AssetName)
	}
}

func newGitLabService(t *testing.T, f *fakeGitLab, current string) *Service {
	t.Helper()
	setGitLabProject(t, testGitLabProject)
	a := New(current, Deps{})
	if err := a.Configure(updater.New(noopUpdaterHost{}), Config{
		CurrentVersion: current,
		Platform:       "wsl-noremote",
		Arch:           "amd64",
		GitLab:         f.gitlab,
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return a
}

// The passive check and the framework download go through gitlabProvider,
// which reads latest off the listing and streams the link from the API.
func TestGitLabProviderServesTheLatestCheckAndDownload(t *testing.T) {
	f := newFakeGitLab(t)
	publishTypical(f)
	a := newGitLabService(t, f, "0.1.0")
	if got := a.providerName(); got != "gitlab" {
		t.Fatalf("provider name = %q, want gitlab", got)
	}

	availability, err := a.CheckForUpdate()
	if err != nil || availability.CheckError != "" {
		t.Fatalf("CheckForUpdate = %+v, %v", availability, err)
	}
	if !availability.Available || availability.LatestVersion != "0.2.0" || availability.ReleaseNotes != "notes for v0.2.0" {
		t.Fatalf("availability = %+v, want 0.2.0 available", availability)
	}

	var progressCalls int
	var lastWritten int64
	rel := a.updater.pending
	var got bytes.Buffer
	err = verifiedProvider{inner: a.updater.provider}.Download(context.Background(), rel, &got, func(written, total int64) {
		progressCalls++
		lastWritten = written
		if total != 0 {
			t.Errorf("total = %d; GitLab links carry no size", total)
		}
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	want := payload(noremoteAssetName, "v0.2.0")
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("downloaded %q, want %q", got.Bytes(), want)
	}
	if progressCalls == 0 || lastWritten != int64(len(want)) {
		t.Fatalf("progress calls = %d, last written = %d, want %d", progressCalls, lastWritten, len(want))
	}

	if err := a.updater.handle.DownloadAndInstall(context.Background()); err != nil {
		t.Fatalf("DownloadAndInstall: %v", err)
	}
	staged, err := os.ReadFile(a.updater.handle.DownloadedPath())
	if err != nil {
		t.Fatalf("read the framework's download: %v", err)
	}
	if !bytes.Equal(staged, want) {
		t.Fatalf("framework downloaded %q, want %q", staged, want)
	}
}

func TestGitLabProviderReportsUpToDate(t *testing.T) {
	f := newFakeGitLab(t)
	publishTypical(f)
	a := newGitLabService(t, f, "0.2.0")
	availability, err := a.CheckForUpdate()
	if err != nil || availability.CheckError != "" || availability.Available {
		t.Fatalf("CheckForUpdate = %+v, %v; want up to date", availability, err)
	}
}

func TestGitLabDownloadUpdateErrorNamesTheGitLabProvider(t *testing.T) {
	f := newFakeGitLab(t)
	publishTypical(f)
	a := newGitLabService(t, f, "0.1.0")
	got := make(chan updater.ErrorInfo, 1)
	a.deps.Emit = func(name eventchan.Channel, data any) {
		if info, ok := data.(updater.ErrorInfo); ok && name == updaterErrorChannel {
			select {
			case got <- info:
			default:
			}
		}
	}
	if err := a.DownloadUpdate("v9.9.9"); err != nil {
		t.Fatalf("DownloadUpdate: %v", err)
	}
	select {
	case info := <-got:
		if info.Provider != "gitlab" || info.Stage != updater.StageCheck {
			t.Fatalf("error = %+v, want a gitlab check error", info)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no updater:error for a tag the project does not have")
	}
}

func TestGitLabFetchRejectsChecksumMismatch(t *testing.T) {
	f := newFakeGitLab(t)
	nr := noremoteAsset("v0.2.0")
	wrong := sumsOf(fakeAsset{name: noremoteAssetName, body: []byte("other bytes")})
	f.publish("v0.2.0", nr, wrong)
	src := f.source(t, "wsl-noremote", "0.1.0")

	var got bytes.Buffer
	_, err := src.Fetch(context.Background(), "v0.2.0", &got, nil)
	if err == nil || !strings.Contains(err.Error(), "published checksum") {
		t.Fatalf("Fetch error = %v, want a checksum refusal", err)
	}
}

func TestGitLabDownloadIsCapped(t *testing.T) {
	f := newFakeGitLab(t)
	big := fakeAsset{name: noremoteAssetName, body: bytes.Repeat([]byte("x"), 256<<10)}
	f.publish("v0.2.0", big, sumsOf(big))
	src := f.source(t, "wsl-noremote", "0.1.0")
	rel, err := src.resolve(context.Background(), "v0.2.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var got bytes.Buffer
	// The writer's refusal is the error itself, not a failed body read the
	// transport would report as an unreachable forge.
	err = downloadBoundedArtifact(context.Background(), src.targetable, rel, &got, nil, 1024)
	if err != errDownloadTooLarge {
		t.Fatalf("download error = %v, want errDownloadTooLarge", err)
	}
	if got.Len() > 1024 {
		t.Fatalf("wrote %d bytes past a 1024-byte cap", got.Len())
	}
}

func TestGitLabSidecarIsCapped(t *testing.T) {
	f := newFakeGitLab(t)
	nr := noremoteAsset("v0.2.0")
	sums := sumsOf(nr)
	sums.body = append(sums.body, bytes.Repeat([]byte("#"), maxChecksumBytes)...)
	f.publish("v0.2.0", nr, sums)
	src := f.source(t, "wsl-noremote", "0.1.0")

	_, err := src.resolve(context.Background(), "v0.2.0")
	if !errors.Is(err, errGitLabResponseTooLarge) {
		t.Fatalf("resolve error = %v, want the sidecar refused for size", err)
	}
}

// The cap holds whether the response declares its length, which the
// transport refuses unread, or streams past it, which the writer refuses.
func TestGitLabListingIsCapped(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run("chunked="+strconv.FormatBool(chunked), func(t *testing.T) {
			f := newFakeGitLab(t)
			f.set(testGitLabListPath, fakeRoute{body: bytes.Repeat([]byte(" "), maxReleaseListBytes+1), chunked: chunked})
			src := f.source(t, "wsl-noremote", "0.1.0")
			_, err := src.List(context.Background())
			if _, transient := errors.AsType[*forgeapi.TransientError](err); !errors.Is(err, errGitLabResponseTooLarge) || transient {
				t.Fatalf("List error = %v, want the listing refused for size", err)
			}
		})
	}
}

// A release link is data, and every request carries the user's token, so
// a link off the configured host's REST API must be refused before any
// request is made.
func TestGitLabFeedRefusesLinksOffTheAPI(t *testing.T) {
	cases := map[string]string{
		"foreign host":     "https://evil.example.net/api/v4/projects/7/packages/generic/agent-overflow/0.2.0/" + noremoteAssetName,
		"http":             "http://" + testGitLabHost + "/api/v4/projects/7/packages/generic/agent-overflow/0.2.0/" + noremoteAssetName,
		"outside /api/v4/": "https://" + testGitLabHost + "/grp/app/-/releases/v0.2.0/downloads/" + noremoteAssetName,
	}
	for name, link := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeGitLab(t)
			nr := fakeAsset{name: noremoteAssetName, body: payload(noremoteAssetName, "v0.2.0"), link: link}
			f.publish("v0.2.0", nr, sumsOf(nr))
			src := f.source(t, "wsl-noremote", "0.1.0")

			var got bytes.Buffer
			_, err := src.Fetch(context.Background(), "v0.2.0", &got, nil)
			if err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("Fetch error = %v, want the link refused", err)
			}
			for _, call := range f.calls() {
				if strings.Contains(call.Path, noremoteAssetName) {
					t.Fatalf("the API was asked for the refused link: %q", call.Path)
				}
			}

			// The same rule holds for the sidecar.
			sideFeed := newFakeGitLab(t)
			sums := sumsOf(nr)
			sums.link = strings.Replace(link, noremoteAssetName, "SHASUMS256", 1)
			plain := noremoteAsset("v0.2.0")
			sideFeed.publish("v0.2.0", plain, sums)
			if _, err := sideFeed.source(t, "wsl-noremote", "0.1.0").resolve(context.Background(), "v0.2.0"); err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("resolve error = %v, want the sidecar link refused", err)
			}
			for _, call := range sideFeed.calls() {
				if strings.Contains(call.Path, "SHASUMS256") {
					t.Fatalf("the API was asked for the refused sidecar: %q", call.Path)
				}
			}
		})
	}
}

func TestGitLabAPIPath(t *testing.T) {
	feed := &gitlabFeed{host: testGitLabHost, project: "grp/app"}
	accepted := map[string]string{
		"https://gitlab.example.com/api/v4/projects/7/packages/generic/agent-overflow/0.2.0/" + noremoteAssetName: "projects/7/packages/generic/agent-overflow/0.2.0/" + noremoteAssetName,
		"https://GitLab.Example.com/api/v4/projects/grp%2Fapp/packages/generic/a/1/SHASUMS256":                    "projects/grp%2Fapp/packages/generic/a/1/SHASUMS256",
	}
	for link, want := range accepted {
		got, err := feed.apiPath(link)
		if err != nil || got != want {
			t.Errorf("apiPath(%q) = %q, %v; want %q", link, got, err, want)
		}
	}
	refused := []string{
		"",
		"projects/7/x",
		"//gitlab.example.com/api/v4/projects/7/x",
		"ftp://gitlab.example.com/api/v4/projects/7/x",
		"http://gitlab.example.com/api/v4/projects/7/x",
		"https://gitlab.example.com.evil.net/api/v4/projects/7/x",
		"https://evil.net/api/v4/projects/7/x",
		"https://gitlab.example.com:8443/api/v4/projects/7/x",
		"https://user:secret@gitlab.example.com/api/v4/projects/7/x",
		"https://gitlab.example.com/api/v4/projects/7/x?private_token=1",
		"https://gitlab.example.com/api/v4/projects/7/x#frag",
		"https://gitlab.example.com/api/v4/",
		"https://gitlab.example.com/api/v40/projects/7/x",
		"https://gitlab.example.com/api/v4/projects/../../x",
		"https://gitlab.example.com/api/v4/projects/%2e%2e/x",
		"https://gitlab.example.com/api/v4/projects//x",
		"https://gitlab.example.com/api/v4/projects/:id/x",
		"https://gitlab.example.com/grp/app/-/releases/v1/downloads/x",
		"https://user:secret@gitlab.example.com/api/v4/%zz",
	}
	for _, link := range refused {
		got, err := feed.apiPath(link)
		if err == nil {
			t.Errorf("apiPath(%q) = %q, want a refusal", link, got)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("refusal of %q repeats its credentials: %v", link, err)
		}
	}
}

func TestGitLabErrorsAreMapped(t *testing.T) {
	cases := []struct {
		name     string
		route    fakeRoute
		setup    string // the SetupError kind, if one
		noAccess bool
		text     string
	}{
		{name: "signed out", route: fakeRoute{status: 401, body: []byte(`{"message":"401 Unauthorized"}`)}, setup: forgeapi.SetupUnauthenticated,
			text: "sign in with glab to check for updates: run `glab auth login --hostname " + testGitLabHost + "`"},
		{name: "no access", route: fakeRoute{status: 404, body: []byte(`{"message":"404 Project Not Found"}`)}, noAccess: true, text: "no access to " + testGitLabProject},
		{name: "other not found", route: fakeRoute{status: 404, body: []byte(`{"message":"404 Not Found"}`)}, text: "HTTP 404"},
		{name: "other", route: fakeRoute{status: 500, body: []byte(`{"message":"500 Internal Server Error"}`)}, text: "HTTP 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitLab(t)
			f.set(testGitLabListPath, tc.route)
			_, err := f.source(t, "wsl-noremote", "0.1.0").List(context.Background())
			if err == nil {
				t.Fatal("List succeeded on a failed API call")
			}
			setup, isSetup := errors.AsType[*forgeapi.SetupError](err)
			if isSetup != (tc.setup != "") || (isSetup && (setup.Kind != tc.setup || setup.Binary != "glab")) {
				t.Errorf("error = %#v, want setup kind %q", err, tc.setup)
			}
			if errors.Is(err, ErrGitLabNoAccess) != tc.noAccess {
				t.Errorf("error = %v, ErrGitLabNoAccess = %v", err, !tc.noAccess)
			}
			if !strings.Contains(err.Error(), tc.text) {
				t.Errorf("error %q does not say %q", err, tc.text)
			}
			if strings.Contains(err.Error(), testGitLabToken) {
				t.Errorf("error %q carries the token", err)
			}
		})
	}
}

// The user-facing state for a signed-out glab: the check's result says so.
func TestGitLabSignedOutReachesTheCheckResult(t *testing.T) {
	f := newFakeGitLab(t)
	f.set(testGitLabListPath, fakeRoute{status: 401, body: []byte(`{"message":"401 Unauthorized"}`)})
	a := newGitLabService(t, f, "0.1.0")
	availability, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if !availability.Supported || !strings.Contains(availability.CheckError, "sign in with glab") {
		t.Fatalf("availability = %+v, want the sign-in message as CheckError", availability)
	}
}

// missingGlab is a token source whose glab is not installed.
type missingGlab struct{}

func (missingGlab) Token(_ context.Context, forge, _ string) ([]byte, forgeapi.SourceInfo, error) {
	return nil, forgeapi.SourceInfo{}, forgeapi.MissingCLIError(forge, nil)
}

func TestGitLabMissingGlabIsReported(t *testing.T) {
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", TokenSource: missingGlab{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	src, err := newReleaseSource(Config{
		CurrentVersion: "0.1.0",
		Platform:       "wsl-noremote",
		Arch:           "amd64",
		GitLab:         func(host string) GitLabClient { return transportGitLab{client: svc.GitLab(host)} },
	}, testGitLabProject)
	if err != nil {
		t.Fatalf("newReleaseSource: %v", err)
	}
	_, err = src.List(context.Background())
	setup, ok := errors.AsType[*forgeapi.SetupError](err)
	if !ok || setup.Kind != forgeapi.SetupMissing || setup.Binary != "glab" {
		t.Fatalf("List error = %#v, want a missing-glab SetupError", err)
	}
	if want := "glab is not installed: install the GitLab CLI and run `glab auth login --hostname " + testGitLabHost + "` to receive updates"; !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("error %q, want it to end %q", err, want)
	}
}

func TestParseGitLabProject(t *testing.T) {
	valid := map[string]gitlabSource{
		"gitlab.com/fortressinfosec/agent-overflow": {host: "gitlab.com", project: "fortressinfosec/agent-overflow"},
		"GitLab.Example.com/a/b_c/d.e":              {host: "gitlab.example.com", project: "a/b_c/d.e"},
	}
	for raw, want := range valid {
		got, err := parseGitLabProject(raw)
		if err != nil || got != want {
			t.Errorf("parseGitLabProject(%q) = %+v, %v; want %+v", raw, got, err, want)
		}
	}
	invalid := []string{
		"",
		"gitlab.com",
		"gitlab.com/project",
		"https://gitlab.com/a/b",
		"gitlab.com:443/a/b",
		"-gitlab.com/a/b",
		"gitlab..com/a/b",
		"gitlab.com/a/../b",
		"gitlab.com/a/b/",
		"gitlab.com//b",
		"gitlab.com/a b/c",
		"gitlab.com/a/.b",
		"gitlab.com/a/b?x",
		"gitlab.com/a/b:id",
	}
	for _, raw := range invalid {
		if got, err := parseGitLabProject(raw); err == nil {
			t.Errorf("parseGitLabProject(%q) = %+v, want an error", raw, got)
		}
	}
}

func TestGitLabSourceConfigurationIsValidated(t *testing.T) {
	config := Config{CurrentVersion: "0.1.0", Platform: "wsl-noremote", Arch: "amd64"}
	if _, err := newReleaseSource(config, testGitLabProject); err == nil || !strings.Contains(err.Error(), "GitLab client") {
		t.Fatalf("source without a client = %v, want refused", err)
	}
	config.GitLab = func(string) GitLabClient { return nil }
	if _, err := newReleaseSource(config, testGitLabProject); err == nil || !strings.Contains(err.Error(), "no GitLab client for "+testGitLabHost) {
		t.Fatalf("source with a nil client = %v, want refused", err)
	}
	config.GitLab = newFakeGitLab(t).gitlab
	if _, err := newReleaseSource(config, "gitlab.com/only-one"); err == nil || !strings.Contains(err.Error(), "HOST/NAMESPACE/PROJECT") {
		t.Fatalf("malformed project = %v, want refused", err)
	}
	setGitLabProject(t, "not a project")
	if err := New("0.1.0", Deps{}).Configure(updater.New(noopUpdaterHost{}), config); err == nil {
		t.Fatal("Configure accepted a malformed linked project")
	}
}
